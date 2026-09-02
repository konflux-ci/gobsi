package oci_test

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mkosiarc/gobsi/pkg/oci"
	"github.com/mkosiarc/gobsi/pkg/source"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestCreateLayer(t *testing.T) {

	dir := t.TempDir()
	oci.CreateOCIDirectory(dir)

	// Create a test artifact file - fake source rpm
	artifactDir := t.TempDir()
	artifactPath := filepath.Join(artifactDir, "test.src.rpm")
	artifactContent := []byte("fake srpm content")
	os.WriteFile(artifactPath, artifactContent, 0o644)

	checksum := fmt.Sprintf("%x", sha256.Sum256(artifactContent))
	artifact := source.Artifact{
		Path:       artifactPath,
		DriverName: "rpm_dir",
		Metadata: source.ArtifactMetadata{
			Name:     "test",
			Checksum: checksum,
		},
	}

	layerInfo, err := oci.CreateLayer(dir, artifact)
	if err != nil {
		t.Fatalf("CreateLayer failed: %v", err)
	}

	// Descriptor should have correct mediaType
	if layerInfo.Descriptor.MediaType != ocispec.MediaTypeImageLayer {
		t.Errorf("expected mediaType %s, got %s", ocispec.MediaTypeImageLayer, layerInfo.Descriptor.MediaType)
	}

	// Blob file should exist
	blobHash := strings.TrimPrefix(string(layerInfo.Descriptor.Digest), "sha256:")
	blobPath := filepath.Join(dir, "blobs", "sha256", blobHash)
	if _, err := os.Stat(blobPath); err != nil {
		t.Fatalf("layer blob missing: %v", err)
	}

	// Extract the tar and verify internal structure
	blobData, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("reading blob: %v", err)
	}
	tr := tar.NewReader(bytes.NewReader(blobData))

	foundBlob := false
	foundSymlink := false

	for {
		header, err := tr.Next()
		if err != nil {
			break
		}

		if strings.HasPrefix(header.Name, "./blobs/sha256/") && header.Typeflag == tar.TypeReg {
			foundBlob = true
		}

		if header.Name == "./rpm_dir/test.src.rpm" && header.Typeflag == tar.TypeSymlink {
			foundSymlink = true
			expectedTarget := filepath.Join("..", "blobs", "sha256", fmt.Sprintf("%x", sha256.Sum256(artifactContent)))
			if header.Linkname != expectedTarget {
				t.Errorf("symlink target: expected %s, got %s", expectedTarget, header.Linkname)
			}
		}

	}

	if !foundBlob {
		t.Error("tar missing blob entry")
	}
	if !foundSymlink {
		t.Error("tar missing symlink entry")
	}

	// 	Annotations should include checksum
	checksumAnnotation := layerInfo.Descriptor.Annotations["source.artifact.filename.checksum"]
	expectedChecksum := fmt.Sprintf("sha256:%x", sha256.Sum256(artifactContent))

	if checksumAnnotation != expectedChecksum {
		t.Errorf("expected checksum annotation %s, got %s", expectedChecksum, checksumAnnotation)
	}

	// DiffID should be set
	if layerInfo.DiffID == "" {
		t.Error("DiffID is empty")
	}
}
