"""Record the PS5 kernel log from klogsrv for a fixed time.

Read-only: connects to the existing klogsrv port and writes what arrives to
logs/. Run it alongside send.py to see what the kernel says about a payload.
"""
import argparse
import socket
import sys
import time
from datetime import datetime
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", default="192.168.0.134")
    parser.add_argument("--port", type=int, default=3232)
    parser.add_argument("--seconds", type=float, default=90)
    parser.add_argument("--log-dir", type=Path, default=Path(__file__).resolve().parent.parent / "logs")
    args = parser.parse_args()

    args.log_dir.mkdir(parents=True, exist_ok=True)
    path = args.log_dir / f"{datetime.now():%Y%m%d-%H%M%S}-klog.log"
    deadline = time.monotonic() + args.seconds
    total = 0
    with path.open("wb") as log, socket.create_connection((args.host, args.port), timeout=5) as conn:
        print(f"recording klog to {path} for {args.seconds:.0f}s", flush=True)
        while (remaining := deadline - time.monotonic()) > 0:
            conn.settimeout(min(remaining, 5))
            try:
                chunk = conn.recv(65536)
            except socket.timeout:
                continue
            if not chunk:
                print("klogsrv closed the connection", flush=True)
                break
            log.write(chunk)
            log.flush()
            total += len(chunk)
    print(f"klog: {total} bytes", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
