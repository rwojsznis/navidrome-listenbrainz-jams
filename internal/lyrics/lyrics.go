// Package lyrics fetches song lyrics from lrclib.net and writes them as an LRC
// file alongside an imported audio file, so Navidrome (and most players) can
// display them. Synced (timestamped) lyrics are preferred; plain lyrics are a
// fallback. The feed gives us only artist + title, which lrclib's /api/get
// accepts directly; on a miss we fall back to the fuzzy /api/search endpoint.
//
// The whole step is best-effort: a network error, a not-found, or an
// instrumental track is not fatal — the import proceeds without lyrics.
package lyrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// userAgent identifies this client to lrclib, as their docs request.
const userAgent = "navidrome-listenbrainz-jams (https://github.com/rwojsznis/navidrome-listenbrainz-jams)"

// Status values recorded for a track after a lyrics attempt. They are plain
// strings so callers can persist them directly without importing this package's
// type.
const (
	StatusSynced = "synced" // timestamped .lrc present
	StatusPlain  = "plain"  // plain-text .lrc present
	StatusNone   = "none"   // looked up, no lyrics found
)

// syncedLine matches an LRC timestamp tag like "[00:12.34]", used to classify an
// existing .lrc file as synced vs plain.
var syncedLine = regexp.MustCompile(`\[\d{1,2}:\d{2}(\.\d{1,3})?\]`)

// Service fetches lyrics from an lrclib-compatible API.
type Service struct {
	baseURL string
	http    *http.Client
	log     *slog.Logger
}

// New returns a Service. baseURL is the lrclib API base (e.g.
// "https://lrclib.net"); a trailing slash is trimmed.
func New(baseURL string, log *slog.Logger) *Service {
	return &Service{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 20 * time.Second},
		log:     log,
	}
}

// lrclibResult is the subset of an lrclib track object we use.
type lrclibResult struct {
	Instrumental bool   `json:"instrumental"`
	PlainLyrics  string `json:"plainLyrics"`
	SyncedLyrics string `json:"syncedLyrics"`
}

// best returns the preferred lyrics text (synced over plain) and whether the
// result carries any usable lyrics.
func (r lrclibResult) best() (text string, ok bool) {
	if r.Instrumental {
		return "", false
	}
	if strings.TrimSpace(r.SyncedLyrics) != "" {
		return r.SyncedLyrics, true
	}
	if strings.TrimSpace(r.PlainLyrics) != "" {
		return r.PlainLyrics, true
	}
	return "", false
}

// Fetch looks up lyrics for artist + title: an exact /api/get first, then a
// fuzzy /api/search fallback. ok is false when no lyrics exist (a normal
// outcome, not an error). synced reports whether the returned text is
// timestamped.
func (s *Service) Fetch(ctx context.Context, artist, title string) (text string, synced bool, ok bool, err error) {
	res, found, err := s.get(ctx, artist, title)
	if err != nil {
		return "", false, false, err
	}
	if !found {
		res, found, err = s.search(ctx, artist, title)
		if err != nil {
			return "", false, false, err
		}
		if !found {
			return "", false, false, nil
		}
	}
	text, ok = res.best()
	if !ok {
		return "", false, false, nil
	}
	return text, strings.TrimSpace(res.SyncedLyrics) != "", true, nil
}

// get queries /api/get for an exact artist+title match. found is false on 404.
func (s *Service) get(ctx context.Context, artist, title string) (lrclibResult, bool, error) {
	q := url.Values{}
	q.Set("artist_name", artist)
	q.Set("track_name", title)
	body, status, err := s.do(ctx, "/api/get?"+q.Encode())
	if err != nil {
		return lrclibResult{}, false, err
	}
	if status == http.StatusNotFound {
		return lrclibResult{}, false, nil
	}
	if status != http.StatusOK {
		return lrclibResult{}, false, fmt.Errorf("lrclib get: status %d", status)
	}
	var res lrclibResult
	if err := json.Unmarshal(body, &res); err != nil {
		return lrclibResult{}, false, fmt.Errorf("decode lrclib get: %w", err)
	}
	return res, true, nil
}

// search queries the fuzzy /api/search endpoint and returns the first result
// that actually carries lyrics. found is false when the list is empty or none
// have lyrics.
func (s *Service) search(ctx context.Context, artist, title string) (lrclibResult, bool, error) {
	q := url.Values{}
	q.Set("artist_name", artist)
	q.Set("track_name", title)
	body, status, err := s.do(ctx, "/api/search?"+q.Encode())
	if err != nil {
		return lrclibResult{}, false, err
	}
	if status != http.StatusOK {
		return lrclibResult{}, false, fmt.Errorf("lrclib search: status %d", status)
	}
	var results []lrclibResult
	if err := json.Unmarshal(body, &results); err != nil {
		return lrclibResult{}, false, fmt.Errorf("decode lrclib search: %w", err)
	}
	for _, r := range results {
		if _, ok := r.best(); ok {
			return r, true, nil
		}
	}
	return lrclibResult{}, false, nil
}

// do performs a GET against baseURL+path and returns the body and status.
func (s *Service) do(ctx context.Context, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// WriteAlongside fetches lyrics for the track and writes them as a sibling
// ".lrc" file next to musicPath (e.g. "Artist - Title.flac" ->
// "Artist - Title.lrc"). It returns the resulting lyrics status (StatusSynced /
// StatusPlain / StatusNone) so the caller can record it. A ".lrc" that already
// exists is left untouched and classified from its contents (no network call).
// Implements downloader.LyricsWriter.
func (s *Service) WriteAlongside(ctx context.Context, musicPath, artist, title string) (string, error) {
	lrcPath := strings.TrimSuffix(musicPath, filepath.Ext(musicPath)) + ".lrc"
	if existing, err := os.ReadFile(lrcPath); err == nil {
		// Already present; don't overwrite or refetch, just classify it.
		return classify(string(existing)), nil
	}

	text, synced, ok, err := s.Fetch(ctx, artist, title)
	if err != nil {
		return "", err
	}
	if !ok {
		s.log.Debug("no lyrics found", "artist", artist, "title", title)
		return StatusNone, nil
	}

	if err := os.WriteFile(lrcPath, []byte(text), 0o644); err != nil {
		return "", fmt.Errorf("write lrc: %w", err)
	}
	s.log.Info("lyrics written", "path", lrcPath, "synced", synced)
	if synced {
		return StatusSynced, nil
	}
	return StatusPlain, nil
}

// classify reports whether .lrc content is synced (has timestamp tags) or plain.
func classify(content string) string {
	if syncedLine.MatchString(content) {
		return StatusSynced
	}
	return StatusPlain
}
