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
  plays a prompt and echoes their audio.
- **Outbound** (`call <number>`): dials and plays a test clip on answer.

## Deploy (Docker / GHCR)

> ⚠️ **Inbound calls need a publicly reachable host.** Behind NAT the carrier
> can't reliably deliver the call's ACK/media. Run on a host with a **public IP**
> and set `BIND_HOST` to it. (Outbound works from anywhere.)

The GitHub Actions workflow ([.github/workflows/docker-publish.yml](.github/workflows/docker-publish.yml))
publishes a multi-arch image to GHCR on every push to `main` and on `v*` tags.
Pull and run it on your server:

```sh
docker run -d --name softphone \
  --network host \
  --env-file .env \
  -e BIND_HOST=<public-ip-of-this-host> \
  ghcr.io/cia-mn/softphone:latest
```

- `--network host` lets SIP + the dynamic RTP ports work without mapping a range.
- Config comes from `--env-file .env` or `-e` flags — **no secrets are baked into the image**.

Build locally instead (needs Docker BuildKit/buildx):

```sh
docker build -t softphone .
docker run --rm --network host --env-file .env -e BIND_HOST=<ip> softphone
```

## Troubleshooting

- Raw SIP messages: set `LOG_LEVEL=debug`.
- `403` after auth → wrong `SIP_USER` / `SIP_PASS`, or the username isn't the
  bare local number (Mobinet uses e.g. `75151407`, not `+976…`).
- Inbound `No ACK received` or one-way audio → NAT. Deploy on a public-IP host
  (see above) or port-forward a fixed UDP port.
