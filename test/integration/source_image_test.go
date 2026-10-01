package integration_test

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/konflux-ci/gobsi/pkg/gobsi"
	"github.com/konflux-ci/gobsi/pkg/oci"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// buildTestSRPM builds a default "testpkg" SRPM and returns the directory
// containing it.
func buildTestSRPM(t *testing.T) string {
	return buildTestSRPMNamed(t, "testpkg", "1.0")
}

// buildTestSRPMNamed builds an SRPM for the given package name and version and
// returns the directory containing it. Distinct names/versions yield distinct
// SRPMs (and thus distinct inner artifact hashes), which the merge tests rely
// on.
func buildTestSRPMNamed(t *testing.T, name, version string) string {
	t.Helper()

	for _, tool := range []string{"rpmbuild", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available, skipping integration test", tool)
		}
	}

	topDir := t.TempDir()
	for _, sub := range []string{"SOURCES", "SPECS", "SRPMS", "BUILD", "RPMS"} {
		os.MkdirAll(filepath.Join(topDir, sub), 0o755)
	}

	srcName := fmt.Sprintf("%s-%s", name, version)
	srcDir := filepath.Join(topDir, "SOURCES", srcName)
	os.MkdirAll(srcDir, 0o755)
	os.WriteFile(filepath.Join(srcDir, "README"), []byte("test"), 0o644)

	tarCmd := exec.Command("tar", "czf",
		filepath.Join(topDir, "SOURCES", srcName+".tar.gz"),
		"-C", filepath.Join(topDir, "SOURCES"),
		srcName,
	)
	if out, err := tarCmd.CombinedOutput(); err != nil {
		t.Fatalf("creating source tarball: %v\n%s", err, out)
	}

	spec := fmt.Sprintf(`Name: %s
Version: %s
Release: 1
Summary: Test package
License: MIT
Source0: %s.tar.gz

%%description
Test
`, name, version, srcName)
	specPath := filepath.Join(topDir, "SPECS", name+".spec")
	os.WriteFile(specPath, []byte(spec), 0o644)

	rpmbuild := exec.Command("rpmbuild",
		"--define", fmt.Sprintf("_topdir %s", topDir),
		"-bs", specPath,
	)
	if out, err := rpmbuild.CombinedOutput(); err != nil {
		t.Fatalf("rpmbuild failed: %v\n%s", err, out)
	}

	matches, _ := filepath.Glob(filepath.Join(topDir, "SRPMS", "*.src.rpm"))
	if len(matches) == 0 {
		t.Fatal("rpmbuild produced no SRPM")
	}

	return filepath.Dir(matches[0])
}

func TestEndToEnd(t *testing.T) {
	srpmDir := buildTestSRPM(t)

	extraDir := t.TempDir()
	os.WriteFile(filepath.Join(extraDir, "extra-file.txt"), []byte("extra content"), 0o644)

	outputDir := t.TempDir()

	err := gobsi.BuildSourceImage(gobsi.BuildConfig{
		SRPMDir:   srpmDir,
		ExtraDirs: []string{extraDir},
		OutputDir: outputDir,
	})
	if err != nil {
		t.Fatalf("BuildSourceImage: %v", err)
	}

	// Validate oci-layout
	layoutData, err := os.ReadFile(filepath.Join(outputDir, "oci-layout"))
	if err != nil {
		t.Fatalf("reading oci-layout: %v", err)
	}
	var layout ocispec.ImageLayout
	if err := json.Unmarshal(layoutData, &layout); err != nil {
		t.Fatalf("parsing oci-layout: %v", err)
	}
	if layout.Version != ocispec.ImageLayoutVersion {
		t.Errorf("oci-layout version: expected %s, got %s", ocispec.ImageLayoutVersion, layout.Version)
	}

	// Validate index.json
	indexData, err := os.ReadFile(filepath.Join(outputDir, "index.json"))
	if err != nil {
		t.Fatalf("reading index.json: %v", err)
	}
	var index ocispec.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		t.Fatalf("parsing index.json: %v", err)
	}
	if len(index.Manifests) != 1 {
		t.Fatalf("expected 1 manifest in index, got %d", len(index.Manifests))
	}

	// Validate manifest blob
	manifestDesc := index.Manifests[0]
	manifestBlobPath := filepath.Join(outputDir, "blobs", "sha256", manifestDesc.Digest.Encoded())
	manifestBlobData, err := os.ReadFile(manifestBlobPath)
	if err != nil {
		t.Fatalf("reading manifest blob: %v", err)
	}
	var savedManifest ocispec.Manifest
	if err := json.Unmarshal(manifestBlobData, &savedManifest); err != nil {
		t.Fatalf("parsing manifest: %v", err)
	}

	expectedLayers := 2
	if len(savedManifest.Layers) != expectedLayers {
		t.Errorf("expected %d layers, got %d", expectedLayers, len(savedManifest.Layers))
	}

	// Validate config blob
	configBlobPath := filepath.Join(outputDir, "blobs", "sha256", savedManifest.Config.Digest.Encoded())
	configBlobData, err := os.ReadFile(configBlobPath)
	if err != nil {
		t.Fatalf("reading config blob: %v", err)
	}
	var savedConfig ocispec.Image
	if err := json.Unmarshal(configBlobData, &savedConfig); err != nil {
		t.Fatalf("parsing config: %v", err)
	}
	if len(savedConfig.RootFS.DiffIDs) != expectedLayers {
		t.Errorf("expected %d diffIDs, got %d", expectedLayers, len(savedConfig.RootFS.DiffIDs))
	}

	// Validate layer blobs exist and have expected annotations
	for i, layerDesc := range savedManifest.Layers {
		blobPath := filepath.Join(outputDir, "blobs", "sha256", layerDesc.Digest.Encoded())
		if _, err := os.Stat(blobPath); err != nil {
			t.Errorf("layer %d blob missing: %v", i, err)
			continue
		}

		if _, ok := layerDesc.Annotations["source.artifact.filename.checksum"]; !ok {
			t.Errorf("layer %d missing source.artifact.filename.checksum annotation", i)
		}

		if layerDesc.MediaType != ocispec.MediaTypeImageLayer {
			t.Errorf("layer %d mediaType: expected %s, got %s", i, ocispec.MediaTypeImageLayer, layerDesc.MediaType)
		}
	}

	// Validate SRPM layer annotations
	srpmLayer := savedManifest.Layers[0]
	for _, key := range []string{
		"source.artifact.filename",
		"source.artifact.name",
		"source.artifact.version",
		"source.artifact.release",
		"source.artifact.license",
	} {
		if v, ok := srpmLayer.Annotations[key]; !ok || v == "" {
			t.Errorf("SRPM layer missing annotation %s", key)
		}
	}
	if srpmLayer.Annotations["source.artifact.name"] != "testpkg" {
		t.Errorf("SRPM name: expected testpkg, got %s", srpmLayer.Annotations["source.artifact.name"])
	}
	if srpmLayer.Annotations["source.artifact.version"] != "1.0" {
		t.Errorf("SRPM version: expected 1.0, got %s", srpmLayer.Annotations["source.artifact.version"])
	}

	// Validate extra source layer annotations
	extraLayer := savedManifest.Layers[1]
	extraName := extraLayer.Annotations["source.artifact.name"]
	if !strings.HasPrefix(extraName, "extra-src-") || !strings.HasSuffix(extraName, ".tar") {
		t.Errorf("extra source name: expected extra-src-<checksum>.tar, got %s", extraName)
	}
	if extraLayer.Annotations["source.artifact.mimetype"] != "application/x-tar" {
		t.Errorf("extra source mimetype: expected application/x-tar, got %s", extraLayer.Annotations["source.artifact.mimetype"])
	}

	// Validate layer tar internal structure has blob + symlink
	for i, layerDesc := range savedManifest.Layers {
		blobPath := filepath.Join(outputDir, "blobs", "sha256", layerDesc.Digest.Encoded())
		blobData, err := os.ReadFile(blobPath)
		if err != nil {
			continue
		}

		tr := tar.NewReader(strings.NewReader(string(blobData)))
		hasBlob := false
		hasSymlink := false
		for {
			header, err := tr.Next()
			if err != nil {
				break
			}
			if strings.HasPrefix(header.Name, "./blobs/sha256/") && header.Typeflag == tar.TypeReg {
				hasBlob = true
			}
			if header.Typeflag == tar.TypeSymlink {
				hasSymlink = true
			}
		}
		if !hasBlob {
			t.Errorf("layer %d tar missing blob entry", i)
		}
		if !hasSymlink {
			t.Errorf("layer %d tar missing symlink entry", i)
		}
	}
}

// readManifest reads the single image manifest of the OCI layout at dir.
func readManifest(t *testing.T, dir string) ocispec.Manifest {
	t.Helper()
	indexData, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatalf("reading index.json: %v", err)
	}
	var index ocispec.Index
	if err := json.Unmarshal(indexData, &index); err != nil {
		t.Fatalf("parsing index.json: %v", err)
	}
	if len(index.Manifests) != 1 {
		t.Fatalf("expected 1 manifest, got %d", len(index.Manifests))
	}
	blob, err := os.ReadFile(filepath.Join(dir, "blobs", "sha256", index.Manifests[0].Digest.Encoded()))
	if err != nil {
		t.Fatalf("reading manifest blob: %v", err)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatalf("parsing manifest: %v", err)
	}
	return m
}

// assertNoDuplicateLayers fails if any layer digest repeats, and checks that
// every layer blob referenced by the manifest actually exists on disk.
func assertNoDuplicateLayers(t *testing.T, dir string, m ocispec.Manifest) {
	t.Helper()
	seen := map[string]bool{}
	for _, l := range m.Layers {
		if seen[l.Digest.String()] {
			t.Errorf("duplicate layer digest in %s: %s", dir, l.Digest)
		}
		seen[l.Digest.String()] = true
		if _, err := os.Stat(filepath.Join(dir, "blobs", "sha256", l.Digest.Encoded())); err != nil {
			t.Errorf("layer blob missing in %s: %v", dir, err)
		}
	}
}

// TestMergeDeduplicatesSRPM verifies that merging a parent image that shares an
// SRPM with the fresh build collapses the duplicate: the shared SRPM appears
// once, while non-shared content (the extra source) survives.
func TestMergeDeduplicatesSRPM(t *testing.T) {
	srpmDir := buildTestSRPM(t)

	// Parent image: the same SRPM only.
	parent := t.TempDir()
	if err := gobsi.BuildSourceImage(gobsi.BuildConfig{SRPMDir: srpmDir, OutputDir: parent}); err != nil {
		t.Fatalf("building parent: %v", err)
	}
	if got := len(readManifest(t, parent).Layers); got != 1 {
		t.Fatalf("expected parent to have 1 layer, got %d", got)
	}

	// Fresh build: same SRPM plus an extra source, merging the parent.
	extraDir := t.TempDir()
	os.WriteFile(filepath.Join(extraDir, "extra-file.txt"), []byte("extra content"), 0o644)

	out := t.TempDir()
	err := gobsi.BuildSourceImage(gobsi.BuildConfig{
		SRPMDir:   srpmDir,
		ExtraDirs: []string{extraDir},
		MergeDirs: []string{parent},
		OutputDir: out,
	})
	if err != nil {
		t.Fatalf("building merged image: %v", err)
	}

	m := readManifest(t, out)
	if len(m.Layers) != 2 {
		t.Fatalf("expected 2 layers (deduped SRPM + extra), got %d", len(m.Layers))
	}
	assertNoDuplicateLayers(t, out, m)
}

// TestMergeCombinesDistinctParents verifies that a merge-only build (no fresh
// sources) combines the layers of multiple parents, deduplicating the SRPM they
// share while keeping the ones they don't.
func TestMergeCombinesDistinctParents(t *testing.T) {
	srpmA := buildTestSRPMNamed(t, "pkg-a", "1.0")
	srpmB := buildTestSRPMNamed(t, "pkg-b", "2.0")

	// p1 has A and B; p2 has A only. A must be deduplicated.
	combined := t.TempDir()
	copyFile(t, firstSRPM(t, srpmA), filepath.Join(combined, "a.src.rpm"))
	copyFile(t, firstSRPM(t, srpmB), filepath.Join(combined, "b.src.rpm"))
	p1 := t.TempDir()
	if err := gobsi.BuildSourceImage(gobsi.BuildConfig{SRPMDir: combined, OutputDir: p1}); err != nil {
		t.Fatalf("building parent 1: %v", err)
	}
	if got := len(readManifest(t, p1).Layers); got != 2 {
		t.Fatalf("expected parent 1 to have 2 layers, got %d", got)
	}
	p2 := t.TempDir()
	if err := gobsi.BuildSourceImage(gobsi.BuildConfig{SRPMDir: srpmA, OutputDir: p2}); err != nil {
		t.Fatalf("building parent 2: %v", err)
	}

	out := t.TempDir()
	err := gobsi.BuildSourceImage(gobsi.BuildConfig{
		MergeDirs: []string{p1, p2},
		OutputDir: out,
	})
	if err != nil {
		t.Fatalf("building merge-only image: %v", err)
	}

	m := readManifest(t, out)
	if len(m.Layers) != 2 {
		t.Fatalf("expected 2 layers (A + B, A's duplicate dropped), got %d", len(m.Layers))
	}
	assertNoDuplicateLayers(t, out, m)
}

// TestMergeGzippedLayerDeduplicates covers merging a source image whose layers
// are gzip-compressed, regardless of what produced them — a registry round-trip
// (e.g. skopeo recompressing on copy), or any tool that emits "+gzip" layers.
func TestMergeGzippedLayerDeduplicates(t *testing.T) {
	srpmDir := buildTestSRPM(t)

	// A normal gobsi image, then a gzipped clone of it.
	gobsiImg := t.TempDir()
	if err := gobsi.BuildSourceImage(gobsi.BuildConfig{SRPMDir: srpmDir, OutputDir: gobsiImg}); err != nil {
		t.Fatalf("building gobsi image: %v", err)
	}
	gzImg := gzipImage(t, gobsiImg)

	// Sanity: the gzipped layer really is gzip-compressed, and its inner
	// artifact hash matches the uncompressed original.
	gzManifest := readManifest(t, gzImg)
	if len(gzManifest.Layers) != 1 {
		t.Fatalf("expected gzipped image to have 1 layer, got %d", len(gzManifest.Layers))
	}
	if mt := gzManifest.Layers[0].MediaType; !strings.Contains(mt, "gzip") {
		t.Fatalf("expected gzip layer mediaType, got %s", mt)
	}
	gzInner, err := oci.InnerArtifactHash(
		filepath.Join(gzImg, "blobs", "sha256", gzManifest.Layers[0].Digest.Encoded()),
		gzManifest.Layers[0].MediaType,
	)
	if err != nil {
		t.Fatalf("reading gzip inner hash: %v", err)
	}
	origInner, err := oci.InnerArtifactHash(
		filepath.Join(gobsiImg, "blobs", "sha256", readManifest(t, gobsiImg).Layers[0].Digest.Encoded()),
		readManifest(t, gobsiImg).Layers[0].MediaType,
	)
	if err != nil {
		t.Fatalf("reading original inner hash: %v", err)
	}
	if gzInner == "" || gzInner != origInner {
		t.Fatalf("cross-format inner hash mismatch: gzip=%q original=%q", gzInner, origInner)
	}

	// Build fresh from the same SRPM, merging the gzipped image: the gzipped
	// layer must dedup against the fresh uncompressed one.
	out := t.TempDir()
	err = gobsi.BuildSourceImage(gobsi.BuildConfig{
		SRPMDir:   srpmDir,
		MergeDirs: []string{gzImg},
		OutputDir: out,
	})
	if err != nil {
		t.Fatalf("building merged image: %v", err)
	}

	m := readManifest(t, out)
	if len(m.Layers) != 1 {
		t.Fatalf("expected 1 layer (gzip duplicate dropped), got %d", len(m.Layers))
	}
	assertNoDuplicateLayers(t, out, m)
}

// firstSRPM returns the path to the single *.src.rpm in dir.
func firstSRPM(t *testing.T, dir string) string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, "*.src.rpm"))
	if len(matches) == 0 {
		t.Fatalf("no SRPM found in %s", dir)
	}
	return matches[0]
}

// copyFile copies src to dst.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("writing %s: %v", dst, err)
	}
}

// gzipImage builds a new OCI image layout that is a copy of the one at srcDir
// with every layer re-encoded as a gzip-compressed layer, mimicking a source
// image whose layers arrived gzip-compressed (e.g. after a registry round-trip).
// Diff IDs (uncompressed digests) are preserved. It returns the new directory.
func gzipImage(t *testing.T, srcDir string) string {
	t.Helper()

	src, err := oci.LoadImage(srcDir)
	if err != nil {
		t.Fatalf("loading source image: %v", err)
	}

	dst := t.TempDir()
	blobDir := filepath.Join(dst, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatalf("mkdir blobs: %v", err)
	}
	writeBlob := func(data []byte) digest.Digest {
		d := digest.FromBytes(data)
		if err := os.WriteFile(filepath.Join(blobDir, d.Encoded()), data, 0o644); err != nil {
			t.Fatalf("writing blob: %v", err)
		}
		return d
	}

	var layers []ocispec.Descriptor
	var diffIDs []digest.Digest
	for _, l := range src.Layers {
		raw, err := os.ReadFile(l.BlobPath)
		if err != nil {
			t.Fatalf("reading layer blob: %v", err)
		}
		var buf strings.Builder
		gz := gzip.NewWriter(&buf)
		if _, err := gz.Write(raw); err != nil {
			t.Fatalf("gzipping layer: %v", err)
		}
		if err := gz.Close(); err != nil {
			t.Fatalf("closing gzip: %v", err)
		}
		gzData := []byte(buf.String())
		dgst := writeBlob(gzData)

		desc := l.Descriptor
		desc.MediaType = ocispec.MediaTypeImageLayerGzip
		desc.Digest = dgst
		desc.Size = int64(len(gzData))
		layers = append(layers, desc)
		diffIDs = append(diffIDs, l.DiffID)
	}

	config := ocispec.Image{
		Platform: ocispec.Platform{OS: "linux", Architecture: "amd64"},
		RootFS:   ocispec.RootFS{Type: "layers", DiffIDs: diffIDs},
	}
	configData, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshaling config: %v", err)
	}
	configDigest := writeBlob(configData)

	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config: ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageConfig,
			Digest:    configDigest,
			Size:      int64(len(configData)),
		},
		Layers: layers,
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshaling manifest: %v", err)
	}
	manifestDigest := writeBlob(manifestData)

	index := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    manifestDigest,
			Size:      int64(len(manifestData)),
		}},
	}
	indexData, err := json.Marshal(index)
	if err != nil {
		t.Fatalf("marshaling index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dst, "index.json"), indexData, 0o644); err != nil {
		t.Fatalf("writing index.json: %v", err)
	}

	layoutData, err := json.Marshal(ocispec.ImageLayout{Version: ocispec.ImageLayoutVersion})
	if err != nil {
		t.Fatalf("marshaling oci-layout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dst, "oci-layout"), layoutData, 0o644); err != nil {
		t.Fatalf("writing oci-layout: %v", err)
	}

	return dst
}
