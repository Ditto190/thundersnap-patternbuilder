#!/bin/sh
# Copy an Alpine Nix frame and remove packages needed only to bootstrap Nix.
# Usage: sudo ./minimize-alpine.sh FRAME

set -eu
exec 3>&1 1>&2

SOURCE_FRAME=${1:?usage: $0 FRAME}

printf '>> Copying Alpine frame %s\n' "$SOURCE_FRAME"
FRAME=$(ts go "root@$SOURCE_FRAME" -c 'ts frame ::')

printf '>> Removing Alpine bootstrap packages from %s\n' "$FRAME"
ts go "root@$FRAME" -c '
    set -e
    apk del curl xz coreutils
    rm -rf /var/cache/apk
'

printf '>> Verifying Nix\n'
ts go "user@$FRAME" -c 'nix --version'

printf '%s\n' "$FRAME" >&3
