package oci

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// CreateOCIDirectory creates a directory with the basic layout of OCI image
// containing oci-layout file and blobs/sha256 subdirectory
// see https://github.com/opencontainers/image-spec/blob/main/image-layout.md
// for the whole OCI image layout specification
func CreateOCIDirectory(dir string) error {

	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		return err
	}

	ociLayoutFileContents := ocispec.ImageLayout{Version: ocispec.ImageLayoutVersion}

	data, err := json.Marshal(ociLayoutFileContents)
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(dir, ocispec.ImageLayoutFile), data, 0o644)

}

// SaveBlob writes data to blobs/sha256/<hash> and returns its digest and size.
func SaveBlob(dir string, data []byte) (string, int64, error) {

	hash := sha256.Sum256(data)
	digest := fmt.Sprintf("sha256:%x", hash)
	blobPath := filepath.Join(dir, "blobs", "sha256", fmt.Sprintf("%x", hash))

	if err := os.WriteFile(blobPath, data, 0o644); err != nil {
		return "", 0, err
	}

	return digest, int64(len(data)), nil
}

// NewConfig creates a minimal OCI image config
func NewConfig() ocispec.Image {

	return ocispec.Image{
		Platform: ocispec.Platform{
			// Hardcoded to match the original behaviour. Architecture is not meaningful for a source
			// image, the OCI schema just requires the field.
			Architecture: "amd64",
			OS:           "linux",
		},
		RootFS: ocispec.RootFS{
			Type: "layers",
		},
	}
}

// NewManifest creates an empty OCI image manifest.
func NewManifest() ocispec.Manifest {
	return ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
	}
}

// SaveConfig serializes the config to a blob and returns its digest and size.
func SaveConfig(dir string, config ocispec.Image) (string, int64, error) {
	data, err := json.Marshal(config)
	if err != nil {
		return "", 0, err
	}
	return SaveBlob(dir, data)
}

// SaveManifest serializes the manifest to a blob and returns its digest and size.
func SaveManifest(dir string, manifest ocispec.Manifest) (string, int64, error) {
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", 0, err
	}
	return SaveBlob(dir, data)
}

// SaveIndex writes the index.json pointing to the manifest with source container annotations.
func SaveIndex(dir string, manifestDigest string, manifestSize int64) error {
	index := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		Manifests: []ocispec.Descriptor{
			{
				MediaType: ocispec.MediaTypeImageManifest,
				Digest:    digest.Digest(manifestDigest),
				Size:      manifestSize,
				Annotations: map[string]string{
					"com.redhat.image.type": "source",
					// "latest-source" is hardcoded for compatibility with the original
					// script, which set this tag to a fixed "latest-source" and left a
					// comment noting it was meant to reference the image the source was built
					// from — something it never actually did:
					// https://github.com/containers/BuildSourceImage/blob/e9feb7b8e50e97fa41b58fc28ea68313794a5659/BuildSourceImage.sh#L1194
					"org.opencontainers.image.ref.name": "latest-source",
				},
			},
		},
	}

	data, err := json.Marshal(index)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "index.json"), data, 0o644)
}
