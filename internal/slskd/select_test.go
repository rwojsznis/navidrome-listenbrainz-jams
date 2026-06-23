package slskd

import "testing"

// gorillaz is a convenience target for tests whose filenames already match.
var anyTarget = Target{Artist: "Song", Title: "Song"}

func TestSelectBestPrefersFormatThenQuality(t *testing.T) {
	responses := []SearchResponse{
		{Username: "alice", HasFreeUploadSlot: true, UploadSpeed: 100, Files: []File{
			{Filename: "Song\\Song.mp3", Extension: ".mp3", BitRate: 320, Size: 1},
		}},
		{Username: "bob", HasFreeUploadSlot: false, UploadSpeed: 50, Files: []File{
			{Filename: "Song\\Song.flac", Extension: ".flac", Size: 2},
		}},
	}
	user, file, ok := SelectBest(responses, Criteria{FormatPreference: []string{"flac", "mp3"}, MinBitrate: 256}, anyTarget)
	if !ok {
		t.Fatal("expected a selection")
	}
	if user != "bob" || fileExt(file) != "flac" {
		t.Fatalf("expected bob's flac (format preference wins), got user=%s ext=%s", user, fileExt(file))
	}
}

func TestSelectBestRejectsLowBitrateMP3(t *testing.T) {
	responses := []SearchResponse{
		{Username: "alice", Files: []File{{Filename: "a.mp3", Extension: ".mp3", BitRate: 128, Size: 1}}},
	}
	if _, _, ok := SelectBest(responses, Criteria{FormatPreference: []string{"mp3"}, MinBitrate: 256}, anyTarget); ok {
		t.Fatal("expected low-bitrate mp3 to be rejected")
	}
}

func TestSelectBestRejectsUnwantedFormats(t *testing.T) {
	responses := []SearchResponse{
		{Username: "alice", Files: []File{{Filename: "a.ogg", Extension: ".ogg", Size: 1}}},
	}
	if _, _, ok := SelectBest(responses, Criteria{FormatPreference: []string{"flac", "mp3"}}, anyTarget); ok {
		t.Fatal("expected ogg to be rejected when not in preferences")
	}
}

func TestSelectBestFreeSlotTieBreak(t *testing.T) {
	responses := []SearchResponse{
		{Username: "slow", HasFreeUploadSlot: false, UploadSpeed: 999, Files: []File{{Filename: "Song\\Song.flac", Extension: ".flac"}}},
		{Username: "free", HasFreeUploadSlot: true, UploadSpeed: 10, Files: []File{{Filename: "Song\\Song.flac", Extension: ".flac"}}},
	}
	user, _, ok := SelectBest(responses, Criteria{FormatPreference: []string{"flac"}}, anyTarget)
	if !ok || user != "free" {
		t.Fatalf("expected free-slot peer to win tie, got %q", user)
	}
}

// TestPrefersExactTitleOverRemix is the real-world case: a search for the
// original recording also returns remix/edit versions with extra words in the
// filename. The closest (exact) title must win, even at the same format.
func TestPrefersExactTitleOverRemix(t *testing.T) {
	target := Target{Artist: "Nina Simone", Title: "Sinnerman"}
	responses := []SearchResponse{
		{Username: "remixer", HasFreeUploadSlot: true, UploadSpeed: 999, Files: []File{
			{Filename: `Music\Nina Simone\Verve Remixed\07 - Sinnerman (Felix Da Housecat's Heavenly House Mix).flac`, Extension: ".flac"},
		}},
		{Username: "purist", HasFreeUploadSlot: false, UploadSpeed: 10, Files: []File{
			{Filename: `Music\Nina Simone\Pastel Blues\Sinnerman.flac`, Extension: ".flac"},
		}},
	}
	user, file, ok := SelectBest(responses, Criteria{FormatPreference: []string{"flac", "mp3"}}, target)
	if !ok {
		t.Fatal("expected a selection")
	}
	if user != "purist" {
		t.Fatalf("expected the exact 'Sinnerman' to win over the remix, got user=%s file=%s", user, file.Filename)
	}
}

// TestPrefersTitleMatchOverFormat: the right song in mp3 beats a wrong/remix song
// in the preferred flac format.
func TestPrefersTitleMatchOverFormat(t *testing.T) {
	target := Target{Artist: "The Who", Title: "Behind Blue Eyes"}
	responses := []SearchResponse{
		{Username: "a", Files: []File{{Filename: `The Who\Behind Blue Eyes (Live at Leeds).flac`, Extension: ".flac"}}},
		{Username: "b", Files: []File{{Filename: `The Who\Behind Blue Eyes.mp3`, Extension: ".mp3", BitRate: 320}}},
	}
	user, _, ok := SelectBest(responses, Criteria{FormatPreference: []string{"flac", "mp3"}}, target)
	if !ok || user != "b" {
		t.Fatalf("expected exact mp3 to beat live flac, got user=%s", user)
	}
}

func TestFileExtFallsBackToFilename(t *testing.T) {
	if ext := fileExt(File{Filename: "Music\\Artist\\track.FLAC"}); ext != "flac" {
		t.Fatalf("expected flac from filename, got %q", ext)
	}
}
