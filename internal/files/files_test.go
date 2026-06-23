package files

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBaseName(t *testing.T) {
	cases := map[string]string{
		`@@abc\Music\Artist\Album\01 - Track.flac`: "01 - Track.flac",
		"already/forward/slash.mp3":                "slash.mp3",
		"plain.flac":                               "plain.flac",
	}
	for in, want := range cases {
		if got := BaseName(in); got != want {
			t.Errorf("BaseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFindByBasename(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "bob", "Album")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(nested, "track.flac")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok, err := FindByBasename(root, "track.flac")
	if err != nil || !ok {
		t.Fatalf("FindByBasename ok=%v err=%v", ok, err)
	}
	if got != target {
		t.Errorf("found %q, want %q", got, target)
	}

	if _, ok, _ := FindByBasename(root, "missing.flac"); ok {
		t.Error("expected not found")
	}
}

func TestMoveWithCollision(t *testing.T) {
	srcDir, dstDir := t.TempDir(), t.TempDir()

	// Pre-existing file in dst to force a collision suffix.
	if err := os.WriteFile(filepath.Join(dstDir, "track.flac"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(srcDir, "track.flac")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst, err := Move(src, dstDir)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if filepath.Base(dst) != "track (1).flac" {
		t.Errorf("expected collision-suffixed name, got %q", filepath.Base(dst))
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source should be removed after move")
	}
	data, _ := os.ReadFile(dst)
	if string(data) != "new" {
		t.Errorf("moved content = %q, want new", data)
	}
}
