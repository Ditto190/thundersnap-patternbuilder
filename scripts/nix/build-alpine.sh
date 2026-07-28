#!/bin/sh
# Build a Nix frame from Alpine. The frame UUID is the only stdout output.

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

prepare_frame() {
    create_frame alpine:latest

    log "Installing Alpine bootstrap packages"
    run_root "
        set -e
        apk add --no-cache sudo curl xz ca-certificates coreutils
        printf '%s\n' 'Defaults:user !log_allowed, !log_denied' '# Thundersnap: allow the user account passwordless sudo' 'user ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/thundersnap-user
        chmod 0440 /etc/sudoers.d/thundersnap-user
        mkdir -p $NIX_BOOT_HOME
        chown -R user:user /var/lib/nix
    "
}

. "$SCRIPT_DIR/build.sh"
