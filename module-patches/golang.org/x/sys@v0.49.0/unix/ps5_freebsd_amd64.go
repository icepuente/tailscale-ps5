// ps5_tailscale: FreeBSD 11 replacements for syscalls that FreeBSD 12+
// renumbered to 532 and above, a range that holds Sony syscalls on the PS5
// kernel. zsyscall_freebsd_amd64.go routes pipe2, Fstat, Fstatat,
// getdirentries and accept4 here. All but accept4 reuse the standard
// library's syscall package, which ps5_tailscale patches the same way
// (goroot-patches/syscall/ps5_freebsd_amd64.go).

package unix

import (
	"syscall"
	"unsafe"
)

// Stat_t here and syscall.Stat_t are both the FreeBSD 12 layout. Both
// lines fail to build if their sizes ever differ.
var _ [unsafe.Sizeof(Stat_t{}) - unsafe.Sizeof(syscall.Stat_t{})]byte
var _ [unsafe.Sizeof(syscall.Stat_t{}) - unsafe.Sizeof(Stat_t{})]byte

func ps5Fstat(fd int, st *Stat_t) error {
	return syscall.Fstat(fd, (*syscall.Stat_t)(unsafe.Pointer(st)))
}

func ps5Fstatat(fd int, path string, st *Stat_t, flags int) error {
	return syscall.Fstatat(fd, path, (*syscall.Stat_t)(unsafe.Pointer(st)), flags)
}

func ps5Getdirentries(fd int, buf []byte, basep *uint64) (int, error) {
	return syscall.Getdirentries(fd, buf, (*uintptr)(unsafe.Pointer(basep)))
}

func ps5Pipe2(p *[2]_C_int, flags int) error {
	var fds [2]int
	if err := syscall.Pipe2(fds[:], flags); err != nil {
		return err
	}
	p[0], p[1] = _C_int(fds[0]), _C_int(fds[1])
	return nil
}

// ps5Accept4 is accept (30) plus fcntl; this package's RawSockaddrAny is its
// own type, so it cannot reuse syscall.Accept4.
func ps5Accept4(fd int, rsa *RawSockaddrAny, addrlen *_Socklen, flags int) (int, error) {
	r0, _, e1 := Syscall(SYS_ACCEPT, uintptr(fd), uintptr(unsafe.Pointer(rsa)), uintptr(unsafe.Pointer(addrlen)))
	if e1 != 0 {
		return -1, errnoErr(e1)
	}
	nfd := int(r0)
	if flags&SOCK_CLOEXEC != 0 {
		if _, err := fcntl(nfd, F_SETFD, FD_CLOEXEC); err != nil {
			Close(nfd)
			return -1, err
		}
	}
	if flags&SOCK_NONBLOCK != 0 {
		if err := SetNonblock(nfd, true); err != nil {
			Close(nfd)
			return -1, err
		}
	}
	return nfd, nil
}
