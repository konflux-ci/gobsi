package oci_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mkosiarc/gobsi/pkg/oci"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestCreateOCIDirectory(t *testing.T) {
	dir := t.TempDir()

	if err := oci.CreateOCIDirectory(dir); err != nil {
		t.Fatalf("CreateOCIDirectory failed: %v", err)
	}

	// oci-layout file should exist and contain correct version
	data, err := os.ReadFile(filepath.Join(dir, "oci-layout"))
	if err != nil {
		t.Fatalf("reading oci-layout: %v", err)
	}

	var ociLayoutFileContents ocispec.ImageLayout
	if err := json.Unmarshal(data, &ociLayoutFileContents); err != nil {
		t.Fatalf("parsing oci-layout: %v", err)
	}

	if ociLayoutFileContents.Version != ocispec.ImageLayoutVersion {
		t.Errorf("expected version %s, got %s", ocispec.ImageLayoutVersion, ociLayoutFileContents.Version)
	}

	// blobs/sha256/ directory should exist
	info, err := os.Stat(filepath.Join(dir, "blobs", "sha256"))
	if err != nil {
		t.Fatalf("blobs/sha256 missing: %v", err)
	}
	if !info.IsDir() {
		t.Error("blobs/sha256 is not a directory")
	}

}

func TestSaveBlob(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755)

	content := []byte("hello world")
	digest, size, err := oci.SaveBlob(dir, content)
	if err != nil {
		t.Fatalf("SaveBlob failed: %v", err)
	}

	if size != int64(len(content)) {
		t.Errorf("expected size %d, got %d", len(content), size)
	}

	expectedHash := sha256.Sum256(content)
	expectedDigest := fmt.Sprintf("sha256:%x", expectedHash)
	if digest != expectedDigest {
		t.Errorf("expected digest %s, got %s", expectedDigest, digest)
	}

	// blob file should exist with correct content
	hash := strings.TrimPrefix(digest, "sha256:")
	blobData, err := os.ReadFile(filepath.Join(dir, "blobs", "sha256", hash))
	if err != nil {
		t.Fatalf("reading blob: %v", err)
	}
	if string(blobData) != "hello world" {
		t.Errorf("blob content mismatch")
	}
}

func TestSaveIndex(t *testing.T) {
	dir := t.TempDir()

	fakeDigest := "sha256:abc123"
	fakeSize := int64(100)
	err := oci.SaveIndex(dir, fakeDigest, fakeSize)

	if err != nil {
		t.Fatalf("SaveIndex failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "index.json"))
	if err != nil {
		t.Fatalf("reading index.json: %v", err)
	}

	var index ocispec.Index
	if err := json.Unmarshal(data, &index); err != nil {
		t.Fatalf("parsing index.json: %v", err)
	}

	if len(index.Manifests) != 1 {
		t.Fatalf("expected 1 manifest, got %d", len(index.Manifests))
	}

	manifest := index.Manifests[0]
	if manifest.Digest.String() != fakeDigest {
		t.Errorf("expected digest %s, got %s", fakeDigest, manifest.Digest)
	}
	if manifest.Size != fakeSize {
		t.Errorf("expected size %d, got %d", fakeSize, manifest.Size)
	}
	if manifest.Annotations["com.redhat.image.type"] != "source" {
		t.Error("missing image type annotation")
	}
	if manifest.Annotations["org.opencontainers.image.ref.name"] != "latest-source" {
		t.Error("missing ref name annotation")
	}
}

func TestNewImage(t *testing.T) {
	image := oci.NewConfig()

	if image.Architecture != "amd64" {
		t.Errorf("expected architecture amd64, got %s", image.Architecture)
	}
	if image.OS != "linux" {
		t.Errorf("expected OS linux, got %s", image.OS)
	}
	if image.RootFS.Type != "layers" {
		t.Errorf("expected rootfs type layers, got %s", image.RootFS.Type)
	}
}

func TestNewManifest(t *testing.T) {
	manifest := oci.NewManifest()

	if manifest.SchemaVersion != 2 {
		t.Errorf("expected schemaVersion 2, got %d", manifest.SchemaVersion)
	}
	if manifest.MediaType != ocispec.MediaTypeImageManifest {
		t.Errorf("expected mediaType %s, got %s", ocispec.MediaTypeImageManifest, manifest.MediaType)
	}
}
