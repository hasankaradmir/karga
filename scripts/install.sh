#!/bin/sh
# Installs a karga release into $KARGA_BIN_DIR (default /usr/local/bin) after checking it against
# the release's checksums.txt.
#
#   KARGA_VERSION       tag to install (default: the latest published release)
#   KARGA_BIN_DIR       where to put the binary
#   KARGA_DOWNLOAD_URL  download from here instead of GitHub (a mirror, or file:// for a local build)
#   KARGA_SYSTEMD=1     also install and start karga-server as a systemd service (Linux, needs root)
#
# The repository is private, so GitHub downloads need credentials: an authenticated GitHub CLI
# (gh auth login) is used when present, otherwise GITHUB_TOKEN (a token with read access to it).
set -eu

repo=hasankaradmir/karga
bin_dir=${KARGA_BIN_DIR:-/usr/local/bin}
api=https://api.github.com/repos/$repo

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $os in linux|darwin) ;; *) echo "unsupported OS: $os" >&2; exit 1 ;; esac
arch=$(uname -m)
case $arch in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) echo "unsupported architecture: $arch" >&2; exit 1 ;; esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if [ -n "${KARGA_DOWNLOAD_URL:-}" ]; then
  how=url
elif command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
  how=gh
elif [ -n "${GITHUB_TOKEN:-}" ]; then
  how=token
else
  echo "karga's repository is private: run 'gh auth login' or set GITHUB_TOKEN (or KARGA_DOWNLOAD_URL)" >&2
  exit 1
fi

auth() { curl -fsSL -H "Authorization: Bearer $GITHUB_TOKEN" "$@"; }

version=${KARGA_VERSION:-}
if [ -z "$version" ]; then
  case $how in
    gh) version=$(gh release view -R "$repo" --json tagName -q .tagName) ;;
    token) version=$(auth "$api/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1) ;;
    url) echo "set KARGA_VERSION with KARGA_DOWNLOAD_URL" >&2; exit 1 ;;
  esac
  [ -n "$version" ] || { echo "no published release found (a draft release must be published first)" >&2; exit 1; }
fi
archive="karga_${version#v}_${os}_${arch}.tar.gz"

# asset_url prints the API URL of a release asset; the release JSON lists each asset's "url" before
# its "name".
asset_url() {
  auth "$api/releases/tags/$version" | tr ',' '\n' |
    awk -v name="$1" '/"url": *".*\/releases\/assets\/[0-9]+"/ { u = $0; sub(/.*"url": *"/, "", u); sub(/".*/, "", u) }
                      $0 ~ "\"name\": *\"" name "\"" { print u; exit }'
}

for f in "$archive" checksums.txt; do
  case $how in
    url) curl -fsSL -o "$tmp/$f" "$KARGA_DOWNLOAD_URL/$f" ;;
    gh) gh release download "$version" -R "$repo" -p "$f" -D "$tmp" ;;
    token)
      u=$(asset_url "$f")
      [ -n "$u" ] || { echo "release $version has no $f" >&2; exit 1; }
      auth -H "Accept: application/octet-stream" -o "$tmp/$f" "$u"
      ;;
  esac
done

want=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)
if command -v sha256sum >/dev/null; then got=$(sha256sum "$tmp/$archive" | cut -d' ' -f1); else got=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1); fi
[ -n "$want" ] && [ "$want" = "$got" ] || { echo "checksum mismatch for $archive" >&2; exit 1; }

as_root() { if [ "$(id -u)" = 0 ]; then "$@"; else sudo "$@"; fi; }
put() { if [ -w "$bin_dir" ]; then mv "$tmp/$1" "$bin_dir/$1"; else as_root mv "$tmp/$1" "$bin_dir/$1"; fi; }

tar -xzf "$tmp/$archive" -C "$tmp"
put karga
echo "karga $version installed to $bin_dir/karga"
if [ -f "$tmp/karga-server" ]; then
  put karga-server
  echo "karga-server $version installed to $bin_dir/karga-server"
fi

# KARGA_SYSTEMD=1 runs karga-server as a service: a karga system user in the docker group owns
# /var/lib/karga; members of the karga group may use the CLI.
if [ "${KARGA_SYSTEMD:-}" = 1 ] && [ -f "$tmp/deploy/systemd/karga-server.service" ]; then
  getent group karga >/dev/null || as_root groupadd --system karga
  id karga >/dev/null 2>&1 || as_root useradd --system --gid karga --groups docker --home-dir /var/lib/karga --shell /usr/sbin/nologin karga
  sed "s#/usr/local/bin/karga-server#$bin_dir/karga-server#" "$tmp/deploy/systemd/karga-server.service" > "$tmp/karga-server.service"
  as_root install -m 0644 "$tmp/karga-server.service" /etc/systemd/system/karga-server.service
  as_root systemctl daemon-reload
  as_root systemctl enable --now karga-server
  echo "karga-server is running; add users to the karga group to use karga: sudo usermod -aG karga \$USER"
fi
