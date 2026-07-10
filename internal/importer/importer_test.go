package importer

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/config"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/store"
)

type fakeTagger struct {
	called    bool
	gotPath   string
	gotMBID   string
	returnErr error
}

func (f *fakeTagger) Tag(_ context.Context, path, feedMBID string) error {
	f.called = true
	f.gotPath = path
	f.gotMBID = feedMBID
	return f.returnErr
}

type fakeLyrics struct {
	status    string
	returnErr error
}

func (f *fakeLyrics) WriteAlongside(_ context.Context, _, _, _ string) (string, error) {
	return f.status, f.returnErr
}

// writeSrc creates a fake downloaded file in a scratch dir and returns its path.
func writeSrc(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestImportMovesRenamesAndTransitions(t *testing.T) {
	importDir := t.TempDir()
	cfg := &config.Config{Paths: config.Paths{ImportDir: importDir}}
	tagger := &fakeTagger{}
	lyr := &fakeLyrics{status: "synced"}

	// scan is nil: TriggerScan must be a no-op, not a panic.
	im := New(nil, cfg, slog.Default())
	im.SetTagger(tagger)
	im.SetLyrics(lyr)

	src := writeSrc(t, "peer-arbitrary-name.flac")
	tr := &store.Track{
		Artist:        "The Beatles",
		Title:         "Come Together",
		RecordingMBID: "mbid-123",
		Status:        store.TrackDownloading,
		LastError:     "stale error",
	}

	if err := im.Import(context.Background(), tr, src); err != nil {
		t.Fatalf("Import: %v", err)
	}

	// Renamed to "<artist> - <title>.<ext>" from feed metadata, keeping the ext.
	wantPath := filepath.Join(importDir, "The Beatles - Come Together.flac")
	if tr.ImportedPath != wantPath {
		t.Errorf("ImportedPath = %q, want %q", tr.ImportedPath, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("imported file missing: %v", err)
	}
	// Source is moved, not copied.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source still present after move: %v", err)
	}
	if tr.Status != store.TrackDownloaded {
		t.Errorf("Status = %q, want downloaded", tr.Status)
	}
	if tr.LastError != "" {
		t.Errorf("LastError = %q, want cleared", tr.LastError)
	}
	if tr.LyricsStatus != "synced" {
		t.Errorf("LyricsStatus = %q, want synced", tr.LyricsStatus)
	}
	// Tagger runs on the imported file with the feed's MBID.
	if !tagger.called || tagger.gotPath != wantPath || tagger.gotMBID != "mbid-123" {
		t.Errorf("tagger got path=%q mbid=%q called=%v", tagger.gotPath, tagger.gotMBID, tagger.called)
	}
}

func TestImportBestEffortOnHookErrors(t *testing.T) {
	importDir := t.TempDir()
	cfg := &config.Config{Paths: config.Paths{ImportDir: importDir}}
	// Both hooks fail: import must still succeed (best-effort) and leave the file.
	im := New(nil, cfg, slog.Default())
	im.SetTagger(&fakeTagger{returnErr: context.DeadlineExceeded})
	im.SetLyrics(&fakeLyrics{returnErr: context.DeadlineExceeded})

	src := writeSrc(t, "x.mp3")
	tr := &store.Track{Artist: "A", Title: "B", Status: store.TrackDownloading}
	if err := im.Import(context.Background(), tr, src); err != nil {
		t.Fatalf("Import should be best-effort, got %v", err)
	}
	if tr.Status != store.TrackDownloaded {
		t.Errorf("Status = %q, want downloaded", tr.Status)
	}
	// A failed lyrics lookup leaves the status unset (not clobbered with garbage).
	if tr.LyricsStatus != "" {
		t.Errorf("LyricsStatus = %q, want empty on lyrics error", tr.LyricsStatus)
	}
	if _, err := os.Stat(tr.ImportedPath); err != nil {
		t.Errorf("imported file missing after hook errors: %v", err)
	}
}

func TestImportNoHooks(t *testing.T) {
	importDir := t.TempDir()
	cfg := &config.Config{Paths: config.Paths{ImportDir: importDir}}
	im := New(nil, cfg, slog.Default())

	src := writeSrc(t, "y.opus")
	tr := &store.Track{Artist: "A", Title: "B", Status: store.TrackDownloading}
	if err := im.Import(context.Background(), tr, src); err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(tr.ImportedPath) != ".opus" {
		t.Errorf("ImportedPath = %q, want .opus extension", tr.ImportedPath)
	}
}
