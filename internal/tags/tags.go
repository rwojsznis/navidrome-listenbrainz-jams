// Package tags writes a MusicBrainz recording id into a downloaded audio file
// so Navidrome indexes it (Navidrome reads tags, not filenames). Each format
// stores the recording id where Picard/Navidrome expect it:
//   - FLAC:      the "MUSICBRAINZ_TRACKID" Vorbis comment (pure Go)
//   - MP3:       an ID3v2 UFID frame owned by "http://musicbrainz.org" (pure Go)
//   - Ogg Opus:  the "MUSICBRAINZ_TRACKID" Vorbis comment, via the `opustags`
//     binary (rewriting the Ogg comment packet safely is not served
//     by a mature pure-Go library, and the fingerprint feature
//     already depends on an external `fpcalc` binary).
//
// Writing is best-effort and idempotent: an existing recording-id tag is
// replaced rather than duplicated, so re-tagging the same file is a no-op.
package tags

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bogem/id3v2/v2"
	"github.com/go-flac/flacvorbis/v2"
	flac "github.com/go-flac/go-flac/v2"
)

// vorbisRecordingIDField is the Vorbis comment key Picard and Navidrome use for
// the MusicBrainz *recording* id (the name is historical — it is not the track
// id). Used for both FLAC and Ogg Opus.
const vorbisRecordingIDField = "MUSICBRAINZ_TRACKID"

// mbOwner is the ID3v2 UFID owner identifier MusicBrainz tools use for the
// recording id.
const mbOwner = "http://musicbrainz.org"

// Writer writes recording ids into files. The zero value works (it finds
// `opustags` on PATH); set OpustagsPath to pin a specific binary.
type Writer struct {
	OpustagsPath string // defaults to "opustags" (resolved via PATH)
}

// WriteRecordingMBID writes mbid as the file's MusicBrainz recording id, picking
// the encoder from the file extension. Unsupported extensions return an error so
// the caller can log and move on. mbid must be non-empty.
func (w Writer) WriteRecordingMBID(ctx context.Context, path, mbid string) error {
	if mbid == "" {
		return fmt.Errorf("empty recording mbid")
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".flac":
		return writeFLAC(path, mbid)
	case ".mp3":
		return writeMP3(path, mbid)
	case ".opus", ".ogg":
		return w.writeOpus(ctx, path, mbid)
	default:
		return fmt.Errorf("unsupported format for tagging: %s", filepath.Ext(path))
	}
}

// writeOpus sets the recording-id Vorbis comment via opustags. `--set` is
// shorthand for delete-field + add, so it is idempotent; `--in-place` edits the
// file directly (opustags writes to a temp file and renames, so a failure can't
// corrupt the original).
func (w Writer) writeOpus(ctx context.Context, path, mbid string) error {
	bin := w.OpustagsPath
	if bin == "" {
		bin = "opustags"
	}
	cmd := exec.CommandContext(ctx, bin, "--in-place",
		"--set", vorbisRecordingIDField+"="+mbid, path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("opustags: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func writeFLAC(path, mbid string) error {
	f, err := flac.ParseFile(path)
	if err != nil {
		return fmt.Errorf("parse flac: %w", err)
	}

	// Reuse the existing Vorbis comment block if present, else create one.
	cmt := flacvorbis.New()
	idx := -1
	for i, m := range f.Meta {
		if m.Type == flac.VorbisComment {
			parsed, perr := flacvorbis.ParseFromMetaDataBlock(*m)
			if perr != nil {
				return fmt.Errorf("parse vorbis comment: %w", perr)
			}
			cmt = parsed
			idx = i
			break
		}
	}

	// Drop any existing recording-id comment so re-tagging doesn't duplicate it.
	prefix := vorbisRecordingIDField + "="
	kept := cmt.Comments[:0]
	for _, c := range cmt.Comments {
		if !strings.HasPrefix(strings.ToUpper(c), prefix) {
			kept = append(kept, c)
		}
	}
	cmt.Comments = kept
	if err := cmt.Add(vorbisRecordingIDField, mbid); err != nil {
		return fmt.Errorf("add vorbis comment: %w", err)
	}

	block := cmt.Marshal()
	if idx >= 0 {
		f.Meta[idx] = &block
	} else {
		f.Meta = append(f.Meta, &block)
	}
	if err := f.Save(path); err != nil {
		return fmt.Errorf("save flac: %w", err)
	}
	return nil
}

func writeMP3(path, mbid string) error {
	tag, err := id3v2.Open(path, id3v2.Options{Parse: true})
	if err != nil {
		return fmt.Errorf("open mp3: %w", err)
	}
	defer tag.Close()

	// Replace rather than append so re-tagging stays idempotent.
	tag.DeleteFrames("UFID")
	tag.AddUFIDFrame(id3v2.UFIDFrame{
		OwnerIdentifier: mbOwner,
		Identifier:      []byte(mbid),
	})
	if err := tag.Save(); err != nil {
		return fmt.Errorf("save mp3: %w", err)
	}
	return nil
}
