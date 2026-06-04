# syntax=docker/dockerfile:1

# ---- build stage ----
# Build on the native builder arch and cross-compile to the target (no QEMU).
# The binary is pure Go (G.711 only — no cgo/libopus), so CGO_ENABLED=0 → fully static.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src

# Download modules first so they cache across source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/softphone .

# ---- runtime stage ----
# Static binary → distroless "static": CA certs + tzdata, no shell, runs as nonroot.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/softphone /usr/local/bin/softphone

# SIP signalling + dynamic RTP ports work best with `--network host` on a
# public-IP host (set BIND_HOST to that IP). All config comes from env vars
# (SIP_USER, SIP_PASS, FORWARD_TO, …) — see .env.example. No secrets are baked in.
ENTRYPOINT ["/usr/local/bin/softphone"]
