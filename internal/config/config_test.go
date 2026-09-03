package config

import (
	"testing"
	"time"
)

func TestRenderPlaylistName(t *testing.T) {
	f := Feed{Name: "jams", PlaylistName: "Weekly — {{.Date}} ({{.FeedName}})"}
	got, err := f.RenderPlaylistName("Original title", time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC))
	if err != nil || got != "Weekly — 2026-06-22 (jams)" {
		t.Fatalf("render = %q, err=%v", got, err)
	}
}

func TestRenderPlaylistNameDefault(t *testing.T) {
	f := Feed{}
	got, err := f.RenderPlaylistName("Original", time.Time{})
	if err != nil || got != "Original" {
		t.Fatalf("render = %q, err=%v", got, err)
	}
}
