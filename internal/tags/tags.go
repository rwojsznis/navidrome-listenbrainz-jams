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

// WriteBasic writes the artist and title tags into the file. It exists for
// sources whose audio arrives with NO embedded tags (yt-dlp rips of YouTube
// audio): without these Navidrome indexes the file as "[Unknown Artist]" with the
// filename as its title. slskd downloads already carry the uploader's tags, so
// this is not used for them. Idempotent: any existing artist/title is replaced.
// Unsupported extensions return an error so the caller can log and move on.
func (w Writer) WriteBasic(ctx context.Context, path, artist, title string) error {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".flac":
		return writeFLACBasic(path, artist, title)
	case ".mp3":
		return writeMP3Basic(path, artist, title)
	case ".opus", ".ogg":
		return w.writeOpusBasic(ctx, path, artist, title)
	default:
		return fmt.Errorf("unsupported format for tagging: %s", filepath.Ext(path))
	}
}

// writeOpus sets the recording-id Vorbis comment via opustags. `--set` is
// shorthand for delete-field + add, so it is idempotent; `--in-place` edits the
// file directly (opustags writes to a temp file and renames, so a failure can't
// corrupt the original).
func (w Writer) writeOpus(ctx context.Context, path, mbid string) error {
	return w.opustags(ctx, path, "--set", vorbisRecordingIDField+"="+mbid)
}

// writeOpusBasic sets the artist and title Vorbis comments via opustags.
func (w Writer) writeOpusBasic(ctx context.Context, path, artist, title string) error {
	return w.opustags(ctx, path, "--set", "ARTIST="+artist, "--set", "TITLE="+title)
}

// opustags runs an in-place opustags edit with the given field operations.
func (w Writer) opustags(ctx context.Context, path string, ops ...string) error {
	bin := w.OpustagsPath
	if bin == "" {
		bin = "opustags"
	}
	args := append([]string{"--in-place"}, ops...)
	args = append(args, path)
	cmd := exec.CommandContext(ctx, bin, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("opustags: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func writeFLAC(path, mbid string) error {
	return updateFLACComments(path, func(cmt *flacvorbis.MetaDataBlockVorbisComment) error {
		dropVorbisField(cmt, vorbisRecordingIDField)
		return cmt.Add(vorbisRecordingIDField, mbid)
	})
}

func writeFLACBasic(path, artist, title string) error {
	return updateFLACComments(path, func(cmt *flacvorbis.MetaDataBlockVorbisComment) error {
		dropVorbisField(cmt, "ARTIST")
		dropVorbisField(cmt, "TITLE")
		if err := cmt.Add("ARTIST", artist); err != nil {
			return err
		}
		return cmt.Add("TITLE", title)
	})
}

// updateFLACComments loads path's Vorbis comment block (creating one if absent),
// applies mutate, and saves. It centralizes the parse/find/marshal/save dance so
// the recording-id and basic-tag writers share identical, safe handling.
func updateFLACComments(path string, mutate func(*flacvorbis.MetaDataBlockVorbisComment) error) error {
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

	if err := mutate(cmt); err != nil {
		return fmt.Errorf("update vorbis comment: %w", err)
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

// dropVorbisField removes every "FIELD=..." comment (case-insensitive) so a
// re-write replaces rather than duplicates it.
func dropVorbisField(cmt *flacvorbis.MetaDataBlockVorbisComment, field string) {
	prefix := strings.ToUpper(field) + "="
	kept := cmt.Comments[:0]
	for _, c := range cmt.Comments {
		if !strings.HasPrefix(strings.ToUpper(c), prefix) {
			kept = append(kept, c)
		}
	}
	cmt.Comments = kept
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

func writeMP3Basic(path, artist, title string) error {
	tag, err := id3v2.Open(path, id3v2.Options{Parse: true})
	if err != nil {
		return fmt.Errorf("open mp3: %w", err)
	}
	defer tag.Close()

	// SetArtist/SetTitle replace the TPE1/TIT2 frames, so this stays idempotent.
	tag.SetArtist(artist)
	tag.SetTitle(title)
	if err := tag.Save(); err != nil {
		return fmt.Errorf("save mp3: %w", err)
	}
	return nil
}
