#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
src="$repo_root/desktop/renderer2/dist"
dst="$repo_root/cmd/workass/embedded/dist"

if [ ! -f "$src/index.html" ]; then
  echo "renderer2 dist missing: run the renderer2 build on the Mac first" >&2
  exit 1
fi

rm -rf "$dst"
mkdir -p "$dst"
cp -R "$src/." "$dst/"
echo "synced renderer2 dist: $src -> $dst"
