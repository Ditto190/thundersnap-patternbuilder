#!/bin/sh
# Build a Nix frame from Debian. The frame UUID is the only stdout output.

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

prepare_frame() {
    create_frame debian:trixie

    log "Installing Debian bootstrap packages"
    run_root "
        set -e
        apt-get update
        apt-get install -y curl xz-utils ca-certificates sudo
        echo 'user ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/thundersnap-user
        chmod 0440 /etc/sudoers.d/thundersnap-user
        chage -d \"\$(date +%F)\" -m 0 -M 99999 -I -1 -E -1 user
        passwd -d user
        mkdir -p $NIX_BOOT_HOME
        chown -R user:user /var/lib/nix
    "
}

. "$SCRIPT_DIR/build.sh"
