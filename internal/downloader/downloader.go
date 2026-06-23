// Package downloader implements the slskd-backed download step of the pipeline.
// It advances a not-in-Navidrome track across ticks: search slskd -> enqueue the
// best candidate -> poll the transfer -> move the completed file into the library
// -> request a rescan. It satisfies pipeline.Downloader.
package downloader

import (
	"context"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/config"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/files"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/navidrome"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/slskd"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/store"
)

// searchTimeout is how long slskd gathers responses before completing a search.
const searchTimeout = 15 * time.Second

// maxEnqueueAttempts caps how many ranked candidates we try per track per pass
// before giving up for this tick (each failed enqueue can take ~10s in slskd).
const maxEnqueueAttempts = 5

// scanThrottle dedupes the rescans triggered when several files import (or
// several downloaded tracks await indexing) within a short window.
const scanThrottle = 10 * time.Second

// Downloader drives slskd downloads for tracks missing from Navidrome.
type Downloader struct {
	slskd *slskd.Client
	scan  *navidrome.Client // admin-capable client used to trigger library scans
	cfg   *config.Config
	log   *slog.Logger

	lastScan time.Time
}

// New returns a Downloader. scan is a Navidrome client used to trigger library
// rescans after imports (must be admin-capable).
func New(client *slskd.Client, scan *navidrome.Client, cfg *config.Config, log *slog.Logger) *Downloader {
	return &Downloader{slskd: client, scan: scan, cfg: cfg, log: log}
}

// triggerScan asks Navidrome to rescan, throttled so a burst of imports doesn't
// fire many scans. Ticks are single-threaded, so no locking is needed.
func (d *Downloader) triggerScan(ctx context.Context) {
	if !d.lastScan.IsZero() && time.Since(d.lastScan) < scanThrottle {
		return
	}
	d.lastScan = time.Now()
	if err := d.scan.StartScan(ctx); err != nil {
		d.log.Warn("trigger rescan", "err", err)
	}
}

// Advance moves a single track one step through the download state machine.
// It mutates t in place and returns whether t changed (so the caller persists it).
func (d *Downloader) Advance(ctx context.Context, t *store.Track) (bool, error) {
	switch t.Status {
	case store.TrackDownloading:
		return d.poll(ctx, t)
	case store.TrackDownloaded, store.TrackImported:
		// File is in the library but not yet found in Navidrome. Ensure a rescan
		// so a later resolve pass can pick it up. No state change here.
		d.triggerScan(ctx)
		return false, nil
	default: // pending or missing: try to (re)start a download
		return d.start(ctx, t)
	}
}

// start searches slskd and enqueues the best candidate.
func (d *Downloader) start(ctx context.Context, t *store.Track) (bool, error) {
	if t.Attempts >= d.cfg.Download.MaxRetries {
		// Give up searching; leave it missing for a future manual/seeded fix.
		if t.Status != store.TrackMissing {
			t.Status = store.TrackMissing
			return true, nil
		}
		return false, nil
	}

	// Soulseek matches every search term against the file path, so including the
	// artist can yield zero results when shared files don't carry the artist in
	// their path (common, e.g. "Come Together.flac"). Try the precise
	// "artist title" query first, then fall back to title-only; the title-aware
	// ranking still prioritizes candidates whose path contains the artist.
	criteria := slskd.Criteria{
		FormatPreference: d.cfg.Download.FormatPreference,
		MinBitrate:       d.cfg.Download.MinBitrate,
	}
	target := slskd.Target{Artist: t.Artist, Title: t.Title}

	var ranked []slskd.Candidate
	for _, query := range searchQueries(t) {
		responses, err := d.slskd.SearchAndWait(ctx, query, searchTimeout)
		if err != nil {
			return false, err
		}
		ranked = slskd.Rank(responses, criteria, target)
		if len(ranked) > 0 {
			break
		}
	}
	if len(ranked) == 0 {
		t.Attempts++
		t.Status = store.TrackMissing
		t.LastError = "no slskd candidate"
		return true, nil
	}

	// Many Soulseek peers are unreachable (enqueue returns a connection/timeout
	// error). Try candidates in ranked order until one accepts the download.
	var lastErr error
	for i, cand := range ranked {
		if i >= maxEnqueueAttempts {
			break
		}
		err := d.slskd.Enqueue(ctx, cand.Username, []slskd.QueueFile{{Filename: cand.File.Filename, Size: cand.File.Size}})
		if err != nil {
			lastErr = err
			d.log.Debug("enqueue failed, trying next candidate", "track", t.Title, "user", cand.Username, "err", err)
			continue
		}
		t.SlskdUsername = cand.Username
		t.SlskdFile = cand.File.Filename
		t.Status = store.TrackDownloading
		t.LastError = ""
		d.log.Info("download enqueued", "track", t.Title, "user", cand.Username, "file", files.BaseName(cand.File.Filename))
		return true, nil
	}

	t.Attempts++
	t.Status = store.TrackMissing
	if lastErr != nil {
		t.LastError = "all enqueues failed: " + lastErr.Error()
	}
	return true, nil
}

// poll checks the transfer state and imports the file once it succeeds.
func (d *Downloader) poll(ctx context.Context, t *store.Track) (bool, error) {
	// Timeout guard: if a download hasn't progressed within the budget, abandon
	// it and let it be retried (re-searched) on a later tick.
	if !t.UpdatedAt.IsZero() && time.Since(t.UpdatedAt) > d.cfg.Download.PerTrackTimeout {
		d.log.Warn("download timed out", "track", t.Title)
		return d.fail(t, "download timeout"), nil
	}

	transfer, found, err := d.slskd.FindDownload(ctx, t.SlskdUsername, t.SlskdFile)
	if err != nil {
		return false, err
	}
	if !found {
		// Transfer record gone (e.g. cleared). Re-search next tick.
		return d.fail(t, "transfer not found"), nil
	}
	if !transfer.IsComplete() {
		return false, nil // still in progress
	}
	if !transfer.Succeeded() {
		d.log.Warn("download failed", "track", t.Title, "state", transfer.State)
		// Drop the failed record so slskd's list doesn't accumulate dead entries.
		if err := d.slskd.RemoveDownload(ctx, t.SlskdUsername, transfer.ID); err != nil {
			d.log.Debug("remove failed transfer", "track", t.Title, "err", err)
		}
		return d.fail(t, "download state: "+transfer.State), nil
	}

	// Completed: locate the file on disk by basename and move it into the library.
	base := files.BaseName(t.SlskdFile)
	src, ok, err := files.FindByBasename(d.cfg.Paths.SlskdDownloads, base)
	if err != nil {
		return false, err
	}
	if !ok {
		// Transfer says done but file not visible yet; try again next tick.
		d.log.Warn("completed download not found on disk yet", "file", base)
		return false, nil
	}
	// Rename to a clean "<artist> - <title>.<ext>" from the feed metadata,
	// instead of the uploader's arbitrary filename. (Navidrome indexes by tags,
	// so this is purely for a tidy library on disk.)
	dstDir := d.cfg.Paths.ImportDir
	name := files.SanitizeFilename(t.Artist + " - " + t.Title)
	if name != "" {
		name += filepath.Ext(src)
	}
	imported, err := files.Move(src, dstDir, name)
	if err != nil {
		return false, err
	}
	t.ImportedPath = imported
	t.Status = store.TrackDownloaded
	t.LastError = ""
	// Remove the now-imported transfer from slskd's list (best-effort; the file
	// is already moved, so this only clears the record).
	if err := d.slskd.RemoveDownload(ctx, t.SlskdUsername, transfer.ID); err != nil {
		d.log.Debug("remove completed transfer", "track", t.Title, "err", err)
	}
	d.triggerScan(ctx)
	d.log.Info("imported download", "track", t.Title, "path", imported)
	return true, nil
}

// searchQueries returns the slskd queries to try for a track, most precise
// first, each used only if the previous returned nothing:
//  1. "artist title"        — precise
//  2. "title"               — recall (artist often absent from shared paths)
//  3. simplified title      — last resort: strip "(...)", "feat. ...", "- Live"
//     decorations that frequently prevent a match
// Duplicates and empties are dropped.
func searchQueries(t *store.Track) []string {
	var queries []string
	seen := map[string]bool{}
	add := func(q string) {
		q = strings.TrimSpace(q)
		if q != "" && !seen[q] {
			seen[q] = true
			queries = append(queries, q)
		}
	}
	add(t.Artist + " " + t.Title)
	add(t.Title)
	add(simplifyTitle(t.Title))
	return queries
}

var (
	parenRe = regexp.MustCompile(`\s*[\(\[][^\)\]]*[\)\]]`)               // (Remastered), [Live]
	featRe  = regexp.MustCompile(`(?i)\s+(feat\.?|ft\.?|featuring|with)\s+.*$`)
)

// simplifyTitle strips decorations that commonly differ between the feed title
// and shared filenames: parentheticals/brackets, "feat./ft." clauses, and a
// trailing " - ..." suffix (e.g. "- Remastered 2019", "- Live").
func simplifyTitle(title string) string {
	s := parenRe.ReplaceAllString(title, "")
	s = featRe.ReplaceAllString(s, "")
	if i := strings.Index(s, " - "); i > 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// fail records a failed attempt and resets the track so it is re-searched, or
// marks it missing once retries are exhausted.
func (d *Downloader) fail(t *store.Track, reason string) bool {
	t.Attempts++
	t.LastError = reason
	t.SlskdUsername = ""
	t.SlskdFile = ""
	t.Status = store.TrackMissing
	return true
}
