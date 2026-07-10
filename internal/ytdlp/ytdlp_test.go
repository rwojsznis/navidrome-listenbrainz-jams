package ytdlp

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/config"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/importer"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/store"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/tags"
)

func TestBuildArgs(t *testing.T) {
	cfg := config.Ytdlp{AudioFormat: "mp3", MaxDuration: 10 * time.Minute}
	args := buildArgs(cfg, "/scratch", "The Beatles", "Come Together")

	joined := strings.Join(args, " ")
	// Search term is the last arg, prefixed for a single top hit.
	if args[len(args)-1] != "ytsearch1:The Beatles Come Together" {
		t.Errorf("query arg = %q", args[len(args)-1])
	}
	if !strings.Contains(joined, "--audio-format mp3") {
		t.Errorf("missing audio format: %q", joined)
	}
	// MaxDuration is rendered as whole seconds in the match-filter.
	if !containsPair(args, "--match-filter", "duration < 600") {
		t.Errorf("match-filter not 600s: %v", args)
	}
	if !containsPair(args, "-o", filepath.Join("/scratch", "%(id)s.%(ext)s")) {
		t.Errorf("output template wrong: %v", args)
	}
	// No cookies flag unless configured.
	if strings.Contains(joined, "--cookies") {
		t.Errorf("unexpected --cookies: %q", joined)
	}
}

func TestBuildArgsWithCookies(t *testing.T) {
	cfg := config.Ytdlp{AudioFormat: "opus", MaxDuration: time.Minute, CookiesFile: "/c/cookies.txt"}
	args := buildArgs(cfg, "/s", "A", "B")
	if !containsPair(args, "--cookies", "/c/cookies.txt") {
		t.Errorf("cookies not passed: %v", args)
	}
}

func containsPair(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}

func TestFindProduced(t *testing.T) {
	dir := t.TempDir()
	if _, ok, _ := mustFind(t, dir); ok {
		t.Fatal("empty dir should yield no file")
	}
	want := filepath.Join(dir, "abc123.mp3")
	if err := os.WriteFile(want, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok, err := findProduced(dir)
	if err != nil || !ok || got != want {
		t.Fatalf("findProduced = %q ok=%v err=%v, want %q", got, ok, err, want)
	}
}

func mustFind(t *testing.T, dir string) (string, bool, error) {
	t.Helper()
	got, ok, err := findProduced(dir)
	if err != nil {
		t.Fatalf("findProduced: %v", err)
	}
	return got, ok, err
}

// writeStub writes an executable shell script at binary path and returns it.
func writeStub(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "yt-dlp")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func newSource(t *testing.T, binary string) (*Source, string) {
	t.Helper()
	importDir := t.TempDir()
	cfg := &config.Config{
		Paths: config.Paths{ImportDir: importDir},
		Ytdlp: config.Ytdlp{
			BinaryPath:  binary,
			AudioFormat: "mp3",
			MaxDuration: 10 * time.Minute,
			Timeout:     30 * time.Second,
		},
	}
	// nil scan -> importer's rescan is a no-op.
	im := importer.New(nil, cfg, slog.Default())
	return New(cfg, im, tags.Writer{}, slog.Default()), importDir
}

func TestAdvanceImportsStubOutput(t *testing.T) {
	// Stub yt-dlp: locate the -o template, write a file into its directory.
	stub := writeStub(t, `
out=""
while [ $# -gt 0 ]; do
  if [ "$1" = "-o" ]; then out="$2"; shift 2; else shift; fi
done
dir=$(dirname "$out")
printf audio > "$dir/dQw4w9.mp3"
`)
	s, importDir := newSource(t, stub)
	tr := &store.Track{Artist: "Rick Astley", Title: "Never Gonna Give You Up", Status: store.TrackMissing, Attempts: 3}

	changed, err := s.Advance(context.Background(), tr)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if !changed {
		t.Error("expected changed=true")
	}
	if tr.Source != "ytdlp" {
		t.Errorf("Source = %q, want ytdlp", tr.Source)
	}
	if tr.Status != store.TrackDownloaded {
		t.Errorf("Status = %q, want downloaded", tr.Status)
	}
	want := filepath.Join(importDir, "Rick Astley - Never Gonna Give You Up.mp3")
	if tr.ImportedPath != want {
		t.Errorf("ImportedPath = %q, want %q", tr.ImportedPath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("imported file missing: %v", err)
	}
	// Scratch dir cleaned up (only the imported file remains in importDir).
	entries, _ := os.ReadDir(importDir)
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), ".ytdlp-") {
			t.Errorf("scratch dir not cleaned up: %s", e.Name())
		}
	}
}

func TestAdvanceFailureStampsSourceAndError(t *testing.T) {
	// Stub fails like a stale yt-dlp hitting YouTube's descrambler.
	stub := writeStub(t, `echo "ERROR: Signature extraction failed" >&2
exit 1
`)
	s, _ := newSource(t, stub)
	tr := &store.Track{Artist: "A", Title: "B", Status: store.TrackMissing, Attempts: 3}

	changed, err := s.Advance(context.Background(), tr)
	if err != nil {
		t.Fatalf("Advance should not return a hard error, got %v", err)
	}
	if !changed {
		t.Error("expected changed=true so the failure is persisted")
	}
	// One-shot: source stamped even on failure, so the chain won't retry yt-dlp.
	if tr.Source != "ytdlp" {
		t.Errorf("Source = %q, want ytdlp", tr.Source)
	}
	if tr.Status != store.TrackMissing {
		t.Errorf("Status = %q, want missing", tr.Status)
	}
	if !strings.Contains(tr.LastError, "Signature extraction failed") {
		t.Errorf("LastError = %q, want it to surface yt-dlp output", tr.LastError)
	}
}

func TestAdvanceNoResultFile(t *testing.T) {
	// Stub succeeds but produces nothing (e.g. everything failed the filter).
	stub := writeStub(t, `exit 0
`)
	s, _ := newSource(t, stub)
	tr := &store.Track{Artist: "A", Title: "B", Status: store.TrackMissing, Attempts: 3}

	if _, err := s.Advance(context.Background(), tr); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if tr.Status != store.TrackMissing || !strings.Contains(tr.LastError, "no file") {
		t.Errorf("Status=%q LastError=%q, want missing + 'no file'", tr.Status, tr.LastError)
	}
}
