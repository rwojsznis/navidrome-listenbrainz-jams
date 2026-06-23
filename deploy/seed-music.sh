#!/usr/bin/env bash
# Generates a few short, tagged FLAC files in ../data/music so Navidrome has
# real, matchable tracks. These deliberately cover only SOME of the feed's
# tracks, so the rest exercise the slskd download path.
#
# Requires ffmpeg. Run from anywhere: deploy/seed-music.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MUSIC="$ROOT/data/music"

# title|artist|album  — these match tracks present in the emq weekly-jams feed.
TRACKS=(
  "Army Dreamers|Kate Bush|Never for Ever"
  "Feel Good Inc.|Gorillaz|Demon Days"
  "vampire|Olivia Rodrigo|GUTS"
)

mkdir -p "$MUSIC"
for entry in "${TRACKS[@]}"; do
  IFS='|' read -r title artist album <<<"$entry"
  safe_artist="${artist//\//_}"
  safe_title="${title//\//_}"
  dir="$MUSIC/$safe_artist/$album"
  mkdir -p "$dir"
  out="$dir/$safe_title.flac"
  echo "seeding: $artist - $title"
  ffmpeg -hide_banner -loglevel error -y \
    -f lavfi -i "anullsrc=r=44100:cl=stereo" -t 3 \
    -metadata title="$title" \
    -metadata artist="$artist" \
    -metadata album="$album" \
    -metadata ALBUMARTIST="$artist" \
    "$out"
done

echo "done. seeded ${#TRACKS[@]} tracks into $MUSIC"
