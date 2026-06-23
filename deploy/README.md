# Local dev stack

Spins up Navidrome + slskd for the service to talk to. The service itself runs
on the host (`go run`) against the published ports, so you can rebuild it
quickly.

## 1. (Optional) Soulseek credentials

To let slskd actually search/download from the Soulseek network, create
`deploy/.env` (gitignored):

```
SLSKD_SLSK_USERNAME=your_soulseek_user
SLSKD_SLSK_PASSWORD=your_soulseek_pass
```

Without these, slskd still starts and its REST API works, but network searches
return nothing — fine for testing the "already in Navidrome" path.

## 2. Seed sample tracks

```
deploy/seed-music.sh
```

Generates 3 tagged FLAC files (Kate Bush – Army Dreamers, Gorillaz – Feel Good
Inc., Olivia Rodrigo – vampire) into `data/music`. These match tracks in the
`emq` weekly-jams feed, so they exercise the match path; the feed's other tracks
exercise the slskd download path.

## 3. Start the stack

```
cd deploy
docker compose up -d
```

- Navidrome: http://localhost:4533
- slskd:     http://localhost:5030

## 4. Create the Navidrome user

On first launch, open http://localhost:4533 and create the initial admin user
with username **`emq`** and password **`emqpassword`** (these match
`config.local.yaml`). The first user created becomes admin; playlists are owned
per-user, so the feed's `navidrome_user` must exist.

Trigger an initial scan (Settings → or it scans on startup) so the seeded tracks
are indexed.

## 5. Run the service against the stack

From the repo root:

```
go run ./cmd/navidrome-lb-jams -config config.local.yaml
```

## slskd REST API key

The service authenticates with the API key defined in `deploy/slskd/slskd.yml`
(`dev-slskd-api-key-change-me`), matching `slskd.api_key` in `config.local.yaml`.
