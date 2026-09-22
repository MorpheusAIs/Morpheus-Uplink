#!/bin/sh
# Ensure DATA_DIR is writable by the uplink user. SecretVM / Docker volume
# mounts often arrive root-owned, which breaks ephemeral key persistence
# (open /data/uplink-state.json.tmp: permission denied).
# Optional bake.env (SecretVM configs mount) applies non-secret defaults
# without putting them on environment: (SecretVM scrapes every key).
set -e
if [ -f /etc/uplink/bake.env ]; then
  set -a
  # shellcheck disable=SC1091
  . /etc/uplink/bake.env
  set +a
fi
DATA_DIR="${DATA_DIR:-/data}"
mkdir -p "$DATA_DIR"
if [ "$(id -u)" = "0" ]; then
  chown -R uplink:uplink "$DATA_DIR" 2>/dev/null || true
  exec su-exec uplink /usr/local/bin/uplink "$@"
fi
exec /usr/local/bin/uplink "$@"
