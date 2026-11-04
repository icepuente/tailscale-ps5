// Command ps5ts joins a tailnet from a jailbroken PS5 using tsnet in
// userspace networking mode, serves a status page on its own tailnet
// address, and forwards selected console services.
//
// Settings come from confPath, which must exist. If the console's internet
// TCP is blocked, set proxy= to tools/ts_proxy.py on a LAN machine: control
// and DERP connections then go through it (HTTPS_PROXY). WireGuard traffic
// is UDP and is not proxied.
//
// It runs until the console reboots. Its log goes to logPath (and to the
// sender's connection while that is open) and is served at http://ps5/log.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	// The PS5 has no CA bundle where Go looks; use Go's bundled roots.
	_ "golang.org/x/crypto/x509roots/fallback"
	"tailscale.com/envknob"
	"tailscale.com/tsnet"
)

const (
	confPath = "/data/ps5_tailscale/ps5ts.conf"
	stateDir = "/data/ps5_tailscale/state"
	logPath  = "/data/ps5_tailscale/ps5ts.log"
	// /system_tmp is cleared at every boot, so neither file can outlive the
	// process across a reboot. Both are used by payloads/stop_ps5ts.c.
	pidPath  = "/system_tmp/ps5ts.pid"
	stopPath = "/system_tmp/ps5ts.stop"
	logMax   = 8 << 20 // then rotate to logPath.1
	logTail  = 256 << 10
)

// config holds the settings from confPath.
type config struct {
	proxy    string // HTTPS proxy for Tailscale's TCP connections; empty: direct
	hostname string // tailnet device name
	ports    []int  // console ports reachable as <hostname>:<port> on the tailnet
}

const exampleConf = `# ps5ts settings. Lines are key = value; # starts a comment.
# HTTPS proxy for Tailscale's TCP connections (tools/ts_proxy.py). Leave
# empty if the console can reach the internet directly.
proxy = http://192.168.0.10:3130
# Device name on the tailnet.
hostname = ps5
# Console ports to forward, comma-separated. 9021 is elfldr.
forward = 9021
`

// loadConfig reads confPath. The file is required, so that nothing
// network-specific is built into the binary.
func loadConfig() (config, error) {
	b, err := os.ReadFile(confPath)
	if err != nil {
		return config{}, err
	}
	return parseConfig(string(b))
}

// parseConfig parses the key = value format of confPath. tools/ps5ts_config.py
// writes it; config_test.go checks the two agree.
func parseConfig(text string) (config, error) {
	cfg := config{hostname: "ps5", ports: []int{9021}}
	for n, line := range strings.Split(text, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return cfg, fmt.Errorf("line %d: expected key = value", n+1)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "proxy":
			if value != "" {
				if u, err := url.Parse(value); err != nil || u.Scheme != "http" || u.Host == "" {
					return cfg, fmt.Errorf("line %d: proxy must look like http://host:port", n+1)
				}
			}
			cfg.proxy = value
		case "hostname":
			if value == "" {
				return cfg, fmt.Errorf("line %d: hostname is empty", n+1)
			}
			cfg.hostname = value
		case "forward":
			cfg.ports = nil
			for _, f := range strings.Split(value, ",") {
				if f = strings.TrimSpace(f); f == "" {
					continue
				}
				p, err := strconv.Atoi(f)
				if err != nil || p < 1 || p > 65535 || p == 80 {
					return cfg, fmt.Errorf("line %d: bad port %q (1-65535, not 80)", n+1, f)
				}
				cfg.ports = append(cfg.ports, p)
			}
		default:
			return cfg, fmt.Errorf("line %d: unknown key %q", n+1, key)
		}
	}
	return cfg, nil
}

var started = time.Now()

var (
	logMu    sync.Mutex
	logFile  *os.File
	logSize  int64
	stdoutOK = true
)

// logLine appends to the log file, rotating it at logMax, and copies the
// line to fd 1 (the sender's connection) until a write there fails.
func logLine(toStdout bool, format string, args ...any) {
	line := time.Now().UTC().Format("2006-01-02 15:04:05 ") + fmt.Sprintf(format, args...) + "\n"
	logMu.Lock()
	defer logMu.Unlock()
	if logFile != nil {
		if logSize+int64(len(line)) > logMax {
			logFile.Close()
			os.Rename(logPath, logPath+".1")
			logFile, _ = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			logSize = 0
		}
		if logFile != nil {
			n, _ := logFile.WriteString(line)
			logSize += int64(n)
		}
	}
	if toStdout && stdoutOK {
		if _, err := os.Stdout.WriteString(line); err != nil {
			stdoutOK = false
		}
	}
}

// say logs ps5ts's own lines to the file and the sender.
func say(format string, args ...any) { logLine(true, format, args...) }

func main() {
	// The sender disconnecting must not end the program: with SIGPIPE
	// ignored, writes to the closed fd 1 just fail.
	signal.Ignore(syscall.SIGPIPE)
	os.MkdirAll(stateDir, 0o700)
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		logFile = f
	}
	// Two copies would share one Tailscale identity and fight over it, e.g.
	// pldmgr's autoload plus a manual send.py.
	if b, err := os.ReadFile(pidPath); err == nil {
		var other int
		if _, err := fmt.Sscan(string(b), &other); err == nil && other > 0 && other != os.Getpid() &&
			syscall.Kill(other, 0) == nil {
			say("ps5ts: already running as pid %d (%s); exiting", other, pidPath)
			os.Exit(0)
		}
	}
	os.Remove(stopPath)
	os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)
	go watchStop()
	cfg, err := loadConfig()
	if err != nil {
		say("ps5ts: config %s: %v", confPath, err)
		say("ps5ts: create it, for example:\n%s", exampleConf)
		os.Remove(pidPath)
		os.Exit(1)
	}
	proxyText := cfg.proxy
	if proxyText == "" {
		proxyText = "none (direct)"
	}
	say("ps5ts: Go %s %s/%s, pid %d, log %s, proxy %s, hostname %s, forward %v",
		runtime.Version(), runtime.GOOS, runtime.GOARCH, os.Getpid(), logPath, proxyText, cfg.hostname, cfg.ports)
	if cfg.proxy != "" {
		os.Setenv("HTTPS_PROXY", cfg.proxy)
	}
	// No log uploads to log.tailscale.io: less traffic leaving the console.
	envknob.SetNoLogsNoSupport()

	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		say("ps5ts: state dir: %v", err)
		os.Exit(1)
	}
	s := &tsnet.Server{
		Hostname: cfg.hostname,
		Dir:      stateDir,
		// tsnet's own log is chatty: file only.
		Logf: func(format string, args ...any) {
			logLine(false, "tsnet: "+format, args...)
		},
		UserLogf: func(format string, args ...any) {
			say("tsnet USER: "+format, args...)
		},
	}
	defer s.Close()

	// Up blocks until the node is running, so watch for the login link
	// separately and print it prominently.
	go watchLogin(s)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	st, err := s.Up(ctx)
	if err != nil {
		say("ps5ts: Up failed: %v", err)
		os.Exit(1)
	}
	say("ps5ts: UP as %s, tailnet IPs %v", st.Self.DNSName, st.TailscaleIPs)
	go heartbeat()

	for _, port := range cfg.ports {
		if err := forward(s, port); err != nil {
			say("ps5ts: forward %d: %v", port, err)
			os.Exit(1)
		}
	}

	ln, err := s.Listen("tcp", ":80")
	if err != nil {
		say("ps5ts: listen :80: %v", err)
		os.Exit(1)
	}
	say("ps5ts: serving http://%s/ (status) and http://%s/log", cfg.hostname, cfg.hostname)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		say("ps5ts: HTTP %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		fmt.Fprintf(w, "hello from PS5\nGo %s %s/%s, up %s\n",
			runtime.Version(), runtime.GOOS, runtime.GOARCH, time.Since(started).Round(time.Second))
	})
	mux.HandleFunc("/log", serveLogTail)
	err = http.Serve(ln, mux)
	say("ps5ts: serve ended: %v", err)
}

// watchStop exits cleanly once stopPath appears; payloads/stop_ps5ts.c
// creates it.
func watchStop() {
	for range time.Tick(3 * time.Second) {
		if _, err := os.Stat(stopPath); err == nil {
			say("ps5ts: stop requested, exiting")
			os.Remove(stopPath)
			os.Remove(pidPath)
			os.Exit(0)
		}
	}
}

// heartbeat logs a line a minute, so a silent death shows when it happened
// and what memory looked like before it.
func heartbeat() {
	for range time.Tick(time.Minute) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		say("ps5ts: alive, up %s, goroutines %d, heap %d MiB, sys %d MiB, gc %d",
			time.Since(started).Round(time.Second), runtime.NumGoroutine(), m.HeapAlloc>>20, m.Sys>>20, m.NumGC)
	}
}

// serveLogTail returns the last logTail bytes of the log file.
func serveLogTail(w http.ResponseWriter, r *http.Request) {
	f, err := os.Open(logPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > logTail {
		f.Seek(fi.Size()-logTail, io.SeekStart)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.Copy(w, f)
}

// forward accepts tailnet connections on port and relays each to the same
// port on the console's loopback interface.
func forward(s *tsnet.Server, port int) error {
	ln, err := s.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	say("ps5ts: forwarding tailnet :%d -> 127.0.0.1:%d", port, port)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				say("ps5ts: accept :%d: %v", port, err)
				return
			}
			go relay(c, port)
		}
	}()
	return nil
}

// closeWriter is implemented by both tsnet's and the OS's TCP connections.
type closeWriter interface{ CloseWrite() error }

func relay(remote net.Conn, port int) {
	defer remote.Close()
	from := remote.RemoteAddr()
	local, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		say("ps5ts: :%d from %s: local dial: %v", port, from, err)
		return
	}
	defer local.Close()
	say("ps5ts: :%d from %s: connected", port, from)
	start := time.Now()

	// Copy both ways. When one side finishes sending, pass the half-close
	// on: elfldr starts a payload only after the sender shuts down writing.
	var wg sync.WaitGroup
	var up, down int64
	pipe := func(dst, src net.Conn, n *int64) {
		defer wg.Done()
		*n, _ = io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			cw.CloseWrite()
		}
	}
	wg.Add(2)
	go pipe(local, remote, &up)
	go pipe(remote, local, &down)
	wg.Wait()
	say("ps5ts: :%d from %s: closed after %s, %d bytes in, %d bytes out",
		port, from, time.Since(start).Round(time.Millisecond), up, down)
}

// watchLogin prints the login URL once each time it changes, until the
// node no longer needs login.
func watchLogin(s *tsnet.Server) {
	lc, err := s.LocalClient()
	if err != nil {
		say("ps5ts: local client: %v", err)
		return
	}
	var last string
	for {
		time.Sleep(3 * time.Second)
		st, err := lc.StatusWithoutPeers(context.Background())
		if err != nil {
			continue
		}
		if st.AuthURL != "" && st.AuthURL != last {
			last = st.AuthURL
			say("========== LOGIN: open %s ==========", st.AuthURL)
		}
		if st.BackendState == "Running" {
			return
		}
	}
}
