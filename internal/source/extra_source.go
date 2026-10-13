package source

import (
	"archive/tar"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	log "github.com/sirupsen/logrus"
)

// ProcessExtraSrcDirs creates a reproducible tar of each directory in dirs
// under workDir and returns an Artifact for each.
func ProcessExtraSrcDirs(dirs []string, workDir string) ([]Artifact, error) {
	var artifacts []Artifact

	for _, dir := range dirs {
		log.Infof("creating tar from extra source directory %s", dir)
		tmpPath := filepath.Join(workDir, "extra-src.tar")

		checksum, err := createTar(tmpPath, dir)
		if err != nil {
			return nil, fmt.Errorf("creating tar for %s: %w", dir, err)
		}

		tarName := fmt.Sprintf("extra-src-%s.tar", checksum)
		tarPath := filepath.Join(workDir, tarName)
		if err := os.Rename(tmpPath, tarPath); err != nil {
			return nil, fmt.Errorf("renaming tar for %s: %w", dir, err)
		}
		log.Debugf("  tar checksum=%s", checksum)

		artifacts = append(artifacts, Artifact{
			Path:       tarPath,
			DriverName: "extra_src_dir",
			Metadata: ArtifactMetadata{
				Name:     tarName,
				Mimetype: "application/x-tar",
				Checksum: checksum,
			},
		})
	}

	return artifacts, nil
}

// createTar writes a reproducible tar of srcDir to tarPath (normalized modes,
// zeroed mtimes/ownership, deterministic ordering) and returns the tar's sha256
// hex checksum.
func createTar(tarPath string, srcDir string) (checksum string, err error) {
	f, err := os.Create(tarPath)
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	h := sha256.New()
	w := io.MultiWriter(f, h)
	tw := tar.NewWriter(w)

	var paths []string
	err = filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel != "." {
			paths = append(paths, rel)
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	// The root "./" entry is built here rather than in the loop below, because
	// WalkDir skips ".". It gets srcDir's own mode with read-write added for
	// everyone (`|= 0o666`, as `tar --mode=a+rw` does) — the same normalization
	// every entry in the loop receives.
	rootInfo, err := os.Stat(srcDir)
	if err != nil {
		return "", err
	}
	rootHeader, err := tar.FileInfoHeader(rootInfo, "")
	if err != nil {
		return "", err
	}
	rootHeader.Name = "./"
	rootHeader.ModTime = time.Unix(0, 0)
	rootHeader.Uid = 0
	rootHeader.Gid = 0
	rootHeader.Uname = "root"
	rootHeader.Gname = "root"
	rootHeader.Mode |= 0o666
	if err := tw.WriteHeader(rootHeader); err != nil {
		return "", err
	}

	for _, rel := range paths {
		fullPath := filepath.Join(srcDir, rel)

		info, err := os.Lstat(fullPath)
		if err != nil {
			return "", err
		}

		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(fullPath)
			if err != nil {
				return "", err
			}
		}

		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return "", err
		}
		header.Name = "./" + rel
		if info.IsDir() {
			header.Name += "/"
		}
		header.ModTime = time.Unix(0, 0)
		header.Uid = 0
		header.Gid = 0
		header.Uname = "root"
		header.Gname = "root"
		header.Mode |= 0o666

		if err := tw.WriteHeader(header); err != nil {
			return "", err
		}

		if !info.Mode().IsRegular() {
			continue
		}

		file, err := os.Open(fullPath)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(tw, file)
		file.Close()
		if err != nil {
			return "", err
		}
	}

	if err := tw.Close(); err != nil {
		return "", err
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
