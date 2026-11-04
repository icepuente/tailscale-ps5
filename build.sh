#!/usr/bin/env bash
# Build a Go program for freebsd/amd64 and wrap it in a PS5 payload.
# Linux (and WSL) counterpart of build.ps1. Output: build/<package name>.elf.
#
# Usage: ./build.sh [-p ./ps5ts] [-d SECONDS] [-t TEXTADDR]
#   -p  Go package to build (default ./probe)
#   -d  goshim dead-man switch in seconds, 0 = none (default 90)
#   -t  link address of the Go image (default 0x4000001000)
# Environment:
#   PS5_PAYLOAD_SDK  ps5-payload-sdk directory (default /opt/ps5-payload-sdk)
#   GO               go binary (default tools/go-linux/bin/go if present, else go)
#   CLANG, LLD       clang and ld.lld (default: first found, plain or versioned)
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
package=./probe
deadline=90
# Must be a 16 KiB-aligned base plus the 0x1000 ELF header, and clear of Go's
# heap hints (0x00c000000000, ...); goshim reports if it is taken.
text_addr=0x4000001000
while getopts "p:d:t:h" opt; do
  case $opt in
    p) package=$OPTARG ;;
    d) deadline=$OPTARG ;;
    t) text_addr=$OPTARG ;;
    *) sed -n '2,15p' "$0"; exit 2 ;;
  esac
done

find_tool() {
  for candidate in "$@"; do
    if command -v "$candidate" >/dev/null 2>&1; then command -v "$candidate"; return; fi
  done
  echo "build.sh: none of $* found" >&2
  exit 1
}

sdk=${PS5_PAYLOAD_SDK:-/opt/ps5-payload-sdk}
[ -f "$sdk/target/lib/crt1.o" ] || { echo "build.sh: no ps5-payload-sdk at $sdk (set PS5_PAYLOAD_SDK)" >&2; exit 1; }
if [ -z "${GO:-}" ]; then
  if [ -x "$here/tools/go-linux/bin/go" ]; then GO=$here/tools/go-linux/bin/go; else GO=go; fi
fi
CLANG=${CLANG:-$(find_tool clang clang-20 clang-19 clang-18 clang-17)}
LLD=${LLD:-$(find_tool ld.lld ld.lld-20 ld.lld-19 ld.lld-18 ld.lld-17)}

# The goroot patches are copies of this exact version's files.
want=go1.27.3
have=$("$GO" env GOVERSION)
[ "$have" = "$want" ] || { echo "build.sh: need $want, found $have ($GO)" >&2; exit 1; }
export GOTOOLCHAIN=local

build=$here/build
name=$(basename "$package")
mkdir -p "$build"

# goroot-patches/<path> replaces GOROOT/src/<path> through -overlay, leaving
# the toolchain unmodified.
goroot_src=$("$GO" env GOROOT)/src
overlay=$build/overlay.json
{
  printf '{"Replace":{'
  sep=
  while IFS= read -r file; do
    printf '%s"%s":"%s"' "$sep" "$goroot_src/${file#"$here"/goroot-patches/}" "$file"
    sep=,
  done < <(find "$here/goroot-patches" -type f | sort)
  printf '}}\n'
} > "$overlay"
goroot_count=$(find "$here/goroot-patches" -type f | wc -l)

# module-patches/<module>@<version>/...: Go refuses overlays inside the
# module cache, so each patched module is copied fresh from the cache to
# build/modules/<module>@<version> and patched there. The program's go.mod
# points at that copy with a replace directive.
modcache=$("$GO" env GOMODCACHE)
mod_count=0
while IFS= read -r moddir; do
  rel=${moddir#"$here"/module-patches/}
  src=$modcache/$rel
  if [ ! -d "$src" ]; then
    (cd "$here/$package" && "$GO" mod download "${rel%@*}@${rel##*@}")
  fi
  dst=$build/modules/$rel
  rm -rf "$dst"
  mkdir -p "$(dirname "$dst")"
  cp -r "$src" "$dst"
  chmod -R u+w "$dst"
  while IFS= read -r file; do
    file_rel=${file#"$moddir"/}
    case $(basename "$file") in
      ps5_*) ;;  # new files have no original
      *) [ -f "$dst/$file_rel" ] || { echo "build.sh: patch $rel/$file_rel has no original; was the module version bumped?" >&2; exit 1; } ;;
    esac
    cp "$file" "$dst/$file_rel"
    mod_count=$((mod_count + 1))
  done < <(find "$moddir" -type f)
done < <(find "$here/module-patches" -type d -name '*@*' 2>/dev/null)

echo "== go build $package (freebsd/amd64, text at $text_addr, $goroot_count GOROOT patches, $mod_count module patches)"
# -R 0x4000 rounds segments to the PS5's 16 KiB pages.
(cd "$here/$package" && GOOS=freebsd GOARCH=amd64 CGO_ENABLED=0 \
  "$GO" build -trimpath -overlay "$overlay" -ldflags "-T $text_addr -R 0x4000" -o "$build/go.elf" .)

echo "== compile goshim (deadline $deadline s, $("$CLANG" --version | head -1))"
cflags=(--target=x86_64-sie-ps5 -isysroot "$sdk" -isystem "$sdk/target/include"
        -fPIC -fno-stack-protector -fno-plt -femulated-tls -Wall -Wextra -Werror -O2)
"$CLANG" "${cflags[@]}" "-DGOSHIM_DEADLINE_SECONDS=$deadline" -c "$here/shim/goshim.c" -o "$build/goshim.o"
# .incbin resolves go.elf relative to the working directory.
(cd "$build" && "$CLANG" --target=x86_64-sie-ps5 -c "$here/shim/blob.S" -o "$build/blob.o")

echo "== link"
elf=$build/$name.elf
"$LLD" -m elf_x86_64 -pie --eh-frame-hdr -z max-page-size=0x4000 -T "$sdk/ldscripts/elf_x86_64.x" \
  --hash-style=gnu -L "$sdk/target/lib" -o "$elf" "$sdk/target/lib/crt1.o" "$build/goshim.o" "$build/blob.o" \
  -lc -lkernel_web -lSceLibcInternal
echo "== built $elf ($(stat -c %s "$elf") bytes)"
