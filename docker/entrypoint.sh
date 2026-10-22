#!/bin/sh
# Make everything under /data owned by PUID:PGID (e.g. after restoring a backup
# as root), then run the app as that user — never as root.
set -e
if [ "$(id -u)" = "0" ]; then
  mkdir -p "$NOTES_DATA_DIR"
  # only touch what's wrong, so large data dirs start fast
  find "$NOTES_DATA_DIR" \( ! -user "$PUID" -o ! -group "$PGID" \) -exec chown "${PUID}:${PGID}" {} + 2>/dev/null || true
  chmod 0700 "$NOTES_DATA_DIR"
fi
exec /usr/local/bin/notes "$@"
