package store

import (
	"path/filepath"
	"testing"
)

func TestPlaylistAndTrackLifecycle(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Insert is idempotent on the LB entry id.
	p1, err := db.UpsertPlaylist("emq-weekly", "lb-entry-1", "Weekly Jams", "emq")
	if err != nil {
		t.Fatalf("UpsertPlaylist: %v", err)
	}
	p2, err := db.UpsertPlaylist("emq-weekly", "lb-entry-1", "Weekly Jams (changed)", "emq")
	if err != nil {
		t.Fatalf("UpsertPlaylist again: %v", err)
	}
	if p1.ID != p2.ID {
		t.Fatalf("expected same playlist id, got %d and %d", p1.ID, p2.ID)
	}
	if p2.Status != PlaylistPending {
		t.Errorf("status = %q, want pending", p2.Status)
	}

	// Tracks upsert idempotently on (playlist, mbid).
	if err := db.UpsertTrack(p1.ID, 1, "mbid-1", "Kate Bush", "Army Dreamers"); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}
	if err := db.UpsertTrack(p1.ID, 1, "mbid-1", "Kate Bush", "Army Dreamers"); err != nil {
		t.Fatalf("UpsertTrack dup: %v", err)
	}
	if err := db.UpsertTrack(p1.ID, 2, "mbid-2", "Gorillaz", "Feel Good Inc."); err != nil {
		t.Fatalf("UpsertTrack 2: %v", err)
	}

	tracks, err := db.TracksFor(p1.ID)
	if err != nil {
		t.Fatalf("TracksFor: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("expected 2 tracks, got %d", len(tracks))
	}

	// Advance a track through the state machine.
	tr := &tracks[0]
	tr.Status = TrackInPlaylist
	tr.NavidromeSongID = "song-123"
	tr.Attempts = 1
	if err := db.UpdateTrack(tr); err != nil {
		t.Fatalf("UpdateTrack: %v", err)
	}
	reloaded, _ := db.TracksFor(p1.ID)
	if reloaded[0].Status != TrackInPlaylist || reloaded[0].NavidromeSongID != "song-123" {
		t.Errorf("track not persisted: %+v", reloaded[0])
	}

	// Active vs done filtering.
	active, err := db.ActivePlaylists()
	if err != nil {
		t.Fatalf("ActivePlaylists: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active playlist, got %d", len(active))
	}
	if err := db.SetPlaylistStatus(p1.ID, PlaylistDone); err != nil {
		t.Fatalf("SetPlaylistStatus: %v", err)
	}
	active, _ = db.ActivePlaylists()
	if len(active) != 0 {
		t.Errorf("expected 0 active after done, got %d", len(active))
	}
}
