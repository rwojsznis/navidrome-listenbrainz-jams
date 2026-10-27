# Build stage
FROM golang:1.27.3-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Pure-Go (modernc sqlite) -> fully static binary, no cgo. Works on arm64/amd64.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/navidrome-lb-jams ./cmd/navidrome-lb-jams

# Fetch stage: download and checksum-verify the pinned yt-dlp and deno binaries
# for the optional yt-dlp fallback source. Kept separate so curl/unzip stay out
# of the runtime image. Both are PINNED with an explicit version + SHA256 so
# builds are reproducible — a bare "latest" fetch would not be.
#
# Version rot is the operational catch: YouTube periodically breaks older yt-dlp
# builds ("Signature extraction failed"). We do NOT self-update at runtime (the
# image stays immutable / --read-only friendly); instead bump these ARGs and cut
# a new image release. Refresh the SHA256s from the release's SHA2-256SUMS
# (yt-dlp) and *.sha256sum (deno) files when bumping.
FROM debian:trixie-slim AS fetch
ARG TARGETARCH
ARG YTDLP_VERSION=2026.10.26
ARG DENO_VERSION=v2.9.8
RUN apt-get update \
	&& apt-get install -y --no-install-recommends curl ca-certificates unzip \
	&& rm -rf /var/lib/apt/lists/*
RUN set -eux; \
	case "$TARGETARCH" in \
	  amd64) \
	    YTDLP_ASSET=yt-dlp_linux; \
	    YTDLP_SHA=767ba9dce7052aa196f8b12652f433d76de128ba168966fa84b6394d5e6c6795; \
	    DENO_ASSET=deno-x86_64-unknown-linux-gnu.zip; \
	    DENO_SHA=e890975bc2549807bbcc0b584ffafda72cc7c93349dd99c80483dd784f6c4609 ;; \
	  arm64) \
	    YTDLP_ASSET=yt-dlp_linux_aarch64; \
	    YTDLP_SHA=e87e9533e89fe5889abaa21f5114dfe7756d6aa6219b83551add04d48b18e483; \
	    DENO_ASSET=deno-aarch64-unknown-linux-gnu.zip; \
	    DENO_SHA=d49596c20b9788cf8f5d8ddf644103d4bde56ed239753f0bdb92ba9361d51905 ;; \
	  *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; \
	esac; \
	curl -fsSL -o /tmp/yt-dlp "https://github.com/yt-dlp/yt-dlp/releases/download/${YTDLP_VERSION}/${YTDLP_ASSET}"; \
	echo "${YTDLP_SHA}  /tmp/yt-dlp" | sha256sum -c -; \
	chmod +x /tmp/yt-dlp; \
	curl -fsSL -o /tmp/deno.zip "https://github.com/denoland/deno/releases/download/${DENO_VERSION}/${DENO_ASSET}"; \
	echo "${DENO_SHA}  /tmp/deno.zip" | sha256sum -c -; \
	unzip -o /tmp/deno.zip -d /tmp; \
	chmod +x /tmp/deno

# Runtime stage. The Go binary is fully static (CGO_ENABLED=0), but the optional
# steps shell out to external tools we carry here (so we use debian-slim, not
# distroless-static):
#   - fpcalc (libchromaprint-tools) + opustags — acoustic fingerprinting.
#   - ffmpeg — required by `yt-dlp -x` for audio extraction/transcode.
#   - yt-dlp + deno (copied from the fetch stage) — the yt-dlp fallback source.
#     deno is the JS runtime yt-dlp needs for YouTube signature descrambling;
#     without it yt-dlp falls back to a client that may break.
# ca-certificates covers HTTPS to ListenBrainz/MusicBrainz/AcoustID/YouTube. If
# you enable neither fingerprinting nor the yt-dlp fallback, these are dead weight
# but harmless. trixie (not bookworm) is required: opustags first ships in Debian 13.
FROM debian:trixie-slim
RUN apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates libchromaprint-tools opustags ffmpeg \
	&& rm -rf /var/lib/apt/lists/*
COPY --from=build /out/navidrome-lb-jams /navidrome-lb-jams
COPY --from=fetch /tmp/yt-dlp /usr/local/bin/yt-dlp
COPY --from=fetch /tmp/deno /usr/local/bin/deno

# Mount points: /config (config.yaml), /data (state.db), plus the Navidrome music
# library and slskd downloads dir (paths set in config.yaml).
VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["/navidrome-lb-jams"]
CMD ["-config", "/config/config.yaml"]
