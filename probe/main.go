// Command probe checks, one stage at a time, whether the Go runtime works on a
// jailbroken PS5. Each stage prints a result line before the next starts, so
// the last line received shows how far the runtime got.
//
// Stages follow what tailscaled needs: threads, timers, memory, signals,
// files, TCP through the kqueue netpoller, outbound TCP and UDP, and DNS.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const workDir = "/data/ps5_tailscale"

// Where goshim maps this binary (build.ps1 -TextAddr), with room to grow.
const (
	goImageStart = 0x4000000000
	goImageEnd   = 0x4100000000
)

var failures int

// spinSink keeps the preemption test's loop from being optimized away.
var spinSink uint64

// say writes straight to fd 1 so a line is never lost in a buffer when the
// process dies.
func say(format string, args ...any) {
	os.Stdout.WriteString(fmt.Sprintf(format, args...) + "\n")
}

// stage runs fn with a recover and a watchdog. A stage that hangs is reported
// once its deadline passes, if timers still work.
func stage(name string, timeout time.Duration, fn func() (string, error)) {
	say("[ .. ] %s", name)
	done := make(chan struct{})
	watchdog := time.AfterFunc(timeout, func() {
		say("[HANG] %s: no result after %s", name, timeout)
	})
	var detail string
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
			close(done)
		}()
		detail, err = fn()
	}()
	<-done
	watchdog.Stop()
	if err != nil {
		failures++
		say("[FAIL] %s: %v", name, err)
		return
	}
	say("[ OK ] %s: %s", name, detail)
}

func main() {
	say("probe: Go runtime reached main (%s %s/%s)", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	stage("runtime info", 2*time.Second, func() (string, error) {
		return fmt.Sprintf("NumCPU=%d GOMAXPROCS=%d pagesize=%d args=%q",
			runtime.NumCPU(), runtime.GOMAXPROCS(0), os.Getpagesize(), os.Args), nil
	})

	stage("Sony-range syscall guard", 2*time.Second, func() (string, error) {
		// 563 is getrandom on FreeBSD 12+ and a Sony call on the PS5. The
		// patched syscall package must refuse it without entering the kernel.
		_, _, errno := syscall.Syscall(563, 0, 0, 0)
		if errno != syscall.ENOSYS {
			return "", fmt.Errorf("syscall 563 returned errno %d, want ENOSYS", errno)
		}
		return "syscall 563 refused with ENOSYS", nil
	})

	stage("clock and sleep", 2*time.Second, func() (string, error) {
		start := time.Now()
		time.Sleep(50 * time.Millisecond)
		elapsed := time.Since(start)
		if elapsed < 40*time.Millisecond || elapsed > time.Second {
			return "", fmt.Errorf("slept %s for a 50ms request", elapsed)
		}
		return fmt.Sprintf("wall=%s slept=%s", start.UTC().Format(time.RFC3339), elapsed), nil
	})

	stage("OS threads", 5*time.Second, func() (string, error) {
		// LockOSThread plus a blocking sleep forces the runtime to create
		// new OS threads (thr_new) rather than reuse one.
		const n = 8
		var wg sync.WaitGroup
		var ran atomic.Int32
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				runtime.LockOSThread()
				defer runtime.UnlockOSThread()
				time.Sleep(20 * time.Millisecond)
				ran.Add(1)
			}()
		}
		wg.Wait()
		return fmt.Sprintf("%d locked goroutines finished", ran.Load()), nil
	})

	stage("memory and GC", 10*time.Second, func() (string, error) {
		blocks := make([][]byte, 0, 64)
		for i := 0; i < 64; i++ {
			b := make([]byte, 1<<20)
			b[len(b)-1] = byte(i)
			blocks = append(blocks, b)
		}
		blocks = nil
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return fmt.Sprintf("allocated 64 MiB, after GC HeapSys=%d MiB NumGC=%d", m.HeapSys>>20, m.NumGC), nil
	})

	stage("files", 5*time.Second, func() (string, error) {
		// stat and getdirentries changed layout in FreeBSD 12; the PS5
		// kernel descends from FreeBSD 11.
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			return "", err
		}
		path := filepath.Join(workDir, "probe.txt")
		want := "written by probe at " + time.Now().UTC().Format(time.RFC3339)
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			return "", err
		}
		got, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if string(got) != want {
			return "", fmt.Errorf("read back %q", got)
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if info.Size() != int64(len(want)) {
			return "", fmt.Errorf("stat size %d, wrote %d", info.Size(), len(want))
		}
		entries, err := os.ReadDir("/")
		if err != nil {
			return "", fmt.Errorf("readdir /: %w", err)
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return fmt.Sprintf("%s ok, stat size %d; / has %d entries: %s",
			path, info.Size(), len(names), strings.Join(names, " ")), nil
	})

	stage("os.Pipe (pipe2 remapped to pipe)", 3*time.Second, func() (string, error) {
		r, w, err := os.Pipe()
		if err != nil {
			return "", err
		}
		defer r.Close()
		defer w.Close()
		if _, err := w.Write([]byte("pipe")); err != nil {
			return "", err
		}
		buf := make([]byte, 4)
		if _, err := r.Read(buf); err != nil {
			return "", err
		}
		return fmt.Sprintf("round-tripped %q through fds %d/%d", buf, r.Fd(), w.Fd()), nil
	})

	stage("loopback TCP (netpoller)", 5*time.Second, func() (string, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", err
		}
		defer ln.Close()
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			buf := make([]byte, 4)
			if _, err := c.Read(buf); err == nil {
				c.Write(buf)
			}
		}()
		c, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
		if err != nil {
			return "", err
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Write([]byte("ping")); err != nil {
			return "", err
		}
		buf := make([]byte, 4)
		if _, err := c.Read(buf); err != nil {
			return "", err
		}
		return fmt.Sprintf("echoed %q via %s", buf, ln.Addr()), nil
	})

	rawNetStages()

	stage("UDP DNS query via 1.1.1.1", 8*time.Second, func() (string, error) {
		// A fixed resolver separates "UDP works" from "/etc/resolv.conf
		// exists". WireGuard needs UDP.
		r := &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "udp", "1.1.1.1:53")
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		addrs, err := r.LookupHost(ctx, "controlplane.tailscale.com")
		if err != nil {
			return "", err
		}
		return strings.Join(addrs, " "), nil
	})

	goNetStages()
	udpStepStages()
	tlsStages()

	signalStages()

	say("PROBE DONE: %d stage(s) failed", failures)
	os.Exit(failures)
}

// rawNetStages talks to the kernel with blocking sockets and no netpoller,
// so each result is the PS5 kernel's own answer. The router (a DNS
// forwarder on this LAN) separates "LAN works" from "internet works".
func rawNetStages() {
	router := [4]byte{192, 168, 0, 1}
	cloudflare := [4]byte{1, 1, 1, 1}
	stage("raw UDP DNS to router 192.168.0.1", 10*time.Second, func() (string, error) { return rawDNS(router) })
	stage("raw UDP DNS to 1.1.1.1", 10*time.Second, func() (string, error) { return rawDNS(cloudflare) })
	stage("raw TCP connect to router 192.168.0.1:80", 10*time.Second, func() (string, error) { return rawTCP(router, 80) })
	stage("raw TCP connect to 1.1.1.1:443", 10*time.Second, func() (string, error) { return rawTCP(cloudflare, 443) })
}

// dnsQuery is an A query for controlplane.tailscale.com with recursion desired.
var dnsQuery = []byte{
	0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	12, 'c', 'o', 'n', 't', 'r', 'o', 'l', 'p', 'l', 'a', 'n', 'e',
	9, 't', 'a', 'i', 'l', 's', 'c', 'a', 'l', 'e',
	3, 'c', 'o', 'm', 0,
	0x00, 0x01, 0x00, 0x01,
}

func errnoText(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Sprintf("%v (errno %d)", err, int(errno))
	}
	return err.Error()
}

func sockaddrText(sa syscall.Sockaddr) string {
	if in4, ok := sa.(*syscall.SockaddrInet4); ok {
		return fmt.Sprintf("%d.%d.%d.%d:%d", in4.Addr[0], in4.Addr[1], in4.Addr[2], in4.Addr[3], in4.Port)
	}
	return fmt.Sprintf("%T", sa)
}

func rawDNS(ip [4]byte) (string, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return "", fmt.Errorf("socket: %s", errnoText(err))
	}
	defer syscall.Close(fd)
	tv := syscall.Timeval{Sec: 3}
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		return "", fmt.Errorf("setsockopt SO_RCVTIMEO: %s", errnoText(err))
	}
	server := &syscall.SockaddrInet4{Port: 53, Addr: ip}
	var steps []string
	if err := syscall.Connect(fd, server); err != nil {
		// Without a connected socket, try the unconnected path too.
		steps = append(steps, "connect: "+errnoText(err))
		if err := syscall.Sendto(fd, dnsQuery, 0, server); err != nil {
			return "", fmt.Errorf("%s; sendto: %s", steps[0], errnoText(err))
		}
		steps = append(steps, "sendto ok")
	} else {
		local, _ := syscall.Getsockname(fd)
		steps = append(steps, "connect ok from "+sockaddrText(local))
		if _, err := syscall.Write(fd, dnsQuery); err != nil {
			return "", fmt.Errorf("%s; write: %s", steps[0], errnoText(err))
		}
		steps = append(steps, "query sent")
	}
	buf := make([]byte, 512)
	n, _, err := syscall.Recvfrom(fd, buf, 0)
	if err != nil {
		return "", fmt.Errorf("%s; recv: %s", strings.Join(steps, "; "), errnoText(err))
	}
	if n < 12 {
		return "", fmt.Errorf("%s; %d-byte reply is too short", strings.Join(steps, "; "), n)
	}
	answers := int(buf[6])<<8 | int(buf[7])
	return fmt.Sprintf("%s; %d-byte reply, %d answer(s)", strings.Join(steps, "; "), n, answers), nil
}

func rawTCP(ip [4]byte, port int) (string, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return "", fmt.Errorf("socket: %s", errnoText(err))
	}
	// A blocking connect can outlast this stage; the fd is then left open
	// rather than closed under a pending syscall.
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- syscall.Connect(fd, &syscall.SockaddrInet4{Port: port, Addr: ip}) }()
	select {
	case err := <-done:
		defer syscall.Close(fd)
		if err != nil {
			return "", fmt.Errorf("connect failed after %s: %s", time.Since(start).Round(time.Millisecond), errnoText(err))
		}
		local, _ := syscall.Getsockname(fd)
		return fmt.Sprintf("connected in %s from %s", time.Since(start).Round(time.Millisecond), sockaddrText(local)), nil
	case <-time.After(6 * time.Second):
		return "", errors.New("blocking connect still pending after 6s: no reply and no error from the kernel")
	}
}

// signalStages runs last because a broken signal path kills the process:
// the first PS5 run with a nil dereference ended with no Go output at all.
func signalStages() {
	stage("SIGSEGV handler registered", 2*time.Second, func() (string, error) {
		// sigaction(SIGSEGV, NULL, &old) through the FreeBSD 11 entry 416.
		// FreeBSD struct sigaction: handler, int flags, 16-byte mask.
		var old [4]uint64
		_, _, errno := syscall.RawSyscall(416, uintptr(syscall.SIGSEGV), 0, uintptr(unsafe.Pointer(&old)))
		if errno != 0 {
			return "", fmt.Errorf("sigaction: errno %d", errno)
		}
		handler := old[0]
		where := "outside the Go image"
		if handler >= goImageStart && handler < goImageEnd {
			where = "inside the Go image"
		}
		return fmt.Sprintf("handler=%#x (%s) flags=%#x", handler, where, uint32(old[1])), nil
	})

	stage("signal delivery (SIGUSR1 to self)", 4*time.Second, func() (string, error) {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGUSR1)
		defer signal.Stop(c)
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGUSR1); err != nil {
			return "", fmt.Errorf("kill: %w", err)
		}
		select {
		case sig := <-c:
			return fmt.Sprintf("received %v through signal.Notify", sig), nil
		case <-time.After(2 * time.Second):
			return "", errors.New("signal not received within 2s")
		}
	})

	stage("nil dereference becomes panic", 2*time.Second, func() (string, error) {
		// The runtime turns SIGSEGV into a panic; this checks signal
		// delivery and return from the handler.
		var p *int
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			_ = *p
		}()
		if recovered == nil {
			return "", errors.New("no panic")
		}
		return fmt.Sprintf("recovered %q", fmt.Sprint(recovered)), nil
	})

	stage("async preemption", 30*time.Second, func() (string, error) {
		// A loop with no function calls can only be interrupted by async
		// preemption, a signal sent to its thread. The loop is bounded, so
		// without preemption the stage is slow rather than stuck: a 20ms
		// sleep then takes as long as the whole spin.
		prev := runtime.GOMAXPROCS(1)
		defer runtime.GOMAXPROCS(prev)
		spun := make(chan time.Duration)
		go func() {
			start := time.Now()
			for i := uint64(0); i < 2_000_000_000; i++ {
				spinSink += i
			}
			spun <- time.Since(start)
		}()
		start := time.Now()
		time.Sleep(20 * time.Millisecond)
		slept := time.Since(start)
		spin := <-spun
		if slept > 500*time.Millisecond {
			return "", fmt.Errorf("20ms sleep took %s (spin %s): spinning goroutine was never preempted", slept, spin)
		}
		return fmt.Sprintf("20ms sleep took %s while another goroutine spun for %s", slept, spin), nil
	})
}
