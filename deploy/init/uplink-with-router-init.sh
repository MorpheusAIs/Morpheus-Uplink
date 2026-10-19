#!/bin/sh
# Railway / single-container variant: mint scoped router user then exec uplink.
# Minting now lives in /docker-entrypoint.sh (runs when COOKIE_CONTENT is set
# and no ROUTER_AUTH_FILE exists). As root it mints into a root-only staging
# dir and hands the files to the uplink user in DATA_DIR, because with
# cap_drop ALL root cannot write the uplink-owned DATA_DIR directly.
# Kept as a thin wrapper for compatibility.
set -eu
exec /docker-entrypoint.sh "$@"
