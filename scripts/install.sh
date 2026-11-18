#!/bin/sh
# Installs a karga CLI release (Linux or macOS) into $KARGA_BIN_DIR (default /usr/local/bin) after
# checking it against the release's checksums.txt.
#
#   KARGA_VERSION       tag to install (default: the latest published release)
#   KARGA_BIN_DIR       where to put the binary
#   KARGA_DOWNLOAD_URL  download from here instead of GitHub (a mirror, or file:// for a local build)
#
# karga needs karga-server on the host it manages: https://github.com/hasankaradmir/karga-server
set -eu

repo=hasankaradmir/karga-cli
bin_dir=${KARGA_BIN_DIR:-/usr/local/bin}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $os in linux|darwin) ;; *) echo "unsupported OS: $os" >&2; exit 1 ;; esac
arch=$(uname -m)
case $arch in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "unsupported architecture: $arch" >&2; exit 1 ;; esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

version=${KARGA_VERSION:-}
if [ -z "$version" ]; then
  [ -z "${KARGA_DOWNLOAD_URL:-}" ] || { echo "set KARGA_VERSION with KARGA_DOWNLOAD_URL" >&2; exit 1; }
  # GitHub redirects /releases/latest to the tag of the latest published release.
  version=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest" | sed -n 's#.*/tag/##p')
  [ -n "$version" ] || { echo "no published release found" >&2; exit 1; }
fi
url=${KARGA_DOWNLOAD_URL:-https://github.com/$repo/releases/download/$version}
archive="karga_${version#v}_${os}_${arch}.tar.gz"

for f in "$archive" checksums.txt; do
  curl -fsSL -o "$tmp/$f" "$url/$f"
done
want=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)
if command -v sha256sum >/dev/null; then got=$(sha256sum "$tmp/$archive" | cut -d' ' -f1); else got=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1); fi
[ -n "$want" ] && [ "$want" = "$got" ] || { echo "checksum mismatch for $archive" >&2; exit 1; }

tar -xzf "$tmp/$archive" -C "$tmp"
if [ -w "$bin_dir" ]; then
  mv "$tmp/karga" "$bin_dir/karga"
elif [ "$(id -u)" = 0 ]; then
  mv "$tmp/karga" "$bin_dir/karga"
else
  sudo mv "$tmp/karga" "$bin_dir/karga"
fi
echo "karga $version installed to $bin_dir/karga"
