# Tailscale on a jailbroken PS5

Puts a jailbroken PS5 on your [Tailscale](https://tailscale.com) tailnet as its own device, and forwards console services over it. You can then send payloads to the console from any of your devices, at home or away. It runs Tailscale's [`tsnet`](https://pkg.go.dev/tailscale.com/tsnet) library natively on the console, built with a patched Go toolchain.

> **Status: working, early.** Tested on a PS5 on firmware 13.60, jailbroken with Y2JB, with payloads loaded by pldmgr and elfldr v0.26. Built with Go 1.27.3, Tailscale v1.104.5 and `golang.org/x/sys` v0.49.0. The node joins the tailnet, reconnects after restarts without a new login, and serves elfldr. One earlier run hung after a few minutes for unknown reasons; a heartbeat in the log is there to catch it if it happens again.

[How it works](#how-it-works) · [Requirements](#requirements) · [Setup](#setup) · [Using it](#using-it) · [What had to change in Go](#what-had-to-change-in-go) · [Limitations](#limitations-and-known-issues) · [Layout](#layout)

## Features

- **The PS5 as a tailnet device** (`ps5`, MagicDNS name included), with its own Tailscale IP. Login is a one-time link you approve in your browser; the identity is kept on the console.
- **elfldr over Tailscale:** `tools/send.py payload.elf --host ps5` from any tailnet device, with the payload's output streamed back.
- **Works with a console that is blocked from the internet.** Tailscale's TCP connections (control server, DERP relays) go through a small allowlist proxy on another LAN machine, which only ever lets through `*.tailscale.com` and `*.tailscale.io`. Your router can keep blocking Sony.
- **Direct WireGuard on the LAN:** UDP is not proxied. On the tested network, PC to PS5 was a direct connection with about 2 ms ping.
- **Status page and log over the tailnet:** `http://ps5/` and `http://ps5/log`, with a heartbeat line every minute.
- **Autoload through pldmgr**, or start it by hand. It runs until the console reboots. A stop payload ends it cleanly, and a duplicate check stops two copies from fighting over one identity.
- **Private by default:** Tailscale's diagnostic log upload is switched off.

## How it works

```
 tailnet devices ──WireGuard (UDP, direct when possible)──┐
                                                          ▼
 ┌──────────────── PS5 (payload process) ─────────────────────────────┐
 │ goshim (C, ps5-payload-sdk)                                        │
 │   SDK crt: escape sandbox, allow syscalls from any address          │
 │   maps the embedded Go ELF, jumps to _rt0_amd64_freebsd             │
 │ ps5ts (Go, freebsd/amd64, patched runtime)                          │
 │   tsnet, userspace networking (gVisor netstack)                     │
 │   :80 status + /log   :9021 ──► 127.0.0.1:9021 (elfldr)              │
 └───────────────┬────────────────────────────────────────────────────┘
                 │ HTTPS_PROXY (CONNECT, TCP 443 only)
                 ▼
 LAN host: tools/ts_proxy.py (console-only client, *.tailscale.com/io only)
                 │
                 ▼
 controlplane.tailscale.com, DERP relays
```

Three problems had to be solved to get Go running on the console:

1. **A payload is not a normal FreeBSD process.** elfldr calls a payload's entry point with its own argument block. The kernel also only accepts `syscall` instructions from an allowed address range, and Go makes raw syscalls from its own code. `shim/goshim.c` is an ordinary SDK payload: the SDK's startup code widens the syscall range and escapes the sandbox, then goshim maps the Go executable at its link address, makes it executable with the SDK's `mprotect`, builds the FreeBSD `argc/argv/envp/auxv` block on a fresh stack, and jumps to Go's entry point.
2. **Go speaks FreeBSD 12+, the PS5 kernel is FreeBSD 11, and Sony reused the newer syscall numbers.** Numbers from about 532 upward are Sony's own calls on the PS5. Go's `kevent` (560), `fstat` (551), `getdirentries` (554), `pipe2` (542), `accept4` (541), `getrandom` (563) and others land on them. The patches remap the ones Go needs to their FreeBSD 11 versions and convert the older struct layouts. A guard in the syscall entry points refuses anything at 532 or above with `ENOSYS`, so a missed call fails visibly instead of running Sony code. See [What had to change in Go](#what-had-to-change-in-go).
3. **The console is cut off from the internet** (on purpose, on the tested network). `ts_proxy.py` on another LAN machine tunnels only Tailscale's TCP 443 connections.

## Requirements

- **A jailbroken PS5 with an ELF loader** on port 9021 (elfldr). Tested on firmware 13.60 with Y2JB and pldmgr.
- **A LAN machine for the proxy** that is always on and can reach the internet, with Python 3.8 or newer. Tested on a Proxmox host. It is not needed if your console is allowed to reach Tailscale's servers directly.
- **A Tailscale account.**
- **To build** (Windows, PowerShell; other systems need small changes to the scripts):
  - [ps5-payload-sdk](https://github.com/ps5-payload-dev/sdk) (tested with v0.43);
  - LLVM with `clang` and `ld.lld` (tested with LLVM 19 from Visual Studio Build Tools);
  - Go **1.27.3**. `build.ps1` uses `tools/go/` if present, otherwise `go` from PATH. The overlay patches are tied to this Go version.
  - Python 3, for the tools.

## Setup

### 1. Run the proxy on a LAN machine

Edit the `ExecStart` line in [`deploy/pve/ts-proxy.service`](deploy/pve/ts-proxy.service): set `--listen` to that machine's LAN address and `--allow-client` to the console's address. Then install it:

```sh
mkdir -p /opt/ps5_tailscale
install -m 644 ts_proxy.py /opt/ps5_tailscale/
install -m 644 ts-proxy.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now ts-proxy
journalctl -u ts-proxy -f      # every request is logged as ALLOW / DENY / REFUSE
```

The unit runs the proxy as an unprivileged throwaway user with a read-only system. **On Proxmox, don't use port 3128:** Proxmox's own `spiceproxy` already listens there. The unit uses 3130. If a host firewall is active, allow the console to reach that port.

### 2. Configure and build

Set your values at the top of [`ps5ts/main.go`](ps5ts/main.go):

| Constant | Meaning | In this repo |
| --- | --- | --- |
| `proxy` | The proxy from step 1 | `http://192.168.0.164:3130` |
| `hostname` | Tailnet device name | `ps5` |
| `forwardPorts` | Console ports to forward | `9021` (elfldr) |
| `stateDir`, `logPath` | Tailscale state and log on the console | `/data/ps5_tailscale/...` |

Then build:

```powershell
./build.ps1 -Package ./ps5ts -DeadlineSeconds 0 -Sdk <path to ps5-payload-sdk> -Llvm <path to LLVM bin>
./payloads/build.ps1 -Sdk <path to ps5-payload-sdk> -Llvm <path to LLVM bin>
```

`build.ps1` builds the Go program for `freebsd/amd64` with the patches applied, wraps it in goshim, and writes `build/ps5ts.elf` (about 31 MB). `-DeadlineSeconds N` makes the payload exit after N seconds whatever happens, which is useful for test runs; `0` means it runs until the console reboots. The payload script builds `build/hello.elf`, a test payload, and `build/stop_ps5ts.elf`.

### 3. First start and login

```powershell
python tools/send.py build/ps5ts.elf --host <console LAN IP> --until "ps5ts: UP as" --timeout 600
```

On the first start, the output shows a line like `========== LOGIN: open https://login.tailscale.com/a/... ==========`. Open it and approve the device. `send.py` disconnects once the node is up, and `ps5ts` keeps running. Later starts reuse the saved identity and come up in a few seconds without a link.

### 4. Optional: start it automatically with pldmgr

In pldmgr's web page (`http://<console IP>:8084`): **Upload ELF Payload** → `build/ps5ts.elf`, switch on autoload, and move it to the end of the autoload sequence. It then starts with every jailbreak.

## Using it

| What | How |
| --- | --- |
| Send a payload over Tailscale | `python tools/send.py payload.elf --host ps5` |
| Status | `http://ps5/` |
| Log, including the heartbeat | `http://ps5/log` (also `/data/ps5_tailscale/ps5ts.log` on the console, rotated at 8 MB) |
| Stop it | `python tools/send.py build/stop_ps5ts.elf --host <console IP>`: asks `ps5ts` to exit, and kills it if it hasn't after 15 s |
| Test the forward | `python tools/send.py build/hello.elf --host ps5` prints `hello from a payload, pid N` |

Other console services, such as FTP: if a machine on your LAN already advertises the LAN as a Tailscale **subnet route**, everything on the console is reachable through it, including FTP in passive mode. Only single-connection protocols can be added to `forwardPorts` directly; FTP's passive mode hands out the console's LAN address and a second port.

Security: anything forwarded is reachable by every device your tailnet policy allows. Forwarding elfldr means those devices can run code on the console. With Tailscale's default allow-all policy, and only your own devices in the tailnet, that is usually acceptable. Otherwise, restrict the ports in your tailnet policy.

## What had to change in Go

All changes are copies of the original files with small, commented edits marked `ps5_tailscale`. They are applied at build time; the Go installation and module cache are never modified.

| File | Change |
| --- | --- |
| `goroot-patches/runtime/sys_freebsd_amd64.s` | `kevent` uses FreeBSD 11's syscall 363 instead of 560 |
| `goroot-patches/runtime/defs_freebsd_amd64.go` | 32-byte FreeBSD 11 `struct kevent` (no `ext[4]`) |
| `goroot-patches/syscall/asm_unix_amd64.s`, `asm9_unix2_amd64.s` | Guard: syscall numbers ≥ 532 return `ENOSYS` without entering the kernel |
| `goroot-patches/syscall/ps5_freebsd_amd64.go` (new) | FreeBSD 11 `fstat` (189), `fstatat` (493), `getdirentries` (196), `accept` (30) and `pipe` (42), with conversion to the FreeBSD 12 `Stat_t`/`Dirent` layouts and `fcntl` for the `accept4`/`pipe2` flags |
| `goroot-patches/syscall/zsyscall_freebsd_amd64.go` | Routes those five functions to the file above |
| `goroot-patches/os/executable_sysctl.go` | `os.Executable` falls back to an absolute `argv[0]`: a payload has no executable file, and tsnet refuses to start without a name |
| `module-patches/golang.org/x/sys@v0.49.0/unix/` | The same five remaps for `x/sys/unix`, which forwards its other syscalls to the guarded `syscall` package |

`build.ps1` applies `goroot-patches/` with `go build -overlay`. Go refuses overlays inside the module cache, so `x/sys` is copied to `build/modules/` and patched there, and [`ps5ts/go.mod`](ps5ts/go.mod) points to that copy with a `replace` directive. Other pieces:

- **`crypto/rand`** works unpatched: `getrandom` is refused with `ENOSYS`, and Go falls back to `/dev/urandom`, which payloads can read.
- **Root certificates** come from `golang.org/x/crypto/x509roots/fallback`, because the console has no CA bundle where Go looks.
- **Async preemption is off** (`GODEBUG=asyncpreemptoff=1`, set by goshim). Signals reach Go, but the PS5 kernel does not let a handler change where the interrupted thread resumes.

## Limitations and known issues

- **A nil-pointer dereference kills the process** instead of becoming a recoverable panic, for the same reason that async preemption is off.
- **One unexplained hang:** in one test run, `ps5ts` stopped responding after about 3 minutes, though its process stayed alive. Later runs have been stable, with flat memory. If it happens again, the heartbeat in the log shows when.
- **The console's internet-facing UDP probes (STUN) failed on the tested network**, so devices outside the LAN reach the console through a DERP relay via the proxy. That works, but it is slower than a direct connection.
- **Large UDP buffers are refused** by the kernel (`no buffer space available`); Tailscale continues with the defaults.
- **Go's built-in DNS resolver** timed out against a public resolver in testing, even though raw UDP worked. Tailscale doesn't need it, because the proxy resolves host names.
- **It doesn't survive a reboot by itself:** use pldmgr's autoload, or start it again after each jailbreak.
- **`statfs`, `getfsstat`, `mknodat` and other FreeBSD 12+ calls are refused** (`ENOSYS`). Code that needs them gets an error.

## Diagnosing with the probe

`probe/` is the program that mapped all of this out. It tests, one step at a time, what a Go network program needs on the console: threads, timers, memory, files, pipes, TCP and UDP (raw and through Go), randomness, TLS through the proxy, and finally signals, which can be fatal. Run `./build.ps1` (the default package is `./probe`, with a 90-second dead-man switch) and send `build/probe.elf`. Each step prints `[ OK ]`, `[FAIL]` or `[HANG]`.

## Layout

```
build.ps1                 build a Go package into a PS5 payload (patches, goshim, link)
shim/goshim.c, blob.S     the C payload that loads and enters the Go executable
ps5ts/                    the Tailscale node (tsnet), with its go.mod
probe/                    the step-by-step runtime probe
goroot-patches/           patched Go standard library files (applied with -overlay)
module-patches/           patched golang.org/x/sys files (copied to build/modules)
payloads/                 hello.c, stop_ps5ts.c and their build script
tools/send.py             send a payload to elfldr and stream its output (--until to detach)
tools/ts_proxy.py         the Tailscale-only CONNECT proxy
tools/klog.py             record the console's kernel log from klogsrv (port 3232)
deploy/pve/               systemd unit for the proxy
```

## Credits and license

- [Tailscale](https://github.com/tailscale/tailscale) and [`tsnet`](https://pkg.go.dev/tailscale.com/tsnet) (BSD-3-Clause).
- [The Go project](https://go.dev) and `golang.org/x/sys` (BSD-3-Clause). The files in `goroot-patches/` and `module-patches/` are modified copies and keep their original copyright notices.
- [ps5-payload-sdk](https://github.com/ps5-payload-dev/sdk) and [elfldr](https://github.com/ps5-payload-dev/elfldr) by John Törnblom (GPL-3.0). goshim links the SDK's runtime.
- [pldmgr](https://github.com/itsPLK/ps5-payload-manager) by itsPLK, for autoload.

This project is licensed under the GNU General Public License v3.0; see [LICENSE](LICENSE). Use it only on hardware you own.
