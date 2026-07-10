# Behavior & lifecycle

How the daemon behaves at runtime — the bits that aren't obvious from the config
or code. For setup/config see the [README](../README.md).

## The tick loop

The daemon runs on a timer (`poll_interval`). Each tick:

1. **Discover** — fetch every *configured* feed and upsert its entries/tracks
   into the SQLite store (idempotent; see below).
2. **Assemble (fast pass, across *all* playlists)** — for each pending playlist:
   1. **Resolve** — look up each unplaced track in Navidrome (MusicBrainz
      recording-id match, else fuzzy; see *Matching strategy*) and mark the ones
      found.
   2. **Create / backfill** — create the Navidrome playlist (or add newly
      resolved tracks to the existing one), then re-read its actual contents to
      confirm what landed (see *Honest placed count*).
3. **Download (slow pass, across *all* playlists)** — for tracks still missing,
   advance the slskd download state machine (search → enqueue → poll → import).

The two passes run **across all playlists**, not playlist-by-playlist: every
playlist is assembled first, then every playlist downloads. This matters because
the download pass is slow (each slskd search can wait 75–105s) — doing it
per-playlist would let one busy playlist starve fast backfills/re-syncs
elsewhere. The assemble pass is cheap; the download pass is the slow part and is
rate-limited (below). All state is persisted, so a restart resumes exactly where
it left off — nothing is re-downloaded.

If a tick takes longer than `poll_interval`, ticks simply run back-to-back (the
timer coalesces) — they never overlap.

## Playlist state machine

A playlist is `pending` until **every** track is placed in the Navidrome
playlist, at which point it becomes `done`.

```
pending ──(all tracks in_playlist)──▶ done
```

- The daemon only processes `pending` playlists. **`done` is terminal** — the
  playlist is never looked at again.
- We do **not** reconcile a `done` playlist against Navidrome. If you delete it
  in Navidrome afterwards, the app will **not** recreate it.
- A playlist with permanently-missing tracks (not on Soulseek, retries
  exhausted) never reaches `done`; it stays `pending` and keeps re-checking the
  gaps cheaply each tick.
- The only way to revive a `done` playlist (or force missing tracks to retry) is
  the **Retry** buttons in the dashboard, which reset tracks **and** flip the
  playlist back to `pending`.

## Track state machine

```
pending ─┬─ (found in Navidrome) ───────────────▶ exists ──▶ in_playlist
         └─ (not found) ─▶ slskd search/enqueue ─▶ downloading
downloading ─┬─ (transfer succeeded) ─▶ move file ─▶ downloaded ─(rescan)─▶ (resolve) ─▶ exists ─▶ in_playlist
             └─ (failed / timed out / no candidate) ─▶ missing
missing ──(retried next tick, until max_retries)──▶ …
missing (slskd retries exhausted) ─┬─ yt-dlp enabled ─▶ yt-dlp fetch ─▶ downloaded ─(rescan)─▶ …
                                   └─ (fetch fails / disabled) ─▶ missing (one-shot; stays put)
```

- `exists` = found in Navidrome, song id known; gets added to the playlist in
  the same tick.
- `downloaded` (and its synonym `imported`) = file moved into the import dir
  (and, if fingerprinting/lyrics are on, tagged with its MusicBrainz recording id
  and given a sibling `.lrc`), awaiting a Navidrome rescan; it's re-resolved on a
  later tick once indexed. The downloader keeps re-triggering a (throttled)
  rescan for any track left in this state, so it self-heals across restarts.
- `missing` = not found and not (yet) downloadable. Re-attempted each tick until
  `attempts` reaches `max_retries`, then left alone (still revivable via Retry) —
  unless the yt-dlp fallback is enabled, in which case one last-resort attempt is
  made at that point (see *Fallback source (yt-dlp)*).

Each track also records a `source` (`""` / `slskd` / `ytdlp`) marking which
backend acquired it — dashboard provenance, and the guard that keeps the yt-dlp
attempt one-shot.

## Idempotency & resume

- Playlists are keyed by the ListenBrainz **entry id**; tracks by
  **(playlist, recording MBID)**. Re-discovering the same feed entry is a no-op
  (`INSERT … ON CONFLICT DO NOTHING`).
- Adding tracks to a Navidrome playlist skips song ids already present, so
  re-runs never create duplicates.
- Each new week is a **new feed entry** → a **new playlist row**, processed
  fresh. Old weeks remain in their final state.

## Honest placed count

A track is marked `in_playlist` only after the create/backfill step **re-reads
the Navidrome playlist** (`getPlaylist`) and confirms the song id is actually
present. Navidrome can silently drop ids it's handed — e.g. when a freshly
imported track is still being indexed, or when ids churn during a reindex — so
the count would otherwise drift above what the playlist really contains. Dropped
ids stay `exists` and are re-added on the next tick (self-healing), and the
dashboard's "N/M placed" stays truthful.

## Dashboard actions

The dashboard is read-only except for a few explicit recovery buttons. Each one
resets state in the store; the running daemon picks the change up on its next
tick (they share the SQLite store).

- **Retry** (per missing track) / **Retry N missing** (per playlist) — reset the
  track(s) to `pending` (attempts/error/`source` cleared) **and** flip the
  playlist back to `pending`. The only way to revive a `done` playlist or
  re-attempt retry-exhausted tracks — and, with yt-dlp enabled, clearing `source`
  is what lets a yt-dlp one-shot start over (slskd first, then yt-dlp again).
- **Re-sync** (per playlist) — demote every `in_playlist` track back to `exists`
  (keeping its song id — **no re-download**) and reactivate the playlist, forcing
  a fresh re-add from actual Navidrome contents. Use it when a `done` playlist's
  count drifted or songs were dropped.
- **Tag MBID** (per stuck `downloaded`/`imported` track) — write the feed's known
  recording MBID straight into the imported file's tags and trigger a rescan, so
  the next resolve matches by id. Fixes files whose tags carry decorations that
  defeat fuzzy matching (e.g. scene-tag titles, translated titles). Falls back to
  AcoustID identification only if the feed track has no recording MBID.
- **Delete & restart** (per stuck `downloaded`/`imported` track) — delete the
  wrong imported file **and its sibling `.lrc`**, then reset the track so it is
  searched and downloaded again from scratch. Use it when the downloader fetched
  the wrong file (wrong version, mislabeled rip).
- **Re-scan lyrics** (per playlist) — fetch lyrics for every imported track that
  doesn't already have a `.lrc`. Runs in the background; reload to see updated
  statuses. Handy for backfilling tracks imported before lyrics were enabled.

## Adding / removing / renaming a feed

- **Add a feed** → discovered and processed on the next tick. ✅
- **Remove or rename a feed** → ⚠️ the feed's existing playlists become
  **orphaned**. The per-playlist Navidrome client is keyed by **feed name** (from
  the current config), so a playlist whose feed name is no longer configured logs
  `no navidrome client for feed "<name>"` every tick and **stalls** — it does not
  continue, is not marked done, and its tracks are not imported. The state row
  survives; processing resumes only if that feed name reappears in config.
  - In-flight slskd transfers that were already enqueued keep running on slskd's
    side, but the app won't poll/import them while the playlist is orphaned.
  - Swapping a feed for the **same `navidrome_user`** still orphans the old
    playlist today, because the lookup is by feed name, not user. (A possible
    future improvement is to resolve the client by `navidrome_user`.)

## Matching strategy (Navidrome)

When resolving a feed track against Navidrome (`search3` on `"<artist> <title>"`,
top 50), candidates are matched in priority order:

1. **MusicBrainz recording id** — if a candidate's `musicBrainzId` equals the
   feed track's recording MBID, it's an exact, authoritative match and wins over
   everything else (no fuzzy needed). This is what the optional fingerprinting
   step enables for downloaded files.
2. **Decoration-aware fuzzy** — otherwise, a normalized artist+title similarity
   (≥ `fuzzy_threshold`). Both sides are also compared with decorations stripped
   (`(PMEDIA)`, `(feat. …)`, `- Remastered`, …) plus a length-guarded containment
   check, so library tags that carry extra cruft the feed lacks — or a leading
   `The` — still match. This tolerance is why a file the downloader fetched (via
   the simplified-title search) resolves back to its library entry.

Without it, decorated tags scored below threshold and tracks could sit in
`downloaded` forever — and the same false-negative would re-download a track
already in the library.

## Search strategy (slskd)

For a missing track, queries are tried **most-precise first**, each only if the
previous returned nothing:

1. `"<artist> <title>"` — precise
2. `"<title>"` — recall (Soulseek matches all terms against the file path, and
   shared files often lack the artist in their path)
3. **simplified title** — last resort; strips `(...)` / `[...]`, `feat./ft.`
   clauses, and a trailing `- …` suffix (e.g. `Song (Remastered 2019)` → `Song`)

slskd searches stay `InProgress` while responses trickle in, so the client waits
for the search to reach `Completed` (up to a generous cap) before reading
results — popular tracks return the most responses and take the longest.

## Candidate ranking

Among a search's files (locked files are ignored), candidates are ranked by:

1. all requested **title** words present in the filename (reject wrong songs)
2. **artist** present in the path
3. fewest **extra** words (so the original beats remix/live/edited versions)
4. preferred **format** (`format_preference`, e.g. `[flac, mp3, opus]`)
5. peer has a **free upload slot**
6. higher **bitrate**, faster uploader, shorter queue

`min_bitrate` rejects lossy files below the threshold (lossless formats are
exempt; opus is treated as lossy, so set a sensible `min_bitrate` if you enable
it). Because many peers are unreachable, the downloader tries the **top several
candidates** in order until one accepts the download.

## File import & naming

- Completed downloads are located in `slskd_downloads` by **basename** (so we
  don't depend on slskd's folder layout) and **moved** into `import_dir`.
- They are **renamed** to `"<artist> - <title>.<ext>"` from the feed metadata.
  This is cosmetic — Navidrome indexes by **tags** — but it also helps Navidrome
  index downloads that have **no tags** (it falls back to the filename).
- `import_dir` must be a subfolder **inside** Navidrome's library so the files
  get indexed. Mount only that folder into the container (least privilege).

## Acoustic fingerprinting (optional)

When `fingerprint.enabled` is set, each imported file is identified before the
rescan and tagged with its MusicBrainz recording id:

1. `fpcalc` (Chromaprint) computes a fingerprint + duration from the audio.
2. [AcoustID](https://acoustid.org) resolves the fingerprint to candidate
   recording MBIDs. The feed track's **own** recording id is chosen when it's
   among them (a confirmed match); otherwise the best-scoring candidate.
3. The id is written into the file's tags — `MUSICBRAINZ_TRACKID` Vorbis comment
   for FLAC (pure Go) and Opus (via the `opustags` binary), an ID3v2 `UFID` frame
   (owner `http://musicbrainz.org`) for MP3 (pure Go).

Navidrome then indexes the recording id, so the next resolve matches by id
exactly (see *Matching strategy*).

Properties:

- **Trusts the download** — it tags with the identified/feed id and never rejects
  on mismatch (no verification gate).
- **Best-effort** — fingerprint/lookup/tag failures are logged and the import
  still proceeds; a file AcoustID can't identify is left untagged.
- **Requirements** — the `fpcalc` and `opustags` binaries (bundled in the Docker
  image) and a free AcoustID API key. Disabled by default; downloads are imported
  untagged.

## Fallback source (yt-dlp)

When `ytdlp.enabled` is set, yt-dlp is chained **after** slskd as a last-resort
source. slskd is always tried first; yt-dlp only gets a turn once slskd has
exhausted its retries for a track.

- **Handoff predicate.** The chain hands off purely on generic track state —
  `missing` **and** `attempts >= max_retries` **and** `source != "ytdlp"`. It
  reaches into neither source's internals.
- **Synchronous, one-shot.** Unlike slskd (a stateful service polled across
  ticks), yt-dlp is a stateless subprocess that searches and downloads in one
  invocation — there is no `downloading` state for it. A single pass either
  imports a file or fails. Before doing any work it stamps `source = "ytdlp"`, so
  the attempt is **exactly once**: a failure leaves the track `missing` and the
  chain won't hand off to yt-dlp again (only a **Retry**, which clears `source`,
  lets slskd — and then yt-dlp — start over).
- **What it runs.** `yt-dlp -x --audio-format <fmt> --audio-quality 0
  --no-playlist --match-filter "duration < <max_duration_s>" -o <scratch>
  "ytsearch1:<artist> <title>"`, bounded by `ytdlp.timeout`. The `--match-filter`
  rejects hour-long album/live uploads; the download goes to a scratch dir inside
  `import_dir` (fast same-fs move, works under `--read-only`).
- **Same import tail.** On success the file goes through the identical importer as
  slskd downloads (move + rename + optional fingerprint-tag + optional lyrics +
  rescan) — the fingerprint tagger is what makes a tag-less YouTube rip index
  correctly, so pairing it with `fingerprint` is recommended.
- **Per-tick cost.** A yt-dlp fetch runs synchronously inside the download pass and
  is counted by the same `maxNewDownloadsPerTick` budget as a slskd search, so a
  burst of fallbacks can't make one tick run for many minutes.
- **Wrong-hit quality.** The top YouTube result can be a live/cover/sped-up
  version. The duration filter catches gross cases; the fingerprint tagger still
  tags it with *some* MBID. yt-dlp-sourced tracks are badged in the dashboard so
  they can be spotted and **Delete & restart**ed.
- **Staleness is operational, not automatic.** The image is immutable and does not
  self-update. When YouTube breaks the pinned yt-dlp (`Signature extraction
  failed` in `last_error`), bump `YTDLP_VERSION`/`DENO_VERSION` in the Dockerfile
  and pull a new image tag.
- **Requirements** — the `yt-dlp`, `ffmpeg`, and `deno` binaries (all bundled in
  the Docker image; deno is the JS runtime yt-dlp needs for YouTube signature
  descrambling). Disabled by default.

## Rescans

After importing a file, the daemon triggers a Navidrome `startScan` (throttled to
at most once per 10s to absorb bursts). The imported track is re-resolved on a
later tick once Navidrome has indexed it. Scans use the **first feed's**
credentials, which must therefore be an **admin-capable** Navidrome user
(Subsonic `startScan` requires admin).

## Download rate-limiting & concurrency

- New slskd searches are **sequential** and capped at **5 per playlist per tick**
  — so a playlist with many missing tracks doesn't make one tick run for many
  minutes, and the resolve/import/backfill cycle stays responsive.
- Already-downloading transfers are polled cheaply every tick (no cap).
- Actual transfer **concurrency is slskd's job** — the app only enqueues; slskd's
  own slot limits govern parallel transfers.

## Cleanup

The app cleans up after itself in slskd so its lists don't grow unbounded:

- **Searches** are deleted after their responses are read.
- **Completed** transfers are removed right after the file is imported (the file
  is already moved out, so only the record is cleared).
- **Failed** transfers are removed when abandoned.

As a backstop (e.g. for anything missed while the app is down), set slskd's own
`retention` in `slskd.yml` — see `deploy/slskd/slskd.yml`. Keep
`transfers.download.succeeded` generous (the app imports *from* that record), and
do **not** set `files.complete` (the app moves completed files out itself;
auto-deleting them risks losing a file before import).

## Failure handling (summary)

| Situation | Result |
|---|---|
| No usable slskd candidate | `missing`, `attempts++`, retried later |
| Enqueue rejected (peer unreachable) | try next ranked peer (up to ~5); else `missing` |
| Transfer errored/cancelled/timed-out | record removed, `missing`, re-searched next tick |
| Download stalls past `per_track_timeout` | abandoned → `missing` |
| `max_retries` reached (yt-dlp disabled) | stays `missing`; only Retry revives it |
| `max_retries` reached (yt-dlp enabled) | one yt-dlp fetch attempt; imported on success |
| yt-dlp fetch fails (no result / error / timeout) | stays `missing`, `source=ytdlp`; one-shot, only Retry re-attempts |
| Feed fetch fails (network) | logged and skipped; stored playlists still processed |
| Fingerprint / AcoustID / tag fails | logged; file imported untagged, import proceeds |
| Playlist's feed removed from config | orphaned/stalled (see above) |
