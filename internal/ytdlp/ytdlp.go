// Package ytdlp implements the optional yt-dlp fallback download source. It is a
// pipeline.Downloader tried only after slskd has exhausted its retries for a
// track (the handoff lives in internal/downloadchain). Unlike slskd — a stateful
// service polled across ticks — yt-dlp is a stateless subprocess that searches
// and downloads in one invocation, so a single Advance either produces an
// imported file or fails. There is no "downloading" state for it.
package ytdlp

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/config"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/importer"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/store"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/tags"
)

// Source fetches a single track's audio from YouTube via yt-dlp and hands the
// result to the shared importer. It satisfies pipeline.Downloader.
type Source struct {
	cfg      *config.Config
	importer *importer.Importer
	tagger   tags.Writer
	log      *slog.Logger
}

// New returns a Source. im is the shared importer used to move the downloaded
// file into the library (identical import path as slskd downloads). tagger writes
// the feed's artist/title into the tag-less YouTube rip so Navidrome doesn't index
// it as "[Unknown Artist]" with the filename as its title.
func New(cfg *config.Config, im *importer.Importer, tagger tags.Writer, log *slog.Logger) *Source {
	return &Source{cfg: cfg, importer: im, tagger: tagger, log: log}
}

// Advance fetches t's audio from YouTube in one shot. It is called only by the
// chain, only for a track slskd has already exhausted. On success the track is
// imported and left in TrackDownloaded; on any failure it is left TrackMissing
// with LastError set. Either way t.Source is stamped "ytdlp" up front so the
// attempt can't loop.
func (s *Source) Advance(ctx context.Context, t *store.Track) (bool, error) {
	// One-shot: stamp provenance before doing any work so a crash or a failure
	// mid-run still records that yt-dlp was tried (the chain won't hand off again).
	t.Source = "ytdlp"

	// yt-dlp writes into a scratch dir (not ImportDir directly — the importer
	// moves it there under the clean name). Keep the scratch inside ImportDir's
	// filesystem so the import move is a fast same-fs rename, and so it works in a
	// read-only container where /tmp may be unwritable. The leading dot keeps
	// Navidrome from indexing the short-lived scratch dir.
	if err := os.MkdirAll(s.cfg.Paths.ImportDir, 0o755); err != nil {
		return s.fail(t, "create import dir: "+err.Error()), nil
	}
	tmpDir, err := os.MkdirTemp(s.cfg.Paths.ImportDir, ".ytdlp-")
	if err != nil {
		return s.fail(t, "create scratch dir: "+err.Error()), nil
	}
	defer os.RemoveAll(tmpDir)

	runCtx, cancel := context.WithTimeout(ctx, s.cfg.Ytdlp.Timeout)
	defer cancel()

	args := buildArgs(s.cfg.Ytdlp, tmpDir, t.Artist, t.Title)
	out, err := exec.CommandContext(runCtx, s.cfg.Ytdlp.BinaryPath, args...).CombinedOutput()
	if err != nil {
		return s.fail(t, "yt-dlp: "+errWithOutput(err, out)), nil
	}

	produced, ok, err := findProduced(tmpDir)
	if err != nil {
		return s.fail(t, "scan scratch dir: "+err.Error()), nil
	}
	if !ok {
		// No result matched (e.g. everything failed the duration filter).
		return s.fail(t, "yt-dlp produced no file"), nil
	}

	// YouTube rips carry no tags, so embed the feed's artist/title before import.
	// Without this Navidrome indexes the file as "[Unknown Artist]" with the
	// filename as its title. Best-effort: the fingerprint tagger (in the importer)
	// may additionally add the recording MBID, and a failure here still imports.
	if err := s.tagger.WriteBasic(ctx, produced, t.Artist, t.Title); err != nil {
		s.log.Warn("write basic tags", "track", t.Title, "err", err)
	}

	// Import runs its own steps (tag/lyrics/rescan) which may hit the network, so
	// use the outer ctx rather than the download timeout.
	if err := s.importer.Import(ctx, t, produced); err != nil {
		return s.fail(t, "import: "+err.Error()), nil
	}
	s.log.Info("imported yt-dlp download", "track", t.Title, "path", t.ImportedPath)
	return true, nil
}

// buildArgs assembles the yt-dlp command line for a search-and-download of the
// top hit for "<artist> <title>". It is pure so it can be unit-tested.
func buildArgs(cfg config.Ytdlp, tmpDir, artist, title string) []string {
	query := strings.TrimSpace(artist + " " + title)
	args := []string{
		"-x",
		"--audio-format", cfg.AudioFormat,
		"--audio-quality", "0",
		"--no-playlist",
		"--match-filter", fmt.Sprintf("duration < %d", int(cfg.MaxDuration.Seconds())),
		"--no-progress",
		"-o", filepath.Join(tmpDir, "%(id)s.%(ext)s"),
	}
	if cfg.CookiesFile != "" {
		args = append(args, "--cookies", cfg.CookiesFile)
	}
	args = append(args, "ytsearch1:"+query)
	return args
}

// findProduced returns the single audio file yt-dlp wrote into dir. yt-dlp names
// it "<id>.<ext>", and with ytsearch1 + --no-playlist there is at most one, so we
// take the first regular file found. ok is false when the dir is empty (no hit).
func findProduced(dir string) (string, bool, error) {
	var found string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return found, found != "", nil
}

// errWithOutput folds the tail of yt-dlp's combined output into the error so the
// dashboard's last_error is actionable (e.g. "Signature extraction failed"),
// bounded so a wall of output doesn't bloat the stored row.
func errWithOutput(err error, out []byte) string {
	msg := strings.TrimSpace(string(out))
	const max = 300
	if len(msg) > max {
		msg = msg[len(msg)-max:]
	}
	if msg == "" {
		return err.Error()
	}
	return err.Error() + ": " + msg
}

// fail records a failed yt-dlp attempt: the track stays missing (the chain's
// one-shot guard, t.Source == "ytdlp", stops it being retried by yt-dlp). It
// always reports the track as changed so the reason is persisted.
func (s *Source) fail(t *store.Track, reason string) bool {
	s.log.Warn("yt-dlp fallback failed", "track", t.Title, "reason", reason)
	t.Status = store.TrackMissing
	t.LastError = reason
	return true
}
