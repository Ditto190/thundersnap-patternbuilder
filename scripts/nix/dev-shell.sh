#!/bin/sh
# Enter (or run a command in) the Nix development environment used to build
# and test Thundersnap from the minimal Alpine+Nix frame.

set -eu

# Tests create long Unix socket paths under t.TempDir; Nix's default
# /tmp/nix-shell-* TMPDIR prefix can push them over sockaddr_un's 108 bytes.
sudo mkdir -p /t /usr/libexec
sudo chmod 1777 /t

# A few project scripts and runtime helpers use fixed FHS paths. The minimal
# Alpine root deliberately lacks these, so bridge them to Nix packages.
for command in bash passt ps pgrep stat; do
    source_path=$(command -v "$command")
    sudo ln -sfn "$source_path" "/usr/bin/$command"
done
static_busybox=$(type -a busybox | while IFS= read -r path; do
    case "$path" in *busybox-static*) printf '%s\n' "${path##* }"; break ;; esac
done)
if [ -z "$static_busybox" ]; then
    echo "dev-shell.sh: pkgsStatic.busybox is required" >&2
    exit 1
fi
sudo ln -sfn "$static_busybox" /usr/bin/busybox-static
sudo ln -sfn "$(command -v virtiofsd)" /usr/libexec/virtiofsd

# The nested e2e test copies `busybox` (not `busybox-static`) into an empty
# frame, so make that name resolve to the static Nix build as well.
sudo ln -sfn "$static_busybox" /usr/bin/busybox

export PATH=/usr/bin:/sbin:$PATH
export TMPDIR=/t
export GOTMPDIR=/t
export THUNDERSNAP_VM_DIR=${THUNDERSNAP_VM_DIR:-$PWD/vm}

if [ "$#" -eq 0 ]; then
    exec "${SHELL:-/bin/sh}"
fi
exec "$@"
