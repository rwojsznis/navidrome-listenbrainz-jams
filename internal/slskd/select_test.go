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

// The following three are regressions from playlist 1: searches whose correct
// file wasn't on Soulseek used to import the least-bad unrelated result. The
// acceptance gate must now reject them so the track stays missing instead.

// Single common word ("Familiar") present in an unrelated track by a different
// artist must not be accepted.
func TestRejectsCoincidentalSingleWordTitle(t *testing.T) {
	target := Target{Artist: "Agnes Obel", Title: "Familiar"}
	responses := []SearchResponse{
		{Username: "x", Files: []File{{Filename: `Touhou\[Liz Triangle]\2011 Memoire\06 - Familiar.opus`, Extension: ".opus"}}},
	}
	if _, _, ok := SelectBest(responses, Criteria{FormatPreference: []string{"opus", "flac", "mp3"}}, target); ok {
		t.Fatal("expected an unrelated 'Familiar' by a different artist to be rejected")
	}
}

// A different song that merely shares a couple of words ("Be Together") must not
// be accepted just because it was the closest result.
func TestRejectsWrongSongSharingSomeWords(t *testing.T) {
	target := Target{Artist: "D-Mad", Title: "Must Be Together (original mix)"}
	responses := []SearchResponse{
		{Username: "x", Files: []File{{Filename: `Frank Sinatra\Swingin' Lovers\11 We'll Be Together Again.mp3`, Extension: ".mp3", BitRate: 320}}},
	}
	if _, _, ok := SelectBest(responses, Criteria{FormatPreference: []string{"mp3", "flac"}}, target); ok {
		t.Fatal("expected 'We'll Be Together Again' to be rejected for 'Must Be Together'")
	}
}

// Single-word title with the wrong artist in the path must be rejected.
func TestRejectsSingleWordTitleWrongArtist(t *testing.T) {
	target := Target{Artist: "The xx", Title: "Intro"}
	responses := []SearchResponse{
		{Username: "x", Files: []File{{Filename: `William B. Tanner Company\The Cat - Custom Audio Traxx\B2 Intro Sing.flac`, Extension: ".flac"}}},
	}
	if _, _, ok := SelectBest(responses, Criteria{FormatPreference: []string{"flac", "mp3"}}, target); ok {
		t.Fatal("expected an unrelated 'Intro' track to be rejected")
	}
}

// A multi-word title is specific enough to accept even when the artist is absent
// from the path — loose shared files often omit the artist. This must keep
// working so the gate doesn't over-reject legitimate downloads.
func TestAcceptsArtistAbsentMultiWordTitle(t *testing.T) {
	target := Target{Artist: "The Beatles", Title: "Come Together"}
	responses := []SearchResponse{
		{Username: "x", Files: []File{{Filename: `Shared\Come Together.flac`, Extension: ".flac"}}},
	}
	if _, _, ok := SelectBest(responses, Criteria{FormatPreference: []string{"flac", "mp3"}}, target); !ok {
		t.Fatal("expected a multi-word title to be accepted even without the artist in the path")
	}
}

// A single-word title IS accepted when the artist is present in the path.
func TestAcceptsSingleWordTitleWhenArtistPresent(t *testing.T) {
	target := Target{Artist: "The xx", Title: "Intro"}
	responses := []SearchResponse{
		{Username: "x", Files: []File{{Filename: `The xx\xx\01 - Intro.flac`, Extension: ".flac"}}},
	}
	if _, _, ok := SelectBest(responses, Criteria{FormatPreference: []string{"flac", "mp3"}}, target); !ok {
		t.Fatal("expected a single-word title with the artist present to be accepted")
	}
}

func TestFileExtFallsBackToFilename(t *testing.T) {
	if ext := fileExt(File{Filename: "Music\\Artist\\track.FLAC"}); ext != "flac" {
		t.Fatalf("expected flac from filename, got %q", ext)
	}
}
