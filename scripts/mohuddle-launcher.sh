#!/bin/sh
# Keep this entry point unchanged across WSL builds. Only the text manifest and
# immutable executable change, so case aliases never mmap a replaced ELF file.
set -eu
mohuddle_bin_dir=$(dirname -- "$0")
mohuddle_image=$(cat "$mohuddle_bin_dir/.mohuddle-install-source")
case "$mohuddle_image" in
  bin/.mohuddle-build.*/mohuddle)
    exec "$mohuddle_bin_dir/${mohuddle_image#bin/}" "$@"
    ;;
  *)
    printf '%s\n' 'MoHuddle build is incomplete; run make build in the repository.' >&2
    exit 1
    ;;
esac
