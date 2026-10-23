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
| `asterisk` | `alpine:3.24` + `apk add asterisk asterisk-sounds-en` | Asterisk 22.9.0 from the Alpine repo; `chan_websocket` and `res_websocket_client` ship in the main package |
| `go-echo` | `golang:1.24-alpine` → `alpine:3.24` | WebSocket server on `:8080/media`, `/healthz` for the compose healthcheck |

## Layout
```
docker-compose.yml
asterisk/
  Dockerfile, entrypoint.sh     # entrypoint renders conf/ templates into /etc/asterisk
  conf/
    pjsip.conf                  # UDP 5060, endpoint 1001/1001, external_* = ${EXTERNAL_IP}
    rtp.conf                    # RTP 10000-10050 (published by compose)
    websocket_client.conf       # [go_echo] -> ws://go-echo:8080/media
    extensions.conf             # 600, 601, smoke test
    asterisk.conf, modules.conf, logger.conf
go-echo/
  Dockerfile, go.mod, go.sum, main.go
```

## Run
```sh
EXTERNAL_IP=<host-LAN-IP> docker compose up -d --build
```
- Files in `asterisk/conf/` are templates; `${EXTERNAL_IP}` is substituted at container start,
  so after editing them run `docker compose up -d --force-recreate asterisk`.
- `EXTERNAL_IP` is the address Asterisk advertises in SIP/SDP for audio. Default `127.0.0.1`
  is fine for a softphone on the same machine; use the host's LAN IP (e.g. `192.168.1.53`)
  for a phone on another device. To avoid passing it every time, put `EXTERNAL_IP=...` in `.env`.

## Extensions (context `internal`)
| Ext | What it does |
|---|---|
| `600` | Echo through the Go service over chan_websocket. DTMF `#` → Go sends `HANGUP`; `*` → `GET_STATUS` (debug level) |
| `601` | Asterisk built-in `Echo()`, for comparison |

## Test without a phone
```sh
docker compose exec asterisk asterisk -rx "channel originate Local/600@internal extension play@smoke"
docker compose logs -f go-echo
```
`play@smoke` plays `demo-congrats` (~30 s) into extension 600 and hangs up. Expect
`MEDIA_START ... format=ulaw optimal_frame_size=160 ptime=20`, then
`disconnected ... frames_in=N frames_out=N` with equal counts.

## Test with a softphone
1. Register `1001` / password `1001` to `<EXTERNAL_IP>:5060` (UDP).
2. Dial `600` and speak — you should hear yourself.

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
