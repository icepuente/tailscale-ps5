"""Write the config files tailscale-ps5 needs.

  console    write ps5ts.conf to the console over FTP (/data/ps5_tailscale/)
  show       print the console's current ps5ts.conf
  proxy-unit write the systemd unit for tools/ts_proxy.py on the proxy machine

Settings are checked the same way ps5ts checks them, so a file written here
is never rejected at startup. Standard library only.

Examples:
  python tools/ps5ts_config.py console --host 192.168.0.134 --proxy http://192.168.0.164:3130
  python tools/ps5ts_config.py console --host 192.168.0.134 --proxy "" --dry-run
  python tools/ps5ts_config.py show --host 192.168.0.134
  python tools/ps5ts_config.py proxy-unit --listen 192.168.0.164 --allow-client 192.168.0.134 -o ts-proxy.service
"""
import argparse
import ftplib
import io
import ipaddress
import sys
from datetime import datetime
from pathlib import Path
from urllib.parse import urlparse

CONF_DIR = "/data/ps5_tailscale"
CONF_PATH = CONF_DIR + "/ps5ts.conf"
UNIT_TEMPLATE = Path(__file__).resolve().parent.parent / "deploy" / "pve" / "ts-proxy.service"


def check_proxy(value):
    if value == "":
        return value
    u = urlparse(value)
    if u.scheme != "http" or not u.hostname or u.port is None or u.path not in ("", "/"):
        raise argparse.ArgumentTypeError("proxy must look like http://host:port (or \"\" for none)")
    return value


def check_hostname(value):
    value = value.strip()
    if not value or any(c.isspace() or c == "#" for c in value):
        raise argparse.ArgumentTypeError("hostname must be one word without '#'")
    return value


def check_ports(value):
    ports = []
    for part in value.split(","):
        part = part.strip()
        if not part:
            continue
        if not part.isdigit() or not 1 <= int(part) <= 65535 or int(part) == 80:
            raise argparse.ArgumentTypeError(f"bad port {part!r} (1-65535, not 80: the status page uses it)")
        ports.append(int(part))
    return ports


def check_ip(value):
    try:
        return str(ipaddress.ip_address(value))
    except ValueError:
        raise argparse.ArgumentTypeError(f"{value!r} is not an IP address")


def render_conf(proxy, hostname, ports):
    stamp = datetime.now().strftime("%Y-%m-%d %H:%M")
    return (
        f"# ps5ts settings, written by tools/ps5ts_config.py on {stamp}.\n"
        "# Lines are key = value; # starts a comment.\n"
        "\n"
        "# HTTPS proxy for Tailscale's TCP connections (tools/ts_proxy.py); empty: direct.\n"
        f"proxy = {proxy}\n"
        "\n"
        "# Device name on the tailnet.\n"
        f"hostname = {hostname}\n"
        "\n"
        "# Console ports to forward, comma-separated. 9021 is elfldr.\n"
        f"forward = {', '.join(str(p) for p in ports)}\n"
    )


def ftp_connect(args):
    ftp = ftplib.FTP()
    ftp.connect(args.host, args.ftp_port, timeout=10)
    ftp.login()
    return ftp


def ftp_read(ftp, path):
    buf = io.BytesIO()
    try:
        ftp.retrbinary("RETR " + path, buf.write)
    except ftplib.error_perm:
        return None
    return buf.getvalue().decode("utf-8", "replace")


def cmd_console(args):
    text = render_conf(args.proxy, args.hostname, args.forward)
    if args.dry_run:
        print(f"# would write {CONF_PATH} on {args.host}:{args.ftp_port}\n")
        print(text, end="")
        return 0
    ftp = ftp_connect(args)
    try:
        old = ftp_read(ftp, CONF_PATH)
        try:
            ftp.mkd(CONF_DIR)
        except ftplib.error_perm:
            pass  # already exists
        if old is not None:
            ftp.storbinary("STOR " + CONF_PATH + ".bak", io.BytesIO(old.encode()))
            print(f"kept the previous file as {CONF_PATH}.bak")
        ftp.storbinary("STOR " + CONF_PATH, io.BytesIO(text.encode()))
        back = ftp_read(ftp, CONF_PATH)
    finally:
        ftp.quit()
    if back != text:
        print("error: the file read back from the console differs from what was written", file=sys.stderr)
        return 1
    print(f"wrote and verified {CONF_PATH} on {args.host}:")
    print(f"  proxy    = {args.proxy or '(none, direct)'}")
    print(f"  hostname = {args.hostname}")
    print(f"  forward  = {', '.join(str(p) for p in args.forward)}")
    print("ps5ts reads it at startup; restart ps5ts (stop_ps5ts.elf, then send ps5ts.elf) to apply it now.")
    return 0


def cmd_show(args):
    ftp = ftp_connect(args)
    try:
        text = ftp_read(ftp, CONF_PATH)
    finally:
        ftp.quit()
    if text is None:
        print(f"{CONF_PATH} does not exist on {args.host}; ps5ts will not start without it.")
        return 1
    print(text, end="")
    return 0


def cmd_proxy_unit(args):
    template = UNIT_TEMPLATE.read_text()
    lines = []
    replaced = False
    for line in template.splitlines():
        if line.startswith("ExecStart="):
            clients = " ".join(f"--allow-client {c}" for c in args.allow_client)
            line = (f"ExecStart=/usr/bin/python3 -u /opt/ps5_tailscale/ts_proxy.py "
                    f"--listen {args.listen} --port {args.port} {clients}")
            replaced = True
        elif "192.168." in line and line.startswith("#"):
            line = line.replace("192.168.0.164", args.listen)
        lines.append(line)
    if not replaced:
        print(f"error: no ExecStart= line in {UNIT_TEMPLATE}", file=sys.stderr)
        return 1
    text = "\n".join(lines) + "\n"
    if args.output in (None, "-"):
        # Raw bytes: on Windows, print() would turn \n into \r\n, which
        # systemd does not strip.
        sys.stdout.flush()
        sys.stdout.buffer.write(text.encode())
        sys.stdout.buffer.flush()
    else:
        Path(args.output).write_bytes(text.encode())
        print(f"wrote {args.output}; install it as /etc/systemd/system/ts-proxy.service (see README)", file=sys.stderr)
    print(f"ps5ts.conf on the console needs: proxy = http://{args.listen}:{args.port}", file=sys.stderr)
    return 0


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)

    def console_conn(p):
        p.add_argument("--host", required=True, type=check_ip, help="console LAN IP")
        p.add_argument("--ftp-port", type=int, default=2121, help="console FTP port (default 2121, ftpsrv)")

    p = sub.add_parser("console", help="write ps5ts.conf to the console over FTP")
    console_conn(p)
    p.add_argument("--proxy", required=True, type=check_proxy,
                   help='http://host:port of ts_proxy.py, or "" to connect directly')
    p.add_argument("--hostname", type=check_hostname, default="ps5", help="tailnet device name (default ps5)")
    p.add_argument("--forward", type=check_ports, default=[9021], help="ports to forward, comma-separated (default 9021)")
    p.add_argument("--dry-run", action="store_true", help="print the file instead of writing it")
    p.set_defaults(func=cmd_console)

    p = sub.add_parser("show", help="print the console's current ps5ts.conf")
    console_conn(p)
    p.set_defaults(func=cmd_show)

    p = sub.add_parser("proxy-unit", help="write the systemd unit for ts_proxy.py")
    p.add_argument("--listen", required=True, type=check_ip, help="proxy machine's LAN IP")
    p.add_argument("--port", type=int, default=3130, help="proxy port (default 3130; 3128 is taken on Proxmox)")
    p.add_argument("--allow-client", required=True, action="append", type=check_ip, help="console IP (repeatable)")
    p.add_argument("-o", "--output", help="output file (default: stdout)")
    p.set_defaults(func=cmd_proxy_unit)

    args = parser.parse_args()
    try:
        return args.func(args)
    except (OSError, ftplib.Error) as e:
        print(f"error: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
