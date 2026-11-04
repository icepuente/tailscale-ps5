"""Send a payload to the PS5 ELF loader and stream its output.

Output is printed as it arrives and appended to a log file, so a stall is
visible immediately. A host-side timeout closes the connection; it does not
stop the process on the console.
"""
import argparse
import socket
import sys
import time
from datetime import datetime
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("elf", type=Path)
    parser.add_argument("--host", default="192.168.0.134")
    parser.add_argument("--port", type=int, default=9021)
    parser.add_argument("--timeout", type=float, default=60, help="total seconds to wait for output")
    parser.add_argument("--idle", type=float, default=20, help="give up after this many silent seconds")
    parser.add_argument("--log-dir", type=Path, default=Path(__file__).resolve().parent.parent / "logs")
    parser.add_argument("--until", help="disconnect once the output contains this text; the payload keeps running")
    args = parser.parse_args()

    payload = args.elf.read_bytes()
    if not payload.startswith(b"\x7fELF"):
        parser.error(f"{args.elf} is not an ELF")
    args.log_dir.mkdir(parents=True, exist_ok=True)
    log_path = args.log_dir / f"{datetime.now():%Y%m%d-%H%M%S}-{args.elf.stem}.log"

    with log_path.open("wb") as log, socket.create_connection((args.host, args.port), timeout=5) as conn:
        conn.sendall(payload)
        conn.shutdown(socket.SHUT_WR)
        print(f"sent {len(payload)} bytes to {args.host}:{args.port}; logging to {log_path}", flush=True)
        deadline = time.monotonic() + args.timeout
        seen = b""
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                print(f"\n[send.py] {args.timeout:.0f}s total timeout reached", flush=True)
                return 2
            conn.settimeout(min(args.idle, remaining))
            try:
                chunk = conn.recv(65536)
            except socket.timeout:
                if remaining > args.idle:
                    print(f"\n[send.py] no output for {args.idle:.0f}s; giving up", flush=True)
                    return 3
                continue
            if not chunk:
                print("\n[send.py] connection closed by console", flush=True)
                return 0
            log.write(chunk)
            log.flush()
            sys.stdout.write(chunk.decode("utf-8", "replace"))
            sys.stdout.flush()
            if args.until:
                seen = (seen + chunk)[-65536:]
                if args.until.encode() in seen:
                    print(f"\n[send.py] saw {args.until!r}; disconnecting, payload keeps running", flush=True)
                    return 0


if __name__ == "__main__":
    sys.exit(main())
