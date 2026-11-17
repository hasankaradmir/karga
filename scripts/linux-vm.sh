#!/usr/bin/env bash
# Runs make targets in a Linux VM with Docker, Go and gcc, for the Linux-only parts of karga
# (gateway, release proxy). Uses OrbStack (orb) when installed, otherwise Lima (limactl).
#
#   scripts/linux-vm.sh [make targets...]     default: test-integration test-e2e
#   KARGA_VM=name   machine name (default karga-dev)
#
# The machine is created and provisioned on first use. The working tree (without .git, bin and
# docs) is copied to ~/karga in the machine before every run.
set -euo pipefail

vm=${KARGA_VM:-karga-dev}
targets=("$@")
[ ${#targets[@]} -eq 0 ] && targets=(test-integration test-e2e)
root=$(cd "$(dirname "$0")/.." && pwd)
go_version=$(awk '/^go /{print $2; exit}' "$root/go.mod")

if command -v orb >/dev/null; then
  exists() { orb list -q 2>/dev/null | grep -qx "$vm"; }
  create() { orb create ubuntu "$vm"; }
  run() { orb run -m "$vm" bash -lc "$1"; }
  copy_in() { orb run -m "$vm" bash -c 'rm -rf ~/karga && mkdir -p ~/karga && tar xf - -C ~/karga'; }
elif command -v limactl >/dev/null; then
  exists() { limactl list -q 2>/dev/null | grep -qx "$vm"; }
  create() { limactl start --name="$vm" --tty=false template://ubuntu; }
  run() { limactl shell "$vm" bash -lc "$1"; }
  copy_in() { limactl shell "$vm" bash -c 'rm -rf ~/karga && mkdir -p ~/karga && tar xf - -C ~/karga'; }
else
  echo "needs OrbStack (orb) or Lima (limactl)" >&2
  exit 1
fi

if ! exists; then
  echo "creating Linux machine $vm" >&2
  create
fi

# Provision what is missing: Docker, gcc (for -race), make and the Go version of go.mod.
run "set -e
command -v docker >/dev/null || { curl -fsSL https://get.docker.com | sudo sh; sudo usermod -aG docker \"\$USER\"; }
command -v gcc >/dev/null && command -v make >/dev/null || { sudo apt-get update -qq; sudo apt-get install -y -qq gcc make; }
have=\$(/usr/local/go/bin/go env GOVERSION 2>/dev/null | sed 's/^go//')
if [ -z \"\$have\" ] || [ \"\$(printf '%s\\n' $go_version \"\$have\" | sort -V | head -1)\" != $go_version ]; then
  arch=\$(uname -m); case \$arch in x86_64) arch=amd64;; aarch64) arch=arm64;; esac
  curl -fsSL https://go.dev/dl/go$go_version.linux-\$arch.tar.gz | sudo tar -C /usr/local -xzf - --overwrite
fi"

cd "$root"
COPYFILE_DISABLE=1 tar --no-xattrs --exclude=.git --exclude=bin --exclude=docs --exclude=graphify-out -cf - . | copy_in
run "cd ~/karga && export PATH=\$PATH:/usr/local/go/bin && make ${targets[*]}"
