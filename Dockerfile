# Build stage
FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Pure-Go (modernc sqlite) -> fully static binary, no cgo. Works on arm64/amd64.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/navidrome-lb-jams ./cmd/navidrome-lb-jams

# Runtime stage: distroless static includes CA certs for HTTPS (ListenBrainz/MusicBrainz).
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/navidrome-lb-jams /navidrome-lb-jams

# Mount points: /config (config.yaml), /data (state.db), plus the Navidrome music
# library and slskd downloads dir (paths set in config.yaml).
VOLUME ["/data"]

ENTRYPOINT ["/navidrome-lb-jams"]
CMD ["-config", "/config/config.yaml"]
