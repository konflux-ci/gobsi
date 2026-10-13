package source

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	rpmlib "github.com/cavaliergopher/rpm"
	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

// fakePackage builds an in-memory *rpm.Package with the header/signature tags
// that extractSRPMMetadata reads, so tests need no real RPM file.
func fakePackage() *rpmlib.Package {
	return &rpmlib.Package{
		Header: rpmlib.Header{Tags: map[int]*rpmlib.Tag{
			1000: {Value: []string{"gobsi-test"}},    // Name
			1001: {Value: []string{"1.2.3"}},         // Version
			1002: {Value: []string{"4.fc42"}},        // Release
			1003: {Value: []int64{5}},                // Epoch
			1006: {Value: []int64{1700000000}},       // BuildTime
			1014: {Value: []string{"MIT and GPLv2"}}, // License
		}},
		Signature: rpmlib.Header{Tags: map[int]*rpmlib.Tag{
			rpmSigTagMD5: {Value: []byte{0xde, 0xad, 0xbe, 0xef}}, // SIGMD5 / pkgid
		}},
	}
}

// stubRPMRead replaces the package-level rpmRead with fn for the duration of the
// test and restores it afterwards.
func stubRPMRead(t *testing.T, fn func(io.Reader) (*rpmlib.Package, error)) {
	t.Helper()
	orig := rpmRead
	rpmRead = fn
	t.Cleanup(func() { rpmRead = orig })
}

func TestProcessSRPMDirMetadataMapping(t *testing.T) {
	dir := t.TempDir()
	rpmPath := filepath.Join(dir, "gobsi-test-1.2.3-4.fc42.src.rpm")
	contents := []byte("not a real rpm, rpmRead is mocked")
	if err := os.WriteFile(rpmPath, contents, 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	stubRPMRead(t, func(io.Reader) (*rpmlib.Package, error) {
		return fakePackage(), nil
	})

	artifacts, err := ProcessSRPMDir(dir)
	if err != nil {
		t.Fatalf("ProcessSRPMDir failed: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(artifacts))
	}

	a := artifacts[0]
	if a.Path != rpmPath {
		t.Errorf("Path: expected %s, got %s", rpmPath, a.Path)
	}
	if a.DriverName != "rpm_dir" {
		t.Errorf("DriverName: expected rpm_dir, got %s", a.DriverName)
	}

	sum := sha256.Sum256(contents)
	wantChecksum := fmt.Sprintf("%x", sum)

	m := a.Metadata
	checks := []struct {
		field string
		got   string
		want  string
	}{
		{"Filename", m.Filename, "gobsi-test-1.2.3-4.fc42.src.rpm"},
		{"Name", m.Name, "gobsi-test"},
		{"Version", m.Version, "1.2.3"},
		{"Release", m.Release, "4.fc42"},
		{"Epoch", m.Epoch, "5"},
		{"BuildTime", m.BuildTime, "1700000000"},
		{"License", m.License, "MIT and GPLv2"},
		{"Mimetype", m.Mimetype, "application/x-rpm"},
		{"PkgID", m.PkgID, "deadbeef"},
		{"Checksum", m.Checksum, wantChecksum},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: expected %q, got %q", c.field, c.want, c.got)
		}
	}
}

func TestProcessSRPMDirReadError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.src.rpm"), []byte("garbage"), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	sentinel := errors.New("bad rpm header")
	stubRPMRead(t, func(io.Reader) (*rpmlib.Package, error) {
		return nil, sentinel
	})

	_, err := ProcessSRPMDir(dir)
	if err == nil {
		t.Fatal("expected error from ProcessSRPMDir, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected wrapped %v, got %v", sentinel, err)
	}
}

func TestProcessSRPMDirMissingMD5Tag(t *testing.T) {
	// A package without the MD5 signature tag (e.g. v6 format, which drops MD5)
	// must not fail the build: it warns, then produces an artifact with an empty
	// pkgid, which Annotations omits.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "v6.src.rpm"), []byte("x"), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	stubRPMRead(t, func(io.Reader) (*rpmlib.Package, error) {
		pkg := fakePackage()
		delete(pkg.Signature.Tags, rpmSigTagMD5)
		return pkg, nil
	})

	// Capture log entries from the global logger.
	hook := test.NewGlobal()

	artifacts, err := ProcessSRPMDir(dir)
	if err != nil {
		t.Fatalf("ProcessSRPMDir failed: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(artifacts))
	}
	if pkgID := artifacts[0].Metadata.PkgID; pkgID != "" {
		t.Errorf("expected empty PkgID, got %q", pkgID)
	}
	if _, ok := artifacts[0].Metadata.Annotations()["source.artifact.pkgid"]; ok {
		t.Error("expected pkgid annotation to be omitted when empty")
	}

	entry := hook.LastEntry()
	if entry == nil {
		t.Fatal("expected a log entry, got none")
	}
	if entry.Level != logrus.WarnLevel {
		t.Errorf("expected warning level, got %s", entry.Level)
	}
	if !strings.Contains(entry.Message, "no MD5 signature tag") {
		t.Errorf("expected warning to mention the missing MD5 tag, got: %q", entry.Message)
	}
}

func TestProcessSRPMDirSkipsNonSRPM(t *testing.T) {
	dir := t.TempDir()
	// Files that are not *.src.rpm must be ignored, and rpmRead must not be
	// called for them.
	for _, name := range []string{"readme.txt", "pkg.rpm", "archive.tar"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	stubRPMRead(t, func(io.Reader) (*rpmlib.Package, error) {
		t.Fatal("rpmRead called for a non-.src.rpm file")
		return nil, nil
	})

	artifacts, err := ProcessSRPMDir(dir)
	if err != nil {
		t.Fatalf("ProcessSRPMDir failed: %v", err)
	}
	if len(artifacts) != 0 {
		t.Errorf("expected 0 artifacts, got %d", len(artifacts))
	}
}

func TestProcessSRPMDirRecursesSubdirs(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "deep.src.rpm"), []byte("x"), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	stubRPMRead(t, func(io.Reader) (*rpmlib.Package, error) {
		return fakePackage(), nil
	})

	artifacts, err := ProcessSRPMDir(dir)
	if err != nil {
		t.Fatalf("ProcessSRPMDir failed: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected 1 artifact from nested dir, got %d", len(artifacts))
	}
}
