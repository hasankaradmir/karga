#!/bin/sh
# Installs the latest karga release (or $KARGA_VERSION, e.g. v0.1.0) into $KARGA_BIN_DIR
# (default /usr/local/bin). Checks the archive against the release's checksums.txt.
# KARGA_DOWNLOAD_URL replaces the release download URL (a mirror, or file:// for a local build).
set -eu

repo=hasankaradmir/karga
bin_dir=${KARGA_BIN_DIR:-/usr/local/bin}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $os in linux|darwin) ;; *) echo "unsupported OS: $os" >&2; exit 1 ;; esac
arch=$(uname -m)
case $arch in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "unsupported architecture: $arch" >&2; exit 1 ;; esac

version=${KARGA_VERSION:-}
if [ -z "$version" ]; then
  version=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
  [ -n "$version" ] || { echo "could not find the latest release" >&2; exit 1; }
fi
archive="karga_${version#v}_${os}_${arch}.tar.gz"
base=${KARGA_DOWNLOAD_URL:-https://github.com/$repo/releases/download/$version}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -o "$tmp/$archive" "$base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"
want=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)
if command -v sha256sum >/dev/null; then got=$(sha256sum "$tmp/$archive" | cut -d' ' -f1); else got=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1); fi
[ -n "$want" ] && [ "$want" = "$got" ] || { echo "checksum mismatch for $archive" >&2; exit 1; }

tar -xzf "$tmp/$archive" -C "$tmp" karga
if [ -w "$bin_dir" ]; then mv "$tmp/karga" "$bin_dir/karga"; else sudo mv "$tmp/karga" "$bin_dir/karga"; fi
echo "karga $version installed to $bin_dir/karga"
