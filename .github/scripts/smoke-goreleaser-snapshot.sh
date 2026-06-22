#!/usr/bin/env bash
set -euo pipefail

dist_dir="${1:-dist}"

if [[ ! -d "$dist_dir" ]]; then
  echo "dist directory not found: $dist_dir" >&2
  exit 1
fi

archive=""
while IFS= read -r candidate; do
  archive="$candidate"
  break
done < <(find "$dist_dir" -maxdepth 1 -type f -name '*_linux_amd64.tar.gz' | sort)

if [[ -z "$archive" ]]; then
  echo "no linux amd64 GoReleaser archive found in $dist_dir" >&2
  find "$dist_dir" -maxdepth 2 -type f | sort >&2
  exit 1
fi

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

tar -xzf "$archive" -C "$tmp_dir"

binary=""
while IFS= read -r candidate; do
  binary="$candidate"
  break
done < <(find "$tmp_dir" -type f -name atteler | sort)

if [[ -z "$binary" ]]; then
  echo "atteler binary not found in $archive" >&2
  find "$tmp_dir" -maxdepth 3 -type f | sort >&2
  exit 1
fi

chmod +x "$binary"
config_path="$tmp_dir/atteler.yaml"
smoke_home="$tmp_dir/home"
mkdir -p "$smoke_home"

export HOME="$smoke_home"
export XDG_CONFIG_HOME="$smoke_home/.config"
export XDG_STATE_HOME="$smoke_home/.local/state"
export ATTELER_CONFIG=
export ATTELER_STATE="$smoke_home/state.yaml"
export ATTELER_SESSION_DIR="$smoke_home/sessions"
export OPENAI_API_KEY=
export ANTHROPIC_API_KEY=
export CODEX_HOME=
export FORGE_CONFIG=

"$binary" --version
"$binary" --doctor-offline
"$binary" --print-config-template
"$binary" --init-config "$config_path"
"$binary" --config "$config_path" --validate-config
