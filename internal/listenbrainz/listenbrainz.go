// Package listenbrainz fetches and parses ListenBrainz recommendation
// syndication feeds (Atom). Each feed entry is one generated playlist (e.g. a
// weekly-jams) whose HTML content embeds the track list as <li> elements, each
// carrying the track title, the MusicBrainz recording MBID, and the artist name.
package listenbrainz

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"encoding/xml"

	"golang.org/x/net/html"
)

// Feed is a parsed ListenBrainz syndication feed.
type Feed struct {
	Title   string
	Entries []Entry
}

// Entry is one playlist within a feed.
type Entry struct {
	// ID is the stable Atom entry id, used as the idempotency key.
	ID      string
	Title   string
	Updated time.Time
	Tracks  []Track
}

// Track is a single recommended recording.
type Track struct {
	Position      int
	RecordingMBID string
	Title         string
	Artist        string
}

// atomFeed mirrors the subset of the Atom document we consume. The <content>
// element is type="html"; encoding/xml unescapes its character data for us.
type atomFeed struct {
	Title   string      `xml:"title"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID      string `xml:"id"`
	Title   string `xml:"title"`
	Updated string `xml:"updated"`
	Content string `xml:"content"`
}

// Client fetches feeds over HTTP.
type Client struct {
	HTTP      *http.Client
	UserAgent string
}

// NewClient returns a Client with sane defaults.
func NewClient() *Client {
	return &Client{
		HTTP:      &http.Client{Timeout: 30 * time.Second},
		UserAgent: "navidrome-listenbrainz-jams/0.1 (+https://github.com/rwojsznis/navidrome-listenbrainz-jams)",
	}
}

// Fetch retrieves and parses the feed at url.
func (c *Client) Fetch(ctx context.Context, url string) (*Feed, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/atom+xml, application/xml")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch feed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch feed: unexpected status %s", resp.Status)
	}
	return Parse(resp.Body)
}

// Parse reads an Atom feed and extracts entries and their tracks.
func Parse(r io.Reader) (*Feed, error) {
	var doc atomFeed
	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode atom: %w", err)
	}

	feed := &Feed{Title: strings.TrimSpace(doc.Title)}
	for _, e := range doc.Entries {
		entry := Entry{
			ID:    strings.TrimSpace(e.ID),
			Title: strings.TrimSpace(e.Title),
		}
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(e.Updated)); err == nil {
			entry.Updated = t
		}
		tracks, err := parseTracks(e.Content)
		if err != nil {
			return nil, fmt.Errorf("parse tracks for entry %q: %w", entry.Title, err)
		}
		entry.Tracks = tracks
		feed.Entries = append(feed.Entries, entry)
	}
	return feed, nil
}

// parseTracks extracts tracks from an entry's HTML content. Each <li> holds a
// recording link (musicbrainz.org/recording/<mbid>, text=title) followed by an
// artist link (listenbrainz.org/artist/..., text=artist).
func parseTracks(content string) ([]Track, error) {
	root, err := html.Parse(strings.NewReader(content))
	if err != nil {
		return nil, err
	}

	var tracks []Track
	var pos int
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "li" {
			if t, ok := trackFromListItem(n); ok {
				pos++
				t.Position = pos
				tracks = append(tracks, t)
			}
			// A track <li> has no nested track <li>, so don't recurse into it.
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return tracks, nil
}

// trackFromListItem extracts a Track from a single <li> node. It returns ok=false
// for list items that don't look like a track row.
func trackFromListItem(li *html.Node) (Track, bool) {
	var t Track
	var seenRecording bool

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			href := attr(n, "href")
			text := textContent(n)
			switch {
			case strings.Contains(href, "musicbrainz.org/recording/"):
				t.RecordingMBID = lastPathSegment(href)
				t.Title = text
				seenRecording = true
			case strings.Contains(href, "/artist/"):
				t.Artist = text
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(li)

	if !seenRecording || t.Title == "" {
		return Track{}, false
	}
	return t, true
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func textContent(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(sb.String())
}

// lastPathSegment returns the final non-empty path segment of a URL-like string,
// tolerating the double-slash that appears in ListenBrainz artist hrefs.
func lastPathSegment(s string) string {
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}
