# softphone

Go SIP user agent for the cia.mn softphone, built on
[diago](https://github.com/emiago/diago) / [sipgo](https://github.com/emiago/sipgo).
It registers to the SIP provider (`ip-phone.mobinet.mn`), places and answers
calls, and forwards inbound callers via DTMF.

End goal: a **browser softphone** — the browser does WebRTC audio and this
service bridges it to SIP. Progress:

1. **Register** to the SIP registrar — ✅
2. **Place / answer calls** (G.711 audio), DTMF, call forwarding — ✅
3. Browser front-end over WebRTC, bridged to the SIP leg — next

## Configure

```sh
cp .env.example .env   # fill in SIP_USER / SIP_PASS (from Mobinet)
```

All config is environment variables (loaded from `.env` if present); see
[.env.example](.env.example) for the full list.

## Run

```sh
go run .                 # register and stay online (answers inbound calls)
go run . call 99xxxxxx   # register, then place a call and play a test clip
```

- **Inbound** (while running): incoming calls are answered. With `FORWARD_TO`
  set, the caller is bridged to that number when they press **1**; otherwise it
  plays a prompt and echoes their audio. At most `FORWARD_CONCURRENCY` forwards
  run at once — extra callers are held on hold in a FIFO queue up to `QUEUE_TIMEOUT`.
- **Outbound** (`call <number>`): dials and plays a test clip on answer.

## Custom audio (prompt / hold music)

Calls use **8 kHz mono 16-bit WAV**. Convert any MP3 with ffmpeg:

```sh
ffmpeg -i sounds/start.mp3 -ar 8000 -ac 1 -c:a pcm_s16le sounds/start.wav
```

Point the app at the WAVs (these are the defaults):

```ini
PROMPT_FILE=sounds/start.wav          # played while waiting for "press 1"
HOLD_FILE=sounds/waiting-queue.wav    # hold music for queued callers
```

A path that doesn't exist falls back to a bundled demo clip. In Docker the
compose file mounts `./sounds` → `/sounds`, and the container's working dir is
`/`, so the default `sounds/*.wav` paths resolve to `/sounds/*.wav`. For plain
`docker run`, add `-v "$PWD/sounds:/sounds"`.

## Deploy (Docker / GHCR)

> ⚠️ **Inbound calls need a publicly reachable host.** Behind NAT the carrier
> can't reliably deliver the call's ACK/media. Run on a host with a **public IP**.
> `BIND_HOST` is the local interface to bind (leave empty to auto-detect — don't
> put a 1:1-NAT public IP there); `PUBLIC_HOST` is the public IP to advertise.
> (Outbound works from anywhere.)

The GitHub Actions workflow ([.github/workflows/docker-publish.yml](.github/workflows/docker-publish.yml))
publishes a multi-arch image to GHCR on every push to `main` and on `v*` tags.
Pull and run it on your server:

```sh
docker run -d --name softphone \
  --network host \
  --env-file .env \
  -e PUBLIC_HOST=<public-ip-of-this-host> \
  ghcr.io/cia-mn/softphone:latest
```

(If the public IP is assigned directly to the host's interface — `ip addr` shows
it — you can instead set `BIND_HOST` to it and skip `PUBLIC_HOST`.)

- `--network host` lets SIP + the dynamic RTP ports work without mapping a range.
- Config comes from `--env-file .env` or `-e` flags — **no secrets are baked into the image**.

Build locally instead (needs Docker BuildKit/buildx):

```sh
docker build -t softphone .
docker run --rm --network host --env-file .env -e PUBLIC_HOST=<public-ip> softphone
```

### Ports to open (firewall / security group)

`network_mode: host` binds directly to the host, so there's no Docker port
mapping — open these **inbound UDP** rules on the server (cloud security group
and/or host firewall). Pin fixed ports so they're predictable:

| Port (UDP) | Purpose |
|------------|---------|
| `BIND_PORT` — e.g. **5060** | SIP signalling |
| `RTP_PORT_START`–`RTP_PORT_END` — e.g. **10000–10100** | RTP/RTCP audio |

Recommended server `.env` for deployment:

```ini
BIND_HOST=                 # empty = auto-detect this host's interface IP
PUBLIC_HOST=<public-ip>    # the IP advertised to the carrier (Contact/SDP)
BIND_PORT=5060
RTP_PORT_START=10000
RTP_PORT_END=10100
```

(If `ip addr` shows the public IP directly on the interface, set `BIND_HOST` to
it and leave `PUBLIC_HOST` empty — then the SIP Contact is public too.)

Each call leg uses 2 ports (RTP + RTCP); a *forwarded* call has 2 legs (4 ports),
so a 100-port range handles plenty of concurrent calls. Everything is **UDP**
(only `SIP_TRANSPORT=tls/tcp` would add a TCP port). Outbound has no inbound-port
requirement — only inbound calls/media do.

## Troubleshooting

- Raw SIP messages: set `LOG_LEVEL=debug`.
- `403` after auth → wrong `SIP_USER` / `SIP_PASS`, or the username isn't the
  bare local number (Mobinet uses e.g. `75151407`, not `+976…`).
- Inbound `No ACK received` or one-way audio → NAT. Deploy on a public-IP host
  (see above) or port-forward a fixed UDP port.
