package downloadchain

import (
	"context"
	"testing"

	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/config"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/store"
)

// fakeSource records that it ran and applies a canned mutation to the track.
type fakeSource struct {
	ran     bool
	mutate  func(*store.Track)
	changed bool
	err     error
}

func (f *fakeSource) Advance(_ context.Context, t *store.Track) (bool, error) {
	f.ran = true
	if f.mutate != nil {
		f.mutate(t)
	}
	return f.changed, f.err
}

func newChain(primary, fallback *fakeSource) *Chain {
	return New(primary, fallback, &config.Config{Download: config.Download{MaxRetries: 3}})
}

func TestFallbackFiresWhenPrimaryExhausted(t *testing.T) {
	primary := &fakeSource{changed: true, mutate: func(t *store.Track) {
		t.Status = store.TrackMissing
		t.Attempts = 3
	}}
	fallback := &fakeSource{changed: true}
	chain := newChain(primary, fallback)

	tr := &store.Track{Status: store.TrackMissing, Attempts: 3}
	if _, err := chain.Advance(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	if !fallback.ran {
		t.Error("fallback should fire when slskd is exhausted and yt-dlp hasn't tried")
	}
}

func TestFallbackSkippedWhenNotExhausted(t *testing.T) {
	// slskd enqueued a download: not missing, so no handoff.
	primary := &fakeSource{changed: true, mutate: func(t *store.Track) {
		t.Status = store.TrackDownloading
	}}
	fallback := &fakeSource{changed: true}
	chain := newChain(primary, fallback)

	tr := &store.Track{Status: store.TrackPending}
	if _, err := chain.Advance(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	if fallback.ran {
		t.Error("fallback must not fire while slskd is still making progress")
	}
}

func TestFallbackSkippedBelowMaxRetries(t *testing.T) {
	// Missing but retries not yet exhausted -> slskd should get more tries first.
	primary := &fakeSource{changed: true, mutate: func(t *store.Track) {
		t.Status = store.TrackMissing
		t.Attempts = 1
	}}
	fallback := &fakeSource{changed: true}
	chain := newChain(primary, fallback)

	tr := &store.Track{Status: store.TrackMissing, Attempts: 1}
	if _, err := chain.Advance(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	if fallback.ran {
		t.Error("fallback must not fire before slskd exhausts max_retries")
	}
}

func TestFallbackIsOneShot(t *testing.T) {
	// yt-dlp already tried (Source set) and left it missing: do not retry yt-dlp.
	primary := &fakeSource{changed: false, mutate: func(t *store.Track) {
		t.Status = store.TrackMissing
		t.Attempts = 3
	}}
	fallback := &fakeSource{changed: true}
	chain := newChain(primary, fallback)

	tr := &store.Track{Status: store.TrackMissing, Attempts: 3, Source: "ytdlp"}
	if _, err := chain.Advance(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	if fallback.ran {
		t.Error("fallback is one-shot: must not re-run once Source == ytdlp")
	}
}

func TestPrimaryErrorShortCircuits(t *testing.T) {
	primary := &fakeSource{err: context.Canceled, mutate: func(t *store.Track) {
		t.Status = store.TrackMissing
		t.Attempts = 3
	}}
	fallback := &fakeSource{changed: true}
	chain := newChain(primary, fallback)

	tr := &store.Track{Status: store.TrackMissing, Attempts: 3}
	if _, err := chain.Advance(context.Background(), tr); err == nil {
		t.Fatal("expected primary error to propagate")
	}
	if fallback.ran {
		t.Error("fallback must not run when primary errored")
	}
}
