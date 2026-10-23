# Asterisk built from net2phone/n2p-asterisk branch websocket-transfer
# (22.8.2 + chan_websocket TRANSFER command). The repo is private, so it is
# cloned next to this file first (see README) and copied in.
FROM alpine:3.24 AS build

RUN apk add --no-cache build-base bsd-compat-headers linux-headers bash wget \
      ncurses-dev libxml2-dev sqlite-dev util-linux-dev jansson-dev libedit-dev \
      openssl-dev pjproject-dev curl-dev

COPY n2p-asterisk/ /src
COPY musl-patches/ /patches
WORKDIR /src

# musl fixes from Alpine aports main/asterisk (what the Alpine package builds with).
RUN for p in /patches/*.patch; do patch -p1 < "$p"; done \
 && sed -i -e 's/ASTSSL_LIBS:=$(OPENSSL_LIB)/ASTSSL_LIBS:=-Wl,--no-as-needed $(OPENSSL_LIB) -Wl,--as-needed/g' main/Makefile

# System pjproject/jansson from the Alpine repo instead of the bundled downloads.
# permanent-dlopen: as Alpine, since musl dlclose() never unloads a module.
# "samples" installs the stock configs (as the Alpine package does); entrypoint.sh
# overwrites the ones in conf/.
RUN ./configure --prefix=/usr --sysconfdir=/etc --localstatedir=/var --libdir=/usr/lib \
      --without-pjproject-bundled --without-jansson-bundled --enable-permanent-dlopen \
 && make menuselect.makeopts \
 && menuselect/menuselect --disable BUILD_NATIVE --disable astdb2sqlite3 --disable astdb2bdb \
      --enable CORE-SOUNDS-EN-ULAW menuselect.makeopts \
 && make -j"$(nproc)" \
 && make install samples DESTDIR=/out \
 && ls /out/usr/lib/asterisk/modules/ | grep -E 'chan_websocket|res_websocket_client'

FROM alpine:3.24

RUN apk add --no-cache pjproject jansson libxml2 sqlite-libs libuuid libedit ncurses-libs \
      libcurl openssl gettext \
 && addgroup -S asterisk && adduser -S -G asterisk -h /var/lib/asterisk asterisk

COPY --from=build /out /
# Sample users.conf is deprecated and only adds a startup warning.
RUN rm /etc/asterisk/users.conf

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
CMD ["asterisk", "-f", "-U", "asterisk", "-G", "asterisk", "-vvv"]
