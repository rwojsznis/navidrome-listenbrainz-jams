# navidrome-listenbrainz-jams

> [!NOTE]
> This is my AI slop. There are many like it, but this one is mine.

A small Go daemon that turns [ListenBrainz](https://listenbrainz.org)
recommendation feeds (e.g. *Weekly Jams*) into [Navidrome](https://www.navidrome.org)
playlists — downloading any missing tracks from [slskd](https://github.com/slskd/slskd)
(Soulseek) along the way.

## tl;dr

It was designed to be ran on same machine (e.g. NAS) alongside Navidrome and slskd (_source_ of music).

I run it like this (simplified version):

```bash
docker run -d \
  --name navidrome \
  --user 1024:100 \
  -v /volume4/docker/navidrome:/data \
  -v /volume1/music:/music:ro \
  --network=music \
  --read-only \
  -p 4533:4533 \
  --tmpfs /tmp:rw,noexec,nosuid,size=64m \
  --tmpfs /var/tmp:rw,noexec,nosuid,size=64m \
  --security-opt no-new-privileges:true \
  --cap-drop ALL \
  --restart unless-stopped \
  deluan/navidrome:0.63.0

docker run -d \
  --name slskd \
  --user 1024:100 \
  -e SLSKD_UMASK=022 \
  -p 5030:5030 \
  -p 50300:50300 \
  -v /volume4/docker/slskd:/app \
  -v /volume4/docker/slskd/slskd.yml:/app/slskd.yml:ro \
  -v /volume1/music:/music:ro \
  -v /volume4/docker/slskd-app/downloads:/downloads \
  -v /volume4/docker/slskd-app/incomplete:/incomplete \
  --network=music \
  --security-opt no-new-privileges:true \
  --cap-drop ALL \
  --restart unless-stopped \
  slskd/slskd:0.25.1

# this service
docker run -d \
  --name=navidrome-listenbrainz-jams \
  --user 1024:100 \
  -v /volume1/music/weekly_jams:/music \
  -v /volume4/docker/slskd-app/downloads:/slskd_downloads \
  -v /volume4/docker/navidrome-listenbrainz-jams/config.yaml:/config/config.yaml:ro \
  -v /volume4/docker/navidrome-listenbrainz-jams/state:/data \
  -p 8081:8080 \
  --network=music \
  --security-opt no-new-privileges:true \
  --cap-drop ALL \
  --restart unless-stopped \
  emqz/navidrome-listenbrainz-jams:latest
```

They key things is mounting download folders with correct permissions/paths as this app requires access to both slskd downloads directory and some directory where we would move those downloads to (in example above - to `/volume1/music/weekly_jams` which is mounted as `/music`)

Then my `config.yaml` looks like this:

```yaml
poll_interval: 1m

navidrome:
  url: http://navidrome:4533
  admin_user: <admin login> # admin user is used to trigger re-sync
  admin_pass: <admin password>

slskd:
  url: http://slskd:5030
  api_key: <api token - see slskd.yml in this repo for references>

paths:
  slskd_downloads: /slskd_downloads
  import_dir: /music

download:
  format_preference: [opus, mp3, flac]
  min_bitrate: 216
  per_track_timeout: 1h
  max_retries: 30

matching:
  fuzzy_threshold: 0.85

state:
  db_path: /data/state.db

web:
  listen: ":8080"

feeds:
  - name: emq-discover-weekly
    rss_url: https://listenbrainz.org/syndication-feed/user/emq/recommendations?recommendation_type=weekly-exploration
    navidrome_user: <my username>
    navidrome_pass: <my password>
  - name: emq-weekly-jams
    rss_url: "https://listenbrainz.org/syndication-feed/user/emq/recommendations?recommendation_type=weekly-jams"
    navidrome_user: <my username>
    navidrome_pass: <my password>
  # some more playlists here

fingerprint:
  enabled: true
  acoustid_api_key: <api key - see readme; optional>

lyrics:
  enabled: true
  lrclib_url: https://lrclib.net
```

## What it does

On a configurable interval, for each configured feed:

1. **Fetch** the ListenBrainz syndication (Atom) feed. Each entry is a playlist
   (e.g. *"Weekly Jams for emq, week of 2026-06-22 Mon"*) whose tracks carry an
   artist, title, and MusicBrainz recording MBID.
2. **Resolve** each track against Navidrome — by **MusicBrainz recording id**
   when the library file carries one, otherwise a decoration-aware fuzzy match on
   artist + title.
3. **Download** tracks not in the library via slskd: pick the best candidate
   (format preference, bitrate, free upload slot), enqueue, and poll the transfer.
4. **Import** completed downloads into the Navidrome music library and trigger a
   rescan. *Optionally* fingerprint each file (Chromaprint + AcoustID) and write
   its MusicBrainz recording id into the tags, so Navidrome — and the resolve
   step — can match it exactly.
5. **Assemble** the playlist, owned by the feed's Navidrome user, adding tracks as
   they become available.

It is **idempotent** and **resumable**: state is kept in SQLite, an existing
playlist is never duplicated, and missing tracks are backfilled on later runs
(best-effort). A playlist is considered done only when every track is placed.

For the full runtime behavior — state machines, feed add/remove semantics,
search/ranking strategy, cleanup, and failure handling — see
[`docs/behavior.md`](docs/behavior.md).

## Status dashboard

A mostly read-only web UI (default `http://localhost:8080`, set via `web.listen`)
lists each discovered playlist with a progress bar, and a click-through page shows
every track's state (in playlist / downloading / downloaded / missing / pending),
lyrics status, attempts, last error, the imported filename, and the original
slskd download path it came from.

A few recovery buttons let you nudge stuck tracks — **Retry** a missing track or
all missing tracks, **Re-sync** a playlist against Navidrome, **Tag MBID** to
force-match an imported-but-unresolved file, **Delete & restart** to throw away a
wrong download and search again, and **Re-scan lyrics** to backfill `.lrc` files.
See [`docs/behavior.md`](docs/behavior.md#dashboard-actions) for what each does.

## How tracks flow between services

```
ListenBrainz RSS ─▶ [service] ─search─▶ Navidrome ── found ─▶ add to playlist
                         │
                         └─ missing ─▶ slskd ─download─▶ slskd downloads dir
                                                              │ (move)
                                       import dir (in Navidrome library) ─rescan─▶ found ─▶ add
```

The service mounts slskd's **completed downloads** directory (read-only) and a
**dedicated import folder inside** Navidrome's library (read-write), and moves
finished files between them. Mount only that subfolder — not the whole library —
so the service can't touch the rest of your collection.

## Configuration

Copy `config.example.yaml` and edit. String values support `${ENV}` and
`${ENV:-default}` interpolation. Key fields:

| Field | Meaning |
|---|---|
| `poll_interval` | How often to re-check feeds and advance downloads |
| `navidrome.url` | Base URL of your Navidrome instance |
| `slskd.url` / `slskd.api_key` | slskd base URL and REST API key |
| `paths.slskd_downloads` | slskd's completed-downloads dir (mount read-only) |
| `paths.import_dir` | Dedicated import dir, a subfolder of Navidrome's library (mount read-write) |
| `download.format_preference` | Ordered preferred formats, e.g. `[flac, mp3]` |
| `download.min_bitrate` | Minimum kbps for lossy candidates (lossless exempt) |
| `download.max_retries` | Search/download attempts before a track is left missing |
| `download.per_track_timeout` | How long a single transfer may stall before it's abandoned (default 2h) |
| `matching.fuzzy_threshold` | 0..1 similarity required to accept a match |
| `fingerprint.enabled` | Turn on acoustic fingerprinting + MBID tagging (default off) |
| `fingerprint.acoustid_api_key` | Free AcoustID **application** key ([register an app](https://acoustid.org/new-application)) — required when enabled |
| `lyrics.enabled` | Fetch lyrics from lrclib.net and write a sibling `.lrc` per import (default off) |
| `lyrics.lrclib_url` | lrclib API base URL (default `https://lrclib.net`) |
| `web.listen` | Dashboard address, e.g. `:8080` (empty disables it) |
| `feeds[]` | One entry per feed: `name`, `rss_url`, `navidrome_user`, `navidrome_pass` |

Playlists are per-user in Navidrome, so each feed authenticates as its own
`navidrome_user`. The post-import **rescan uses the first feed's credentials**, so
that user must be Navidrome-admin (Subsonic `startScan` requires it).

### Acoustic fingerprinting (optional)

When `fingerprint.enabled` is true, each freshly downloaded file is identified by
its audio — `fpcalc` (Chromaprint) computes a fingerprint, [AcoustID](https://acoustid.org)
resolves it to a MusicBrainz **recording id**, and that id is written into the
file's tags (FLAC/MP3 in pure Go; Opus via `opustags`). Navidrome then indexes the
recording id, and the resolve step matches by id — an exact join that survives
mislabeled tags like `Sacrifice (PMEDIA)`.

It **trusts the download** (the feed's own recording id is preferred when AcoustID
lists it; otherwise the best-scoring result is used) and never rejects on
mismatch. A file AcoustID can't identify is simply left untagged — never a hard
failure. The Docker image bundles the required `fpcalc` and `opustags` binaries;
you only need a free AcoustID **application** API key — register an application at
[acoustid.org/new-application](https://acoustid.org/new-application). (This is the
"client" key for lookups; the user/account key is only for *submitting*
fingerprints and is rejected here with `invalid API key`.)

### Lyrics (optional)

When `lyrics.enabled` is true, each freshly imported file gets a sibling LRC
written next to it (`Artist - Title.flac` → `Artist - Title.lrc`) so Navidrome
can display lyrics. The track's artist + title (already in the feed) are looked
up on [lrclib.net](https://lrclib.net) — first an exact `/api/get`, then a fuzzy
`/api/search` fallback. **Synced** (timestamped) lyrics are preferred, with plain
text as a fallback. An existing `.lrc` is never overwritten, and a track with no
lyrics (or an instrumental) is skipped. Best-effort: a lookup failure never
blocks the import. No API key or extra binary is needed.

Each track's lyrics state (`synced` / `plain` / `none` / blank if not yet
attempted) is stored and shown in the dashboard's per-playlist table. A **Re-scan
lyrics** button on the playlist page fetches lyrics for every imported track that
doesn't already have a `.lrc` — handy for backfilling tracks imported before the
feature was enabled. It runs in the background; reload the page to see updated
statuses.

## Running

### Docker (recommended, e.g. on a NAS)

See `docker-compose.example.yaml`. In short:

```bash
docker build -t navidrome-lb-jams .
docker run --rm \
  -v $PWD/config.yaml:/config/config.yaml:ro \
  -v $PWD/state:/data \
  -v /path/to/navidrome/music/lb-jams:/import \
  -v /path/to/slskd/downloads:/downloads:ro \
  navidrome-lb-jams
```

### From source

```bash
go run ./cmd/navidrome-lb-jams -config config.yaml        # daemon
go run ./cmd/navidrome-lb-jams -config config.yaml -once  # single pass, then exit
```

## Networking note (important for downloads)

slskd needs its **Soulseek listening port (default 50300) reachable** for searches
to return results and downloads to work — forward it on your router. If inbound
connections can't reach slskd, searches return nothing and downloads won't work;
the Navidrome-matching path (resolving tracks already in your library) still does.

## Development

A local stack (Navidrome + slskd + sample tracks) lives in `deploy/`. See
[`deploy/README.md`](deploy/README.md). Run the test suite with:

```bash
go test ./...
```

Live integration tests are gated behind env vars (`NAVIDROME_LIVE=1`,
`SLSKD_LIVE=1`).

## Project layout

```
cmd/navidrome-lb-jams   daemon entrypoint
internal/config         YAML config + ${ENV} interpolation
internal/listenbrainz   Atom feed fetch + track parser
internal/navidrome      Subsonic API client
internal/slskd          slskd REST client + candidate ranking
internal/match          fuzzy artist/title matching
internal/files          locate, move, and delete completed downloads
internal/store          SQLite state
internal/downloader     slskd-backed download step (pipeline.Downloader)
internal/fingerprint    optional Chromaprint/AcoustID identification
internal/lyrics         optional lrclib.net lyrics -> sibling .lrc
internal/tags           write MusicBrainz recording id into FLAC/MP3/Opus
internal/pipeline       orchestration / state machine
internal/web            read-only status dashboard
```
