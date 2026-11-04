"""Tailscale-only HTTPS proxy for the internet-blocked PS5.

The router keeps the console off the internet. tailscaled honours
HTTPS_PROXY for its TCP connections (control plane, DERP relays), so this
proxy runs on a LAN machine and tunnels only those:

- clients: only the addresses in --allow-client (the console)
- method: only CONNECT, so the proxy never sees TLS contents
- destinations: only host names ending in an allowed suffix, port 443;
  IP literals are refused, so the allowlist cannot be bypassed

Every request is printed with its decision.
"""
import argparse
import asyncio
import sys
from datetime import datetime

ALLOWED_SUFFIXES = (".tailscale.com", ".tailscale.io")


def log(msg):
    print(f"{datetime.now():%H:%M:%S} {msg}", flush=True)


def allowed_host(host):
    host = host.lower().rstrip(".")
    if not host or host.replace(".", "").isdigit() or ":" in host:
        return False
    return any(host.endswith(s) or host == s[1:] for s in ALLOWED_SUFFIXES)


async def pipe(reader, writer):
    try:
        while data := await reader.read(65536):
            writer.write(data)
            await writer.drain()
    except (ConnectionError, asyncio.CancelledError):
        pass
    finally:
        writer.close()


async def handle(reader, writer, allow_clients):
    peer = writer.get_extra_info("peername")[0]
    try:
        if peer not in allow_clients:
            log(f"REFUSE client {peer}")
            return
        head = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), 10)
        method, target, _ = head.split(b"\r\n", 1)[0].decode("latin-1").split(" ", 2)
        host, _, port = target.rpartition(":")
        if method != "CONNECT" or port != "443" or not allowed_host(host):
            log(f"DENY   {peer} {method} {target}")
            writer.write(b"HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
            await writer.drain()
            return
        try:
            up_reader, up_writer = await asyncio.wait_for(asyncio.open_connection(host, 443), 10)
        except (OSError, asyncio.TimeoutError) as e:
            log(f"FAIL   {peer} CONNECT {target}: {e}")
            writer.write(b"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
            await writer.drain()
            return
        log(f"ALLOW  {peer} CONNECT {target}")
        writer.write(b"HTTP/1.1 200 Connection established\r\n\r\n")
        await writer.drain()
        await asyncio.gather(pipe(reader, up_writer), pipe(up_reader, writer))
    except (asyncio.TimeoutError, asyncio.IncompleteReadError, ValueError, ConnectionError) as e:
        log(f"DROP   {peer}: {type(e).__name__}")
    finally:
        writer.close()


async def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--listen", default="192.168.0.193")
    parser.add_argument("--port", type=int, default=3128)
    parser.add_argument("--allow-client", action="append", default=None)
    args = parser.parse_args()
    clients = set(args.allow_client or ["192.168.0.134"])
    server = await asyncio.start_server(lambda r, w: handle(r, w, clients), args.listen, args.port)
    log(f"listening on {args.listen}:{args.port}; clients {sorted(clients)}; hosts *{' *'.join(ALLOWED_SUFFIXES)}")
    async with server:
        await server.serve_forever()


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        sys.exit(0)
