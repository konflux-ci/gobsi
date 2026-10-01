package gobsi_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/konflux-ci/gobsi/pkg/gobsi"
	"github.com/konflux-ci/gobsi/pkg/oci"
)

// writeDir creates a temp directory containing a single file with the given
// contents, to serve as an extra-source directory.
func writeDir(t *testing.T, name, contents string) string {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, name), []byte(contents), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return d
}

// layerDigests returns the layer digests of the image built at dir.
func layerDigests(t *testing.T, dir string) []string {
	t.Helper()
	img, err := oci.LoadImage(dir)
	if err != nil {
		t.Fatalf("loading image %s: %v", dir, err)
	}
	var digests []string
	for _, l := range img.Layers {
		digests = append(digests, l.Descriptor.Digest.String())
	}
	return digests
}

func TestMergeDeduplicatesSharedSource(t *testing.T) {
	x := writeDir(t, "x.txt", "x contents")
	y := writeDir(t, "y.txt", "y contents")

	// Parent image contains source X.
	parent := t.TempDir()
	if err := gobsi.BuildSourceImage(gobsi.BuildConfig{ExtraDirs: []string{x}, OutputDir: parent}); err != nil {
		t.Fatalf("building parent: %v", err)
	}

	// New build contains X (a duplicate of the parent's) and Y, then merges the
	// parent. X must appear only once; Y and the parent's non-shared content
	// must survive.
	out := t.TempDir()
	err := gobsi.BuildSourceImage(gobsi.BuildConfig{
		ExtraDirs: []string{x, y},
		MergeDirs: []string{parent},
		OutputDir: out,
	})
	if err != nil {
		t.Fatalf("building merged image: %v", err)
	}

	digests := layerDigests(t, out)
	if len(digests) != 2 {
		t.Fatalf("expected 2 deduplicated layers, got %d: %v", len(digests), digests)
	}

	// No layer digest may repeat.
	seen := map[string]bool{}
	for _, d := range digests {
		if seen[d] {
			t.Errorf("duplicate layer digest in output: %s", d)
		}
		seen[d] = true
	}
}

func TestMergeOnlyCombinesParents(t *testing.T) {
	x := writeDir(t, "x.txt", "x contents")
	y := writeDir(t, "y.txt", "y contents")

	// Two parents that overlap on X.
	p1 := t.TempDir()
	if err := gobsi.BuildSourceImage(gobsi.BuildConfig{ExtraDirs: []string{x, y}, OutputDir: p1}); err != nil {
		t.Fatalf("building parent 1: %v", err)
	}
	p2 := t.TempDir()
	if err := gobsi.BuildSourceImage(gobsi.BuildConfig{ExtraDirs: []string{x}, OutputDir: p2}); err != nil {
		t.Fatalf("building parent 2: %v", err)
	}

	// Merge both parents into a fresh output with no freshly built sources.
	out := t.TempDir()
	err := gobsi.BuildSourceImage(gobsi.BuildConfig{
		MergeDirs: []string{p1, p2},
		OutputDir: out,
	})
	if err != nil {
		t.Fatalf("building merge-only image: %v", err)
	}

	// p1 contributes X and Y; p2's X is a duplicate and dropped.
	if digests := layerDigests(t, out); len(digests) != 2 {
		t.Fatalf("expected 2 layers, got %d: %v", len(digests), digests)
	}

	// Every merged layer blob must actually exist in the output blob store.
	img, err := oci.LoadImage(out)
	if err != nil {
		t.Fatalf("loading output: %v", err)
	}
	for _, l := range img.Layers {
		if _, err := os.Stat(l.BlobPath); err != nil {
			t.Errorf("merged layer blob missing: %v", err)
		}
	}
}

func TestMergeNoInputIsError(t *testing.T) {
	if err := gobsi.BuildSourceImage(gobsi.BuildConfig{OutputDir: t.TempDir()}); err == nil {
		t.Error("expected error when no input is provided")
	}
}

// TestMergeSelfIsError guards against merging an output image into itself, which
// would otherwise truncate its own content-addressed layer blobs.
func TestMergeSelfIsError(t *testing.T) {
	x := writeDir(t, "x.txt", "x contents")

	// Build a valid image at out.
	out := t.TempDir()
	if err := gobsi.BuildSourceImage(gobsi.BuildConfig{ExtraDirs: []string{x}, OutputDir: out}); err != nil {
		t.Fatalf("building image: %v", err)
	}

	// Record the layer blob contents so we can prove they survive the attempt.
	before := layerDigests(t, out)

	// Merging out into itself must error, not silently corrupt the image.
	err := gobsi.BuildSourceImage(gobsi.BuildConfig{
		MergeDirs: []string{out},
		OutputDir: out,
	})
	if err == nil {
		t.Fatal("expected error when merging the output directory into itself")
	}

	// The original layers must be intact and non-empty.
	img, err := oci.LoadImage(out)
	if err != nil {
		t.Fatalf("loading image after failed self-merge: %v", err)
	}
	if got := len(img.Layers); got != len(before) {
		t.Fatalf("layer count changed after failed self-merge: before=%d after=%d", len(before), got)
	}
	for _, l := range img.Layers {
		info, err := os.Stat(l.BlobPath)
		if err != nil {
			t.Fatalf("layer blob missing after failed self-merge: %v", err)
		}
		if info.Size() == 0 {
			t.Errorf("layer blob truncated to empty: %s", l.BlobPath)
		}
	}
}
