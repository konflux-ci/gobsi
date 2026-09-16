package source_test

import (
	"archive/tar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/konflux-ci/gobsi/pkg/source"
)

func TestProcessExtraSrcDirs(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello"), 0o644)

	artifacts, err := source.ProcessExtraSrcDirs([]string{dir}, t.TempDir())
	if err != nil {
		t.Fatalf("ProcessExtraSrcDirs failed: %v", err)
	}

	if len(artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(artifacts))
	}

	a := artifacts[0]
	if a.DriverName != "extra_src_dir" {
		t.Errorf("expected driver extra_src_dir, got %s", a.DriverName)
	}
	if !strings.HasPrefix(a.Metadata.Name, "extra-src-") || !strings.HasSuffix(a.Metadata.Name, ".tar") {
		t.Errorf("expected name extra-src-<checksum>.tar, got %s", a.Metadata.Name)
	}
	if a.Metadata.Mimetype != "application/x-tar" {
		t.Errorf("expected mimetype application/x-tar, got %s", a.Metadata.Mimetype)
	}
	if a.Metadata.Checksum == "" {
		t.Error("expected checksum to be set")
	}
	if _, err := os.Stat(a.Path); err != nil {
		t.Errorf("tar file does not exist: %v", err)
	}
}

func TestProcessExtraSrcDirsMultiple(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	os.WriteFile(filepath.Join(dir1, "a.txt"), []byte("aaa"), 0o644)
	os.WriteFile(filepath.Join(dir2, "b.txt"), []byte("bbb"), 0o644)

	artifacts, err := source.ProcessExtraSrcDirs([]string{dir1, dir2}, t.TempDir())
	if err != nil {
		t.Fatalf("ProcessExtraSrcDirs failed: %v", err)
	}

	if len(artifacts) != 2 {
		t.Fatalf("expected 2 artifacts, got %d", len(artifacts))
	}

	name0 := artifacts[0].Metadata.Name
	name1 := artifacts[1].Metadata.Name
	if !strings.HasPrefix(name0, "extra-src-") || !strings.HasSuffix(name0, ".tar") {
		t.Errorf("expected name extra-src-<checksum>.tar, got %s", name0)
	}
	if !strings.HasPrefix(name1, "extra-src-") || !strings.HasSuffix(name1, ".tar") {
		t.Errorf("expected name extra-src-<checksum>.tar, got %s", name1)
	}
	if name0 == name1 {
		t.Errorf("expected different names for different dirs, both got %s", name0)
	}
}

func TestCreateTarContents(t *testing.T) {
	srcDir := t.TempDir()
	os.MkdirAll(filepath.Join(srcDir, "subdir"), 0o755)
	os.WriteFile(filepath.Join(srcDir, "b.txt"), []byte("bbb"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("aaa"), 0o644)
	os.WriteFile(filepath.Join(srcDir, "subdir", "c.txt"), []byte("ccc"), 0o644)
	// "subdir.tar" is the wedge case: '.' (0x2E) sorts before '/' (0x2F), so a
	// naive string sort would slip it between "subdir/" and "subdir/c.txt".
	// Depth-first traversal must keep the directory's contents grouped.
	os.WriteFile(filepath.Join(srcDir, "subdir.tar"), []byte("tar"), 0o644)

	artifacts, err := source.ProcessExtraSrcDirs([]string{srcDir}, t.TempDir())
	if err != nil {
		t.Fatalf("ProcessExtraSrcDirs failed: %v", err)
	}

	f, err := os.Open(artifacts[0].Path)
	if err != nil {
		t.Fatalf("opening tar: %v", err)
	}
	defer f.Close()

	tr := tar.NewReader(f)
	var names []string
	for {
		header, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, header.Name)

		if header.Uid != 0 || header.Gid != 0 {
			t.Errorf("entry %s has non-zero uid/gid: %d/%d", header.Name, header.Uid, header.Gid)
		}
		if !header.ModTime.Equal(time.Unix(0, 0)) {
			t.Errorf("entry %s has non-epoch modtime: %v", header.Name, header.ModTime)
		}

	}

	expected := []string{"./", "./a.txt", "./b.txt", "./subdir/", "./subdir/c.txt", "./subdir.tar"}
	if len(names) != len(expected) {
		t.Fatalf("expected %d entries, got %d: %v", len(expected), len(names), names)
	}
	for i, name := range names {
		if name != expected[i] {
			t.Errorf("entry %d: expected %s, got %s", i, expected[i], name)
		}
	}
}

func TestCreateTarRootModeFromSrcDir(t *testing.T) {
	// The root "./" entry reflects srcDir's own mode with read-write added for
	// everyone (as `tar --mode=a+rw` does), so a 0700 dir yields 0766.
	srcDir := t.TempDir()
	if err := os.Chmod(srcDir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("aaa"), 0o644)

	artifacts, err := source.ProcessExtraSrcDirs([]string{srcDir}, t.TempDir())
	if err != nil {
		t.Fatalf("ProcessExtraSrcDirs failed: %v", err)
	}

	f, err := os.Open(artifacts[0].Path)
	if err != nil {
		t.Fatalf("opening tar: %v", err)
	}
	defer f.Close()

	tr := tar.NewReader(f)
	found := false
	for {
		header, err := tr.Next()
		if err != nil {
			break
		}
		if header.Name == "./" {
			found = true
			if got := header.Mode & 0o777; got != 0o766 {
				t.Errorf("root mode: expected 0766, got %#o", got)
			}
		}
	}
	if !found {
		t.Error("tar missing root ./ entry")
	}
}

func TestProcessExtraSrcDirsWithSymlink(t *testing.T) {
	srcDir := t.TempDir()
	os.WriteFile(filepath.Join(srcDir, "file.txt"), []byte("hello"), 0o644)
	os.Symlink("file.txt", filepath.Join(srcDir, "link.txt"))

	artifacts, err := source.ProcessExtraSrcDirs([]string{srcDir}, t.TempDir())
	if err != nil {
		t.Fatalf("ProcessExtraSrcDirs failed with symlink: %v", err)
	}

	f, err := os.Open(artifacts[0].Path)
	if err != nil {
		t.Fatalf("opening tar: %v", err)
	}
	defer f.Close()

	tr := tar.NewReader(f)
	foundSymlink := false
	for {
		header, err := tr.Next()
		if err != nil {
			break
		}
		if header.Name == "./link.txt" && header.Typeflag == tar.TypeSymlink {
			foundSymlink = true
			if header.Linkname != "file.txt" {
				t.Errorf("symlink target: expected file.txt, got %s", header.Linkname)
			}
		}
	}
	if !foundSymlink {
		t.Error("tar missing symlink entry")
	}
}

func TestProcessExtraSrcDirsEmpty(t *testing.T) {
	artifacts, err := source.ProcessExtraSrcDirs(nil, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(artifacts) != 0 {
		t.Errorf("expected 0 artifacts, got %d", len(artifacts))
	}
}
