# Build stage
FROM golang:1.26.5-alpine AS build
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
ARG YTDLP_VERSION=2026.07.04
ARG DENO_VERSION=v2.9.4
RUN apt-get update \
	&& apt-get install -y --no-install-recommends curl ca-certificates unzip \
	&& rm -rf /var/lib/apt/lists/*
RUN set -eux; \
	case "$TARGETARCH" in \
	  amd64) \
	    YTDLP_ASSET=yt-dlp_linux; \
	    YTDLP_SHA=6bbb3d314cde4febe36e5fa1d55462e29c974f63444e707871834f6d8cc210ae; \
	    DENO_ASSET=deno-x86_64-unknown-linux-gnu.zip; \
	    DENO_SHA=c24f955d9fbfe0ea5ae2b501c8e71ae76e31e4c9782390a54a284b3364fda725 ;; \
	  arm64) \
	    YTDLP_ASSET=yt-dlp_linux_aarch64; \
	    YTDLP_SHA=b6ce97646773070d7a7ffd6bbbdcaecb47c48483909c54c915bf08a7a9b5e0b1; \
	    DENO_ASSET=deno-aarch64-unknown-linux-gnu.zip; \
	    DENO_SHA=111da5c05c240cfdc4340f234a0e3539d39dbcb6755221f19dcd60bacc8be5aa ;; \
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
