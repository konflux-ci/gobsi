package oci_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/konflux-ci/gobsi/internal/oci"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// diffIDFor returns a layer's diff ID, deliberately different from the digest
// of the blob itself so that tests can tell the two apart. Real images differ
// here too: the diff ID covers the uncompressed layer, the descriptor digest
// covers the blob as stored.
func diffIDFor(blob []byte) digest.Digest {
	return digest.FromString("diff:" + digest.FromBytes(blob).Encoded())
}

// putBlob writes data into dir's blob store and returns its digest.
func putBlob(t *testing.T, dir string, data []byte) digest.Digest {
	t.Helper()
	d := digest.FromBytes(data)
	if err := os.WriteFile(blobFile(dir, d), data, 0o644); err != nil {
		t.Fatalf("writing blob: %v", err)
	}
	return d
}

// writeLayout builds a minimal single-manifest OCI layout containing the given
// layer blobs, and returns its directory.
func writeLayout(t *testing.T, layerBlobs ...[]byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatalf("mkdir blobs: %v", err)
	}

	var layers []ocispec.Descriptor
	var diffIDs []digest.Digest
	for _, b := range layerBlobs {
		d := putBlob(t, dir, b)
		layers = append(layers, ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageLayer, Digest: d, Size: int64(len(b)),
		})
		diffIDs = append(diffIDs, diffIDFor(b))
	}

	cfg, _ := json.Marshal(ocispec.Image{RootFS: ocispec.RootFS{Type: "layers", DiffIDs: diffIDs}})
	cfgDigest := putBlob(t, dir, cfg)
	mf, _ := json.Marshal(ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.Descriptor{MediaType: ocispec.MediaTypeImageConfig, Digest: cfgDigest, Size: int64(len(cfg))},
		Layers:    layers,
	})
	mfDigest := putBlob(t, dir, mf)

	writeIndex(t, dir, ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest, Digest: mfDigest, Size: int64(len(mf)),
	})
	return dir
}

// writeIndex writes dir's index.json pointing at the given manifests.
func writeIndex(t *testing.T, dir string, manifests ...ocispec.Descriptor) {
	t.Helper()
	data, _ := json.Marshal(ocispec.Index{Manifests: manifests})
	if err := os.WriteFile(filepath.Join(dir, "index.json"), data, 0o644); err != nil {
		t.Fatalf("writing index.json: %v", err)
	}
}

// layerTar builds a source layer tar holding one artifact at
// ./blobs/sha256/<name> plus a driver symlink, optionally gzipped.
func layerTar(t *testing.T, name string, content []byte, compress bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	var gzipWriter *gzip.Writer
	tarWriter := tar.NewWriter(&buf)
	if compress {
		gzipWriter = gzip.NewWriter(&buf)
		tarWriter = tar.NewWriter(gzipWriter)
	}

	if name != "" {
		hdr := &tar.Header{
			Name: "./blobs/sha256/" + name, Typeflag: tar.TypeReg,
			Size: int64(len(content)), Mode: 0o644,
		}
		if err := tarWriter.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar header: %v", err)
		}
		if _, err := tarWriter.Write(content); err != nil {
			t.Fatalf("writing tar content: %v", err)
		}
	}
	if err := tarWriter.WriteHeader(&tar.Header{
		Name: "rpm_dir", Typeflag: tar.TypeSymlink, Linkname: "./blobs/sha256", Mode: 0o777,
	}); err != nil {
		t.Fatalf("writing symlink header: %v", err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	if gzipWriter != nil {
		if err := gzipWriter.Close(); err != nil {
			t.Fatalf("closing gzip: %v", err)
		}
	}
	return buf.Bytes()
}

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

	got, err := os.ReadFile(blobFile(dstDir, dgst))
	if err != nil {
		t.Fatalf("reading copied blob: %v", err)
	}
	if !bytes.Equal(got, contents) {
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

	err := oci.CopyBlob(dstDir, src, wrong)
	if err == nil {
		t.Fatal("expected error for digest mismatch, got nil")
	}
	if want := "does not match its digest"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not mention %q", err, want)
	}

	// No blob may be left behind under the advertised digest.
	if _, err := os.Stat(blobFile(dstDir, wrong)); !os.IsNotExist(err) {
		t.Errorf("mismatched blob was written: stat err = %v", err)
	}
}

func TestCopyBlobInvalidDigestRejected(t *testing.T) {
	src := writeBlobFile(t, []byte("contents"))
	dstDir := t.TempDir()

	for _, tc := range []struct {
		name string
		dgst digest.Digest
	}{
		{"empty", ""},
		{"no separator", "notadigest"},
		{"malformed encoding", "sha256:nothex"},
		{"unsupported algorithm", "sha999:deadbeef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := oci.CopyBlob(dstDir, src, tc.dgst)
			if err == nil {
				t.Fatal("expected error for invalid digest, got nil")
			}
			if want := "invalid digest"; !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		})
	}
}

func TestLoadImage(t *testing.T) {
	a := layerTar(t, "aaa", []byte("artifact a"), false)
	b := layerTar(t, "bbb", []byte("artifact b"), false)
	dir := writeLayout(t, a, b)

	img, err := oci.LoadImage(dir)
	if err != nil {
		t.Fatalf("LoadImage: %v", err)
	}
	if img.Dir != dir {
		t.Errorf("Dir: got %q, want %q", img.Dir, dir)
	}
	if len(img.Layers) != 2 {
		t.Fatalf("expected 2 layers, got %d", len(img.Layers))
	}
	for i, want := range [][]byte{a, b} {
		l := img.Layers[i]
		if l.Descriptor.Digest != digest.FromBytes(want) {
			t.Errorf("layer %d digest: got %s, want %s", i, l.Descriptor.Digest, digest.FromBytes(want))
		}
		// The diff ID must come from the config, paired by position with the
		// manifest's layers, not from the layer descriptor itself.
		if l.DiffID != diffIDFor(want) {
			t.Errorf("layer %d diffID: got %s, want %s", i, l.DiffID, diffIDFor(want))
		}
		got, err := os.ReadFile(l.BlobPath)
		if err != nil {
			t.Fatalf("layer %d blob: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("layer %d blob content mismatch", i)
		}
	}
}

func TestLoadImageErrors(t *testing.T) {
	t.Run("missing index.json", func(t *testing.T) {
		_, err := oci.LoadImage(t.TempDir())
		if err == nil {
			t.Fatal("expected error for a missing index.json, got nil")
		}
		if want := "reading index.json"; !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	})

	t.Run("no manifests", func(t *testing.T) {
		dir := writeLayout(t, layerTar(t, "aaa", []byte("a"), false))
		writeIndex(t, dir)
		_, err := oci.LoadImage(dir)
		if err == nil {
			t.Fatal("expected error for an index with no manifests, got nil")
		}
		if want := "no manifests"; !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	})

	t.Run("diff ID count mismatch", func(t *testing.T) {
		dir := writeLayout(t, layerTar(t, "aaa", []byte("a"), false))
		// Replace the config with one that has no diff IDs, leaving the
		// manifest's single layer unmatched.
		var index ocispec.Index
		readJSON(t, filepath.Join(dir, "index.json"), &index)
		var mf ocispec.Manifest
		readJSON(t, blobFile(dir, index.Manifests[0].Digest), &mf)
		cfg, _ := json.Marshal(ocispec.Image{RootFS: ocispec.RootFS{Type: "layers"}})
		mf.Config.Digest = putBlob(t, dir, cfg)
		mf.Config.Size = int64(len(cfg))
		data, _ := json.Marshal(mf)
		writeIndex(t, dir, ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    putBlob(t, dir, data), Size: int64(len(data)),
		})

		_, err := oci.LoadImage(dir)
		if err == nil {
			t.Fatal("expected error for a diff ID count mismatch, got nil")
		}
		if want := "diff ID count"; !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	})

	// index.json's entry points at a nested image index rather than a manifest,
	// which unmarshals as a manifest with an empty config digest. This is how
	// multi-arch binary images are laid out, so it reaches gobsi if one is
	// passed to --merge by mistake.
	t.Run("nested image index", func(t *testing.T) {
		dir := writeLayout(t)
		var index ocispec.Index
		readJSON(t, filepath.Join(dir, "index.json"), &index)
		nested, _ := json.Marshal(ocispec.Index{
			MediaType: ocispec.MediaTypeImageIndex,
			Manifests: []ocispec.Descriptor{index.Manifests[0]},
		})
		writeIndex(t, dir, ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageIndex,
			Digest:    putBlob(t, dir, nested), Size: int64(len(nested)),
		})

		_, err := oci.LoadImage(dir)
		if err == nil {
			t.Fatal("expected error for a nested image index, got nil")
		}
		if want := "invalid digest"; !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	})

	t.Run("empty layer digest", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
			t.Fatalf("mkdir blobs: %v", err)
		}
		cfg, _ := json.Marshal(ocispec.Image{
			RootFS: ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{digest.FromString("x")}},
		})
		mf, _ := json.Marshal(ocispec.Manifest{
			MediaType: ocispec.MediaTypeImageManifest,
			Config:    ocispec.Descriptor{Digest: putBlob(t, dir, cfg), Size: int64(len(cfg))},
			Layers:    []ocispec.Descriptor{{MediaType: ocispec.MediaTypeImageLayer}},
		})
		writeIndex(t, dir, ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    putBlob(t, dir, mf), Size: int64(len(mf)),
		})

		_, err := oci.LoadImage(dir)
		if err == nil {
			t.Fatal("expected error for a layer with an empty digest, got nil")
		}
		if want := "invalid digest"; !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	})
}

func TestInnerArtifactHash(t *testing.T) {
	// The same layer uncompressed and gzipped must yield the same hash: that is
	// what lets a registry-recompressed layer deduplicate against a fresh one.
	for _, tc := range []struct {
		name     string
		compress bool
	}{
		{"uncompressed", false},
		{"gzipped", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := writeBlobFile(t, layerTar(t, "deadbeef", []byte("artifact"), tc.compress))
			got, err := oci.InnerArtifactHash(p)
			if err != nil {
				t.Fatalf("InnerArtifactHash: %v", err)
			}
			if got != "deadbeef" {
				t.Errorf("got %q, want %q", got, "deadbeef")
			}
		})
	}

	// A layer with no inner artifact entry is not a source layer. That is
	// reported as an empty hash rather than an error, so the caller can skip it.
	t.Run("no inner artifact", func(t *testing.T) {
		p := writeBlobFile(t, layerTar(t, "", nil, false))
		got, err := oci.InnerArtifactHash(p)
		if err != nil || got != "" {
			t.Fatalf(`got (%q, %v), want ("", nil)`, got, err)
		}
	})

	t.Run("empty blob", func(t *testing.T) {
		p := writeBlobFile(t, nil)
		got, err := oci.InnerArtifactHash(p)
		if err != nil || got != "" {
			t.Fatalf(`got (%q, %v), want ("", nil)`, got, err)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := oci.InnerArtifactHash(filepath.Join(t.TempDir(), "nope"))
		if !os.IsNotExist(err) {
			t.Fatalf("expected a not-exist error, got %v", err)
		}
	})
}

// readJSON decodes the JSON file at path into v.
func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
}

// blobFile returns the on-disk path of a blob in dir's store.
func blobFile(dir string, dgst digest.Digest) string {
	return filepath.Join(dir, "blobs", "sha256", dgst.Encoded())
}
