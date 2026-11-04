#!/usr/bin/env bash
# Build the small C payloads in this folder (hello.c, stop_ps5ts.c) with the
# PS5 payload SDK. Linux (and WSL) counterpart of build.ps1.
# Output: build/<name>.elf, ready for tools/send.py.
# Environment: PS5_PAYLOAD_SDK (default /opt/ps5-payload-sdk), CLANG, LLD.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)

find_tool() {
  for candidate in "$@"; do
    if command -v "$candidate" >/dev/null 2>&1; then command -v "$candidate"; return; fi
  done
  echo "payloads/build.sh: none of $* found" >&2
  exit 1
}

sdk=${PS5_PAYLOAD_SDK:-/opt/ps5-payload-sdk}
[ -f "$sdk/target/lib/crt1.o" ] || { echo "payloads/build.sh: no ps5-payload-sdk at $sdk (set PS5_PAYLOAD_SDK)" >&2; exit 1; }
CLANG=${CLANG:-$(find_tool clang clang-20 clang-19 clang-18 clang-17)}
LLD=${LLD:-$(find_tool ld.lld ld.lld-20 ld.lld-19 ld.lld-18 ld.lld-17)}

build=$here/../build
mkdir -p "$build"
for source in "$here"/*.c; do
  name=$(basename "$source" .c)
  "$CLANG" --target=x86_64-sie-ps5 -isysroot "$sdk" -isystem "$sdk/target/include" \
    -fPIC -fno-stack-protector -fno-plt -femulated-tls -Wall -Wextra -Werror -O2 \
    -c "$source" -o "$build/$name.o"
  "$LLD" -m elf_x86_64 -pie --eh-frame-hdr -z max-page-size=0x4000 -T "$sdk/ldscripts/elf_x86_64.x" \
    --hash-style=gnu -L "$sdk/target/lib" -o "$build/$name.elf" "$sdk/target/lib/crt1.o" "$build/$name.o" \
    -lc -lkernel_web -lSceLibcInternal
  echo "built $(cd "$build" && pwd)/$name.elf"
done
