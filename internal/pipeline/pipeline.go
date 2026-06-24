// Package pipeline drives playlist assembly: for each active playlist it
// resolves every track against Navidrome, downloads missing ones via the
// Downloader (when configured), and creates or backfills the Navidrome playlist
// owned by the feed's user. It is idempotent and resumable across ticks.
package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/config"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/match"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/navidrome"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/store"
)

// Downloader is the slskd-backed download step. It advances a track that is not
// present in Navidrome toward becoming available (searching slskd, enqueuing a
// download, importing a completed file). It returns whether the track's state
// changed so the pipeline knows to persist it. A nil Downloader disables the
// download path (tracks not in Navidrome are marked missing and retried later).
type Downloader interface {
	Advance(ctx context.Context, t *store.Track) (changed bool, err error)
}

// maxNewDownloadsPerTick caps how many fresh slskd searches a single playlist
// starts per tick, so ticks stay short and the import/backfill cycle is frequent.
const maxNewDownloadsPerTick = 5

// Pipeline orchestrates assembly across all feeds/playlists.
type Pipeline struct {
	store      *store.Store
	cfg        *config.Config
	clients    map[string]*navidrome.Client // keyed by feed name
	downloader Downloader
	log        *slog.Logger
}

// New builds a Pipeline with one Navidrome client per feed (per-user auth).
func New(st *store.Store, cfg *config.Config, log *slog.Logger) *Pipeline {
	clients := make(map[string]*navidrome.Client, len(cfg.Feeds))
	for _, f := range cfg.Feeds {
		clients[f.Name] = navidrome.New(cfg.Navidrome.URL, f.NavidromeUser, f.NavidromePass)
	}
	return &Pipeline{store: st, cfg: cfg, clients: clients, log: log}
}

// SetDownloader wires the slskd download step (optional).
func (p *Pipeline) SetDownloader(d Downloader) { p.downloader = d }

// Run advances every active (not-done) playlist by one step. It runs in two
// phases ACROSS ALL playlists: first the fast assemble pass (resolve against
// Navidrome + create/backfill the playlist), then the slow download pass. This
// ordering matters — a single playlist with many in-flight slskd downloads can
// take minutes in its download pass, and doing assemble-then-download per
// playlist would let that starve another playlist's backfill (e.g. a manual
// re-sync) for the rest of the tick. Splitting the phases keeps assembly
// responsive regardless of download load elsewhere.
func (p *Pipeline) Run(ctx context.Context) {
	playlists, err := p.store.ActivePlaylists()
	if err != nil {
		p.log.Error("list active playlists", "err", err)
		return
	}

	// Phase 1: assemble every playlist (fast).
	tracksByPlaylist := make(map[int64][]store.Track, len(playlists))
	for _, pl := range playlists {
		tracks, err := p.assemble(ctx, pl)
		if err != nil {
			p.log.Error("assemble playlist", "title", pl.Title, "err", err)
			continue
		}
		tracksByPlaylist[pl.ID] = tracks
	}

	// Phase 2: advance downloads (slow) and finalize completion.
	for _, pl := range playlists {
		tracks, ok := tracksByPlaylist[pl.ID]
		if !ok {
			continue // assemble failed; retry next tick
		}
		if err := p.download(ctx, pl, tracks); err != nil {
			p.log.Error("download playlist", "title", pl.Title, "err", err)
		}
	}
}

// assemble resolves a playlist's tracks against Navidrome and creates/backfills
// the Navidrome playlist from whatever is already available. It does NOT touch
// slskd. It returns the (mutated) track slice so the download pass can continue
// from the same in-memory state.
func (p *Pipeline) assemble(ctx context.Context, pl store.Playlist) ([]store.Track, error) {
	client := p.clients[pl.FeedName]
	if client == nil {
		return nil, fmt.Errorf("no navidrome client for feed %q", pl.FeedName)
	}

	tracks, err := p.store.TracksFor(pl.ID)
	if err != nil {
		return nil, fmt.Errorf("load tracks: %w", err)
	}

	// 1. Resolve every track not yet placed in the playlist.
	// 1. Resolve pass: match tracks against Navidrome (fast). Runs first so the
	//    playlist is created/backfilled from what's already available before we
	//    spend time on slow slskd downloads. Tracks mid-download are skipped here
	//    and handled in the download pass.
	for i := range tracks {
		t := &tracks[i]
		if t.Status == store.TrackInPlaylist {
			continue
		}
		if t.NavidromeSongID != "" {
			t.Status = store.TrackExists
			continue
		}
		// Downloading tracks aren't in the library yet; everything else
		// (including freshly imported "downloaded" tracks) is worth resolving.
		if t.Status == store.TrackDownloading {
			continue
		}
		if song, ok := p.resolve(ctx, client, t); ok {
			t.NavidromeSongID = song.ID
			t.Status = store.TrackExists
			t.LastError = ""
			if err := p.store.UpdateTrack(t); err != nil {
				return nil, err
			}
		}
	}

	// 2. Collect resolved song ids, in feed order.
	var toAdd []string
	for i := range tracks {
		if tracks[i].Status == store.TrackExists && tracks[i].NavidromeSongID != "" {
			toAdd = append(toAdd, tracks[i].NavidromeSongID)
		}
	}

	// 3. Find or create the Navidrome playlist, then add new songs (skipping any
	//    already present so re-runs don't duplicate entries). We do NOT return
	//    early when nothing is resolved yet — the download pass still needs to run.
	//    `placed` ends up holding every song id that is in the playlist now.
	placed := make(map[string]bool)
	navPl, err := client.FindPlaylistByName(ctx, pl.Title)
	if err != nil {
		return nil, fmt.Errorf("find playlist: %w", err)
	}
	switch {
	case navPl == nil && len(toAdd) == 0:
		// nothing to place yet; fall through to downloads
	case navPl == nil:
		navPl, err = client.CreatePlaylist(ctx, pl.Title, toAdd)
		if err != nil {
			return nil, fmt.Errorf("create playlist: %w", err)
		}
		p.log.Info("created playlist", "title", pl.Title, "user", pl.NavidromeUser, "songs", len(toAdd))
		_ = p.store.SetPlaylistNavidromeID(pl.ID, navPl.ID)
		// Re-read the actual contents: createPlaylist can silently drop ids
		// (e.g. a song reindexed to a new id mid-tick), so only mark tracks
		// placed that Navidrome really has. Dropped ones stay "exists" and are
		// retried next tick.
		placed = p.existingSongIDs(ctx, client, navPl.ID)
	default:
		if pl.NavidromePlaylistID == "" {
			_ = p.store.SetPlaylistNavidromeID(pl.ID, navPl.ID)
		}
		// Songs already in the playlist count as placed (idempotent reconcile).
		placed = p.existingSongIDs(ctx, client, navPl.ID)
		var newOnes []string
		for _, id := range toAdd {
			if !placed[id] {
				newOnes = append(newOnes, id)
			}
		}
		if len(newOnes) > 0 {
			if err := client.AddToPlaylist(ctx, navPl.ID, newOnes); err != nil {
				return nil, fmt.Errorf("add to playlist: %w", err)
			}
			p.log.Info("backfilled playlist", "title", pl.Title, "added", len(newOnes))
			// Re-read rather than trusting the add: Navidrome can accept the
			// request but not persist every id. Confirming against the real
			// contents keeps the dashboard honest and lets dropped songs (left
			// as "exists") be re-added on a later tick instead of being stuck.
			placed = p.existingSongIDs(ctx, client, navPl.ID)
		}
	}

	// 4. Mark every resolved track now present in the playlist as placed.
	for i := range tracks {
		t := &tracks[i]
		if t.Status == store.TrackExists && placed[t.NavidromeSongID] {
			t.Status = store.TrackInPlaylist
			if err := p.store.UpdateTrack(t); err != nil {
				return nil, err
			}
		}
	}

	return tracks, nil
}

// download advances any not-yet-available tracks via slskd and marks the
// playlist done once every track is placed. It is the slow pass and runs after
// every playlist has been assembled (see Run).
func (p *Pipeline) download(ctx context.Context, pl store.Playlist, tracks []store.Track) error {
	// 5. Download pass: poll in-flight downloads and import completed ones every
	//    tick (cheap), but cap NEW slskd searches per tick so a playlist with
	//    many missing tracks doesn't make a single tick run for many minutes —
	//    keeping the resolve/import/backfill cycle responsive.
	newSearches := 0
	for i := range tracks {
		t := &tracks[i]
		if t.Status == store.TrackInPlaylist || t.Status == store.TrackExists || t.NavidromeSongID != "" {
			continue
		}
		if p.downloader == nil {
			if t.Status != store.TrackMissing {
				t.Status = store.TrackMissing
				if err := p.store.UpdateTrack(t); err != nil {
					return err
				}
			}
			continue
		}
		// pending/missing need a fresh (slow) slskd search; bound those per tick.
		// downloading/downloaded are only polled/rescanned, which is cheap.
		if t.Status == store.TrackPending || t.Status == store.TrackMissing {
			if newSearches >= maxNewDownloadsPerTick {
				continue
			}
			newSearches++
		}
		changed, derr := p.downloader.Advance(ctx, t)
		if derr != nil {
			p.log.Warn("download advance", "track", t.Title, "err", derr)
		}
		if changed {
			if err := p.store.UpdateTrack(t); err != nil {
				return err
			}
		}
	}

	// 6. Mark the playlist done only when every track is placed. Tracks still
	//    missing keep the playlist active so later ticks can backfill them.
	allPlaced := true
	for i := range tracks {
		if tracks[i].Status != store.TrackInPlaylist {
			allPlaced = false
			break
		}
	}
	if allPlaced {
		p.log.Info("playlist complete", "title", pl.Title, "tracks", len(tracks))
		return p.store.SetPlaylistStatus(pl.ID, store.PlaylistDone)
	}
	return nil
}

// resolve searches Navidrome for a track and returns the best match, preferring
// an exact MusicBrainz recording-id match over fuzzy text matching.
func (p *Pipeline) resolve(ctx context.Context, client *navidrome.Client, t *store.Track) (*navidrome.Song, bool) {
	songs, err := client.Search3(ctx, t.Artist+" "+t.Title, 50)
	if err != nil {
		p.log.Warn("navidrome search", "track", t.Title, "err", err)
		return nil, false
	}
	return selectMatch(songs, t, p.cfg.Matching.FuzzyThreshold)
}

// selectMatch picks the library song for a feed track. A matching MusicBrainz
// recording id is authoritative (it provably identifies the same recording) and
// takes priority over any fuzzy text match; fuzzy is the fallback for songs
// without an MBID tag. It is pure so it can be tested without a Navidrome client.
func selectMatch(songs []navidrome.Song, t *store.Track, threshold float64) (*navidrome.Song, bool) {
	if t.RecordingMBID != "" {
		for i := range songs {
			if songs[i].MusicBrainzID != "" && strings.EqualFold(songs[i].MusicBrainzID, t.RecordingMBID) {
				return &songs[i], true
			}
		}
	}
	cands := make([]match.Candidate, len(songs))
	for i, s := range songs {
		cands[i] = match.Candidate{Artist: s.Artist, Title: s.Title}
	}
	res, ok := match.Best(match.Candidate{Artist: t.Artist, Title: t.Title}, cands, threshold)
	if !ok {
		return nil, false
	}
	return &songs[res.Index], true
}

func (p *Pipeline) existingSongIDs(ctx context.Context, client *navidrome.Client, playlistID string) map[string]bool {
	set := make(map[string]bool)
	full, err := client.GetPlaylist(ctx, playlistID)
	if err != nil {
		p.log.Warn("get playlist entries", "id", playlistID, "err", err)
		return set
	}
	for _, e := range full.Entry {
		set[e.ID] = true
	}
	return set
}
