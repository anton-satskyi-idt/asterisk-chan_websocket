FROM alpine:3.24

# Asterisk 22.x from the Alpine repo; chan_websocket ships in the main package.
RUN apk add --no-cache asterisk asterisk-sounds-en gettext \
 && ls /usr/lib/asterisk/modules/ | grep -E 'chan_websocket|res_websocket_client'

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
CMD ["asterisk", "-f", "-U", "asterisk", "-G", "asterisk", "-vvv"]
