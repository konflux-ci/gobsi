package oci

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	log "github.com/sirupsen/logrus"
)

// MergeLayer is a single layer of a loaded source image: the LayerInfo
// describing it plus the path to its blob on disk (which merging needs in order
// to read and copy the layer).
type MergeLayer struct {
	LayerInfo
	BlobPath string
}

// LoadedImage is the subset of an on-disk OCI image layout that merging needs:
// the directory it was loaded from, and its manifest's layer info with each
// layer paired to its blob path.
type LoadedImage struct {
	Dir    string
	Layers []MergeLayer
}

// LoadImage reads the OCI image layout at dir and returns its layers. It uses
// the first manifest in index.json (source images carry exactly one) and pairs
// each layer with the diff ID at the same position in the image config.
func LoadImage(dir string) (*LoadedImage, error) {
	var index ocispec.Index
	if err := readJSONFile(filepath.Join(dir, "index.json"), &index); err != nil {
		return nil, fmt.Errorf("reading index.json: %w", err)
	}
	if len(index.Manifests) == 0 {
		return nil, fmt.Errorf("%s: index.json has no manifests", dir)
	}
	if len(index.Manifests) > 1 {
		log.Warnf("%s: index.json has %d manifests; using the first", dir, len(index.Manifests))
	}

	var manifest ocispec.Manifest
	if err := readBlobJSON(dir, index.Manifests[0].Digest, &manifest); err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}

	var config ocispec.Image
	if err := readBlobJSON(dir, manifest.Config.Digest, &config); err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	if len(config.RootFS.DiffIDs) != len(manifest.Layers) {
		return nil, fmt.Errorf("%s: layer count (%d) does not match diff ID count (%d)",
			dir, len(manifest.Layers), len(config.RootFS.DiffIDs))
	}

	img := &LoadedImage{Dir: dir}
	for i, layer := range manifest.Layers {
		img.Layers = append(img.Layers, MergeLayer{
			LayerInfo: LayerInfo{
				Descriptor: layer,
				DiffID:     config.RootFS.DiffIDs[i],
			},
			BlobPath: blobPath(dir, layer.Digest),
		})
	}
	return img, nil
}

// innerBlobRe matches a layer's inner content-addressed artifact entry, e.g.
// "./blobs/sha256/<hex>", capturing the hash.
var innerBlobRe = regexp.MustCompile(`(?:^|/)blobs/sha256/([0-9a-f]+)$`)

// InnerArtifactHash returns the hash of the artifact packed inside a layer tar
// (the "./blobs/sha256/<hash>" entry). This is the deduplication key: the raw
// artifact's own checksum.
//
// What that checksum covers sets the dedup scope. For an SRPM it is the SRPM
// file's own hash, identical across tools. For an extra_source it is gobsi's
// deterministic tar hash, unique to gobsi, so those deduplicate only against
// other gobsi-built layers.
//
// mediaType is used to detect gzip-compressed layers, which must be decompressed
// before the inner entry can be read.
func InnerArtifactHash(blobPath, mediaType string) (string, error) {
	f, err := os.Open(blobPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var r io.Reader = bufio.NewReader(f)
	if strings.Contains(mediaType, "gzip") {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return "", err
		}
		defer gz.Close()
		r = gz
	}

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if m := innerBlobRe.FindStringSubmatch(hdr.Name); m != nil {
			return m[1], nil
		}
	}
	return "", nil
}

// CopyBlob copies the blob at srcPath into dstDir's blob store, named by dgst,
// after verifying that its contents actually hash to dgst. A corrupt or tampered
// merge input must not be published into the output as a (non-content-addressed)
// invalid blob, so a digest mismatch is an error and no blob is written.
func CopyBlob(dstDir, srcPath string, dgst digest.Digest) (err error) {
	// Validate up front: blobPath and Verifier both panic on a malformed or
	// unsupported digest.
	if err := dgst.Validate(); err != nil {
		return fmt.Errorf("invalid blob digest %q: %w", dgst, err)
	}

	dstPath := blobPath(dstDir, dgst)
	// Blobs are content-addressed: a blob with this digest is byte-identical
	// wherever it lives. If the destination already exists the copy is
	// redundant — and skipping it avoids truncating the source when src and
	// dst are the same file (e.g. an output dir passed as its own merge source).
	if _, err := os.Stat(dstPath); err == nil {
		return nil
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := dst.Close(); cerr != nil && err == nil {
			err = cerr
		}
		// Never leave a partial or digest-mismatched blob behind.
		if err != nil {
			os.Remove(dstPath)
		}
	}()

	verifier := dgst.Verifier()
	if _, err = io.Copy(io.MultiWriter(dst, verifier), src); err != nil {
		return err
	}
	if !verifier.Verified() {
		return fmt.Errorf("blob %s content does not match its digest", dgst)
	}
	return nil
}

// blobPath returns the on-disk path of a blob within an OCI layout directory.
func blobPath(dir string, dgst digest.Digest) string {
	return filepath.Join(dir, "blobs", dgst.Algorithm().String(), dgst.Encoded())
}

func readBlobJSON(dir string, dgst digest.Digest, v any) error {
	return readJSONFile(blobPath(dir, dgst), v)
}

func readJSONFile(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
