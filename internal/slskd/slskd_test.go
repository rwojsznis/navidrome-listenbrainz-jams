package slskd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestStartSearchAndResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "k" {
			t.Errorf("missing api key header")
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v0/searches":
			_, _ = w.Write([]byte(`{"id":"sid","state":"InProgress","isComplete":false}`))
		case r.URL.Path == "/api/v0/searches/sid/responses":
			_, _ = w.Write([]byte(`[{"username":"bob","hasFreeUploadSlot":true,"files":[{"filename":"m\\a.flac","extension":".flac","size":99}]}]`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "k")

	id, err := c.StartSearch(context.Background(), "query", 5*time.Second)
	if err != nil || id != "sid" {
		t.Fatalf("StartSearch id=%q err=%v", id, err)
	}
	resp, err := c.GetResponses(context.Background(), id)
	if err != nil {
		t.Fatalf("GetResponses: %v", err)
	}
	if len(resp) != 1 || resp[0].Username != "bob" || resp[0].Files[0].Size != 99 {
		t.Fatalf("unexpected responses: %+v", resp)
	}
}

func TestEnqueueSendsArrayBody(t *testing.T) {
	var got []QueueFile
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/api/v0/transfers/downloads/") {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	c := New(srv.URL, "k")

	err := c.Enqueue(context.Background(), "bob", []QueueFile{{Filename: "m\\a.flac", Size: 99}})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if len(got) != 1 || got[0].Filename != "m\\a.flac" || got[0].Size != 99 {
		t.Fatalf("unexpected enqueue body: %+v", got)
	}
}

func TestFindDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"username":"bob","directories":[
				{"directory":"m","files":[
					{"id":"t1","username":"bob","filename":"m\\a.flac","state":"Completed, Succeeded","percentComplete":100}
				]}
			]}
		]`))
	}))
	defer srv.Close()
	c := New(srv.URL, "k")

	tr, ok, err := c.FindDownload(context.Background(), "bob", "m\\a.flac")
	if err != nil || !ok {
		t.Fatalf("FindDownload ok=%v err=%v", ok, err)
	}
	if !tr.IsComplete() || !tr.Succeeded() {
		t.Fatalf("expected completed+succeeded, got state %q", tr.State)
	}

	if _, ok, _ := c.FindDownload(context.Background(), "bob", "nope"); ok {
		t.Fatal("expected not found for missing filename")
	}
}

func TestRemoveSearch(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := New(srv.URL, "k")

	if err := c.RemoveSearch(context.Background(), "sid"); err != nil {
		t.Fatalf("RemoveSearch: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v0/searches/sid" {
		t.Errorf("got %s %s, want DELETE /api/v0/searches/sid", gotMethod, gotPath)
	}
}

func TestRemoveDownload(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := New(srv.URL, "k")

	if err := c.RemoveDownload(context.Background(), "bob", "t1"); err != nil {
		t.Fatalf("RemoveDownload: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %s, want DELETE", gotMethod)
	}
	if gotPath != "/api/v0/transfers/downloads/bob/t1" {
		t.Errorf("path = %s", gotPath)
	}
}

func TestTransferStateHelpers(t *testing.T) {
	cases := map[string][2]bool{ // state -> {IsComplete, Succeeded}
		"InProgress":            {false, false},
		"Queued, Remotely":      {false, false},
		"Completed, Succeeded":  {true, true},
		"Completed, Errored":    {true, false},
		"Completed, TimedOut":   {true, false},
		"Completed, Cancelled":  {true, false},
	}
	for state, want := range cases {
		tr := Transfer{State: state}
		if tr.IsComplete() != want[0] || tr.Succeeded() != want[1] {
			t.Errorf("state %q: IsComplete=%v Succeeded=%v, want %v", state, tr.IsComplete(), tr.Succeeded(), want)
		}
	}
}

// TestLiveSearch hits a real slskd. Requires network connectivity that allows
// inbound peer connections (NOT mobile/CGNAT). Enable with:
//   SLSKD_LIVE=1 SLSKD_URL=http://localhost:5030 SLSKD_API_KEY=... \
//   go test ./internal/slskd/ -run TestLiveSearch -v
func TestLiveSearch(t *testing.T) {
	if os.Getenv("SLSKD_LIVE") == "" {
		t.Skip("set SLSKD_LIVE=1 to run live search test")
	}
	c := New(envOr("SLSKD_URL", "http://localhost:5030"), os.Getenv("SLSKD_API_KEY"))
	responses, err := c.SearchAndWait(context.Background(), "gorillaz feel good inc", 15*time.Second)
	if err != nil {
		t.Fatalf("SearchAndWait: %v", err)
	}
	t.Logf("got %d responses", len(responses))
	user, file, ok := SelectBest(responses, Criteria{FormatPreference: []string{"flac", "mp3"}, MinBitrate: 256}, Target{Artist: "Gorillaz", Title: "Feel Good Inc"})
	t.Logf("selected ok=%v user=%s file=%s", ok, user, file.Filename)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
