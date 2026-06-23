# navidrome-listenbrainz-jams

A small Go daemon that turns [ListenBrainz](https://listenbrainz.org)
recommendation feeds (e.g. *Weekly Jams*) into [Navidrome](https://www.navidrome.org)
playlists — downloading any missing tracks from [slskd](https://github.com/slskd/slskd)
(Soulseek) along the way.

## What it does

On a configurable interval, for each configured feed:

1. **Fetch** the ListenBrainz syndication (Atom) feed. Each entry is a playlist
   (e.g. *"Weekly Jams for emq, week of 2026-06-22 Mon"*) whose tracks carry an
   artist, title, and MusicBrainz recording MBID.
2. **Resolve** each track against Navidrome (fuzzy match on artist + title).
3. **Download** tracks not in the library via slskd: pick the best candidate
   (format preference, bitrate, free upload slot), enqueue, and poll the transfer.
4. **Import** completed downloads into the Navidrome music library and trigger a
   rescan.
5. **Assemble** the playlist, owned by the feed's Navidrome user, adding tracks as
   they become available.

It is **idempotent** and **resumable**: state is kept in SQLite, an existing
playlist is never duplicated, and missing tracks are backfilled on later runs
(best-effort). A playlist is considered done only when every track is placed.

## Status dashboard

A read-only web UI (default `http://localhost:8080`, set via `web.listen`) lists
each discovered playlist with a progress bar, and a click-through page shows every
track's state (in playlist / downloading / downloaded / missing / pending),
attempts, last error, and the imported filename.

## How tracks flow between services

```
ListenBrainz RSS ─▶ [service] ─search─▶ Navidrome ── found ─▶ add to playlist
                         │
                         └─ missing ─▶ slskd ─download─▶ slskd downloads dir
                                                              │ (move)
                                              Navidrome music library ─rescan─▶ found ─▶ add
```

The service mounts slskd's **completed downloads** directory (read) and
Navidrome's **music library** (read-write), and moves finished files between them.

## Configuration

Copy `config.example.yaml` and edit. String values support `${ENV}` and
`${ENV:-default}` interpolation. Key fields:

| Field | Meaning |
|---|---|
| `poll_interval` | How often to re-check feeds and advance downloads |
| `navidrome.url` | Base URL of your Navidrome instance |
| `slskd.url` / `slskd.api_key` | slskd base URL and REST API key |
| `paths.slskd_downloads` | slskd's completed-downloads dir (read) |
| `paths.navidrome_music` | Navidrome's music library root (write) |
| `paths.import_subdir` | Subfolder under the library where imports land |
| `download.format_preference` | Ordered preferred formats, e.g. `[flac, mp3]` |
| `download.min_bitrate` | Minimum kbps for lossy candidates |
| `download.max_retries` | Search/download attempts before a track is left missing |
| `matching.fuzzy_threshold` | 0..1 similarity required to accept a match |
| `web.listen` | Dashboard address, e.g. `:8080` (empty disables it) |
| `feeds[]` | One entry per feed: `name`, `rss_url`, `navidrome_user`, `navidrome_pass` |

Playlists are per-user in Navidrome, so each feed authenticates as its own
`navidrome_user`. The post-import **rescan uses the first feed's credentials**, so
that user must be Navidrome-admin (Subsonic `startScan` requires it).

## Running

### Docker (recommended, e.g. on a NAS)

See `docker-compose.example.yaml`. In short:

```bash
docker build -t navidrome-lb-jams .
docker run --rm \
  -v $PWD/config.yaml:/config/config.yaml:ro \
  -v $PWD/state:/data \
  -v /path/to/navidrome/music:/music \
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
to return results and downloads to work — forward it on your router. Behind
**CGNAT (e.g. mobile internet)** inbound connections are impossible, so searches
return nothing; the Navidrome-matching path still works, but downloads won't.

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
internal/files          locate + move completed downloads
internal/store          SQLite state
internal/downloader     slskd-backed download step (pipeline.Downloader)
internal/pipeline       orchestration / state machine
internal/web            read-only status dashboard
```
