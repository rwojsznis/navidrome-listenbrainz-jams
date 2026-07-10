// Package importer holds the shared post-download import tail used by every
// acquisition source (slskd, yt-dlp). Once a source has a freshly acquired file
// on disk, it hands the file to Import, which moves it into the library under a
// clean name, runs the optional fingerprint/tag and lyrics steps, and triggers a
// throttled Navidrome rescan. It is source-agnostic: it knows nothing about how
// the file was obtained (no slskd transfer-record cleanup lives here).
package importer

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/config"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/files"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/navidrome"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/store"
)

// scanThrottle dedupes the rescans triggered when several files import (or
// several downloaded tracks await indexing) within a short window.
const scanThrottle = 10 * time.Second

// MBIDTagger optionally identifies a freshly imported file and writes its
// MusicBrainz recording id into the file's tags (so Navidrome indexes it).
// feedMBID is the feed's recording id, used to prefer a confirmed match. A nil
// tagger disables the step; failures are non-fatal (logged, import proceeds).
type MBIDTagger interface {
	Tag(ctx context.Context, path, feedMBID string) error
}

// LyricsWriter optionally fetches lyrics for a freshly imported file and writes
// them as a sibling ".lrc" (no-op if one already exists). A nil writer disables
// the step; failures are non-fatal (logged, import proceeds).
type LyricsWriter interface {
	// WriteAlongside writes a sibling .lrc for the imported file and returns the
	// resulting lyrics status ("synced"/"plain"/"none") to record on the track.
	WriteAlongside(ctx context.Context, musicPath, artist, title string) (string, error)
}

// Importer moves a freshly acquired file into the library and runs the optional
// tag + lyrics steps, then triggers a (throttled) rescan. It is shared by every
// download source.
type Importer struct {
	scan   *navidrome.Client // admin-capable client used to trigger library scans
	cfg    *config.Config
	tagger MBIDTagger   // optional acoustic-fingerprint tagger
	lyrics LyricsWriter // optional lyrics fetcher
	log    *slog.Logger

	lastScan time.Time
}

// New returns an Importer. scan is a Navidrome client used to trigger library
// rescans after imports (must be admin-capable); it may be nil in tests, in
// which case rescans are skipped.
func New(scan *navidrome.Client, cfg *config.Config, log *slog.Logger) *Importer {
	return &Importer{scan: scan, cfg: cfg, log: log}
}

// SetTagger wires the optional fingerprint/tag step (run on each imported file).
func (im *Importer) SetTagger(t MBIDTagger) { im.tagger = t }

// SetLyrics wires the optional lyrics-fetch step (run on each imported file).
func (im *Importer) SetLyrics(l LyricsWriter) { im.lyrics = l }

// TriggerScan asks Navidrome to rescan, throttled so a burst of imports doesn't
// fire many scans. Ticks are single-threaded, so no locking is needed. It is
// exported so a source can request a rescan for a track already imported on a
// previous tick (awaiting indexing) without re-importing.
func (im *Importer) TriggerScan(ctx context.Context) {
	if im.scan == nil {
		return
	}
	if !im.lastScan.IsZero() && time.Since(im.lastScan) < scanThrottle {
		return
	}
	im.lastScan = time.Now()
	if err := im.scan.StartScan(ctx); err != nil {
		im.log.Warn("trigger rescan", "err", err)
	}
}

// Import moves srcPath into cfg.Paths.ImportDir renamed from the feed metadata,
// tags + fetches lyrics (best-effort), and triggers a rescan. It sets
// t.ImportedPath, t.LyricsStatus and t.Status = TrackDownloaded. It does NOT
// know about the acquisition source (no transfer-record cleanup, no provenance):
// the caller records t.Source.
func (im *Importer) Import(ctx context.Context, t *store.Track, srcPath string) error {
	// Rename to a clean "<artist> - <title>.<ext>" from the feed metadata,
	// instead of the source's arbitrary filename. (Navidrome indexes by tags, so
	// this is purely for a tidy library on disk — and helps Navidrome index
	// tag-less rips by falling back to the filename.)
	dstDir := im.cfg.Paths.ImportDir
	name := files.SanitizeFilename(t.Artist + " - " + t.Title)
	if name != "" {
		name += filepath.Ext(srcPath)
	}
	imported, err := files.Move(srcPath, dstDir, name)
	if err != nil {
		return err
	}
	t.ImportedPath = imported
	t.Status = store.TrackDownloaded
	t.LastError = ""

	// Optionally fingerprint the imported file and write its MusicBrainz recording
	// id into the tags before the rescan, so Navidrome indexes it with the id.
	// Best-effort: a failure leaves the file untagged but still imported.
	if im.tagger != nil {
		if err := im.tagger.Tag(ctx, imported, t.RecordingMBID); err != nil {
			im.log.Warn("fingerprint/tag", "track", t.Title, "err", err)
		}
	}
	// Optionally fetch lyrics and write them as a sibling .lrc. Best-effort: a
	// failure (or no lyrics found) leaves the file imported without lyrics.
	if im.lyrics != nil {
		if status, err := im.lyrics.WriteAlongside(ctx, imported, t.Artist, t.Title); err != nil {
			im.log.Warn("fetch lyrics", "track", t.Title, "err", err)
		} else {
			t.LyricsStatus = status
		}
	}
	im.TriggerScan(ctx)
	im.log.Info("imported file", "track", t.Title, "path", imported)
	return nil
}
