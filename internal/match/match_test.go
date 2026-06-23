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
