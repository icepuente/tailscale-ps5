// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build freebsd || dragonfly || netbsd

package os

import (
	"syscall"
	"unsafe"
)

// ps5_tailscale: a PS5 payload has no executable file, so the sysctl below
// fails with ENOENT. goshim passes an absolute argv[0]; report that instead,
// since callers (tsnet, tailscale's version and logging) only use the name.
func executable() (string, error) {
	path, err := executableSysctl()
	if err != nil && len(Args) > 0 && len(Args[0]) > 0 && Args[0][0] == '/' {
		return Args[0], nil
	}
	return path, err
}

func executableSysctl() (string, error) {
	n := uintptr(0)
	// get length
	_, _, err := syscall.Syscall6(syscall.SYS___SYSCTL, uintptr(unsafe.Pointer(&executableMIB[0])), 4, 0, uintptr(unsafe.Pointer(&n)), 0, 0)
	if err != 0 {
		return "", err
	}
	if n == 0 { // shouldn't happen
		return "", nil
	}
	buf := make([]byte, n)
	_, _, err = syscall.Syscall6(syscall.SYS___SYSCTL, uintptr(unsafe.Pointer(&executableMIB[0])), 4, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)), 0, 0)
	if err != 0 {
		return "", err
	}
	if n == 0 { // shouldn't happen
		return "", nil
	}
	return string(buf[:n-1]), nil
}
