#!/bin/sh
# Run in the target-platform builder, not the cross-Go builder.
set -eu
export LC_ALL=C
arch=${1:?target architecture}
out=${2:?output binary}
case "$arch:$(cc -dumpmachine)" in
 amd64:x86_64-*) machine='Advanced Micro Devices X86-64' ;;
 arm64:aarch64-*) machine='AArch64' ;;
 *) echo "execenv capsule: compiler does not target $arch" >&2; exit 1 ;;
esac
mkdir -p "$(dirname "$out")"
cc -static -nostdlib -ffreestanding -fno-builtin -fno-stack-protector  -fno-pie -Wl,-no-pie -O2 -Wall -Wextra -Werror  -o "$out" native/execenv/capsule.c
readelf -h "$out" | grep -F "$machine" >/dev/null
readelf -h "$out" | grep -E 'Type: +EXEC' >/dev/null
if readelf -lW "$out" | grep -E 'INTERP|DYNAMIC' >/dev/null; then
 echo "execenv capsule: unexpected runtime loader" >&2
 exit 1
fi
chmod 0555 "$out"
