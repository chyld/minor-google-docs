#!/bin/sh
# `docker compose exec notes notes ...` runs as root by default; drop to the
# data owner so files created by CLI commands stay readable by the server.
if [ "$(id -u)" = "0" ]; then
  exec su-exec "${PUID:-1000}:${PGID:-1000}" /usr/local/bin/notes-bin "$@"
fi
exec /usr/local/bin/notes-bin "$@"
