#!/bin/sh
# Verify abi.go against the vendor header for 32- and 64-bit Windows.
# Usage: tools/abicheck/check.sh [path/to/sbigudrv.h]
set -eu
cd "$(dirname "$0")/../.."
HDR=${1:-tools/abicheck/sbigudrv.h}
if [ ! -f "$HDR" ]; then
  curl -fsSL -o tools/abicheck/sbigudrv.h https://raw.githubusercontent.com/indilib/indi-3rdparty/master/libsbig/sbigudrv.h
  HDR=tools/abicheck/sbigudrv.h
fi
SBIG_ABI_CHECK_OUT="$PWD/tools/abicheck/abi_check.c" go test -run TestGenerateABICheck -count=1 . >/dev/null
for T in x86_64-pc-windows-msvc i686-pc-windows-msvc; do
  clang -target $T -fsyntax-only -ffreestanding -fms-extensions -Wno-pragma-pack \
    -I tools/abicheck/stub -I "$(dirname "$HDR")" -include "$HDR" tools/abicheck/abi_check.c
  echo "ABI check OK for $T ($(grep -c _Static_assert tools/abicheck/abi_check.c) assertions)"
done
