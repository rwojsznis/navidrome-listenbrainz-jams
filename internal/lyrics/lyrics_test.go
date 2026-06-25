package lyrics

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func testService(t *testing.T, h http.HandlerFunc) *Service {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestFetchPrefersSyncedFromGet(t *testing.T) {
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/get" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("artist_name"); got != "Lorde" {
			t.Errorf("artist_name = %q", got)
		}
		w.Write([]byte(`{"syncedLyrics":"[00:01.00] hi","plainLyrics":"hi","instrumental":false}`))
	})
	text, synced, ok, err := s.Fetch(context.Background(), "Lorde", "Royals")
	if err != nil || !ok {
		t.Fatalf("Fetch ok=%v err=%v", ok, err)
	}
	if !synced || text != "[00:01.00] hi" {
		t.Errorf("got synced=%v text=%q", synced, text)
	}
}

func TestFetchFallsBackToPlain(t *testing.T) {
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"syncedLyrics":"","plainLyrics":"just words","instrumental":false}`))
	})
	text, synced, ok, err := s.Fetch(context.Background(), "A", "B")
	if err != nil || !ok {
		t.Fatalf("Fetch ok=%v err=%v", ok, err)
	}
	if synced || text != "just words" {
		t.Errorf("got synced=%v text=%q", synced, text)
	}
}

func TestFetchInstrumentalIsNoLyrics(t *testing.T) {
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"syncedLyrics":"","plainLyrics":"","instrumental":true}`))
	})
	if _, _, ok, err := s.Fetch(context.Background(), "A", "B"); ok || err != nil {
		t.Errorf("expected no lyrics, got ok=%v err=%v", ok, err)
	}
}

func TestFetchGetMissFallsBackToSearch(t *testing.T) {
	var sawSearch bool
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/get":
			w.WriteHeader(http.StatusNotFound)
		case "/api/search":
			sawSearch = true
			// First result instrumental/empty, second has lyrics: take the second.
			w.Write([]byte(`[{"instrumental":true},{"plainLyrics":"found via search"}]`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	text, _, ok, err := s.Fetch(context.Background(), "A", "B")
	if err != nil || !ok {
		t.Fatalf("Fetch ok=%v err=%v", ok, err)
	}
	if !sawSearch || text != "found via search" {
		t.Errorf("sawSearch=%v text=%q", sawSearch, text)
	}
}

func TestFetchNoneFound(t *testing.T) {
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/get":
			w.WriteHeader(http.StatusNotFound)
		case "/api/search":
			w.Write([]byte(`[]`))
		}
	})
	if _, _, ok, err := s.Fetch(context.Background(), "A", "B"); ok || err != nil {
		t.Errorf("expected no lyrics, got ok=%v err=%v", ok, err)
	}
}

func TestWriteAlongsideWritesSiblingLrc(t *testing.T) {
	dir := t.TempDir()
	music := filepath.Join(dir, "Lorde - Royals.flac")
	if err := os.WriteFile(music, []byte("fake audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"syncedLyrics":"[00:01.00] words","instrumental":false}`))
	})
	if err := s.WriteAlongside(context.Background(), music, "Lorde", "Royals"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "Lorde - Royals.lrc"))
	if err != nil {
		t.Fatalf("expected .lrc written: %v", err)
	}
	if string(got) != "[00:01.00] words" {
		t.Errorf("lrc content = %q", got)
	}
}

func TestWriteAlongsideSkipsExistingLrc(t *testing.T) {
	dir := t.TempDir()
	music := filepath.Join(dir, "song.mp3")
	lrc := filepath.Join(dir, "song.lrc")
	os.WriteFile(music, []byte("audio"), 0o644)
	os.WriteFile(lrc, []byte("existing"), 0o644)

	var called bool
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{"plainLyrics":"new"}`))
	})
	if err := s.WriteAlongside(context.Background(), music, "A", "B"); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("should not call API when .lrc already exists")
	}
	got, _ := os.ReadFile(lrc)
	if string(got) != "existing" {
		t.Errorf("existing .lrc was overwritten: %q", got)
	}
}

func TestWriteAlongsideNoLyricsNoFile(t *testing.T) {
	dir := t.TempDir()
	music := filepath.Join(dir, "song.flac")
	os.WriteFile(music, []byte("audio"), 0o644)
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/get":
			w.WriteHeader(http.StatusNotFound)
		case "/api/search":
			w.Write([]byte(`[]`))
		}
	})
	if err := s.WriteAlongside(context.Background(), music, "A", "B"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "song.lrc")); !os.IsNotExist(err) {
		t.Error("expected no .lrc file when no lyrics found")
	}
}
