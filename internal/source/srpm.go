package source

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	rpmlib "github.com/cavaliergopher/rpm"
	log "github.com/sirupsen/logrus"
)

// rpmSigTagMD5 is RPMSIGTAG_MD5: the signature-header tag holding the package's
// MD5 digest. Hex-encoded it is the RPM "pkgid" (== `rpm -qp --qf '%{pkgid}'`,
// or `%{SIGMD5}` on rpm >= 6, which no longer interprets %{pkgid}). The rpm
// library does not export a named constant for it.
//
// 1004 is the MD5 tag in the v3/v4 RPM header format:
// https://github.com/rpm-software-management/rpm/blob/master/docs/manual/format_v4.md
// The newer v6 format drops MD5 (and the whole legacy signature layout) in
// favor of SHA-based payload digest tags, so this tag won't exist for SRPMs
// built as v6 and this code will need revisiting once those become common:
// https://github.com/rpm-software-management/rpm/blob/master/docs/manual/format_v6.md
const rpmSigTagMD5 = 1004

// rpmRead parses an RPM package. It is a variable so tests can substitute
// a fake package without needing a real RPM file on disk.
var rpmRead = rpmlib.Read

// ArtifactMetadata holds the per-artifact metadata that is emitted as OCI
// descriptor annotations.
type ArtifactMetadata struct {
	Filename  string
	Name      string
	Mimetype  string
	Checksum  string
	Version   string
	Epoch     string
	Release   string
	License   string
	BuildTime string
	PkgID     string
}

// Annotations renders the metadata as OCI descriptor annotations, omitting any
// empty fields.
func (m ArtifactMetadata) Annotations() map[string]string {
	a := make(map[string]string)
	if m.Filename != "" {
		a["source.artifact.filename"] = m.Filename
	}
	if m.Checksum != "" {
		a["source.artifact.filename.checksum"] = "sha256:" + m.Checksum
	}
	if m.Name != "" {
		a["source.artifact.name"] = m.Name
	}
	if m.Mimetype != "" {
		a["source.artifact.mimetype"] = m.Mimetype
	}
	if m.Version != "" {
		a["source.artifact.version"] = m.Version
	}
	if m.Epoch != "" {
		a["source.artifact.epoch"] = m.Epoch
	}
	if m.Release != "" {
		a["source.artifact.release"] = m.Release
	}
	if m.License != "" {
		a["source.artifact.license"] = m.License
	}
	if m.BuildTime != "" {
		a["source.artifact.buildtime"] = m.BuildTime
	}
	if m.PkgID != "" {
		a["source.artifact.pkgid"] = m.PkgID
	}
	return a
}

// Artifact is a source file to be packed into the image, together with the
// driver that produced it and its metadata.
type Artifact struct {
	Path       string
	DriverName string
	Metadata   ArtifactMetadata
}

// ProcessSRPMDir walks srpmDir for *.src.rpm files and returns an Artifact for
// each, with metadata extracted from the RPM.
func ProcessSRPMDir(srpmDir string) ([]Artifact, error) {
	var matches []string
	err := filepath.WalkDir(srpmDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".src.rpm") {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	var artifacts []Artifact
	for _, path := range matches {
		log.Infof("processing SRPM %s", filepath.Base(path))
		metadata, err := extractSRPMMetadata(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		log.Debugf("  name=%s version=%s epoch=%s", metadata.Name, metadata.Version, metadata.Epoch)
		artifacts = append(artifacts, Artifact{
			Path:       path,
			DriverName: "rpm_dir",
			Metadata:   metadata,
		})
	}

	return artifacts, nil
}

// extractSRPMMetadata reads the RPM header and computes the file checksum for
// the SRPM at path.
func extractSRPMMetadata(path string) (ArtifactMetadata, error) {
	f, err := os.Open(path)
	if err != nil {
		return ArtifactMetadata{}, err
	}
	defer f.Close()

	pkg, err := rpmRead(f)
	if err != nil {
		return ArtifactMetadata{}, err
	}

	// rpmlib.Read only consumes the header, so hash the whole file for the
	// artifact checksum. Doing it here (the file is already open) means every
	// artifact carries a checksum and CreateLayer never has to re-read the file.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ArtifactMetadata{}, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ArtifactMetadata{}, err
	}
	checksum := fmt.Sprintf("%x", h.Sum(nil))

	// A missing MD5 tag means we can't compute the pkgid. This is expected for
	// v6-format packages, which drop MD5 entirely. Emit warning for these cases.
	var pkgID string
	if md5Tag := pkg.Signature.GetTag(rpmSigTagMD5); md5Tag != nil {
		pkgID = fmt.Sprintf("%x", md5Tag.Bytes())
	} else {
		log.Warnf("SRPM %s has no MD5 signature tag (%d); skipping pkgid annotation "+
			"(it may be built in the v6 RPM format)", path, rpmSigTagMD5)
	}

	return ArtifactMetadata{
		Filename:  filepath.Base(path),
		Name:      pkg.Name(),
		Version:   pkg.Version(),
		Epoch:     strconv.Itoa(pkg.Epoch()),
		Release:   pkg.Release(),
		License:   pkg.License(),
		Mimetype:  "application/x-rpm",
		Checksum:  checksum,
		BuildTime: strconv.FormatInt(pkg.BuildTime().Unix(), 10),
		PkgID:     pkgID,
	}, nil
}
