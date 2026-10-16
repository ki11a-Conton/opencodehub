#!/usr/bin/env bash
# Build the opencodehub c-shared plugin.
#
#   ./build.sh [output-dir]
#
# Defaults to the current directory. The output file name must stay
# "opencodehub" (plus the platform extension): CLIProxyAPI matches the plugin id
# against the DLL base name and the plugins.configs key.
set -euo pipefail

out_dir="${1:-.}"
mkdir -p "$out_dir"

case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*) ext=dll ;;
  Darwin)               ext=dylib ;;
  *)                    ext=so ;;
esac

export CGO_ENABLED=1
go build -buildmode=c-shared -trimpath -ldflags="-s -w" -o "$out_dir/opencodehub.$ext" .
echo "built $out_dir/opencodehub.$ext"
