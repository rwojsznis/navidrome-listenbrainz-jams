package pipeline

import (
	"testing"

	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/navidrome"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/store"
)

func TestSelectMatch(t *testing.T) {
	const mbid = "20655d93-c325-4825-9018-50fa1b54bf77"

	t.Run("MBID match beats fuzzy and ignores decorated tags", func(t *testing.T) {
		songs := []navidrome.Song{
			{ID: "wrong", Artist: "Someone Else", Title: "Different Song"},
			{ID: "right", Artist: "The Weeknd", Title: "Sacrifice (PMEDIA)", MusicBrainzID: mbid},
		}
		tr := &store.Track{Artist: "The Weeknd", Title: "Sacrifice", RecordingMBID: mbid}
		got, ok := selectMatch(songs, tr, 0.85)
		if !ok || got.ID != "right" {
			t.Fatalf("got %+v ok=%v, want id=right via MBID", got, ok)
		}
	})

	t.Run("MBID wins even when another song is a better text match", func(t *testing.T) {
		songs := []navidrome.Song{
			{ID: "fuzzy", Artist: "The Weeknd", Title: "Sacrifice"},                              // exact text, no MBID
			{ID: "mbid", Artist: "The Weeknd", Title: "Sacrifice (PMEDIA)", MusicBrainzID: mbid}, // MBID match
		}
		tr := &store.Track{Artist: "The Weeknd", Title: "Sacrifice", RecordingMBID: mbid}
		got, ok := selectMatch(songs, tr, 0.85)
		if !ok || got.ID != "mbid" {
			t.Fatalf("got %+v ok=%v, want id=mbid (authoritative over fuzzy)", got, ok)
		}
	})

	t.Run("falls back to fuzzy when no MBID matches", func(t *testing.T) {
		songs := []navidrome.Song{
			{ID: "other", Artist: "X", Title: "Y", MusicBrainzID: "different-mbid"},
			{ID: "fuzzy", Artist: "The Weeknd", Title: "Sacrifice"},
		}
		tr := &store.Track{Artist: "The Weeknd", Title: "Sacrifice", RecordingMBID: mbid}
		got, ok := selectMatch(songs, tr, 0.85)
		if !ok || got.ID != "fuzzy" {
			t.Fatalf("got %+v ok=%v, want id=fuzzy fallback", got, ok)
		}
	})

	t.Run("empty feed MBID skips MBID path", func(t *testing.T) {
		songs := []navidrome.Song{{ID: "fuzzy", Artist: "The Weeknd", Title: "Sacrifice"}}
		tr := &store.Track{Artist: "The Weeknd", Title: "Sacrifice"} // no RecordingMBID
		got, ok := selectMatch(songs, tr, 0.85)
		if !ok || got.ID != "fuzzy" {
			t.Fatalf("got %+v ok=%v, want fuzzy", got, ok)
		}
	})

	t.Run("no match", func(t *testing.T) {
		songs := []navidrome.Song{{ID: "x", Artist: "Foo", Title: "Bar"}}
		tr := &store.Track{Artist: "The Weeknd", Title: "Sacrifice", RecordingMBID: mbid}
		if _, ok := selectMatch(songs, tr, 0.85); ok {
			t.Fatal("expected no match")
		}
	})
}

func TestResolveQueries(t *testing.T) {
	// Featured-artist track (the Lady Gaga / R. Kelly case): the full query is
	// over-specified for Navidrome's token-AND search, so we must also emit the
	// primary-artist and title-only fallbacks.
	tr := &store.Track{Artist: "Lady Gaga featuring R. Kelly", Title: "Do What U Want"}
	got := resolveQueries(tr)
	want := []string{
		"Lady Gaga featuring R. Kelly Do What U Want",
		"Lady Gaga Do What U Want",
		"Do What U Want",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("query[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}

	// No featured artist / no decorations: the full query already equals the
	// title-only fallback's artist prefix, so we should not emit duplicates.
	tr2 := &store.Track{Artist: "Daft Punk", Title: "One More Time"}
	if got := resolveQueries(tr2); len(got) != 2 {
		t.Fatalf("expected 2 deduped queries, got %v", got)
	}
}
