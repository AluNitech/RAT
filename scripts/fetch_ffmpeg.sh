#!/usr/bin/env bash
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")"/.. && pwd)"
FFMPEG_ROOT="$PROJECT_ROOT/third_party/ffmpeg"
LINUX_URL="https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-linux64-gpl.tar.xz"
WINDOWS_URL="https://www.gyan.dev/ffmpeg/builds/ffmpeg-git-essentials.7z"

usage() {
  cat <<'EOF'
Fetch platform-specific FFmpeg/FFplay binaries into third_party/ffmpeg/.

Usage: fetch_ffmpeg.sh <linux|windows>

Downloads official prebuilt archives, extracts the ffmpeg/ffplay binaries,
and places them under third_party/ffmpeg/<platform>/.
EOF
}

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "Error: required command '$1' not found in PATH" >&2
    exit 1
  fi
}

fetch_linux() {
  require_cmd curl
  require_cmd tar

  local workdir archive target dir
  workdir="$(mktemp -d)"
  archive="$workdir/ffmpeg.tar.xz"
  target="$FFMPEG_ROOT/linux"
  mkdir -p "$target"

  echo "Downloading FFmpeg (linux) from $LINUX_URL"
  curl -L "$LINUX_URL" -o "$archive"

  echo "Extracting archive..."
  tar -xf "$archive" -C "$workdir"

  local ffmpeg_bin ffplay_bin
  ffmpeg_bin="$(find "$workdir" -type f -name ffmpeg -perm -u+x | head -n 1 || true)"
  ffplay_bin="$(find "$workdir" -type f -name ffplay -perm -u+x | head -n 1 || true)"

  if [[ -z "$ffmpeg_bin" || -z "$ffplay_bin" ]]; then
    echo "Error: failed to locate ffmpeg/ffplay in extracted archive" >&2
    exit 1
  fi

  install -m 755 "$ffmpeg_bin" "$target/ffmpeg"
  install -m 755 "$ffplay_bin" "$target/ffplay"
  echo "Linux FFmpeg bundle installed to $target"
}

fetch_windows() {
  require_cmd curl
  require_cmd 7z

  local workdir archive extract_dir target
  workdir="$(mktemp -d)"
  archive="$workdir/ffmpeg.7z"
  extract_dir="$workdir/extracted"
  target="$FFMPEG_ROOT/windows"
  mkdir -p "$target" "$extract_dir"

  echo "Downloading FFmpeg (windows) from $WINDOWS_URL"
  curl -L "$WINDOWS_URL" -o "$archive"

  echo "Extracting archive..."
  7z x "$archive" -o"$extract_dir" >/dev/null

  local ffmpeg_bin ffplay_bin
  ffmpeg_bin="$(find "$extract_dir" -type f -name ffmpeg.exe | head -n 1 || true)"
  ffplay_bin="$(find "$extract_dir" -type f -name ffplay.exe | head -n 1 || true)"

  if [[ -z "$ffmpeg_bin" || -z "$ffplay_bin" ]]; then
    echo "Error: failed to locate ffmpeg.exe/ffplay.exe in extracted archive" >&2
    exit 1
  fi

  install -m 755 "$ffmpeg_bin" "$target/ffmpeg.exe"
  install -m 755 "$ffplay_bin" "$target/ffplay.exe"
  echo "Windows FFmpeg bundle installed to $target"
}

main() {
  if [[ $# -ne 1 ]]; then
    usage >&2
    exit 1
  fi

  case "$1" in
    linux)
      fetch_linux
      ;;
    windows)
      fetch_windows
      ;;
    -h|--help)
      usage
      ;;
    *)
      echo "Error: unknown platform '$1'" >&2
      usage >&2
      exit 1
      ;;
  esac
}

main "$@"
