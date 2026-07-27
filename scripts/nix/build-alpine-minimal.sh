#!/bin/sh
# Build a minimal Alpine Nix frame. The frame UUID is the only stdout output.

set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
FRAME=$($SCRIPT_DIR/build-alpine.sh)
FRAME=$($SCRIPT_DIR/minimize-alpine.sh "$FRAME")

printf '>> Built minimal Alpine frame: %s\n' "$FRAME" >&2
printf '>> Enter with: sudo ts go %s\n' "$FRAME" >&2
printf '%s\n' "$FRAME"
