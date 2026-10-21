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
	"regexp"
	"sort"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

var airgapFilenamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

const airgapReadme = `EVA Airgap Bundle

Extract this archive into a new directory on the target host, then run:
  sha256sum --check --strict checksums.sha256
  sudo bash ./eva-tool-installer.sh
  eva verify .

Keep release.yaml, checksums.sha256, the installer and all artifacts together.
The eva-offline artifact contains the runtime/cache payload prepared by the caller.
Keep customer workspace configuration and credentials outside this Release.
`

// BuildAirgapBundle packages an original, verified Release with an already built
// runtime/cache artifact. It never reads customer workspace directories, changes
// the source Release, or replaces an existing destination (including symlinks).
func BuildAirgapBundle(source Resolved, offlinePath, destination string) error {
	if destination == "" {
		return errors.New("Airgap Bundle destination is required")
	}
	destination, err := filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("resolve Airgap Bundle destination: %w", err)
	}
	// Lstat also refuses dangling symlinks, before reading or hashing any input.
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("Airgap Bundle destination already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read Airgap Bundle destination: %w", err)
	}
	if source.Root == "" || source.Prepared {
		return errors.New("Airgap packaging requires an original Release, not a prepared Release")
	}
	for _, marker := range []string{preparedMarkerName, airgapMarkerName} {
		if _, err := os.Lstat(filepath.Join(source.Root, marker)); err == nil {
			return errors.New("Airgap packaging requires an original Release, not a prepared or imported Release")
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read Release marker: %w", err)
		}
	}
	// Check file types before Resolve hashes them: links and special files must
	// not be followed, and a FIFO must not block verification.
	if err := checkAirgapRegularFile(filepath.Join(source.Root, "release.yaml")); err != nil {
		return fmt.Errorf("read original Release metadata: %w", err)
	}
	metadata, err := loadMetadata(filepath.Join(source.Root, "release.yaml"))
	if err != nil {
		return err
	}
	for _, artifact := range metadata.Artifacts {
		if err := validateAirgapSourceArtifact(artifact.File); err != nil {
			return fmt.Errorf("source artifact %q: %w", artifact.Name, err)
		}
		if err := checkAirgapRegularFile(filepath.Join(source.Root, artifact.File)); err != nil {
			return fmt.Errorf("source artifact %q: %w", artifact.Name, err)
		}
	}
	// The caller's Resolved metadata may be stale; only the on-disk Release is
	// authoritative, and every original artifact (even the old offline) must pass.
	verified, err := Resolve(source.Root)
	if err != nil {
		return fmt.Errorf("verify original Release: %w", err)
	}
	if verified.Prepared {
		return errors.New("Airgap packaging requires an original Release, not a prepared Release")
	}
	if !tagPattern.MatchString(verified.Metadata.Version) || strings.Contains(verified.Metadata.Version, "..") {
		return fmt.Errorf("release version %q cannot be used in an Airgap artifact filename", verified.Metadata.Version)
	}
	if _, err := verified.ArtifactPath("eva-tool-installer"); err != nil {
		return fmt.Errorf("Airgap packaging requires eva-tool-installer: %w", err)
	}
	if offlinePath == "" {
		return errors.New("Airgap offline artifact path is required")
	}
	if err := checkAirgapRegularFile(offlinePath); err != nil {
		return fmt.Errorf("read offline artifact: %w", err)
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create Airgap Bundle output directory: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".eva-airgap-build-")
	if err != nil {
		return fmt.Errorf("create Airgap Bundle staging directory: %w", err)
	}
	defer os.RemoveAll(staging)
	metadata = Metadata{Version: verified.Metadata.Version, Platform: verified.Metadata.Platform}
	used := map[string]bool{"release.yaml": true, "checksums.sha256": true, "README.md": true}
	for _, artifact := range verified.Metadata.Artifacts {
		if artifact.Name == "eva-offline" {
			continue
		}
		if err := validateAirgapSourceArtifact(artifact.File); err != nil {
			return fmt.Errorf("source artifact %q: %w", artifact.Name, err)
		}
		name := filepath.Base(artifact.File)
		mode := os.FileMode(0o644)
		switch artifact.Name {
		case "eva-tool":
			// The first-target installer discovers this canonical sibling name.
			name = fmt.Sprintf("eva-tool_%s_%s_%s.tar.gz", metadata.Version, metadata.Platform.OS, metadata.Platform.Arch)
		case "eva-tool-installer":
			name, mode = "eva-tool-installer.sh", 0o755
		}
		if !validAirgapArtifactFilename(name) || used[name] {
			return fmt.Errorf("unsafe or duplicate Airgap artifact filename %q", name)
		}
		used[name] = true
		digest, err := copyAirgapArtifact(filepath.Join(verified.Root, artifact.File), filepath.Join(staging, name), mode)
		if err != nil {
			return fmt.Errorf("copy artifact %q: %w", artifact.Name, err)
		}
		if !strings.EqualFold(digest, artifact.SHA256) {
			return fmt.Errorf("checksum mismatch while copying artifact %q", artifact.Name)
		}
		artifact.File, artifact.SHA256 = name, digest
		metadata.Artifacts = append(metadata.Artifacts, artifact)
	}
	offlineName := fmt.Sprintf("eva-offline_%s_%s_%s.tar.gz", metadata.Version, metadata.Platform.OS, metadata.Platform.Arch)
	if used[offlineName] {
		return fmt.Errorf("duplicate Airgap artifact filename %q", offlineName)
	}
	offlineDigest, err := copyAirgapArtifact(offlinePath, filepath.Join(staging, offlineName), 0o644)
	if err != nil {
		return fmt.Errorf("copy offline artifact: %w", err)
	}
	metadata.Artifacts = append(metadata.Artifacts, Artifact{Name: "eva-offline", File: offlineName, SHA256: offlineDigest})
	contents, err := yaml.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode Airgap release metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(staging, "release.yaml"), contents, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staging, "README.md"), []byte(airgapReadme), 0o644); err != nil {
		return err
	}
	if err := writeAirgapChecksums(staging, metadata); err != nil {
		return err
	}
	archive, err := os.CreateTemp(parent, ".eva-airgap-bundle-*.tar.gz")
	if err != nil {
		return fmt.Errorf("create temporary Airgap Bundle: %w", err)
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	if err := writeAirgapArchive(archive, staging); err != nil {
		return fmt.Errorf("write Airgap Bundle: %w", err)
	}
	if err := archive.Sync(); err != nil {
		return fmt.Errorf("sync Airgap Bundle: %w", err)
	}
	if err := archive.Close(); err != nil {
		return fmt.Errorf("close Airgap Bundle: %w", err)
	}
	if _, err := VerifyAirgapBundle(archive.Name()); err != nil {
		return fmt.Errorf("verify built Airgap Bundle: %w", err)
	}
	// The temporary archive is on the destination filesystem. Link is atomic
	// and fails if another publisher creates any entry at destination meanwhile.
	if err := os.Link(archive.Name(), destination); err != nil {
		return fmt.Errorf("publish Airgap Bundle without replacing destination: %w", err)
	}
	return nil
}

func validAirgapArtifactFilename(name string) bool {
	return airgapFilenamePattern.MatchString(name) && !strings.Contains(name, "..") &&
		name != "release.yaml" && name != "checksums.sha256" && name != "README.md" && name != "artifacts"
}

func validateAirgapSourceArtifact(name string) error {
	if name == "" || filepath.IsAbs(name) || filepath.ToSlash(filepath.Clean(name)) != name || strings.Contains(name, "\\") {
		return fmt.Errorf("unsafe artifact path %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		lower := strings.ToLower(part)
		switch lower {
		case "..", "workspace", "credentials", "secrets", ".ssh", ".aws", ".docker":
			return fmt.Errorf("customer workspace/credential path is not an artifact: %q", name)
		}
		if lower == ".env" || strings.HasPrefix(lower, ".env.") || lower == "aws_key.ini" ||
			strings.HasPrefix(lower, "id_rsa") || strings.HasPrefix(lower, "id_ed25519") ||
			strings.HasSuffix(lower, ".pem") || strings.HasSuffix(lower, ".key") {
			return fmt.Errorf("credential file is not an artifact: %q", name)
		}
	}
	if !validAirgapArtifactFilename(filepath.Base(name)) {
		return fmt.Errorf("unsafe artifact filename %q", name)
	}
	return nil
}

// Reject links in every path component, not just the final file. Files are also
// checked through the open descriptor so replacement by a link cannot be copied.
func openAirgapRegularFile(path string) (*os.File, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	for parent := filepath.Dir(abs); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("not a non-symlink directory: %s", parent)
		}
		if parent == filepath.Dir(parent) {
			break
		}
	}
	file, err := os.OpenFile(abs, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil {
		if stat, ok := info.Sys().(*syscall.Stat_t); !info.Mode().IsRegular() || !ok || stat.Nlink != 1 {
			err = fmt.Errorf("not a regular non-symlink/non-hardlink file: %s", path)
		}
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func checkAirgapRegularFile(path string) error {
	file, err := openAirgapRegularFile(path)
	if err != nil {
		return err
	}
	return file.Close()
}

func copyAirgapArtifact(source, destination string, mode os.FileMode) (string, error) {
	input, err := openAirgapRegularFile(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	hash := sha256.New()
	if err := copyArchiveFile(io.TeeReader(input, hash), destination, mode); err != nil {
		return "", err
	}
	if err := os.Chmod(destination, mode); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeAirgapChecksums(root string, metadata Metadata) error {
	digests := make(map[string]string, len(metadata.Artifacts)+2)
	for _, artifact := range metadata.Artifacts {
		digests[artifact.File] = artifact.SHA256
	}
	for _, name := range []string{"release.yaml", "README.md"} {
		digest, err := fileChecksum(filepath.Join(root, name))
		if err != nil {
			return err
		}
		digests[name] = digest
	}
	names := make([]string, 0, len(digests))
	for name := range digests {
		names = append(names, name)
	}
	sort.Strings(names)
	manifest, err := os.OpenFile(filepath.Join(root, "checksums.sha256"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer manifest.Close()
	for _, name := range names {
		if _, err := fmt.Fprintf(manifest, "%s  %s\n", digests[name], name); err != nil {
			return err
		}
	}
	return manifest.Close()
}

func writeAirgapArchive(output io.Writer, root string) error {
	entries, err := os.ReadDir(root) // ReadDir returns entries sorted by filename.
	if err != nil {
		return err
	}
	compressed := gzip.NewWriter(output)
	defer compressed.Close()
	archive := tar.NewWriter(compressed)
	defer archive.Close()
	for _, entry := range entries {
		if err := appendAirgapArchiveFile(archive, root, entry.Name()); err != nil {
			return err
		}
	}
	if err := archive.Close(); err != nil {
		return err
	}
	return compressed.Close()
}

func appendAirgapArchiveFile(archive *tar.Writer, root, name string) error {
	file, err := openAirgapRegularFile(filepath.Join(root, name))
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	header := &tar.Header{Name: name, Mode: int64(info.Mode().Perm()), Size: info.Size(), Typeflag: tar.TypeReg}
	if err := archive.WriteHeader(header); err != nil {
		return err
	}
	_, err = io.Copy(archive, file)
	return err
}
