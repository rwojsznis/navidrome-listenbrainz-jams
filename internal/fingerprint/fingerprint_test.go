package fingerprint

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestChoose(t *testing.T) {
	mbids := []string{"aaaa", "bbbb", "cccc"}

	if got, ok := choose(mbids, "bbbb"); !ok || got != "bbbb" {
		t.Errorf("prefer present: got %q ok=%v, want bbbb", got, ok)
	}
	if got, ok := choose(mbids, "zzzz"); !ok || got != "aaaa" {
		t.Errorf("prefer absent: got %q ok=%v, want best aaaa", got, ok)
	}
	if got, ok := choose(mbids, ""); !ok || got != "aaaa" {
		t.Errorf("no preference: got %q ok=%v, want aaaa", got, ok)
	}
	if _, ok := choose(nil, "x"); ok {
		t.Error("empty candidates should be not-ok")
	}
}

func TestLookupParsesRecordingIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.PostForm.Get("fingerprint") == "" || r.PostForm.Get("client") == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","results":[
			{"score":0.95,"recordings":[{"id":"rec-1"}]},
			{"score":0.40,"recordings":[{"id":"rec-2"}]}
		]}`))
	}))
	defer srv.Close()

	s := &Service{apiKey: "k", httpClient: srv.Client(), log: slog.Default()}
	// Point the lookup at the test server by overriding the package endpoint.
	old := lookupURL
	lookupURL = srv.URL
	defer func() { lookupURL = old }()

	mbids, err := s.lookup(context.Background(), "AQADtABC", 188)
	if err != nil {
		t.Fatal(err)
	}
	if len(mbids) != 2 || mbids[0] != "rec-1" || mbids[1] != "rec-2" {
		t.Fatalf("got %v, want [rec-1 rec-2] best-score first", mbids)
	}
}

func TestLookupSurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"error","error":{"message":"invalid API key"}}`))
	}))
	defer srv.Close()

	s := &Service{apiKey: "bad", httpClient: srv.Client(), log: slog.Default()}
	old := lookupURL
	lookupURL = srv.URL
	defer func() { lookupURL = old }()

	if _, err := s.lookup(context.Background(), "AQADtABC", 188); err == nil {
		t.Fatal("expected error for status=error response")
	}
}

func TestFingerprintRealFile(t *testing.T) {
	if _, err := exec.LookPath("fpcalc"); err != nil {
		t.Skip("fpcalc not installed")
	}
	matches, _ := filepath.Glob("../../data/music/weekly-jams/*.opus")
	if len(matches) == 0 {
		t.Skip("no audio fixture")
	}
	s := &Service{fpcalcPath: "fpcalc", log: slog.Default()}
	fp, dur, err := s.fingerprint(context.Background(), matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if fp == "" || dur <= 0 {
		t.Fatalf("got fp=%q dur=%v", fp, dur)
	}
}
