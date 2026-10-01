package oci_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/konflux-ci/gobsi/pkg/oci"
	"github.com/opencontainers/go-digest"
)

// writeBlobFile writes contents to a temp file and returns its path.
func writeBlobFile(t *testing.T, contents []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(p, contents, 0o644); err != nil {
		t.Fatalf("writing blob file: %v", err)
	}
	return p
}

func TestCopyBlobMatchingDigest(t *testing.T) {
	contents := []byte("source blob contents")
	src := writeBlobFile(t, contents)
	dgst := digest.FromBytes(contents)

	dstDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dstDir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatalf("mkdir blobs: %v", err)
	}

	if err := oci.CopyBlob(dstDir, src, dgst); err != nil {
		t.Fatalf("CopyBlob: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dstDir, "blobs", "sha256", dgst.Encoded()))
	if err != nil {
		t.Fatalf("reading copied blob: %v", err)
	}
	if string(got) != string(contents) {
		t.Errorf("copied blob mismatch: got %q want %q", got, contents)
	}
}

func TestCopyBlobMismatchedDigestRejected(t *testing.T) {
	src := writeBlobFile(t, []byte("actual contents"))
	// A well-formed digest that does NOT match the source bytes.
	wrong := digest.FromBytes([]byte("different contents"))

	dstDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dstDir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatalf("mkdir blobs: %v", err)
	}

	if err := oci.CopyBlob(dstDir, src, wrong); err == nil {
		t.Fatal("expected error for digest mismatch, got nil")
	}

	// No blob may be left behind under the advertised digest.
	if _, err := os.Stat(filepath.Join(dstDir, "blobs", "sha256", wrong.Encoded())); !os.IsNotExist(err) {
		t.Errorf("mismatched blob was written: stat err = %v", err)
	}
}
