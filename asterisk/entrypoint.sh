#!/bin/sh
set -e

# Render configs from the mounted templates, substituting only ${EXTERNAL_IP}.
mkdir -p /etc/asterisk
for f in /etc/asterisk-templates/*.conf; do
  envsubst '${EXTERNAL_IP}' < "$f" > "/etc/asterisk/$(basename "$f")"
done
chown -R asterisk:asterisk /etc/asterisk /var/lib/asterisk /var/log/asterisk /var/spool/asterisk /var/run/asterisk 2>/dev/null || true

exec "$@"
