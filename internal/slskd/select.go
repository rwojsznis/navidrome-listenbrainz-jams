package slskd

import (
	"path"
	"sort"
	"strings"

	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/match"
)

// Criteria controls candidate ranking.
type Criteria struct {
	// FormatPreference is an ordered list of preferred extensions (no dot),
	// e.g. ["flac", "mp3"]. Files of other formats are rejected.
	FormatPreference []string
	// MinBitrate rejects lossy files below this kbps. Lossless formats (flac)
	// are exempt (their bitRate field is often absent/variable).
	MinBitrate int
}

// Target is the track we're trying to download, used to score how closely a
// candidate filename matches the requested recording.
type Target struct {
	Artist string
	Title  string
}

// Candidate pairs a downloadable file with the peer offering it.
type Candidate struct {
	Username string
	File     File
}

// minStandaloneTitleTokens is how many title words a match needs to be trusted
// when the artist is absent from the file's path. Single-word titles ("Intro",
// "Familiar") coincidentally appear in countless unrelated files, so they need
// the artist present; multi-word titles ("Come Together") are specific enough to
// stand on their own (loose shared files often omit the artist from the path).
const minStandaloneTitleTokens = 2

// Rank returns the acceptable candidates across the responses, ordered best
// first. A candidate is only acceptable if it plausibly IS the requested
// recording (see acceptable) — never merely the least-bad search result, which
// previously let a search for "Familiar" import an unrelated track that happened
// to share that one word. Among acceptable candidates, ranking prioritizes how
// closely the filename matches the requested title (so the original recording
// beats remixes/live/edited versions that carry extra words), then artist
// presence, then format/quality. Many Soulseek peers are unreachable, so callers
// should try candidates in order until an enqueue succeeds. Locked files are
// ignored (they require sharing to access).
func Rank(responses []SearchResponse, c Criteria, target Target) []Candidate {
	losslessSet := map[string]bool{"flac": true, "wav": true, "aiff": true, "alac": true, "ape": true}

	titleTokens := tokenize(target.Title)
	titleSet := toSet(titleTokens)
	artistSet := toSet(tokenize(target.Artist))
	// Acceptance uses the simplified title (decorations like "(original mix)" or
	// a "feat." clause stripped) so a legitimate file that omits them isn't
	// rejected, while still requiring the core title words to be present.
	reqTitleTokens := tokenize(match.SimplifyTitle(target.Title))

	type scored struct {
		cand          Candidate
		resp          SearchResponse
		fmtRank       int
		titleMissing  int // requested title tokens absent from the filename
		artistMissing int // requested artist tokens absent from the full path
		extra         int // non-title, non-artist words (remix/live/edit/...)
	}
	var scoredList []scored

	for _, r := range responses {
		for _, f := range r.Files {
			ext := fileExt(f)
			rank := formatRank(ext, c.FormatPreference)
			if rank < 0 {
				continue // not an accepted format
			}
			lossless := losslessSet[ext]
			if !lossless && c.MinBitrate > 0 && f.BitRate > 0 && f.BitRate < c.MinBitrate {
				continue
			}

			nameSet := toSet(tokenize(baseNoExt(f.Filename)))
			pathSet := toSet(tokenize(f.Filename))

			titleMissing := 0
			for _, t := range titleTokens {
				if !nameSet[t] {
					titleMissing++
				}
			}
			artistMissing := 0
			for t := range artistSet {
				if !pathSet[t] {
					artistMissing++
				}
			}
			// Acceptance gate: drop candidates that aren't plausibly this
			// recording, so a tick downloads nothing rather than the wrong file.
			if !acceptable(reqTitleTokens, nameSet, artistMissing) {
				continue
			}
			extra := 0
			for tok := range nameSet {
				if titleSet[tok] || artistSet[tok] || isNumeric(tok) {
					continue
				}
				extra++
			}

			scoredList = append(scoredList, scored{
				cand:          Candidate{Username: r.Username, File: f},
				resp:          r,
				fmtRank:       rank,
				titleMissing:  titleMissing,
				artistMissing: artistMissing,
				extra:         extra,
			})
		}
	}

	sort.SliceStable(scoredList, func(i, j int) bool {
		a, b := scoredList[i], scoredList[j]
		// 1. fewest requested title words missing (closest title among the
		//    already-acceptable candidates; wrong songs are dropped by acceptable)
		if a.titleMissing != b.titleMissing {
			return a.titleMissing < b.titleMissing
		}
		// 2. artist present in the path
		if a.artistMissing != b.artistMissing {
			return a.artistMissing < b.artistMissing
		}
		// 3. closest to the exact title — fewest extra words (remix/live/edit)
		if a.extra != b.extra {
			return a.extra < b.extra
		}
		// 4. preferred format
		if a.fmtRank != b.fmtRank {
			return a.fmtRank < b.fmtRank
		}
		// 5. peers with a free upload slot first
		if a.resp.HasFreeUploadSlot != b.resp.HasFreeUploadSlot {
			return a.resp.HasFreeUploadSlot
		}
		// 6. higher quality (bitrate)
		if a.cand.File.BitRate != b.cand.File.BitRate {
			return a.cand.File.BitRate > b.cand.File.BitRate
		}
		// 7. faster uploader
		if a.resp.UploadSpeed != b.resp.UploadSpeed {
			return a.resp.UploadSpeed > b.resp.UploadSpeed
		}
		// 8. shorter queue
		return a.resp.QueueLength < b.resp.QueueLength
	})

	out := make([]Candidate, len(scoredList))
	for i, s := range scoredList {
		out[i] = s.cand
	}
	return out
}

// SelectBest returns the single best candidate, or ok=false if none qualify.
func SelectBest(responses []SearchResponse, c Criteria, target Target) (username string, file File, ok bool) {
	ranked := Rank(responses, c, target)
	if len(ranked) == 0 {
		return "", File{}, false
	}
	return ranked[0].Username, ranked[0].File, true
}

// acceptable reports whether a candidate file plausibly IS the requested
// recording, rather than just the closest of several wrong results. It rejects:
//   - wrong songs: any required (simplified) title word absent from the
//     filename, e.g. "Must Be Together" vs a file named "We'll Be Together Again";
//   - coincidental single-word titles: the title is one common word present in
//     an unrelated file by a different artist (e.g. "Familiar" -> a Touhou track,
//     "Intro" -> "B2 Intro Sing"), with the artist absent from the path.
//
// When the artist appears in the path the match is trusted outright; otherwise
// it is trusted only if the title is specific enough to stand alone, preserving
// downloads of loose shared files that omit the artist (e.g. "Come Together.flac").
func acceptable(reqTitleTokens []string, nameSet map[string]bool, artistMissing int) bool {
	for _, t := range reqTitleTokens {
		if !nameSet[t] {
			return false
		}
	}
	if artistMissing == 0 {
		return true
	}
	return len(reqTitleTokens) >= minStandaloneTitleTokens
}

// tokenize normalizes s (lowercase, strip diacritics/punctuation) and splits it
// into words.
func tokenize(s string) []string {
	n := match.Normalize(s)
	if n == "" {
		return nil
	}
	return strings.Fields(n)
}

func toSet(tokens []string) map[string]bool {
	m := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		m[t] = true
	}
	return m
}

// baseNoExt returns the filename's base name without extension, handling the
// backslash separators slskd uses.
func baseNoExt(filename string) string {
	s := strings.ReplaceAll(filename, "\\", "/")
	s = path.Base(s)
	return strings.TrimSuffix(s, path.Ext(s))
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// fileExt returns the lowercase extension without a dot, preferring the File's
// own Extension field and falling back to the filename.
func fileExt(f File) string {
	ext := strings.TrimPrefix(strings.ToLower(f.Extension), ".")
	if ext != "" {
		return ext
	}
	// slskd filenames use backslash separators; normalize before path.Ext.
	name := strings.ReplaceAll(f.Filename, "\\", "/")
	return strings.TrimPrefix(strings.ToLower(path.Ext(name)), ".")
}

// formatRank returns the index of ext in prefs (0 = most preferred), or -1 if
// not present.
func formatRank(ext string, prefs []string) int {
	for i, p := range prefs {
		if strings.EqualFold(strings.TrimPrefix(p, "."), ext) {
			return i
		}
	}
	return -1
}
