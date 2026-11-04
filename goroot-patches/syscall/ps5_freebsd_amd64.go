// ps5_tailscale: FreeBSD 11 replacements for syscalls that FreeBSD 12+
// renumbered to 532 and above, a range that holds Sony syscalls on the PS5
// kernel. zsyscall_freebsd_amd64.go routes pipe2, Fstat, Fstatat,
// getdirentries and accept4 here. Results are converted to the FreeBSD 12
// types the rest of Go expects.
//
// The flags pipe2 and accept4 would set atomically are applied with fcntl
// afterwards; the window only matters for fork/exec, which payloads do not do.

package syscall

import "unsafe"

const (
	sys11Accept        = 30
	sys11Pipe          = 42 // freebsd10_pipe: the two fds come back in rax and rdx
	sys11Fstat         = 189
	sys11Getdirentries = 196
	sys11Fstatat       = 493
)

// stat11 is struct freebsd11_stat on amd64 (120 bytes).
type stat11 struct {
	Dev      uint32
	Ino      uint32
	Mode     uint16
	Nlink    uint16
	Uid      uint32
	Gid      uint32
	Rdev     uint32
	Atim     Timespec
	Mtim     Timespec
	Ctim     Timespec
	Size     int64
	Blocks   int64
	Blksize  int32
	Flags    uint32
	Gen      uint32
	Lspare   int32
	Birthtim Timespec
}

// Compile-time size check: both fail to build unless stat11 is 120 bytes.
var _ [120 - unsafe.Sizeof(stat11{})]byte
var _ [unsafe.Sizeof(stat11{}) - 120]byte

func (st *Stat_t) fromStat11(o *stat11) {
	*st = Stat_t{
		Dev:           uint64(o.Dev),
		Ino:           uint64(o.Ino),
		Nlink:         uint64(o.Nlink),
		Mode:          o.Mode,
		Uid:           o.Uid,
		Gid:           o.Gid,
		Rdev:          uint64(o.Rdev),
		Atimespec:     o.Atim,
		Mtimespec:     o.Mtim,
		Ctimespec:     o.Ctim,
		Birthtimespec: o.Birthtim,
		Size:          o.Size,
		Blocks:        o.Blocks,
		Blksize:       o.Blksize,
		Flags:         o.Flags,
		Gen:           uint64(o.Gen),
	}
}

func ps5Fstat(fd int, st *Stat_t) error {
	var o stat11
	_, _, e1 := Syscall(sys11Fstat, uintptr(fd), uintptr(unsafe.Pointer(&o)), 0)
	if e1 != 0 {
		return errnoErr(e1)
	}
	st.fromStat11(&o)
	return nil
}

func ps5Fstatat(fd int, path *byte, st *Stat_t, flags int) error {
	var o stat11
	_, _, e1 := Syscall6(sys11Fstatat, uintptr(fd), uintptr(unsafe.Pointer(path)), uintptr(unsafe.Pointer(&o)), uintptr(flags), 0, 0)
	if e1 != 0 {
		return errnoErr(e1)
	}
	st.fromStat11(&o)
	return nil
}

// ps5SetFdFlags applies close-on-exec and non-blocking after the fact.
func ps5SetFdFlags(fd int, cloexec, nonblock bool) error {
	if cloexec {
		if _, err := fcntl(fd, F_SETFD, FD_CLOEXEC); err != nil {
			return err
		}
	}
	if nonblock {
		fl, err := fcntl(fd, F_GETFL, 0)
		if err != nil {
			return err
		}
		if _, err := fcntl(fd, F_SETFL, fl|O_NONBLOCK); err != nil {
			return err
		}
	}
	return nil
}

func ps5Pipe2(p *[2]_C_int, flags int) error {
	r, w, e1 := RawSyscall(sys11Pipe, 0, 0, 0)
	if e1 != 0 {
		return errnoErr(e1)
	}
	for _, fd := range []int{int(r), int(w)} {
		if err := ps5SetFdFlags(fd, flags&O_CLOEXEC != 0, flags&O_NONBLOCK != 0); err != nil {
			Close(int(r))
			Close(int(w))
			return err
		}
	}
	p[0], p[1] = _C_int(r), _C_int(w)
	return nil
}

func ps5Accept4(fd int, rsa *RawSockaddrAny, addrlen *_Socklen, flags int) (int, error) {
	r0, _, e1 := Syscall(sys11Accept, uintptr(fd), uintptr(unsafe.Pointer(rsa)), uintptr(unsafe.Pointer(addrlen)))
	if e1 != 0 {
		return -1, errnoErr(e1)
	}
	nfd := int(r0)
	if err := ps5SetFdFlags(nfd, flags&SOCK_CLOEXEC != 0, flags&SOCK_NONBLOCK != 0); err != nil {
		Close(nfd)
		return -1, err
	}
	return nfd, nil
}

// FreeBSD 11 struct dirent: fileno u32 @0, reclen u16 @4, type u8 @6,
// namlen u8 @7, name @8. Go's Dirent (FreeBSD 12) puts the name at 24 and
// 8-byte aligns records. A record grows by less than 3x (12 -> 32 bytes at
// worst), so reading at most a third of buf always fits after conversion.
const (
	dirent11NameOff = 8
	direntNameOff   = int(unsafe.Offsetof(Dirent{}.Name))
	dirblksiz       = 512
)

func ps5Getdirentries(fd int, buf []byte, basep *uint64) (int, error) {
	oldLen := len(buf) / 3 / dirblksiz * dirblksiz
	if oldLen < dirblksiz {
		return 0, EINVAL
	}
	old := make([]byte, oldLen)
	var base int64
	r0, _, e1 := Syscall6(sys11Getdirentries, uintptr(fd), uintptr(unsafe.Pointer(&old[0])), uintptr(oldLen), uintptr(unsafe.Pointer(&base)), 0, 0)
	if e1 != 0 {
		return 0, errnoErr(e1)
	}
	if basep != nil {
		*basep = uint64(base)
	}
	in, out := 0, 0
	for in+dirent11NameOff <= int(r0) {
		src := old[in:]
		reclen := int(*(*uint16)(unsafe.Pointer(&src[4])))
		if reclen < dirent11NameOff || in+reclen > int(r0) {
			return 0, EIO
		}
		namlen := int(src[7])
		if dirent11NameOff+namlen > reclen {
			return 0, EIO
		}
		newlen := (direntNameOff + namlen + 1 + 7) &^ 7
		if out+newlen > len(buf) {
			return 0, EIO // cannot happen given oldLen; refuse rather than drop entries
		}
		// Set header fields one by one: assigning a whole Dirent would write
		// all 280 bytes and run past a record shorter than that.
		d := (*Dirent)(unsafe.Pointer(&buf[out]))
		d.Fileno = uint64(*(*uint32)(unsafe.Pointer(&src[0])))
		d.Off = 0
		d.Reclen = uint16(newlen)
		d.Type = src[6]
		d.Pad0 = 0
		d.Namlen = uint16(namlen)
		d.Pad1 = 0
		name := buf[out+direntNameOff : out+newlen]
		clear(name)
		copy(name, src[dirent11NameOff:dirent11NameOff+namlen])
		in += reclen
		out += newlen
	}
	return out, nil
}
