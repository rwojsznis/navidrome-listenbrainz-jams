// Package downloadchain composes acquisition sources into a fallback chain: the
// primary source (slskd) is always tried first, and only once it has exhausted
// its retries for a track does the fallback (yt-dlp) get a turn. It lives outside
// internal/pipeline so the pipeline.Downloader interface stays a clean single
// method, and it reaches into neither source's internals — the handoff decision
// is made purely from generic track state.
package downloadchain

import (
	"context"

	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/config"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/pipeline"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/store"
)

// Chain runs primary first, then fallback when primary has given up on a track.
type Chain struct {
	primary  pipeline.Downloader // slskd
	fallback pipeline.Downloader // yt-dlp
	cfg      *config.Config
}

// New builds a Chain. Both sources satisfy pipeline.Downloader.
func New(primary, fallback pipeline.Downloader, cfg *config.Config) *Chain {
	return &Chain{primary: primary, fallback: fallback, cfg: cfg}
}

// Advance runs the primary source, then hands off to the fallback when — and
// only when — slskd has exhausted this track (missing + retries used up) and
// yt-dlp hasn't already tried it. The predicate is generic track state, so the
// chain never touches slskd or yt-dlp internals.
func (c *Chain) Advance(ctx context.Context, t *store.Track) (bool, error) {
	changed, err := c.primary.Advance(ctx, t)
	if err != nil {
		return changed, err
	}
	if t.Status == store.TrackMissing &&
		t.Attempts >= c.cfg.Download.MaxRetries &&
		t.Source != "ytdlp" {
		return c.fallback.Advance(ctx, t)
	}
	return changed, nil
}
