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
| `download.min_bitrate` | Minimum kbps for lossy candidates |
| `download.max_retries` | Search/download attempts before a track is left missing |
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
internal/fingerprint    optional Chromaprint/AcoustID identification
internal/lyrics         optional lrclib.net lyrics -> sibling .lrc
internal/tags           write MusicBrainz recording id into FLAC/MP3/Opus
internal/pipeline       orchestration / state machine
internal/web            read-only status dashboard
```
