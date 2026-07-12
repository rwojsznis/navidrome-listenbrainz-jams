#!/usr/bin/env bash
# Recompute the pinned per-arch SHA256 checksums for the yt-dlp and deno binaries
# in the Dockerfile. Renovate bumps `ARG YTDLP_VERSION` / `ARG DENO_VERSION` but
# cannot recompute the four `*_SHA` values that the build verifies, so this script
# fetches the upstream checksum files and rewrites those ARG lines in place.
#
# Idempotent: re-running with checksums already current leaves the file untouched.
# Usage: .github/scripts/update-binary-shas.sh [path/to/Dockerfile]
set -euo pipefail

DOCKERFILE="${1:-Dockerfile}"

ytdlp_version="$(awk -F= '/^ARG YTDLP_VERSION=/ {print $2; exit}' "$DOCKERFILE")"
deno_version="$(awk -F= '/^ARG DENO_VERSION=/ {print $2; exit}' "$DOCKERFILE")"
echo "yt-dlp version: ${ytdlp_version}"
echo "deno version:   ${deno_version}"

# yt-dlp ships one SHA2-256SUMS file covering every asset ("<sha>  <filename>").
ytdlp_sums="$(curl -fsSL "https://github.com/yt-dlp/yt-dlp/releases/download/${ytdlp_version}/SHA2-256SUMS")"
sha_for() { awk -v f="$2" '$2 == f { print $1 }' <<<"$1"; }
ytdlp_amd64="$(sha_for "$ytdlp_sums" yt-dlp_linux)"
ytdlp_arm64="$(sha_for "$ytdlp_sums" yt-dlp_linux_aarch64)"

# deno ships a per-asset "<asset>.sha256sum" file.
deno_url="https://github.com/denoland/deno/releases/download/${deno_version}"
deno_amd64="$(curl -fsSL "${deno_url}/deno-x86_64-unknown-linux-gnu.zip.sha256sum" | awk '{print $1}')"
deno_arm64="$(curl -fsSL "${deno_url}/deno-aarch64-unknown-linux-gnu.zip.sha256sum" | awk '{print $1}')"

for name in ytdlp_amd64 ytdlp_arm64 deno_amd64 deno_arm64; do
  val="${!name}"
  [[ "$val" =~ ^[0-9a-fA-F]{64}$ ]] || { echo "ERROR: bad checksum for ${name}: '${val}'" >&2; exit 1; }
  echo "${name}: ${val}"
done

# Rewrite each `*_SHA=` line using the asset named on the preceding `*_ASSET=`
# line, so the right arch's checksum lands in the right case branch. Portable
# awk (no gawk 3-arg match) so it runs under Ubuntu's default mawk too.
awk \
  -v s_ytdlp_linux="$ytdlp_amd64" \
  -v s_ytdlp_linux_aarch64="$ytdlp_arm64" \
  -v s_deno_amd64="$deno_amd64" \
  -v s_deno_arm64="$deno_arm64" '
  BEGIN {
    sha["yt-dlp_linux"]=s_ytdlp_linux
    sha["yt-dlp_linux_aarch64"]=s_ytdlp_linux_aarch64
    sha["deno-x86_64-unknown-linux-gnu.zip"]=s_deno_amd64
    sha["deno-aarch64-unknown-linux-gnu.zip"]=s_deno_arm64
  }
  /YTDLP_ASSET=/ { t=$0; sub(/.*YTDLP_ASSET=/,"",t); sub(/[; ].*/,"",t); cur_ytdlp=t }
  /DENO_ASSET=/  { t=$0; sub(/.*DENO_ASSET=/,"",t);  sub(/[; ].*/,"",t); cur_deno=t }
  /YTDLP_SHA=/   { sub(/YTDLP_SHA=[0-9a-fA-F]+/, "YTDLP_SHA=" sha[cur_ytdlp]) }
  /DENO_SHA=/    { sub(/DENO_SHA=[0-9a-fA-F]+/,  "DENO_SHA="  sha[cur_deno]) }
  { print }
' "$DOCKERFILE" > "${DOCKERFILE}.tmp"
mv "${DOCKERFILE}.tmp" "$DOCKERFILE"
echo "Done."
