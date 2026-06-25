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

func TestRemoveDeletesFileAndLrc(t *testing.T) {
	dir := t.TempDir()
	music := filepath.Join(dir, "Artist - Title.flac")
	lrc := filepath.Join(dir, "Artist - Title.lrc")
	for _, p := range []string{music, lrc} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := Remove(music); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(music); !os.IsNotExist(err) {
		t.Error("music file should be gone")
	}
	if _, err := os.Stat(lrc); !os.IsNotExist(err) {
		t.Error("sibling .lrc should be gone")
	}
}

func TestRemoveIdempotentAndEmpty(t *testing.T) {
	if err := Remove(""); err != nil {
		t.Errorf("Remove(\"\") = %v, want nil", err)
	}
	// Missing file (no sibling .lrc) must not error.
	if err := Remove(filepath.Join(t.TempDir(), "gone.mp3")); err != nil {
		t.Errorf("Remove(missing) = %v, want nil", err)
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

func TestMoveRenames(t *testing.T) {
	srcDir, dstDir := t.TempDir(), t.TempDir()
	src := filepath.Join(srcDir, "01-08. Lizzy McAlpine - ceilings.flac")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst, err := Move(src, dstDir, "Lizzy McAlpine - ceilings.flac")
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if filepath.Base(dst) != "Lizzy McAlpine - ceilings.flac" {
		t.Errorf("expected renamed file, got %q", filepath.Base(dst))
	}
}

func TestMoveWithCollision(t *testing.T) {
	srcDir, dstDir := t.TempDir(), t.TempDir()

	// Pre-existing file in dst to force a collision suffix.
	if err := os.WriteFile(filepath.Join(dstDir, "track.flac"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(srcDir, "orig.flac")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst, err := Move(src, dstDir, "track.flac")
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

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"Foo Fighters - Everlong":       "Foo Fighters - Everlong",
		"AC/DC - T.N.T":                 "AC_DC - T.N.T",
		"Sigur Rós - Hoppípolla":        "Sigur Rós - Hoppípolla", // unicode preserved
		"Band: The Album <feat>":        "Band_ The Album _feat_",
		"  spaced   out  ":              "spaced out",
		"trailing dots...":              "trailing dots",
	}
	for in, want := range cases {
		if got := SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}
