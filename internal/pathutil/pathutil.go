package pathutil

import (
	"path/filepath"
	"slices"
	"strings"
)

// Segments splits a path into its components, ignoring a leading "./" and any
// trailing slash, so directory and file paths compare consistently.
func Segments(p string) []string {
	p = filepath.Clean(p)
	sep := string(filepath.Separator)
	if p == "." || p == sep {
		return nil
	}
	return strings.Split(strings.TrimPrefix(p, sep), sep)
}

// Less reports whether path a sorts before path b when compared
// segment-by-segment. This matches the depth-first order GNU tar produces when
// walking a filesystem: a parent directory sorts before its contents, and
// siblings sort lexically. Plain string comparison gets this wrong because the
// path separator '/' (0x2F) sorts after characters like '.' (0x2E), which can
// wedge a sibling file between a directory and its contents.
func Less(a, b string) bool {
	return slices.Compare(Segments(a), Segments(b)) < 0
}
