#!/usr/bin/env bash
# Downloads the MAME 0.289 reference sources into .reference/.
set -euo pipefail

tag=mame0289
base="https://raw.githubusercontent.com/mamedev/mame/${tag}"
dest="$(cd "$(dirname "$0")/.." && pwd)/.reference"

files=(
  src/tools/chdman.cpp
  src/lib/util/chd.cpp
  src/lib/util/chd.h
  src/lib/util/chdcodec.cpp
  src/lib/util/chdcodec.h
  src/lib/util/cdrom.cpp
  src/lib/util/cdrom.h
  src/lib/util/flac.cpp
  src/lib/util/huffman.cpp
  3rdparty/lzma/C/LzmaEnc.c
)

for f in "${files[@]}"; do
  mkdir -p "${dest}/$(dirname "$f")"
  curl -fsSL "${base}/${f}" -o "${dest}/${f}"
  echo "fetched ${f}"
done
