package oci

import (
	"archive/tar"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/konflux-ci/gobsi/pkg/pathutil"
	"github.com/konflux-ci/gobsi/pkg/source"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// LayerInfo describes a layer written to the OCI layout: its manifest
// descriptor and its uncompressed diff ID.
type LayerInfo struct {
	Descriptor ocispec.Descriptor
	DiffID     digest.Digest
}

// tarEntry is a single tar header plus an optional body reader (nil for
// directories and symlinks).
type tarEntry struct {
	header *tar.Header
	body   io.Reader
}

func dirHeader(name string, mode int64) *tar.Header {
	return &tar.Header{
		Name:     name,
		Typeflag: tar.TypeDir,
		Mode:     mode,
		Uname:    "root",
		Gname:    "root",
	}
}

// CreateLayer writes a single-artifact layer tar into ociDir's blob store and
// returns its descriptor and diff ID. The artifact's checksum must be set.
func CreateLayer(ociDir string, a source.Artifact) (LayerInfo, error) {
	artifactPath := a.Path
	driverName := a.DriverName
	checksum := a.Metadata.Checksum
	annotations := a.Metadata.Annotations()

	if checksum == "" {
		return LayerInfo{}, fmt.Errorf("checksum required for %s", artifactPath)
	}

	artifactFile, err := os.Open(artifactPath)
	if err != nil {
		return LayerInfo{}, err
	}
	defer artifactFile.Close()

	fi, err := artifactFile.Stat()
	if err != nil {
		return LayerInfo{}, err
	}
	artifactSize := fi.Size()

	artifactName := filepath.Base(artifactPath)

	// Write the layer tar to a temp file. We use a temp file because the
	// final blob filename is the tar's own sha256, which we don't know until
	// we've finished writing it. MultiWriter hashes the tar while writing.
	blobDir := filepath.Join(ociDir, "blobs", "sha256")
	tmpFile, err := os.CreateTemp(blobDir, "layer-*.tar.tmp")
	if err != nil {
		return LayerInfo{}, err
	}
	tmpPath := tmpFile.Name()
	defer func() {
		tmpFile.Close()
		os.Remove(tmpPath)
	}()

	tarHasher := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(tmpFile, tarHasher))

	blobPath := fmt.Sprintf("./blobs/sha256/%s", checksum)
	linkTarget := fmt.Sprintf("../blobs/sha256/%s", checksum)

	// Collect every tar entry, then sort segment-wise so the layer is written in
	// the depth-first order GNU tar produces. Directory modes are
	// each directory's original mode with read-write-for-everyone layered on top
	// (as `tar --mode=a+rw` does). The behaviour is based on the original script where:
	//   - root "./" originated from `mktemp -d`, which is always 0700, so a+rw
	//     makes it 0766.
	//   - the nested dirs originated from `mkdir` (0755 under the usual umask
	//     022), so a+rw makes them 0777.
	entries := []tarEntry{
		{header: dirHeader("./", 0o766)},
		{header: dirHeader("./blobs/", 0o777)},
		{header: dirHeader("./blobs/sha256/", 0o777)},
		{header: dirHeader("./"+driverName+"/", 0o777)},
		{
			header: &tar.Header{
				Name:     blobPath,
				Size:     artifactSize,
				Mode:     0o666,
				Typeflag: tar.TypeReg,
				Uname:    "root",
				Gname:    "root",
			},
			body: artifactFile,
		},
		{
			header: &tar.Header{
				Name:     fmt.Sprintf("./%s/%s", driverName, artifactName),
				Typeflag: tar.TypeSymlink,
				Linkname: linkTarget,
				Mode:     0o777,
				Uname:    "root",
				Gname:    "root",
			},
		},
	}

	sort.SliceStable(entries, func(i, j int) bool {
		return pathutil.Less(entries[i].header.Name, entries[j].header.Name)
	})

	for _, e := range entries {
		if err := tw.WriteHeader(e.header); err != nil {
			return LayerInfo{}, err
		}
		if e.body != nil {
			if _, err := io.Copy(tw, e.body); err != nil {
				return LayerInfo{}, err
			}
		}
	}

	if err := tw.Close(); err != nil {
		return LayerInfo{}, err
	}
	if err := tmpFile.Close(); err != nil {
		return LayerInfo{}, err
	}

	tarDigest := fmt.Sprintf("sha256:%x", tarHasher.Sum(nil))
	tarHash := fmt.Sprintf("%x", tarHasher.Sum(nil))
	finalPath := filepath.Join(blobDir, tarHash)
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return LayerInfo{}, err
	}

	tarFi, err := os.Stat(finalPath)
	if err != nil {
		return LayerInfo{}, err
	}
	tarSize := tarFi.Size()

	diffID := digest.Digest(tarDigest)

	return LayerInfo{
		Descriptor: ocispec.Descriptor{
			MediaType:   ocispec.MediaTypeImageLayer,
			Digest:      digest.Digest(tarDigest),
			Size:        tarSize,
			Annotations: annotations,
		},
		DiffID: diffID,
	}, nil
}
