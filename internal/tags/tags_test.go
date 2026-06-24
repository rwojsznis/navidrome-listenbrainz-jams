package tags

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bogem/id3v2/v2"
	"github.com/go-flac/flacvorbis/v2"
	flac "github.com/go-flac/go-flac/v2"
)

const testMBID = "11111111-2222-3333-4444-555555555555"

// copyFixture copies the first real library file with the given extension into a
// temp dir and returns the copy's path, or skips if none exist. It only ever
// READS the originals — never modifies files under data/music.
func copyFixture(t *testing.T, ext string) string {
	t.Helper()
	matches, _ := filepath.Glob("../../data/music/weekly-jams/*" + ext)
	if len(matches) == 0 {
		t.Skipf("no %s fixture available", ext)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "track"+ext)
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("write copy: %v", err)
	}
	return dst
}

func TestWriteFLAC(t *testing.T) {
	path := copyFixture(t, ".flac")
	w := Writer{}
	if err := w.WriteRecordingMBID(context.Background(), path, testMBID); err != nil {
		t.Fatal(err)
	}
	// Idempotent re-tag.
	if err := w.WriteRecordingMBID(context.Background(), path, testMBID); err != nil {
		t.Fatal(err)
	}

	f, err := flac.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range f.Meta {
		if m.Type == flac.VorbisComment {
			cmt, _ := flacvorbis.ParseFromMetaDataBlock(*m)
			got, _ = cmt.Get(vorbisRecordingIDField)
		}
	}
	if len(got) != 1 || got[0] != testMBID {
		t.Fatalf("recording id = %v, want exactly [%s]", got, testMBID)
	}
}

func TestWriteMP3(t *testing.T) {
	path := copyFixture(t, ".mp3")
	w := Writer{}
	if err := w.WriteRecordingMBID(context.Background(), path, testMBID); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteRecordingMBID(context.Background(), path, testMBID); err != nil {
		t.Fatal(err)
	}

	tag, err := id3v2.Open(path, id3v2.Options{Parse: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tag.Close()
	frames := tag.AllFrames()["UFID"]
	if len(frames) != 1 {
		t.Fatalf("got %d UFID frames, want 1", len(frames))
	}
	ufid := frames[0].(id3v2.UFIDFrame)
	if ufid.OwnerIdentifier != mbOwner || string(ufid.Identifier) != testMBID {
		t.Fatalf("UFID = %q/%q, want %q/%q", ufid.OwnerIdentifier, ufid.Identifier, mbOwner, testMBID)
	}
}

func TestWriteOpus(t *testing.T) {
	if _, err := exec.LookPath("opustags"); err != nil {
		t.Skip("opustags not installed")
	}
	path := copyFixture(t, ".opus")
	w := Writer{}
	if err := w.WriteRecordingMBID(context.Background(), path, testMBID); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteRecordingMBID(context.Background(), path, testMBID); err != nil {
		t.Fatal(err)
	}

	// Tag present exactly once.
	out, err := exec.Command("opustags", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(out, []byte(vorbisRecordingIDField+"="+testMBID)); n != 1 {
		t.Fatalf("recording id present %d times, want 1\n%s", n, out)
	}

	// Still decodes (structure intact) — skip if fpcalc absent.
	if _, err := exec.LookPath("fpcalc"); err == nil {
		fp, err := exec.Command("fpcalc", "-json", path).Output()
		if err != nil || !bytes.Contains(fp, []byte("fingerprint")) {
			t.Fatalf("tagged opus no longer decodes: %v", err)
		}
	}
}
