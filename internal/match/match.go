// Package match provides normalized fuzzy matching of search results to feed
// tracks. Feed metadata and library tags rarely agree byte-for-byte (smart
// quotes, diacritics, "feat." vs "ft.", remaster suffixes), so we normalize
// aggressively and compare with a similarity ratio.
package match

import (
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Candidate is anything with an artist and title that can be matched.
type Candidate struct {
	Artist string
	Title  string
}

// Result is a scored match.
type Result struct {
	Index       int     // index into the candidate slice
	ArtistScore float64 // 0..1
	TitleScore  float64 // 0..1
	Score       float64 // combined
}

// stripDiacritics removes combining marks (é -> e).
var stripTransformer = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

// Normalize lowercases, replaces smart quotes, strips diacritics and
// punctuation, and collapses whitespace, producing a comparable key.
func Normalize(s string) string {
	s = strings.NewReplacer(
		// Apostrophes are elided (no separator) so "I’m" -> "im".
		"’", "", "‘", "", "'", "",
		"“", "", "”", "", "–", " ", "—", " ", "&", "and",
	).Replace(s)

	if out, _, err := transform.String(stripTransformer, s); err == nil {
		s = out
	}
	s = strings.ToLower(s)

	var b strings.Builder
	lastSpace := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			b.WriteRune(r)
			lastSpace = false
		case unicode.IsSpace(r):
			if !lastSpace {
				b.WriteRune(' ')
				lastSpace = true
			}
		default:
			// drop punctuation, but treat it as a separator
			if !lastSpace {
				b.WriteRune(' ')
				lastSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// Similarity returns a 0..1 score between two strings based on normalized
// Levenshtein distance. Identical (after Normalize) strings score 1.
func Similarity(a, b string) float64 {
	na, nb := Normalize(a), Normalize(b)
	if na == nb {
		return 1
	}
	if na == "" || nb == "" {
		return 0
	}
	dist := levenshtein(na, nb)
	maxLen := len(na)
	if len(nb) > maxLen {
		maxLen = len(nb)
	}
	return 1 - float64(dist)/float64(maxLen)
}

// Best finds the candidate that best matches the target artist/title, requiring
// both artist and title similarity to meet threshold. ok is false if none do.
func Best(target Candidate, candidates []Candidate, threshold float64) (Result, bool) {
	best := Result{Index: -1}
	for i, c := range candidates {
		artistScore := artistSimilarity(target.Artist, c.Artist)
		titleScore := Similarity(target.Title, c.Title)
		if artistScore < threshold || titleScore < threshold {
			continue
		}
		combined := (artistScore + titleScore) / 2
		if combined > best.Score {
			best = Result{Index: i, ArtistScore: artistScore, TitleScore: titleScore, Score: combined}
		}
	}
	return best, best.Index >= 0
}

// artistSimilarity is lenient about extra credited artists: the feed often lists
// one artist while the library tags include collaborators (or vice versa). A
// containment match counts as a strong score.
func artistSimilarity(a, b string) float64 {
	na, nb := Normalize(a), Normalize(b)
	if na == "" || nb == "" {
		return 0
	}
	if na == nb {
		return 1
	}
	if strings.Contains(nb, na) || strings.Contains(na, nb) {
		return 0.95
	}
	return Similarity(a, b)
}

// levenshtein computes edit distance between two strings (rune-aware).
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min3(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
