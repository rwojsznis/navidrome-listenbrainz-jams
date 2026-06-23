package listenbrainz

import (
	"os"
	"testing"
)

func TestParseWeeklyJams(t *testing.T) {
	f, err := os.Open("testdata/weekly-jams.xml")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	feed, err := Parse(f)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(feed.Entries) == 0 {
		t.Fatal("expected at least one entry")
	}

	e := feed.Entries[0]
	if e.ID == "" {
		t.Error("entry ID is empty")
	}
	if e.Title == "" {
		t.Error("entry Title is empty")
	}
	if len(e.Tracks) == 0 {
		t.Fatal("expected tracks in first entry")
	}

	// Verify the first known track parsed correctly (from the live feed).
	first := e.Tracks[0]
	if first.Position != 1 {
		t.Errorf("first track position = %d, want 1", first.Position)
	}
	if first.RecordingMBID == "" {
		t.Error("first track has empty RecordingMBID")
	}
	if first.Title == "" {
		t.Error("first track has empty Title")
	}
	if first.Artist == "" {
		t.Error("first track has empty Artist")
	}

	// Every track must have the fields we rely on for matching/state.
	for i, tr := range e.Tracks {
		if tr.RecordingMBID == "" || tr.Title == "" || tr.Artist == "" {
			t.Errorf("track[%d] incomplete: %+v", i, tr)
		}
		if len(tr.RecordingMBID) != 36 {
			t.Errorf("track[%d] MBID looks malformed: %q", i, tr.RecordingMBID)
		}
	}

	t.Logf("parsed %d tracks; first = %q by %q (%s)",
		len(e.Tracks), first.Title, first.Artist, first.RecordingMBID)
}
