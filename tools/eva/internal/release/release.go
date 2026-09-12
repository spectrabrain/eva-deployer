package release

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

var checksumPattern = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
var tagPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?$`)

const DefaultInstallRoot = "/opt/eva/releases"
const preparedMarkerName = ".eva-prepared-release"

var requiredArtifacts = map[string]struct{}{
	"eva-tool":     {},
	"eva-infra":    {},
	"eva-solution": {},
}

type Platform struct {
	OS   string `yaml:"os"`
	Arch string `yaml:"arch"`
}

type Artifact struct {
	Name   string `yaml:"name"`
	File   string `yaml:"file"`
	SHA256 string `yaml:"sha256"`
}

type Metadata struct {
	Version   string     `yaml:"version"`
	Platform  Platform   `yaml:"platform"`
	Artifacts []Artifact `yaml:"artifacts"`
}

type Resolved struct {
	Root         string
	MetadataPath string
	Metadata     Metadata
	Prepared     bool
}

type Prepared struct {
	Root     string
	Metadata Metadata
}

func Resolve(input string) (Resolved, error) {
	if input == "" {
		input = "."
	}
	if tagPattern.MatchString(input) {
		return Resolved{}, fmt.Errorf("release tag %q requires the S3 resolver, which is not implemented yet", input)
	}

	path, err := filepath.Abs(input)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve release path: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return Resolved{}, fmt.Errorf("read release path %s: %w", path, err)
	}

	root := path
	metadataPath := filepath.Join(path, "release.yaml")
	if !info.IsDir() {
		if filepath.Base(path) != "release.yaml" {
			return Resolved{}, errors.New("release input must be a directory containing release.yaml; Airgap Bundle support is not implemented yet")
		}
		root = filepath.Dir(path)
		metadataPath = path
	}

	metadata, err := loadMetadata(metadataPath)
	if err != nil {
		return Resolved{}, err
	}
	prepared, err := hasPreparedMarker(root)
	if err != nil {
		return Resolved{}, err
	}
	if prepared {
		if err := validatePrepared(root, metadata); err != nil {
			return Resolved{}, err
		}
	} else if err := validate(root, metadata); err != nil {
		return Resolved{}, err
	}

	return Resolved{Root: root, MetadataPath: metadataPath, Metadata: metadata, Prepared: prepared}, nil
}

func Prepare(resolved Resolved, installRoot string) (Prepared, error) {
	if resolved.Prepared {
		return Prepared{Root: resolved.Root, Metadata: resolved.Metadata}, nil
	}
	if installRoot == "" {
		installRoot = DefaultInstallRoot
	}
	if !tagPattern.MatchString(resolved.Metadata.Version) {
		return Prepared{}, fmt.Errorf("release version %q cannot be used as an installed Release directory", resolved.Metadata.Version)
	}
	absInstallRoot, err := filepath.Abs(installRoot)
	if err != nil {
		return Prepared{}, fmt.Errorf("resolve Release install root: %w", err)
	}
	destination := filepath.Join(absInstallRoot, resolved.Metadata.Version)
	if existing, err := existingPrepared(destination); err != nil {
		return Prepared{}, err
	} else if existing != nil {
		if !reflect.DeepEqual(existing.Metadata, resolved.Metadata) {
			return Prepared{}, fmt.Errorf("installed Release %s already exists with different metadata", destination)
		}
		return *existing, nil
	}
	if err := os.MkdirAll(absInstallRoot, 0o755); err != nil {
		return Prepared{}, fmt.Errorf("create Release install root: %w", err)
	}
	staging, err := os.MkdirTemp(absInstallRoot, ".eva-release-")
	if err != nil {
		return Prepared{}, fmt.Errorf("create Release staging directory: %w", err)
	}
	defer os.RemoveAll(staging)
	for _, name := range []string{"eva-infra", "eva-solution"} {
		artifact, err := resolved.ArtifactPath(name)
		if err != nil {
			return Prepared{}, err
		}
		if err := extractArchive(artifact, staging); err != nil {
			return Prepared{}, fmt.Errorf("extract %s: %w", name, err)
		}
	}
	if err := copyFile(resolved.MetadataPath, filepath.Join(staging, "release.yaml"), 0o644); err != nil {
		return Prepared{}, fmt.Errorf("copy release metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staging, preparedMarkerName), []byte("prepared: true\n"), 0o644); err != nil {
		return Prepared{}, fmt.Errorf("write Release marker: %w", err)
	}
	if err := validatePrepared(staging, resolved.Metadata); err != nil {
		return Prepared{}, err
	}
	if err := os.Rename(staging, destination); err != nil {
		if errors.Is(err, os.ErrExist) {
			return Prepared{}, fmt.Errorf("installed Release %s already exists", destination)
		}
		return Prepared{}, fmt.Errorf("publish prepared Release: %w", err)
	}
	return Prepared{Root: destination, Metadata: resolved.Metadata}, nil
}

func (resolved Resolved) ArtifactPath(name string) (string, error) {
	for _, artifact := range resolved.Metadata.Artifacts {
		if artifact.Name != name {
			continue
		}
		path, err := artifactPath(resolved.Root, artifact.File)
		if err != nil {
			return "", fmt.Errorf("artifact %q: %w", name, err)
		}
		return path, nil
	}
	return "", fmt.Errorf("release is missing artifact %q", name)
}

func loadMetadata(path string) (Metadata, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return Metadata{}, fmt.Errorf("read release metadata %s: %w", path, err)
	}

	decoder := yaml.NewDecoder(strings.NewReader(string(contents)))
	decoder.KnownFields(true)
	var metadata Metadata
	if err := decoder.Decode(&metadata); err != nil {
		return Metadata{}, fmt.Errorf("parse release metadata %s: %w", path, err)
	}
	return metadata, nil
}

func validate(root string, metadata Metadata) error {
	if err := validateMetadata(metadata); err != nil {
		return err
	}

	for _, artifact := range metadata.Artifacts {
		path, err := artifactPath(root, artifact.File)
		if err != nil {
			return fmt.Errorf("artifact %q: %w", artifact.Name, err)
		}
		if err := verifyChecksum(path, artifact.SHA256); err != nil {
			return fmt.Errorf("artifact %q: %w", artifact.Name, err)
		}
	}
	return nil
}

func validateMetadata(metadata Metadata) error {
	if metadata.Version == "" {
		return errors.New("release version is required")
	}
	if metadata.Platform.OS != runtime.GOOS || metadata.Platform.Arch != runtime.GOARCH {
		return fmt.Errorf("release platform %s/%s does not match this host %s/%s", metadata.Platform.OS, metadata.Platform.Arch, runtime.GOOS, runtime.GOARCH)
	}

	found := make(map[string]struct{}, len(metadata.Artifacts))
	for _, artifact := range metadata.Artifacts {
		if artifact.Name == "" || artifact.File == "" || artifact.SHA256 == "" {
			return errors.New("each artifact requires name, file, and sha256")
		}
		if _, duplicate := found[artifact.Name]; duplicate {
			return fmt.Errorf("duplicate artifact %q", artifact.Name)
		}
		found[artifact.Name] = struct{}{}
		if !checksumPattern.MatchString(artifact.SHA256) {
			return fmt.Errorf("artifact %q has an invalid sha256", artifact.Name)
		}
	}
	for name := range requiredArtifacts {
		if _, ok := found[name]; !ok {
			return fmt.Errorf("release is missing required artifact %q", name)
		}
	}
	return nil
}

func hasPreparedMarker(root string) (bool, error) {
	info, err := os.Stat(filepath.Join(root, preparedMarkerName))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read Release marker: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("Release marker is not a regular file")
	}
	return true, nil
}

func validatePrepared(root string, metadata Metadata) error {
	if err := validateMetadata(metadata); err != nil {
		return err
	}
	for _, path := range []string{
		"ansible.cfg",
		"src/playbook-preflight.yaml",
		"src/playbook-vars.yaml",
		"src/infra/playbooks/site_infra.yaml",
	} {
		info, err := os.Stat(filepath.Join(root, path))
		if err != nil {
			return fmt.Errorf("prepared Release is missing %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("prepared Release path is not a regular file: %s", path)
		}
	}
	info, err := os.Stat(filepath.Join(root, "src", "solution"))
	if err != nil {
		return fmt.Errorf("prepared Release is missing src/solution: %w", err)
	}
	if !info.IsDir() {
		return errors.New("prepared Release src/solution is not a directory")
	}
	return nil
}

func existingPrepared(path string) (*Prepared, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read installed Release: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("installed Release path is not a directory: %s", path)
	}
	prepared, err := hasPreparedMarker(path)
	if err != nil {
		return nil, err
	}
	if !prepared {
		return nil, fmt.Errorf("installed Release exists but is not managed: %s", path)
	}
	metadata, err := loadMetadata(filepath.Join(path, "release.yaml"))
	if err != nil {
		return nil, err
	}
	if err := validatePrepared(path, metadata); err != nil {
		return nil, err
	}
	return &Prepared{Root: path, Metadata: metadata}, nil
}

func extractArchive(path, destination string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open gzip stream: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := archiveTarget(destination, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, header.FileInfo().Mode().Perm()); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := copyArchiveFile(tarReader, target, header.FileInfo().Mode().Perm()); err != nil {
				return err
			}
		default:
			return fmt.Errorf("archive entry type is not allowed: %s", header.Name)
		}
	}
}

func archiveTarget(destination, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) {
		return "", fmt.Errorf("archive entry has invalid path %q", name)
	}
	target := filepath.Clean(filepath.Join(destination, name))
	relative, err := filepath.Rel(destination, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry escapes Release staging directory: %q", name)
	}
	return target, nil
}

func copyArchiveFile(source io.Reader, destination string, mode os.FileMode) error {
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, source); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}

func copyFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}

func artifactPath(root, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", errors.New("artifact file must be relative to the release directory")
	}
	path := filepath.Clean(filepath.Join(root, name))
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("artifact file escapes the release directory")
	}
	return path, nil
}

func verifyChecksum(path, expected string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read artifact file %s: %w", path, err)
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return fmt.Errorf("checksum artifact file %s: %w", path, err)
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); !strings.EqualFold(actual, expected) {
		return fmt.Errorf("checksum mismatch for %s", path)
	}
	return nil
}
