package navidrome

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// newMock returns a Client pointed at an httptest server whose handler maps the
// Subsonic endpoint (last path segment) to a canned JSON envelope.
func newMock(t *testing.T, responses map[string]string) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every request must carry token auth params.
		q := r.URL.Query()
		for _, k := range []string{"u", "t", "s", "v", "c"} {
			if q.Get(k) == "" {
				t.Errorf("%s: missing auth param %q", r.URL.Path, k)
			}
		}
		endpoint := r.URL.Path[len("/rest/"):]
		body, ok := responses[endpoint]
		if !ok {
			t.Errorf("unexpected endpoint %q", endpoint)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "emq", "pass"), srv
}

func TestSearch3(t *testing.T) {
	c, _ := newMock(t, map[string]string{
		"search3": `{"subsonic-response":{"status":"ok","version":"1.16.1","searchResult3":{"song":[
			{"id":"s1","title":"Army Dreamers","artist":"Kate Bush","album":"Never for Ever"},
			{"id":"s2","title":"Wuthering Heights","artist":"Kate Bush","album":"The Kick Inside"}
		]}}}`,
	})
	songs, err := c.Search3(context.Background(), "Army Dreamers", 20)
	if err != nil {
		t.Fatalf("Search3: %v", err)
	}
	if len(songs) != 2 || songs[0].ID != "s1" || songs[0].Artist != "Kate Bush" {
		t.Fatalf("unexpected songs: %+v", songs)
	}
}

func TestSubsonicError(t *testing.T) {
	c, _ := newMock(t, map[string]string{
		"ping": `{"subsonic-response":{"status":"failed","version":"1.16.1","error":{"code":40,"message":"Wrong username or password"}}}`,
	})
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("expected error for failed status")
	}
}

func TestFindPlaylistByName(t *testing.T) {
	c, _ := newMock(t, map[string]string{
		"getPlaylists": `{"subsonic-response":{"status":"ok","playlists":{"playlist":[
			{"id":"p1","name":"Weekly Jams for emq, week of 2026-06-22 Mon","songCount":3,"owner":"emq"},
			{"id":"p2","name":"Other","songCount":1}
		]}}}`,
	})
	pl, err := c.FindPlaylistByName(context.Background(), "Weekly Jams for emq, week of 2026-06-22 Mon")
	if err != nil {
		t.Fatalf("FindPlaylistByName: %v", err)
	}
	if pl == nil || pl.ID != "p1" {
		t.Fatalf("expected p1, got %+v", pl)
	}
	missing, err := c.FindPlaylistByName(context.Background(), "Nope")
	if err != nil {
		t.Fatalf("FindPlaylistByName missing: %v", err)
	}
	if missing != nil {
		t.Fatalf("expected nil for missing playlist, got %+v", missing)
	}
}

func TestCreatePlaylistSendsSongIDs(t *testing.T) {
	var gotSongIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSongIDs = r.URL.Query()["songId"]
		_, _ = w.Write([]byte(`{"subsonic-response":{"status":"ok","playlist":{"id":"new1","name":"X","songCount":2}}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "emq", "pass")

	pl, err := c.CreatePlaylist(context.Background(), "X", []string{"s1", "s2"})
	if err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}
	if pl.ID != "new1" {
		t.Errorf("playlist id = %q, want new1", pl.ID)
	}
	if len(gotSongIDs) != 2 || gotSongIDs[0] != "s1" || gotSongIDs[1] != "s2" {
		t.Errorf("songId params = %v, want [s1 s2]", gotSongIDs)
	}
}

// TestLive hits a real Navidrome instance. Enable with:
//   NAVIDROME_LIVE=1 NAVIDROME_URL=http://localhost:4533 \
//   NAVIDROME_USER=emq NAVIDROME_PASS=emqpassword go test ./internal/navidrome/ -run TestLive -v
func TestLive(t *testing.T) {
	if os.Getenv("NAVIDROME_LIVE") == "" {
		t.Skip("set NAVIDROME_LIVE=1 to run live test")
	}
	base := envOr("NAVIDROME_URL", "http://localhost:4533")
	c := New(base, envOr("NAVIDROME_USER", "emq"), envOr("NAVIDROME_PASS", "emqpassword"))
	ctx := context.Background()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	songs, err := c.Search3(ctx, "Army Dreamers", 10)
	if err != nil {
		t.Fatalf("Search3: %v", err)
	}
	t.Logf("search3 returned %d songs", len(songs))
	for _, s := range songs {
		t.Logf("  %s — %s [%s] id=%s", s.Artist, s.Title, s.Album, s.ID)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
