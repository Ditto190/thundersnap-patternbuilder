#!/bin/sh
# Common Nix installer. Source this file after defining prepare_frame.

set -eu
if (set -o pipefail) 2>/dev/null; then
    set -o pipefail
fi

exec 3>&1 1>&2

BOOT_TIMEOUT=${BOOT_TIMEOUT:-300}
VERIFY_TIMEOUT=${VERIFY_TIMEOUT:-900}
NIX_BOOT_HOME=/var/lib/nix/home

log() {
    printf '>> %s\n' "$*" >&2
}

run_root() {
    timeout "$BOOT_TIMEOUT" ts go "root@$FRAME" -c "$1"
}

create_frame() {
    image=$1
    log "Creating frame from $image"
    snap=$(ts download-docker "$image" | awk 'NR == 1 { print $1 }')
    FRAME=$(ts frame "$snap:nil:nil")
}

install_nix() {
    log "Installing Nix"
    timeout "$BOOT_TIMEOUT" ts go "user@$FRAME" -c "
        set -e
        export HOME=$NIX_BOOT_HOME
        curl -sSL https://nixos.org/nix/install -o /tmp/nix-install.sh
        sh /tmp/nix-install.sh --no-daemon --yes
    "

    log "Installing the system profile"
    PROFILE_B64=$(base64 -w0 "$SCRIPT_DIR/profile.sh")
    run_root "
        set -e
        mkdir -p /nix/var/nix/profiles/per-user/root /etc/profile.d
        cp -d $NIX_BOOT_HOME/.local/state/nix/profiles/profile-1-link /nix/var/nix/profiles/default-1-link
        ln -sfn default-1-link /nix/var/nix/profiles/default
        cp -d $NIX_BOOT_HOME/.local/state/nix/profiles/channels-1-link /nix/var/nix/profiles/per-user/root/channels-1-link
        ln -sfn channels-1-link /nix/var/nix/profiles/per-user/root/channels
        echo '$PROFILE_B64' | base64 -d > /etc/profile.d/nix.sh
        chmod 0644 /etc/profile.d/nix.sh
        rm -rf $NIX_BOOT_HOME /tmp/nix-install.sh
        rmdir /var/lib/nix 2>/dev/null || true
    "
}

verify_nix() {
    log "Verifying Nix"
    timeout "$VERIFY_TIMEOUT" ts go "user@$FRAME" -c '
        set -e
        nix --version
        out=$(nix-build --no-out-link -E "(import <nixpkgs> {}).hello")
        "$out/bin/hello"
        nix-shell -p cowsay --run "cowsay works"
    '
    run_root 'rm -rf /home/.cache /home/.local /home/result'
}

prepare_frame
install_nix
verify_nix
log "Built frame: $FRAME"
log "Enter with: sudo ts go $FRAME"
printf '%s\n' "$FRAME" >&3
