# UNITEP-18900 — Asterisk 22 → Go echo over chan_websocket

Lab that dials from an Asterisk extension to a Go service over the Asterisk
WebSocket channel driver (`chan_websocket`) and echoes the caller's audio back.

```
softphone 1001 ──SIP/RTP──▶ asterisk ──ws://go-echo:8080/media──▶ go-echo
                  ext 600   Dial(WebSocket/go_echo/c(ulaw)f(json))   echoes every
                                                                     binary frame
```

| Service | Image | Notes |
|---|---|---|
| `asterisk` | `alpine:3.24`, built from source | [net2phone/n2p-asterisk](https://github.com/net2phone/n2p-asterisk) branch `websocket-transfer` ([PR #29](https://github.com/net2phone/n2p-asterisk/pull/29)): Asterisk 22.8.2 + chan_websocket `TRANSFER` command. pjproject/jansson come from the Alpine repo |
| `go-echo` | `golang:1.24-alpine` → `alpine:3.24` | WebSocket server on `:8080/media`, `/healthz` for the compose healthcheck |

## Layout
```
docker-compose.yml
asterisk/
  Dockerfile, entrypoint.sh     # entrypoint renders conf/ templates into /etc/asterisk
  n2p-asterisk/                 # private fork checkout (gitignored), see Run
  musl-patches/                 # musl fixes from Alpine aports main/asterisk
  conf/
    pjsip.conf                  # UDP 5060, endpoint 1001/1001, external_* = ${EXTERNAL_IP}
    rtp.conf                    # RTP 10000-10050 (published by compose)
    websocket_client.conf       # [go_echo] -> ws://go-echo:8080/media
    chan_websocket.conf         # TRANSFER enabled, allowed context: internal
    extensions.conf             # 600, 601, smoke test
    asterisk.conf, modules.conf, logger.conf
go-echo/
  Dockerfile, go.mod, go.sum, main.go
```

## Run
The fork is private, so clone it next to the Dockerfile first (needs access to net2phone):
```sh
git clone --depth 1 -b websocket-transfer https://github.com/net2phone/n2p-asterisk.git asterisk/n2p-asterisk
EXTERNAL_IP=<host-LAN-IP> docker compose up -d --build   # first build compiles Asterisk, ~10 min
# or, with go-echo transferring every call to 601 after 10 s (see "Transfer from the Go service"):
TRANSFER_AFTER=10s EXTERNAL_IP=<host-LAN-IP> docker compose up -d --build
```
- Files in `asterisk/conf/` are templates; `${EXTERNAL_IP}` is substituted at container start,
  so after editing them run `docker compose up -d --force-recreate asterisk`.
- `EXTERNAL_IP` is the address Asterisk advertises in SIP/SDP for audio. Default `127.0.0.1`
  is fine for a softphone on the same machine; use the host's LAN IP (e.g. `192.168.1.53`)
  for a phone on another device. To avoid passing it every time, put `EXTERNAL_IP=...` in `.env`.

## Extensions (context `internal`)
| Ext | What it does |
|---|---|
| `600` | Echo through the Go service over chan_websocket. DTMF `#` → Go sends `HANGUP`; `*` → `GET_STATUS` (debug level); `0` → Go sends `TRANSFER` to `601` |
| `601` | Transfer target: plays `demo-congrats`, then Asterisk built-in `Echo()` |

## Test without a phone
```sh
docker compose exec asterisk asterisk -rx "channel originate Local/600@internal extension play@smoke"
docker compose logs -f go-echo
```
`play@smoke` plays `demo-congrats` (~30 s) into extension 600 and hangs up. Expect
`MEDIA_START ... format=ulaw optimal_frame_size=160 ptime=20`, then
`disconnected ... frames_in=N frames_out=N` with equal counts.

## Transfer from the Go service
Go sends a cold transfer over the same websocket:
```json
{"command":"TRANSFER","correlation_id":"xfer-<connection_id>-1","exten":"601","context":"internal"}
```
Asterisk moves the caller (the channel bridged to the WebSocket channel) to `601@internal`,
answers with `{"event":"TRANSFER_RESULT","status":"success",...}` and closes the socket.
`invalid` / `not_permitted` / `failed` leave the caller with Go. Allowed contexts are set in
`chan_websocket.conf` (`transfer_contexts`).

| go-echo env | Default | Meaning |
|---|---|---|
| `TRANSFER_EXTEN` / `TRANSFER_CONTEXT` | `601` / `internal` | Transfer target |
| `TRANSFER_AFTER` | off | e.g. `10s`: go-echo transfers every call by itself this long after `MEDIA_START` (no key press needed) |

Start with the automatic transfer on (dial `600`, after 10 s you land on `601` and hear `demo-congrats`):
```sh
TRANSFER_AFTER=10s EXTERNAL_IP=<host-LAN-IP> docker compose up -d
```
Only `go-echo` reads it, so to switch it on or off later recreate just that service:
```sh
TRANSFER_AFTER=10s docker compose up -d go-echo   # on
docker compose up -d go-echo                      # off (DTMF 0 still transfers)
```
To keep it on by default, put `TRANSFER_AFTER=10s` (and `EXTERNAL_IP=...`) in `.env` next to `docker-compose.yml`.
The timer fires on the first audio frame after the deadline, so a muted phone is not transferred.

Phoneless test (the `xfer@smoke` leg presses `0` after 3 s):
```sh
docker compose exec asterisk asterisk -rx "channel originate Local/600@internal extension xfer@smoke"
docker compose logs go-echo   # transferring ... TRANSFER_RESULT status=success ... disconnected
```

## Test with a softphone
1. Register `1001` / password `1001` to `<EXTERNAL_IP>:5060` (UDP).
2. Dial `600` and speak — you should hear yourself.
3. Press `0` — the call moves to `601` (Asterisk `Echo()`); you still hear yourself, and
   `core show channels` no longer lists a `WebSocket/` channel.

## Go service logs (`LOG_LEVEL`)
| Level | Adds |
|---|---|
| `info` (default) | connected, `MEDIA_START` (format, frame size, ptime), DTMF, XOFF/XON, `disconnected` with frame totals |
| `debug` | `media stats` every `STATS_INTERVAL` (default `5s`): fps, kbps, frame sizes, avg/max inter-frame gap, late frames, RMS/peak dBFS, `silent_pct` (frames below -50 dBFS); Asterisk `STATUS` (queue state); channel variables; call summary |
| `trace` | request headers, raw control JSON, every media frame (50 lines/s) |

```sh
LOG_LEVEL=debug EXTERNAL_IP=192.168.1.53 docker compose up -d go-echo   # back to info: drop LOG_LEVEL
```
Stats and `GET_STATUS` polling only run at `debug` and above.

## Troubleshooting: call connects but no audio
- Check `debug` logs during the call:
  - `silent_pct` ≈ 100 / `rms_dbfs` very low → no audio from the phone reaches Asterisk (RTP/NAT problem).
  - Normal levels but you hear nothing → audio is lost on the way back to the phone.
- Dial `601`: if the built-in echo is silent too, the issue is phone ↔ Asterisk RTP, not the Go service.
- With Docker/Podman port publishing, phone traffic reaches Asterisk from a container-network
  address. That's why `pjsip.conf` has no `local_net`: with `local_net` covering that range,
  Asterisk advertises its container IP in SDP and the phone's RTP never arrives.
- Make sure `EXTERNAL_IP` is reachable from the phone and UDP `10000-10050` is published.

## Useful Asterisk commands
```sh
docker compose exec asterisk asterisk -rx "module show like websocket"
docker compose exec asterisk asterisk -rx "pjsip show contacts"
docker compose exec asterisk asterisk -rx "core show channels"
docker compose exec asterisk asterisk -rvvv          # interactive CLI
```
