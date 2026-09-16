package integration_test

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/konflux-ci/gobsi/pkg/gobsi"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func buildTestSRPM(t *testing.T) string {
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

	srcDir := filepath.Join(topDir, "SOURCES", "testpkg-1.0")
	os.MkdirAll(srcDir, 0o755)
	os.WriteFile(filepath.Join(srcDir, "README"), []byte("test"), 0o644)

	tarCmd := exec.Command("tar", "czf",
		filepath.Join(topDir, "SOURCES", "testpkg-1.0.tar.gz"),
		"-C", filepath.Join(topDir, "SOURCES"),
		"testpkg-1.0",
	)
	if out, err := tarCmd.CombinedOutput(); err != nil {
		t.Fatalf("creating source tarball: %v\n%s", err, out)
	}

	spec := `Name: testpkg
Version: 1.0
Release: 1
Summary: Test package
License: MIT
Source0: testpkg-1.0.tar.gz

%description
Test
`
	specPath := filepath.Join(topDir, "SPECS", "testpkg.spec")
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
