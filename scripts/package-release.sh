#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 VERSION_OR_COMMIT_SHA [--dry-run|--validate]" >&2
  exit 2
fi

release=$1
shift
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd "$script_dir/.." && pwd)
dist_dir=${DIST_DIR:-dist}

cd "$repo_root"
exec go run ./scripts/package-release \
  --root "$repo_root" \
  --output "$dist_dir" \
  --release "$release" \
  "$@"
