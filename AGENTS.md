# AGENTS.md

Rules for LLM/coding agents working on this repo. Keep changes small, tested, and
faithful to the conventions below.

## What this is

A long-running Go daemon: fetch ListenBrainz recommendation feeds → ensure each
track exists in Navidrome (downloading missing ones via slskd/Soulseek) → build
the playlist per Navidrome user. Deployed as a Docker container. See
`README.md` (setup) and `docs/behavior.md` (runtime behavior — read this).

## Always, before finishing a change

```
go build ./... && go vet ./... && go test ./...
```
All three must pass. Add/update tests for any logic you touch.

## Architecture (don't blur these boundaries)

```
cmd/navidrome-lb-jams  daemon: config load, tick loop, web server, shutdown
internal/config        YAML + ${ENV} interpolation + validation
internal/listenbrainz  Atom feed fetch + track parser
internal/navidrome     Subsonic API client (transport only)
internal/slskd         slskd REST client + candidate ranking (transport + select.go)
internal/match         normalize + fuzzy match (no I/O)
internal/files         locate/move/rename files on disk (no network)
internal/store         SQLite state (all SQL lives here)
internal/downloader    slskd-backed pipeline.Downloader (search→enqueue→poll→import)
internal/pipeline      orchestration / 3-phase tick (resolve → playlist → download)
internal/web           read-only dashboard (stdlib net/http + html/template)
```

- The pipeline tick has three phases in this order: **resolve (Navidrome) →
  create/backfill playlist → download (slskd)**. Keep them ordered and distinct;
  don't re-interleave resolve and download.
- `internal/store` owns **all** SQL. Other packages call its methods; they never
  embed SQL.
- Clients (`navidrome`, `slskd`) are thin transport. Business logic (matching,
  ranking, state transitions) lives in `match`, `slskd/select.go`, `downloader`,
  `pipeline`.

## Hard constraints

- **Pure Go, no cgo.** SQLite is `modernc.org/sqlite`; builds with
  `CGO_ENABLED=0` for arm64/amd64 Docker. Don't add cgo deps.
- **Minimal dependencies.** Stdlib HTTP for all clients. The web UI is
  stdlib-only (`net/http` + `html/template`) — no JS framework, no frontend deps.
  Current third-party deps: `modernc.org/sqlite`, `gopkg.in/yaml.v3`,
  `golang.org/x/net/html`, `golang.org/x/text`. Justify any addition.
- **Least privilege.** The app writes only to `import_dir` (a subfolder of
  Navidrome's library) and reads `slskd_downloads`. Never require access to the
  whole music library.
- **Never touch Navidrome's or slskd's databases.** They are owned by running
  processes — querying or (worse) checkpointing/writing their SQLite files,
  including from `sqlite3` for diagnosis, corrupts the live WAL/FTS state. (A
  `PRAGMA wal_checkpoint(TRUNCATE)` on a live `navidrome.db` removed its
  `-wal`/`-shm` files and broke search globally.) Read Navidrome/slskd state
  **only** through their APIs (Subsonic `getSong`/`search3`/`getPlaylist`/
  `getScanStatus`; slskd REST) — they return live, authoritative data. The app's
  own `state.db` is the only database to read or write directly.

## Invariants to preserve

- **Idempotent & resumable.** All progress is in SQLite. Re-runs must not
  duplicate playlists or songs. Playlists key on LB entry id; tracks on
  (playlist, recording MBID). Adding to a playlist skips song ids already present.
- **Best-effort.** A missing/failed track never blocks the rest; the playlist
  fills in over later ticks. A playlist is `done` only when every track is
  `in_playlist`; `done` is terminal.
- **Self-cleaning in slskd.** Delete searches after reading responses; remove
  completed transfers after import and failed ones when abandoned.
- **Bounded ticks.** New slskd searches are capped per playlist per tick
  (`maxNewDownloadsPerTick`) and run sequentially (polite). Don't add unbounded
  parallel searching.

## Testing conventions

- Unit-test pure logic directly (`match`, `slskd/select.go`, `files`,
  `searchQueries`, store methods).
- Test API clients with `httptest` (see `navidrome`/`slskd` tests).
- Live integration tests are **gated by env vars** and skipped by default:
  `NAVIDROME_LIVE=1`, `SLSKD_LIVE=1`. Keep new live-only tests gated the same way.
- The ListenBrainz parser is tested against a real saved feed in
  `internal/listenbrainz/testdata/`.

## Gotchas learned the hard way (keep these)

- **slskd `/responses` only returns data once the search reaches `Completed`.**
  Wait for completion (popular tracks take longest); don't read on a short timeout.
- **slskd search ANDs all terms against the file path.** Including the artist can
  return zero results. Hence the query escalation: `artist title` → `title` →
  simplified title.
- **Many Soulseek peers are unreachable** (enqueue → 500/timeout). Rank returns
  *all* candidates; try several until one accepts.
- **Navidrome indexes by tags**, but tagless downloads are saved by our
  `"<artist> - <title>.<ext>"` rename (Navidrome falls back to filename). Keep the
  rename.
- **A `downloaded` track must be re-resolved** after the rescan — the resolve pass
  handles `downloaded`; only `downloading` is skipped there.
- **Rescan needs an admin Navidrome user** (Subsonic `startScan`); it uses the
  first feed's credentials.
- **Per-playlist Navidrome client is keyed by feed name** → removing/renaming a
  feed orphans its playlists. Known limitation; don't "fix" silently.
- Don't set slskd `retention.files.complete` and keep
  `transfers.download.succeeded` long — the app imports from that record.

## When you change X, also update Y

- **Config schema** (`internal/config`) → update `config.example.yaml`,
  `config.local.yaml`, `docker-compose.example.yaml`, `README.md` table, and
  `docs/behavior.md`.
- **Runtime behavior / state machine** → update `docs/behavior.md`.
- **A new package or data-flow change** → update the layout in `README.md` and
  `AGENTS.md`.

## Dev workflow

- Local stack (Navidrome + slskd + sample tracks): `deploy/` — see
  `deploy/README.md`. Run the daemon on the host against it with
  `go run ./cmd/navidrome-lb-jams -config config.local.yaml` (or `-once`).
- Real slskd searches need a connection that allows inbound peer connections
  (port 50300 forwarded). Behind CGNAT/mobile they return nothing — that's
  environmental, not a bug.
- Commit only when asked.