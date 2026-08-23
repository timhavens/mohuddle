#!/bin/sh
set -eu

UV_VERSION=0.12.5
MODEL_SHA256=7d5df8ecf7d4b1878015a32686053fd0eebe2bc377234608764cc0ef3636a6c5
VOICES_SHA256=bca610b8308e8d99f32e6fe4197e7ec01679264efed0cac9140fe9c29f1fbf7d
MODEL_URL=https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/kokoro-v1.0.onnx
VOICES_URL=https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/voices-v1.0.bin

data_root=${XDG_DATA_HOME:-"$HOME/.local/share"}
install_root="$data_root/mohuddle/tts/kokoro"
script_root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
requirements="$script_root/kokoro-requirements.lock"
bootstrap_root="$install_root/bootstrap"
uv_binary="$bootstrap_root/bin/uv"
venv_python="$install_root/venv/bin/python"

mkdir -p "$bootstrap_root" "$install_root/cache" "$install_root/python"

if [ ! -x "$uv_binary" ]; then
  python3 -m pip install --disable-pip-version-check --no-cache-dir \
    --target "$bootstrap_root" "uv==$UV_VERSION"
fi

export UV_CACHE_DIR="$install_root/cache"
export UV_PYTHON_INSTALL_DIR="$install_root/python"

if [ ! -x "$venv_python" ]; then
  "$uv_binary" venv --python 3.12 "$install_root/venv"
fi
"$uv_binary" pip install --python "$venv_python" --requirements "$requirements"

download_checked() {
  destination=$1
  expected=$2
  url=$3
  if [ -f "$destination" ] && printf '%s  %s\n' "$expected" "$destination" | sha256sum -c - >/dev/null 2>&1; then
    return
  fi
  temporary="$destination.download"
  curl -fL --retry 3 --output "$temporary" "$url"
  printf '%s  %s\n' "$expected" "$temporary" | sha256sum -c -
  mv "$temporary" "$destination"
}

download_checked "$install_root/kokoro-v1.0.onnx" "$MODEL_SHA256" "$MODEL_URL"
download_checked "$install_root/voices-v1.0.bin" "$VOICES_SHA256" "$VOICES_URL"

printf '\nKokoro is installed at %s\n' "$install_root"
printf 'Run: go run ./cmd/tts-smoke --voice am_adam\n'
