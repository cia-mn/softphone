# Softphone dev tasks. Run `make` (or `make help`) to list targets.

SOUNDS_DIR := sounds
MP3        := $(wildcard $(SOUNDS_DIR)/*.mp3)
WAV        := $(MP3:.mp3=.wav)
BIN        := softphone
IMAGE      := ghcr.io/cia-mn/softphone:latest

.DEFAULT_GOAL := help

## sounds: convert sounds/*.mp3 -> 8 kHz mono 16-bit WAV (telephony format)
.PHONY: sounds
sounds: $(WAV)

# Pattern rule: each .wav is (re)built from its .mp3 when the .mp3 is newer.
$(SOUNDS_DIR)/%.wav: $(SOUNDS_DIR)/%.mp3
	@command -v ffmpeg >/dev/null || { echo "ffmpeg not found - please install it"; exit 1; }
	ffmpeg -y -loglevel error -i "$<" -ar 8000 -ac 1 -c:a pcm_s16le "$@"
	@echo "converted $< -> $@"

## clean-sounds: remove the generated WAV files
.PHONY: clean-sounds
clean-sounds:
	rm -f $(WAV)

## build: compile a static binary to ./softphone
.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) .

## run: register and answer inbound calls (loads .env)
.PHONY: run
run:
	go run .

## call: place a test call, e.g. `make call TO=99xxxxxx`
.PHONY: call
call:
	go run . call $(TO)

## vet: run go vet
.PHONY: vet
vet:
	go vet ./...

## tidy: run go mod tidy
.PHONY: tidy
tidy:
	go mod tidy

## docker: build the container image
.PHONY: docker
docker:
	docker build -t $(IMAGE) .

## clean: remove the built binary
.PHONY: clean
clean:
	rm -f $(BIN)

## help: list available targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
