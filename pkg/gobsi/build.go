package gobsi

import (
	"fmt"
	"os"
	"time"

	"github.com/konflux-ci/gobsi/pkg/oci"
	"github.com/konflux-ci/gobsi/pkg/source"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	log "github.com/sirupsen/logrus"
)

// BuildConfig holds the inputs for a source image build: the SRPM directory,
// any extra source directories, any source images to merge and the output
// OCI image path.
type BuildConfig struct {
	SRPMDir   string
	ExtraDirs []string
	MergeDirs []string
	OutputDir string
}

// BuildSourceImage builds an OCI source-container image at cfg.OutputDir from
// the configured SRPM and extra source directories, then merges any source
// images listed in cfg.MergeDirs, deduplicating layers by their inner artifact
// hash. At least one input is required.
func BuildSourceImage(cfg BuildConfig) error {
	if cfg.SRPMDir == "" && len(cfg.ExtraDirs) == 0 && len(cfg.MergeDirs) == 0 {
		return fmt.Errorf("provide at least one input: SRPMDir, ExtraDirs, or MergeDirs")
	}

	log.Infof("building source image to %s", cfg.OutputDir)

	if err := oci.CreateOCIDirectory(cfg.OutputDir); err != nil {
		return fmt.Errorf("creating OCI directory: %w", err)
	}

	var artifacts []source.Artifact

	if cfg.SRPMDir != "" {
		log.Infof("processing SRPMs from %s", cfg.SRPMDir)
		srpms, err := source.ProcessSRPMDir(cfg.SRPMDir)
		if err != nil {
			return fmt.Errorf("processing SRPMs: %w", err)
		}
		log.Debugf("found %d SRPMs", len(srpms))
		artifacts = append(artifacts, srpms...)
	}

	if len(cfg.ExtraDirs) > 0 {
		log.Infof("processing %d extra source directories", len(cfg.ExtraDirs))
		workDir, err := os.MkdirTemp("", "gobsi-")
		if err != nil {
			return fmt.Errorf("creating work directory: %w", err)
		}
		defer os.RemoveAll(workDir)

		extras, err := source.ProcessExtraSrcDirs(cfg.ExtraDirs, workDir)
		if err != nil {
			return fmt.Errorf("processing extra sources: %w", err)
		}
		log.Debugf("created %d extra source tars", len(extras))
		artifacts = append(artifacts, extras...)
	}

	config := oci.NewConfig()
	manifest := oci.NewManifest()

	// seen tracks the inner artifact hash of every layer already added, so
	// duplicates (the same SRPM or the same source content) are dropped. The
	// hash is the artifact's own checksum — see oci.InnerArtifactHash. What that
	// checksum covers differs by artifact type, which sets the dedup scope:
	//   - SRPM layers: the checksum is the SRPM file's own hash, identical
	//     across tools.
	//   - extra_source layers: the checksum is gobsi's deterministic tar hash,
	//     unique to gobsi, so these deduplicate only against other gobsi-built
	//     layers.
	seen := make(map[string]bool)
	addLayer := func(desc ocispec.Descriptor, diffID digest.Digest, innerHash string) {
		manifest.Layers = append(manifest.Layers, desc)
		config.RootFS.DiffIDs = append(config.RootFS.DiffIDs, diffID)
		now := time.Now()
		config.History = append(config.History, ocispec.History{
			Created:   &now,
			CreatedBy: fmt.Sprintf("#(nop) gobsi adding artifact: %s", innerHash),
		})
		if innerHash != "" {
			seen[innerHash] = true
		}
	}

	// Freshly built layers are added before merged ones so that when the same
	// artifact exists in both, the merged copy is the one dropped as a
	// duplicate.
	for _, a := range artifacts {
		inner := a.Metadata.Checksum
		if inner != "" && seen[inner] {
			log.Infof("skipping duplicate artifact %s (%s)", a.Metadata.Name, inner)
			continue
		}
		log.Infof("adding layer for %s", a.Metadata.Name)
		layer, err := oci.CreateLayer(cfg.OutputDir, a)
		if err != nil {
			return fmt.Errorf("creating layer for %s: %w", a.Path, err)
		}
		addLayer(layer.Descriptor, layer.DiffID, inner)
		log.Debugf("layer digest=%s size=%d", layer.Descriptor.Digest, layer.Descriptor.Size)
	}

	// Merge layers from each source image, deduplicating by inner artifact hash.
	var outInfo os.FileInfo
	if len(cfg.MergeDirs) > 0 {
		fi, err := os.Stat(cfg.OutputDir)
		if err != nil {
			return fmt.Errorf("stat output dir: %w", err)
		}
		outInfo = fi
	}
	for _, dir := range cfg.MergeDirs {
		dirInfo, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("stat merge source %s: %w", dir, err)
		}
		// os.SameFile compares device+inode, so this also catches differently
		// spelled paths and symlinks that resolve to the output directory.
		if os.SameFile(outInfo, dirInfo) {
			return fmt.Errorf("merge source %s is the output directory; cannot merge an image into itself", dir)
		}

		log.Infof("merging source image from %s", dir)
		img, err := oci.LoadImage(dir)
		if err != nil {
			return fmt.Errorf("loading merge source %s: %w", dir, err)
		}
		for _, l := range img.Layers {
			inner, err := oci.InnerArtifactHash(l.BlobPath, l.Descriptor.MediaType)
			if err != nil {
				return fmt.Errorf("reading layer %s from %s: %w", l.Descriptor.Digest, dir, err)
			}
			if inner != "" && seen[inner] {
				log.Debugf("skipping duplicate merged layer %s", inner)
				continue
			}
			// A layer with no ./blobs/sha256/<hash> artifact entry is not a source
			// layer (e.g. a base/filesystem layer, or a non-source image passed to
			// --merge). gobsi only merges source content, so skip it rather than
			// merge non-source layers into the output.
			if inner == "" {
				log.Warnf("skipping non-source layer %s in %s (no inner artifact entry)",
					l.Descriptor.Digest, dir)
				continue
			}
			if err := oci.CopyBlob(cfg.OutputDir, l.BlobPath, l.Descriptor.Digest); err != nil {
				return fmt.Errorf("copying layer blob %s from %s: %w", l.Descriptor.Digest, dir, err)
			}
			addLayer(l.Descriptor, l.DiffID, inner)
			log.Debugf("merged layer digest=%s", l.Descriptor.Digest)
		}
	}

	now := time.Now()
	config.Created = &now

	configDigest, configSize, err := oci.SaveConfig(cfg.OutputDir, config)
	if err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	log.Debugf("config digest=%s size=%d", configDigest, configSize)

	manifest.Config = ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    digest.Digest(configDigest),
		Size:      configSize,
	}

	manifestDigest, manifestSize, err := oci.SaveManifest(cfg.OutputDir, manifest)
	if err != nil {
		return fmt.Errorf("saving manifest: %w", err)
	}
	log.Debugf("manifest digest=%s size=%d", manifestDigest, manifestSize)

	if err := oci.SaveIndex(cfg.OutputDir, manifestDigest, manifestSize); err != nil {
		return fmt.Errorf("saving index: %w", err)
	}

	log.Infof("source image successfully built at %s", cfg.OutputDir)
	return nil
}
