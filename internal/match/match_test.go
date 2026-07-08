package match

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"I’m Yours":              "im yours",
		"Café del Mar":           "cafe del mar",
		"Feel Good Inc.":         "feel good inc",
		"AC/DC":                  "ac dc",
		"Sigur Rós":              "sigur ros",
		"Simon & Garfunkel":      "simon and garfunkel",
		"  Multiple   Spaces  ":  "multiple spaces",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSimilarity(t *testing.T) {
	if s := Similarity("Army Dreamers", "army dreamers"); s != 1 {
		t.Errorf("identical-after-normalize should be 1, got %v", s)
	}
	if s := Similarity("vampire", "Vampire"); s != 1 {
		t.Errorf("case-insensitive should be 1, got %v", s)
	}
	if s := Similarity("Feel Good Inc.", "Feel Good Inc"); s < 0.99 {
		t.Errorf("punctuation-only diff should be ~1, got %v", s)
	}
	if s := Similarity("Army Dreamers", "Wuthering Heights"); s > 0.5 {
		t.Errorf("different titles should score low, got %v", s)
	}
}

func TestBest(t *testing.T) {
	candidates := []Candidate{
		{Artist: "Kate Bush", Title: "Wuthering Heights"},
		{Artist: "Kate Bush", Title: "Army Dreamers"},
		{Artist: "Gorillaz", Title: "Feel Good Inc."},
	}

	// Exact-ish match with smart-quote/punctuation noise.
	res, ok := Best(Candidate{Artist: "Gorillaz", Title: "Feel Good Inc"}, candidates, 0.85)
	if !ok || res.Index != 2 {
		t.Fatalf("expected match at index 2, got %+v ok=%v", res, ok)
	}

	// Featured-artist leniency: feed has plain artist, library has collaborator.
	collab := []Candidate{{Artist: "Gorillaz feat. De La Soul", Title: "Feel Good Inc."}}
	if _, ok := Best(Candidate{Artist: "Gorillaz", Title: "Feel Good Inc."}, collab, 0.85); !ok {
		t.Error("expected featured-artist containment to match")
	}

	// No reasonable match -> not ok.
	if _, ok := Best(Candidate{Artist: "Daft Punk", Title: "One More Time"}, candidates, 0.85); ok {
		t.Error("expected no match for absent track")
	}
}

func TestSimplifyArtist(t *testing.T) {
	cases := map[string]string{
		"Lady Gaga featuring R. Kelly": "Lady Gaga",
		"Gorillaz feat. De La Soul":    "Gorillaz",
		"Jay-Z ft. Alicia Keys":        "Jay-Z",
		"Calvin Harris with Rihanna":   "Calvin Harris",
		"Lady Gaga & Bruno Mars":       "Lady Gaga & Bruno Mars", // "&" collab kept, not a feat clause
		"Daft Punk":                    "Daft Punk",
	}
	for in, want := range cases {
		if got := SimplifyArtist(in); got != want {
			t.Errorf("SimplifyArtist(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBestTitleDecorations covers library tags that carry decorations the feed
// title lacks. Without leniency these files (which the downloader fetched via
// SimplifyTitle) get stuck unresolved because resolve() can't match them back.
func TestBestTitleDecorations(t *testing.T) {
	matches := []struct {
		name              string
		feedArtist, feedTitle string
		libArtist, libTitle   string
	}{
		{"scene tag", "The Weeknd", "Sacrifice", "The Weeknd", "Sacrifice (PMEDIA)"},
		{"feat in title", "Gorillaz feat. Kali Uchis", "She’s My Collar", "Gorillaz", "She's My Collar (feat. Kali Uchis)"},
		{"leading article", "Corona", "The Rhythm of the Night", "Corona", "Rhythm Of The Night"},
	}
	for _, m := range matches {
		if _, ok := Best(Candidate{Artist: m.feedArtist, Title: m.feedTitle},
			[]Candidate{{Artist: m.libArtist, Title: m.libTitle}}, 0.85); !ok {
			t.Errorf("%s: expected match for %q/%q vs %q/%q", m.name,
				m.feedArtist, m.feedTitle, m.libArtist, m.libTitle)
		}
	}

	// Guard: a short title must not be swallowed by a longer one for the same
	// artist (distinct songs).
	if _, ok := Best(Candidate{Artist: "ABBA", Title: "Money"},
		[]Candidate{{Artist: "ABBA", Title: "Money, Money, Money"}}, 0.85); ok {
		t.Error("expected no match between distinct songs sharing a word")
	}
}

// TestBestMultiArtist covers collaborations where the feed and library credit
// the same artists in a different order, with different separators, and with
// extra collaborators — so neither full artist string contains the other.
func TestBestMultiArtist(t *testing.T) {
	feed := Candidate{Artist: "The Weeknd with Playboi Carti & Madonna", Title: "Popular"}
	lib := []Candidate{{
		Artist: "The Weeknd; Madonna; Playboi Carti; xxtristanxo",
		Title:  "Popular (Slowed) (with Playboi Carti & Madonna)",
	}}
	if _, ok := Best(feed, lib, 0.85); !ok {
		t.Errorf("expected multi-artist collaboration to match on primary artist")
	}

	// Guard: sharing only a primary artist is not enough when titles differ —
	// Best() still requires an independent title match.
	other := []Candidate{{Artist: "The Weeknd, Daft Punk", Title: "Starboy"}}
	if _, ok := Best(feed, other, 0.85); ok {
		t.Errorf("expected no match: same lead artist but a different song")
	}
}
