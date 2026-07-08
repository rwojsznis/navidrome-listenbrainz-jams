# Build stage
FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Pure-Go (modernc sqlite) -> fully static binary, no cgo. Works on arm64/amd64.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/navidrome-lb-jams ./cmd/navidrome-lb-jams

# Runtime stage. The Go binary is fully static (CGO_ENABLED=0), but the optional
# acoustic-fingerprinting step shells out to two external tools — fpcalc
# (libchromaprint-tools) and opustags — so we use debian-slim instead of
# distroless-static to carry them. ca-certificates covers HTTPS to
# ListenBrainz/MusicBrainz/AcoustID. If you never enable fingerprinting, these
# packages are dead weight but harmless. trixie (not bookworm) is required:
# opustags first ships in Debian 13.
FROM debian:trixie-slim
RUN apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates libchromaprint-tools opustags \
	&& rm -rf /var/lib/apt/lists/*
COPY --from=build /out/navidrome-lb-jams /navidrome-lb-jams

# Mount points: /config (config.yaml), /data (state.db), plus the Navidrome music
# library and slskd downloads dir (paths set in config.yaml).
VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["/navidrome-lb-jams"]
CMD ["-config", "/config/config.yaml"]
