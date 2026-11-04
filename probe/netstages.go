package main

import (
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"

	// The PS5 has no CA bundle where Go looks; use Go's bundled roots.
	_ "golang.org/x/crypto/x509roots/fallback"
)

// tsProxy is tools/ts_proxy.py on the PC; the router blocks the console's
// internet TCP, so Tailscale's control and DERP connections go through it.
const tsProxy = "http://192.168.0.193:3128"

// goNetStages repeats the raw-socket checks through Go's net package, which
// uses non-blocking sockets and the netpoller.
func goNetStages() {
	stage("Go TCP dial to router 192.168.0.1:80", 5*time.Second, func() (string, error) {
		start := time.Now()
		c, err := net.DialTimeout("tcp", "192.168.0.1:80", 3*time.Second)
		if err != nil {
			return "", fmt.Errorf("after %s: %w", time.Since(start).Round(time.Millisecond), err)
		}
		defer c.Close()
		return fmt.Sprintf("connected in %s from %s", time.Since(start).Round(time.Millisecond), c.LocalAddr()), nil
	})

	var udp net.Conn
	stage("Go UDP dial to router 192.168.0.1:53", 5*time.Second, func() (string, error) {
		start := time.Now()
		c, err := net.DialTimeout("udp", "192.168.0.1:53", 3*time.Second)
		if err != nil {
			return "", fmt.Errorf("after %s: %w", time.Since(start).Round(time.Millisecond), err)
		}
		udp = c
		return fmt.Sprintf("dialed in %s from %s", time.Since(start).Round(time.Millisecond), c.LocalAddr()), nil
	})
	if udp == nil {
		return
	}
	defer udp.Close()
	stage("Go UDP DNS exchange with router", 5*time.Second, func() (string, error) {
		udp.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := udp.Write(dnsQuery); err != nil {
			return "", fmt.Errorf("write: %w", err)
		}
		buf := make([]byte, 512)
		n, err := udp.Read(buf)
		if err != nil {
			return "", fmt.Errorf("read: %w", err)
		}
		return fmt.Sprintf("%d-byte reply", n), nil
	})
}

// kevent11 is the FreeBSD 11 struct kevent used by syscall 363.
type kevent11 struct {
	ident  uint64
	filter int16
	flags  uint16
	fflags uint32
	data   int64
	udata  uint64
}

const (
	evfiltRead  = -1
	evfiltWrite = -2
	evAdd       = 0x1
	evClear     = 0x20
	evError     = 0x4000
	sysFcntl    = 92
	sysKevent11 = 363
)

func kevent(kq int, changes, events []kevent11, timeout time.Duration) (int, error) {
	ts := syscall.NsecToTimespec(int64(timeout))
	var cp, ep unsafe.Pointer
	if len(changes) > 0 {
		cp = unsafe.Pointer(&changes[0])
	}
	if len(events) > 0 {
		ep = unsafe.Pointer(&events[0])
	}
	n, _, errno := syscall.Syscall6(sysKevent11, uintptr(kq), uintptr(cp), uintptr(len(changes)),
		uintptr(ep), uintptr(len(events)), uintptr(unsafe.Pointer(&ts)))
	if errno != 0 {
		return -1, errno
	}
	return int(n), nil
}

func keventText(evs []kevent11) string {
	var parts []string
	for _, ev := range evs {
		s := fmt.Sprintf("filter=%d flags=%#x data=%d", ev.filter, ev.flags, ev.data)
		if ev.flags&evError != 0 {
			s += fmt.Sprintf(" (EV_ERROR errno %d)", ev.data)
		}
		parts = append(parts, s)
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

// udpStepStages walks the steps Go's net package takes for a UDP dial and
// exchange, one syscall at a time, printing each result.
func udpStepStages() {
	stage("Go-style UDP socket, step by step (router)", 10*time.Second, func() (string, error) {
		var steps []string
		fail := func(format string, args ...any) (string, error) {
			return "", fmt.Errorf("%s | "+format, append([]any{strings.Join(steps, " | ")}, args...)...)
		}
		fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, 0)
		if err != nil {
			return fail("socket(SOCK_DGRAM|SOCK_NONBLOCK|SOCK_CLOEXEC): %s", errnoText(err))
		}
		defer syscall.Close(fd)
		fl, _, e1 := syscall.Syscall(sysFcntl, uintptr(fd), syscall.F_GETFL, 0)
		fdfl, _, e2 := syscall.Syscall(sysFcntl, uintptr(fd), syscall.F_GETFD, 0)
		steps = append(steps, fmt.Sprintf("socket ok, O_NONBLOCK=%v (errno %d) FD_CLOEXEC=%v (errno %d)",
			fl&syscall.O_NONBLOCK != 0, e1, fdfl&syscall.FD_CLOEXEC != 0, e2))

		kq, err := syscall.Kqueue()
		if err != nil {
			return fail("kqueue: %s", errnoText(err))
		}
		defer syscall.Close(kq)
		// Go's netpollopen registers read and write, edge-triggered.
		changes := []kevent11{
			{ident: uint64(fd), filter: evfiltRead, flags: evAdd | evClear},
			{ident: uint64(fd), filter: evfiltWrite, flags: evAdd | evClear},
		}
		if _, err := kevent(kq, changes, nil, 0); err != nil {
			return fail("kevent register: %s", errnoText(err))
		}
		steps = append(steps, "registered")

		err = syscall.Connect(fd, &syscall.SockaddrInet4{Port: 53, Addr: [4]byte{192, 168, 0, 1}})
		if err != nil {
			steps = append(steps, "connect: "+errnoText(err))
		} else {
			steps = append(steps, "connect ok")
		}

		evs := make([]kevent11, 4)
		n, err := kevent(kq, nil, evs, time.Second)
		if err != nil {
			return fail("kevent wait: %s", errnoText(err))
		}
		steps = append(steps, fmt.Sprintf("after connect %d event(s) %s", n, keventText(evs[:n])))

		if _, err := syscall.Write(fd, dnsQuery); err != nil {
			return fail("write: %s", errnoText(err))
		}
		n, err = kevent(kq, nil, evs, 3*time.Second)
		if err != nil {
			return fail("kevent wait: %s", errnoText(err))
		}
		steps = append(steps, fmt.Sprintf("after write %d event(s) %s", n, keventText(evs[:n])))
		buf := make([]byte, 512)
		m, err := syscall.Read(fd, buf)
		if err != nil {
			return fail("read: %s", errnoText(err))
		}
		steps = append(steps, fmt.Sprintf("read %d bytes", m))
		return strings.Join(steps, " | "), nil
	})
}

// tlsStages checks randomness and TLS through the Tailscale proxy. A
// crypto/rand failure is fatal to the process, so it only runs once
// /dev/urandom (its fallback when getrandom is refused) is known to work.
func tlsStages() {
	urandomOK := false
	stage("/dev/urandom readable", 3*time.Second, func() (string, error) {
		f, err := os.Open("/dev/urandom")
		if err != nil {
			return "", err
		}
		defer f.Close()
		b := make([]byte, 32)
		if _, err := io.ReadFull(f, b); err != nil {
			return "", err
		}
		urandomOK = true
		return fmt.Sprintf("read %d bytes, first %x", len(b), b[:4]), nil
	})
	if !urandomOK {
		say("[SKIP] crypto/rand and TLS: /dev/urandom unusable, crypto/rand would be fatal")
		return
	}
	stage("crypto/rand", 3*time.Second, func() (string, error) {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		return fmt.Sprintf("read %d bytes, first %x", len(b), b[:4]), nil
	})

	proxy, _ := url.Parse(tsProxy)
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxy)},
	}
	stage("HTTPS to controlplane.tailscale.com via proxy", 15*time.Second, func() (string, error) {
		resp, err := client.Get("https://controlplane.tailscale.com/key?v=71")
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("status %s: %q", resp.Status, body)
		}
		return fmt.Sprintf("%s, TLS %s, body %q", resp.Status, tlsVersion(resp), body), nil
	})
	stage("proxy refuses a non-Tailscale host", 15*time.Second, func() (string, error) {
		resp, err := client.Get("https://www.playstation.com/")
		if err == nil {
			resp.Body.Close()
			return "", fmt.Errorf("reached www.playstation.com (%s); the allowlist is not working", resp.Status)
		}
		if !strings.Contains(err.Error(), "Forbidden") {
			return "", fmt.Errorf("failed, but not with 403 from the proxy: %w", err)
		}
		return "refused with 403 Forbidden", nil
	})
}

func tlsVersion(resp *http.Response) string {
	if resp.TLS == nil {
		return "none"
	}
	switch resp.TLS.Version {
	case 0x0304:
		return "1.3"
	case 0x0303:
		return "1.2"
	}
	return fmt.Sprintf("%#x", resp.TLS.Version)
}
