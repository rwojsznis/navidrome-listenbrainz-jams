// Package navidrome is a small Subsonic API client for the operations this
// service needs: searching for tracks, listing/creating/updating playlists, and
// triggering library scans. It authenticates per-user with Subsonic token auth
// (salt + md5), so playlists are created owned by the correct user.
package navidrome

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	apiVersion = "1.16.1"
	clientName = "navidrome-lb-jams"
)

// Client talks to a Navidrome/Subsonic server as a single user.
type Client struct {
	baseURL string
	user    string
	pass    string
	http    *http.Client
}

// New returns a Client authenticating as user with pass against baseURL
// (e.g. http://navidrome:4533).
func New(baseURL, user, pass string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		user:    user,
		pass:    pass,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Song is a track returned by search/playlist endpoints.
type Song struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Artist        string `json:"artist"`
	Album         string `json:"album"`
	Duration      int    `json:"duration"`
	MusicBrainzID string `json:"musicBrainzId"` // recording MBID (OpenSubsonic), empty if untagged
}

// Playlist is a Subsonic playlist, optionally with its entries populated.
type Playlist struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SongCount int    `json:"songCount"`
	Owner     string `json:"owner"`
	Entry     []Song `json:"entry"`
}

// apiResponse is the envelope every Subsonic endpoint returns.
type apiResponse struct {
	Subsonic struct {
		Status  string `json:"status"`
		Version string `json:"version"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		SearchResult3 struct {
			Song []Song `json:"song"`
		} `json:"searchResult3"`
		Playlists struct {
			Playlist []Playlist `json:"playlist"`
		} `json:"playlists"`
		Playlist   Playlist `json:"playlist"`
		ScanStatus struct {
			Scanning bool `json:"scanning"`
			Count    int  `json:"count"`
		} `json:"scanStatus"`
	} `json:"subsonic-response"`
}

// authParams builds the per-request Subsonic auth/query parameters using token
// auth (a fresh salt per request).
func (c *Client) authParams() (url.Values, error) {
	saltBytes := make([]byte, 8)
	if _, err := rand.Read(saltBytes); err != nil {
		return nil, fmt.Errorf("generate salt: %w", err)
	}
	salt := hex.EncodeToString(saltBytes)
	sum := md5.Sum([]byte(c.pass + salt))
	token := hex.EncodeToString(sum[:])

	v := url.Values{}
	v.Set("u", c.user)
	v.Set("t", token)
	v.Set("s", salt)
	v.Set("v", apiVersion)
	v.Set("c", clientName)
	v.Set("f", "json")
	return v, nil
}

// get performs a GET against a Subsonic endpoint with the given params merged
// onto the auth params, and decodes the response envelope.
func (c *Client) get(ctx context.Context, endpoint string, params url.Values) (*apiResponse, error) {
	auth, err := c.authParams()
	if err != nil {
		return nil, err
	}
	for k, vals := range params {
		for _, val := range vals {
			auth.Add(k, val)
		}
	}

	u := c.baseURL + "/rest/" + endpoint + "?" + auth.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: unexpected status %s", endpoint, resp.Status)
	}

	var out apiResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("%s: decode response: %w", endpoint, err)
	}
	if out.Subsonic.Status != "ok" {
		if out.Subsonic.Error != nil {
			return nil, fmt.Errorf("%s: subsonic error %d: %s", endpoint, out.Subsonic.Error.Code, out.Subsonic.Error.Message)
		}
		return nil, fmt.Errorf("%s: subsonic status %q", endpoint, out.Subsonic.Status)
	}
	return &out, nil
}

// Ping verifies connectivity and credentials.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.get(ctx, "ping", nil)
	return err
}

// Search3 searches songs matching query. songCount caps results; artists and
// albums are excluded since we only resolve tracks.
func (c *Client) Search3(ctx context.Context, query string, songCount int) ([]Song, error) {
	p := url.Values{}
	p.Set("query", query)
	p.Set("songCount", fmt.Sprintf("%d", songCount))
	p.Set("artistCount", "0")
	p.Set("albumCount", "0")
	resp, err := c.get(ctx, "search3", p)
	if err != nil {
		return nil, err
	}
	return resp.Subsonic.SearchResult3.Song, nil
}

// GetPlaylists returns the authenticated user's playlists (without entries).
func (c *Client) GetPlaylists(ctx context.Context) ([]Playlist, error) {
	resp, err := c.get(ctx, "getPlaylists", nil)
	if err != nil {
		return nil, err
	}
	return resp.Subsonic.Playlists.Playlist, nil
}

// FindPlaylistByName returns the user's playlist with the exact given name, or
// nil if none exists.
func (c *Client) FindPlaylistByName(ctx context.Context, name string) (*Playlist, error) {
	lists, err := c.GetPlaylists(ctx)
	if err != nil {
		return nil, err
	}
	for i := range lists {
		if lists[i].Name == name {
			return &lists[i], nil
		}
	}
	return nil, nil
}

// GetPlaylist returns a playlist with its entries populated.
func (c *Client) GetPlaylist(ctx context.Context, id string) (*Playlist, error) {
	p := url.Values{}
	p.Set("id", id)
	resp, err := c.get(ctx, "getPlaylist", p)
	if err != nil {
		return nil, err
	}
	pl := resp.Subsonic.Playlist
	return &pl, nil
}

// CreatePlaylist creates a new playlist with the given name and song ids, and
// returns it. Note: Subsonic does not dedupe by name, so callers should check
// FindPlaylistByName first for idempotency.
func (c *Client) CreatePlaylist(ctx context.Context, name string, songIDs []string) (*Playlist, error) {
	p := url.Values{}
	p.Set("name", name)
	for _, id := range songIDs {
		p.Add("songId", id)
	}
	resp, err := c.get(ctx, "createPlaylist", p)
	if err != nil {
		return nil, err
	}
	// Navidrome returns the created playlist; if empty, fall back to lookup.
	if resp.Subsonic.Playlist.ID != "" {
		pl := resp.Subsonic.Playlist
		return &pl, nil
	}
	return c.FindPlaylistByName(ctx, name)
}

// AddToPlaylist appends songs to an existing playlist.
func (c *Client) AddToPlaylist(ctx context.Context, playlistID string, songIDs []string) error {
	if len(songIDs) == 0 {
		return nil
	}
	p := url.Values{}
	p.Set("playlistId", playlistID)
	for _, id := range songIDs {
		p.Add("songIdToAdd", id)
	}
	_, err := c.get(ctx, "updatePlaylist", p)
	return err
}

// StartScan triggers a library scan.
func (c *Client) StartScan(ctx context.Context) error {
	_, err := c.get(ctx, "startScan", nil)
	return err
}

// ScanStatus reports whether a scan is in progress and how many items it has seen.
func (c *Client) ScanStatus(ctx context.Context) (scanning bool, count int, err error) {
	resp, err := c.get(ctx, "getScanStatus", nil)
	if err != nil {
		return false, 0, err
	}
	return resp.Subsonic.ScanStatus.Scanning, resp.Subsonic.ScanStatus.Count, nil
}
