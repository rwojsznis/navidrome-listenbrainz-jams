// Package slskd is a client for the slskd REST API: starting searches, reading
// responses, ranking candidate files, enqueuing downloads, and polling transfer
// progress. It authenticates with an API key via the X-API-Key header.
//
// Response shapes are based on slskd v0.25.x. The list endpoints are not fully
// described in slskd's OpenAPI spec, so transfer tracking matches on filename
// rather than relying on an id returned at enqueue time.
package slskd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to a slskd instance.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New returns a Client for baseURL (e.g. http://slskd:5030) authenticating with
// apiKey.
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// File is a file offered in a search response.
type File struct {
	Filename          string `json:"filename"`
	Size              int64  `json:"size"`
	BitRate           int    `json:"bitRate"`
	BitDepth          int    `json:"bitDepth"`
	SampleRate        int    `json:"sampleRate"`
	Extension         string `json:"extension"`
	Length            int    `json:"length"`
	IsVariableBitRate bool   `json:"isVariableBitRate"`
	Code              int    `json:"code"`
}

// SearchResponse is one peer's response to a search.
type SearchResponse struct {
	Username          string `json:"username"`
	HasFreeUploadSlot bool   `json:"hasFreeUploadSlot"`
	QueueLength       int    `json:"queueLength"`
	UploadSpeed       int    `json:"uploadSpeed"`
	FileCount         int    `json:"fileCount"`
	Files             []File `json:"files"`
	LockedFiles       []File `json:"lockedFiles"`
	Token             int    `json:"token"`
}

// Search is the state of a search operation.
type Search struct {
	ID            string `json:"id"`
	SearchText    string `json:"searchText"`
	State         string `json:"state"`
	IsComplete    bool   `json:"isComplete"`
	ResponseCount int    `json:"responseCount"`
	FileCount     int    `json:"fileCount"`
}

// Transfer is the state of a download (or upload) transfer.
type Transfer struct {
	ID               string  `json:"id"`
	Username         string  `json:"username"`
	Filename         string  `json:"filename"`
	Direction        string  `json:"direction"`
	Size             int64   `json:"size"`
	BytesTransferred int64   `json:"bytesTransferred"`
	PercentComplete  float64 `json:"percentComplete"`
	State            string  `json:"state"`
	AverageSpeed     float64 `json:"averageSpeed"`
	Exception        string  `json:"exception"`
}

// IsComplete reports whether the transfer has finished (in any outcome).
func (t *Transfer) IsComplete() bool { return strings.HasPrefix(t.State, "Completed") }

// Succeeded reports whether the transfer completed successfully.
func (t *Transfer) Succeeded() bool { return strings.Contains(t.State, "Succeeded") }

// userDownloads mirrors an entry of GET /transfers/downloads.
type userDownloads struct {
	Username    string `json:"username"`
	Directories []struct {
		Directory string     `json:"directory"`
		Files     []Transfer `json:"files"`
	} `json:"directories"`
}

// QueueFile identifies a file to download.
type QueueFile struct {
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: decode: %w", method, path, err)
		}
	}
	return nil
}

// StartSearch begins a search and returns its id.
func (c *Client) StartSearch(ctx context.Context, text string, searchTimeout time.Duration) (string, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"searchText":    text,
		"searchTimeout": int(searchTimeout.Milliseconds()),
	})
	var s Search
	if err := c.do(ctx, http.MethodPost, "/api/v0/searches", bytes.NewReader(reqBody), &s); err != nil {
		return "", err
	}
	return s.ID, nil
}

// GetSearch returns the current state of a search.
func (c *Client) GetSearch(ctx context.Context, id string) (*Search, error) {
	var s Search
	if err := c.do(ctx, http.MethodGet, "/api/v0/searches/"+url.PathEscape(id), nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// GetResponses returns the responses gathered for a search.
func (c *Client) GetResponses(ctx context.Context, id string) ([]SearchResponse, error) {
	var r []SearchResponse
	if err := c.do(ctx, http.MethodGet, "/api/v0/searches/"+url.PathEscape(id)+"/responses", nil, &r); err != nil {
		return nil, err
	}
	return r, nil
}

// SearchAndWait runs a search and polls until it completes, then returns the
// responses. slskd only serves /responses once a search reaches the Completed
// state, and a busy search (popular track, many peers) stays InProgress well
// past searchTimeout while responses trickle in — so we must wait for
// completion, not just for searchTimeout to elapse. maxWait is a generous hard
// cap so a never-completing search can't hang a tick forever; on hitting it we
// return whatever is available (possibly empty).
func (c *Client) SearchAndWait(ctx context.Context, text string, searchTimeout time.Duration) ([]SearchResponse, error) {
	id, err := c.StartSearch(ctx, text, searchTimeout)
	if err != nil {
		return nil, err
	}
	maxWait := searchTimeout + 75*time.Second
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	deadline := time.NewTimer(maxWait)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return c.GetResponses(ctx, id)
		case <-ticker.C:
			s, err := c.GetSearch(ctx, id)
			if err != nil {
				return nil, err
			}
			if s.IsComplete {
				return c.GetResponses(ctx, id)
			}
		}
	}
}

// Enqueue requests downloads of files from a user.
func (c *Client) Enqueue(ctx context.Context, username string, files []QueueFile) error {
	body, _ := json.Marshal(files)
	return c.do(ctx, http.MethodPost, "/api/v0/transfers/downloads/"+url.PathEscape(username), bytes.NewReader(body), nil)
}

// ListDownloads returns all download transfers grouped by user.
func (c *Client) ListDownloads(ctx context.Context) ([]userDownloads, error) {
	var u []userDownloads
	if err := c.do(ctx, http.MethodGet, "/api/v0/transfers/downloads", nil, &u); err != nil {
		return nil, err
	}
	return u, nil
}

// FindDownload locates a download transfer by username and remote filename.
func (c *Client) FindDownload(ctx context.Context, username, filename string) (*Transfer, bool, error) {
	users, err := c.ListDownloads(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, u := range users {
		if u.Username != username {
			continue
		}
		for _, d := range u.Directories {
			for i := range d.Files {
				if d.Files[i].Filename == filename {
					t := d.Files[i]
					return &t, true, nil
				}
			}
		}
	}
	return nil, false, nil
}
