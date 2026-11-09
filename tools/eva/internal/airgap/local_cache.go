package airgap

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
	"strings"

	"eva-deployer/tools/eva/internal/release"
	"gopkg.in/yaml.v3"
)

const localCacheReceipt = ".eva-airgap-cache.yaml"

type cacheFile struct {
	Path   string `yaml:"path"`
	SHA256 string `yaml:"sha256"`
}

type cacheReceipt struct {
	OfflineSHA256 string      `yaml:"offline_sha256"`
	Files         []cacheFile `yaml:"files"`
}

// MaterializeLocalCache restores only out/cache, leaving Runtime and prepared
// sources separate. Unknown existing caches are never merged or overwritten.
func MaterializeLocalCache(source release.Resolved, preparedRoot string) (string, error) {
	if source.Prepared {
		return "", errors.New("Local cache preparation requires the original Bundle Release")
	}
	offline, err := source.ArtifactPath("eva-offline")
	if err != nil {
		return "", err
	}
	var digest string
	for _, artifact := range source.Metadata.Artifacts {
		if artifact.Name == "eva-offline" {
			digest = strings.ToLower(artifact.SHA256)
		}
	}
	parent := filepath.Join(preparedRoot, "out")
	if err := makeLocalDirectory(parent, 0o755); err != nil {
		return "", err
	}
	destination := filepath.Join(parent, "cache")
	if _, err := os.Lstat(destination); err == nil {
		if err := validateLocalCache(destination, digest); err != nil {
			return "", fmt.Errorf("existing Local cache cannot be reused; preserve it and check its Release: %w", err)
		}
		return destination, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	staging, err := os.MkdirTemp(parent, ".eva-local-cache-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	if err := extractLocalCache(offline, staging, digest); err != nil {
		return "", err
	}
	if err := os.Rename(staging, destination); err != nil {
		return "", err
	}
	return destination, nil
}

func localDirectory(directory string) error {
	for current := directory; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Local cache directory must not be a symlink: %s", current)
		}
		if current == filepath.Dir(current) {
			return nil
		}
	}
}

// Check each existing ancestor before creating any child directory.
func makeLocalDirectory(directory string, mode os.FileMode) error {
	directory = filepath.Clean(directory)
	if _, err := os.Lstat(directory); err == nil {
		return localDirectory(directory)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(directory)
	if parent == directory {
		return errors.New("Local directory root is unavailable")
	}
	if err := makeLocalDirectory(parent, mode); err != nil {
		return err
	}
	if err := os.Mkdir(directory, mode); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return localDirectory(directory)
}

func extractLocalCache(offline, destination, expectedDigest string) error {
	file, err := openArchiveSource(offline, nil)
	if err != nil {
		return err
	}
	defer file.Close()
	archiveHash := sha256.New()
	compressed, err := gzip.NewReader(io.TeeReader(file, archiveHash))
	if err != nil {
		return err
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	receipt := cacheReceipt{OfflineSHA256: expectedDigest}
	seen := map[string]bool{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name, err := checkArchiveHeader(header)
		if err != nil {
			return err
		}
		if name == "out/cache" {
			continue
		}
		if !strings.HasPrefix(name, "out/cache/") {
			continue
		}
		relative := strings.TrimPrefix(name, "out/cache/")
		if relative == localCacheReceipt || seen[relative] {
			return fmt.Errorf("duplicate or reserved Local cache entry: %s", name)
		}
		seen[relative] = true
		if err := rejectArchiveCredentialName(name); err != nil {
			return err
		}
		target := filepath.Join(destination, filepath.FromSlash(relative))
		if header.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(header.Mode)&0o777)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(io.MultiWriter(output, hash), reader)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		receipt.Files = append(receipt.Files, cacheFile{Path: relative, SHA256: hex.EncodeToString(hash.Sum(nil))})
	}
	if _, err := io.Copy(io.Discard, compressed); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, io.TeeReader(file, archiveHash)); err != nil {
		return err
	}
	if hex.EncodeToString(archiveHash.Sum(nil)) != expectedDigest {
		return errors.New("eva-offline changed while restoring the Local cache")
	}
	if len(receipt.Files) == 0 {
		return errors.New("eva-offline has no Local cache files")
	}
	contents, err := yaml.Marshal(receipt)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(destination, localCacheReceipt), contents, 0o600)
}

func validateLocalCache(root, offlineDigest string) error {
	if err := localDirectory(root); err != nil {
		return err
	}
	file, err := openArchiveSource(filepath.Join(root, localCacheReceipt), nil)
	if err != nil {
		return errors.New("Local cache has no valid EVA preparation receipt")
	}
	defer file.Close()
	var receipt cacheReceipt
	if err := yaml.NewDecoder(io.LimitReader(file, 16<<20)).Decode(&receipt); err != nil {
		return errors.New("invalid Local cache receipt")
	}
	if receipt.OfflineSHA256 != offlineDigest || len(receipt.Files) == 0 {
		return errors.New("Local cache belongs to a different Bundle")
	}
	for _, entry := range receipt.Files {
		name, err := safeArchiveName(entry.Path)
		if err != nil || name != entry.Path {
			return errors.New("invalid Local cache receipt path")
		}
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := localDirectory(filepath.Dir(target)); err != nil {
			return err
		}
		input, err := openArchiveSource(target, nil)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, input)
		input.Close()
		if copyErr != nil {
			return copyErr
		}
		if hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
			return fmt.Errorf("Local cache checksum mismatch: %s", name)
		}
	}
	return nil
}
