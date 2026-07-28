#!/bin/sh
# Build and test the amd64 Debian package in a nested Thundersnap frame.
# Set TS_AUTHKEY to enroll the nested daemon in Tailscale. Without it, the
# script uses the daemon's local test listener while validating SSH sessions.

set -eu
if (set -o pipefail) 2>/dev/null; then
    set -o pipefail
fi

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
# The outer frame may expose the checkout through a /home symlink, while a
# newly composed frame receives it at its canonical /work path.
NESTED_REPO=${NESTED_REPO:-/work/$(basename "$REPO")}
BOOT_TIMEOUT=${BOOT_TIMEOUT:-600}
TEST_TIMEOUT=${TEST_TIMEOUT:-180}

log() { printf '>> %s\n' "$*" >&2; }
run_root() { timeout "$BOOT_TIMEOUT" ts go "root@$FRAME" -c "$1"; }

log "Building Debian packages"
make -C "$REPO" build-deb

log "Snapshotting the current frame so the nested frame gets this checkout"
triad=$(ts snap)
work_snap=${triad##*:}

log "Creating a minimal Debian frame"
root_snap=$(ts download-docker debian:trixie)
FRAME=$(ts frame "$root_snap:nil:$work_snap")
log "Nested frame: $FRAME"

log "Installing the package and its declared dependencies"
run_root "
    set -e
    apt-get update
    apt-get install -y $NESTED_REPO/dist/thundersnap_latest_amd64.deb openssh-client
    dpkg-query -W thundersnap btrfs-progs busybox-static passt virtiofsd
    test -x /usr/sbin/vm/cloud-hypervisor
    test -r /usr/sbin/vm/vmlinux
"

if [ -n "${TS_AUTHKEY:-}" ]; then
    log "Enrolling the nested daemon with TS_AUTHKEY"
    run_root "
        set -e
        TS_AUTHKEY='$TS_AUTHKEY' /usr/sbin/thundersnapd \
          --data-dir=/var/lib/thundersnap --state-dir=/var/lib/thundersnap \
          --libexec-dir=/usr/libexec/thundersnap --vm-dir=/usr/sbin/vm \
          --policy=/etc/thundersnap/policy.jsonc >/tmp/thundersnapd.log 2>&1 &
        echo \$! >/tmp/thundersnapd.pid
        i=0
        while [ \$i -lt 90 ]; do
            /usr/sbin/thundersnapd --status >/tmp/thundersnapd.status 2>&1 && break
            sleep 1
            i=\$((i + 1))
        done
        cat /tmp/thundersnapd.status
    "
    log "The enrolled daemon is ready; connect from a Tailscale peer using the hostname above."
fi

# A loopback listener makes the session checks deterministic even when the
# outer container has no route back into the nested tsnet node.
log "Validating SSH container and VM entry paths"
run_root "
    set -e
    rm -rf /var/lib/thundersnap-test
    mkdir -p /var/lib/thundersnap-test
    /usr/sbin/thundersnapd \
      --data-dir=/var/lib/thundersnap-test --state-dir=/var/lib/thundersnap-test \
      --libexec-dir=/usr/libexec/thundersnap --vm-dir=/usr/sbin/vm \
      --policy=/etc/thundersnap/policy.jsonc \
      --test-listen=127.0.0.1:2222 --test-user=test@example.com \
      >/tmp/thundersnapd-test.log 2>&1 &
    daemon=\$!
    trap 'kill \$daemon 2>/dev/null || true' EXIT
    i=0
    while ! grep -q 'Waiting for SSH' /tmp/thundersnapd-test.log; do
        i=\$((i + 1)); [ \$i -lt 100 ] || { cat /tmp/thundersnapd-test.log; exit 1; }
        sleep .1
    done
    ssh='ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p 2222'
    frame=\$(timeout $TEST_TIMEOUT \$ssh root@@127.0.0.1 '/bin/ts ping >/dev/null; /bin/ts frame')
    test -n "\$frame"
    echo container-ok
    timeout $TEST_TIMEOUT \$ssh "vm/\$frame@127.0.0.1" 'echo vm-ok'
"

log "PASS: package install, activation path, container mode, and VM mode"
printf '%s\n' "$FRAME"
