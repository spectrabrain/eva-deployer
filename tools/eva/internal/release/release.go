package release

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

var checksumPattern = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)
var tagPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?$`)

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
	if err := validate(root, metadata); err != nil {
		return Resolved{}, err
	}

	return Resolved{Root: root, MetadataPath: metadataPath, Metadata: metadata}, nil
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

		path, err := artifactPath(root, artifact.File)
		if err != nil {
			return fmt.Errorf("artifact %q: %w", artifact.Name, err)
		}
		if err := verifyChecksum(path, artifact.SHA256); err != nil {
			return fmt.Errorf("artifact %q: %w", artifact.Name, err)
		}
	}
	for name := range requiredArtifacts {
		if _, ok := found[name]; !ok {
			return fmt.Errorf("release is missing required artifact %q", name)
		}
	}
	return nil
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
