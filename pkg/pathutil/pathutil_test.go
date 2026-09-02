package pathutil_test

import (
	"sort"
	"testing"

	"github.com/mkosiarc/gobsi/pkg/pathutil"
)

func TestLessSegmentOrder(t *testing.T) {
	// A sibling file whose name sorts lower than '/' must not wedge between a
	// directory and its contents. Plain string sort puts "subdir.tar" before
	// "subdir/c.txt" because '.' (0x2E) < '/' (0x2F); segment sort keeps the
	// directory's contents grouped.
	in := []string{
		"subdir/c.txt",
		"subdir.tar",
		"subdir",
		"a.txt",
	}
	sort.Slice(in, func(i, j int) bool { return pathutil.Less(in[i], in[j]) })

	want := []string{"a.txt", "subdir", "subdir/c.txt", "subdir.tar"}
	for i := range want {
		if in[i] != want[i] {
			t.Fatalf("index %d: got %q, want %q (full: %v)", i, in[i], want[i], in)
		}
	}
}

func TestLessDepthFirst(t *testing.T) {
	in := []string{
		"./extra_src_dir/link.tar",
		"./blobs/sha256/abc",
		"./extra_src_dir/",
		"./",
		"./blobs/",
		"./blobs/sha256/",
	}
	sort.Slice(in, func(i, j int) bool { return pathutil.Less(in[i], in[j]) })

	want := []string{
		"./",
		"./blobs/",
		"./blobs/sha256/",
		"./blobs/sha256/abc",
		"./extra_src_dir/",
		"./extra_src_dir/link.tar",
	}
	for i := range want {
		if in[i] != want[i] {
			t.Fatalf("index %d: got %q, want %q (full: %v)", i, in[i], want[i], in)
		}
	}
}

func TestLessRootSortsFirst(t *testing.T) {
	// The root "./" must always sort first, even before files whose names begin
	// with a byte lower than '.' (0x2E), e.g. '!' (0x21) or '#' (0x23). This
	// works because Segments("./") is empty, so the root has no segment to lose a
	// comparison on. It matches GNU tar's depth-first order (parent before its
	// contents).
	for _, name := range []string{"./!important.txt", "./#notes", "./readme.txt"} {
		if !pathutil.Less("./", name) {
			t.Errorf("Less(%q, %q) = false, want true (root must sort first)", "./", name)
		}
	}
}

func TestSegments(t *testing.T) {
	cases := map[string][]string{
		"./":              nil,
		"./blobs/":        {"blobs"},
		"./blobs/sha256/": {"blobs", "sha256"},
		"a.txt":           {"a.txt"},
		"subdir/c.txt":    {"subdir", "c.txt"},
	}
	for in, want := range cases {
		got := pathutil.Segments(in)
		if len(got) != len(want) {
			t.Errorf("Segments(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("Segments(%q) = %v, want %v", in, got, want)
				break
			}
		}
	}
}
