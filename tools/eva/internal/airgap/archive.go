package airgap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"eva-deployer/tools/eva/internal/remote"
)

const archiveJSONLimit = 16 << 20

// buildOfflineArchive joins the exported runtime and the download cache without
// following links or transporting host ownership, extended attributes or secrets.
// A failed build never leaves a partially usable destination behind.
func buildOfflineArchive(runtimeArchive, cacheRoot, destination string) (resultErr error) {
	if runtimeArchive == "" || cacheRoot == "" || destination == "" {
		return errors.New("runtime archive, cache root and destination are required")
	}
	root, err := filepath.Abs(cacheRoot)
	if err != nil {
		return err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("cache root must be a non-symlink directory")
	}
	// Older/offline-only caches may lack Vision weights. If present, never
	// archive a tree that would fail validation after target-side restoration.
	if _, err := os.Lstat(filepath.Join(root, "vision-models")); err == nil {
		if err := remote.ValidateVisionModels(cacheRoot); err != nil {
			return fmt.Errorf("validate Vision cache before archive: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	output, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	if relative, err := filepath.Rel(root, output); err != nil {
		return err
	} else if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("offline archive destination must be outside the cache")
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create offline archive: %w", err)
	}
	defer func() {
		if err := file.Close(); resultErr == nil && err != nil {
			resultErr = err
		}
		if resultErr != nil {
			_ = os.Remove(output)
		}
	}()
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	if err := copyOfflineRuntime(tw, runtimeArchive); err != nil {
		return fmt.Errorf("copy runtime: %w", err)
	}
	if err := filepath.Walk(root, func(full string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if full == root {
			return nil
		}
		relative, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		name, err := safeArchiveName(filepath.ToSlash(relative))
		if err != nil {
			return err
		}
		if err := checkArchiveSource(info); err != nil {
			return fmt.Errorf("cache entry %q: %w", name, err)
		}
		// The model backend intentionally leaves its flock inode in the cache
		// parent. It is empty process coordination state, not an offline asset.
		// Never archive it, and never treat nonempty data as a lock to hide it.
		if name == ".vision-models.lock" {
			if !info.Mode().IsRegular() || info.Size() != 0 {
				return errors.New("Vision cache lock must be an empty regular file")
			}
			return nil
		}
		// Helm rendering includes Kubernetes Secrets; none of this subtree is
		// an offline asset. In particular, do not inspect/copy its secret data.
		if name == "images/rendered" {
			if !info.IsDir() {
				return errors.New("images/rendered must be a directory")
			}
			return filepath.SkipDir
		}
		if err := rejectArchiveCredentialName(name); err != nil {
			return err
		}
		top, _, nested := strings.Cut(name, "/")
		if !offlineCacheDirectory(top) {
			if nested || info.IsDir() || top != "manifest.txt" && top != "airgap-preparation.yaml" {
				return fmt.Errorf("unsupported cache entry %q", name)
			}
		} else if !nested && !info.IsDir() {
			return fmt.Errorf("cache category %q must be a directory", name)
		}
		return writeOfflineCacheEntry(tw, full, "out/cache/"+name, root, cacheRoot, info)
	}); err != nil {
		return fmt.Errorf("copy cache: %w", err)
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func offlineCacheDirectory(name string) bool {
	switch name {
	case "aws", "apt", "docker", "nvidia", "cuda", "helm", "k3s", "k8s", "nfs",
		"eva-app", "eva-vision", "eva-agent", "eva-iam", "qdrant", "tools", "images",
		"display_mode_selector", "models", "python-debs", "wheels", "qdrant-snapshots", "vision-models", "harbor":
		return true
	}
	return false
}

func safeArchiveName(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:\x00\r\n") {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("archive traversal path %q", name)
		}
	}
	return path.Clean(name), nil
}

func rejectArchiveCredentialName(name string) error {
	for _, part := range strings.Split(strings.ToLower(name), "/") {
		switch part {
		case ".aws", ".docker", ".ssh", "credentials", "secrets", "workspace", "aws_key.ini":
			return fmt.Errorf("credential/workspace path %q is forbidden", name)
		}
		if strings.HasSuffix(part, ".key") || strings.HasSuffix(part, ".pem") ||
			strings.HasPrefix(part, "id_rsa") || strings.HasPrefix(part, "id_ed25519") ||
			strings.HasPrefix(part, ".env") {
			return fmt.Errorf("credential path %q is forbidden", name)
		}
	}
	return nil
}

func checkArchiveSource(info os.FileInfo) error {
	if !info.IsDir() && !info.Mode().IsRegular() {
		return errors.New("links and special files are forbidden")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && info.Mode().IsRegular() && stat.Nlink > 1 {
		return errors.New("hard-linked regular files are forbidden")
	}
	return nil
}

// O_NONBLOCK prevents a concurrently substituted FIFO from hanging the build;
// O_NOFOLLOW and SameFile prevent a substituted final symlink/file being read.
func openArchiveSource(name string, info os.FileInfo) (*os.File, error) {
	var err error
	if info == nil {
		info, err = os.Lstat(name)
		if err != nil {
			return nil, err
		}
	}
	if err := checkArchiveSource(info); err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", name)
	}
	file, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	actual, err := file.Stat()
	if err == nil {
		err = checkArchiveSource(actual)
	}
	if err == nil && (!os.SameFile(info, actual) || !actual.Mode().IsRegular() || actual.Size() != info.Size()) {
		err = fmt.Errorf("archive source changed: %s", name)
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func cleanArchiveHeader(name string, mode, size int64, directory bool) *tar.Header {
	header := &tar.Header{Name: name, Mode: mode & 0o777, Size: size, Typeflag: tar.TypeReg,
		ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
	if directory {
		header.Name, header.Typeflag, header.Size = name+"/", tar.TypeDir, 0
	}
	return header
}

func checkArchiveHeader(header *tar.Header) (string, error) {
	name, err := safeArchiveName(header.Name)
	if err != nil {
		return "", err
	}
	if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA && header.Typeflag != tar.TypeDir ||
		header.Linkname != "" || header.Typeflag == tar.TypeDir && header.Size != 0 {
		return "", fmt.Errorf("links and special archive entries are forbidden: %q", header.Name)
	}
	for key := range header.PAXRecords {
		if strings.HasPrefix(key, "GNU.sparse.") {
			return "", fmt.Errorf("sparse archive entry is forbidden: %q", header.Name)
		}
	}
	return name, nil
}

func copyOfflineRuntime(tw *tar.Writer, archive string) error {
	file, err := openArchiveSource(archive, nil)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	entries := make(map[string]dockerArchiveFile)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name, err := checkArchiveHeader(header)
		if err != nil {
			return err
		}
		directory := header.Typeflag == tar.TypeDir
		if name != "runtime" && !strings.HasPrefix(name, "runtime/") || name == "runtime" && !directory {
			return fmt.Errorf("entry outside runtime: %q", header.Name)
		}
		// The exporter accepts only managed Runtime categories. Python's
		// bundled certifi CA certificates are public package data, not TLS keys.
		publicCA := strings.HasPrefix(name, "runtime/venv/") && strings.Contains(name, "/site-packages/") && strings.HasSuffix(name, "/certifi/cacert.pem")
		if err := rejectArchiveCredentialName(name); err != nil && !publicCA {
			return err
		}
		if _, exists := entries[name]; exists {
			return fmt.Errorf("duplicate runtime entry %q", name)
		}
		entries[name] = dockerArchiveFile{directory: directory}
		if err := tw.WriteHeader(cleanArchiveHeader(name, header.Mode, header.Size, directory)); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
	if err := checkArchiveParents(entries); err != nil {
		return err
	}
	// tar EOF precedes the gzip trailer. Closing gzip alone does NOT verify
	// its checksum, so always read through padding and the compressed EOF.
	return consumeArchivePadding(gz)
}

func writeOfflineCacheEntry(tw *tar.Writer, full, name, root, originalRoot string, info os.FileInfo) error {
	header := cleanArchiveHeader(name, int64(info.Mode().Perm()), info.Size(), info.IsDir())
	if info.IsDir() {
		return tw.WriteHeader(header)
	}
	file, err := openArchiveSource(full, info)
	if err != nil {
		return err
	}
	defer file.Close()
	if filepath.Base(full) == "manifest.txt" && !strings.HasPrefix(name, "out/cache/vision-models/") {
		// Count a streaming rewrite before emitting the tar header; even a
		// large model manifest never needs to reside in memory as a whole.
		prefix := root
		if !filepath.IsAbs(originalRoot) {
			prefix = filepath.Clean(originalRoot)
		}
		counter := &archiveByteCounter{}
		if err := rewriteCacheManifest(counter, file, root, prefix); err != nil {
			return err
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		header.Size = counter.size
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		return rewriteCacheManifest(tw, file, root, prefix)
	}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err = io.Copy(tw, file)
	return err
}

type archiveByteCounter struct{ size int64 }

func (c *archiveByteCounter) Write(data []byte) (int, error) {
	c.size += int64(len(data))
	return len(data), nil
}

// Replace cache-root prefixes anywhere in manifest text, including across read
// boundaries, but never rewrite a sibling such as /cache-other. Keep only one
// fixed-size chunk plus enough lookbehind for the longest prefix.
func rewriteCacheManifest(dst io.Writer, src io.Reader, roots ...string) error {
	var prefixes [][]byte
	longest := 0
	for _, root := range roots {
		if root == "" || root == "." || root == "/" {
			continue
		}
		prefixes = append(prefixes, []byte(root))
		if len(root) > longest {
			longest = len(root)
		}
	}
	buffer := make([]byte, 32<<10)
	var pending []byte
	for {
		n, readErr := src.Read(buffer)
		pending = append(pending, buffer[:n]...)
		end := len(pending)
		if readErr == nil {
			end -= longest
		}
		start := 0
		for i := 0; i < end; {
			matched := 0
			for _, prefix := range prefixes {
				if !bytes.HasPrefix(pending[i:], prefix) {
					continue
				}
				after := i + len(prefix)
				if after == len(pending) || strings.ContainsRune("/ \t\r\n\"'=:;,)]}", rune(pending[after])) {
					matched = len(prefix)
					break
				}
			}
			if matched == 0 {
				i++
				continue
			}
			if _, err := dst.Write(pending[start:i]); err != nil {
				return err
			}
			if _, err := io.WriteString(dst, "out/cache"); err != nil {
				return err
			}
			i += matched
			start = i
			if i > end {
				end = i
			}
		}
		if end > start {
			if _, err := dst.Write(pending[start:end]); err != nil {
				return err
			}
		}
		if end > 0 {
			pending = append(pending[:0], pending[end:]...)
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

type dockerArchiveFile struct {
	size      int64
	offset    int64
	digest    string
	directory bool
}

func checkArchiveParents(entries map[string]dockerArchiveFile) error {
	for name := range entries {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if entry, exists := entries[parent]; exists && !entry.directory {
				return fmt.Errorf("regular archive entry %q is also a parent directory", parent)
			}
		}
	}
	return nil
}

func consumeArchivePadding(reader io.Reader) error {
	buffer := make([]byte, 32<<10)
	for {
		n, err := reader.Read(buffer)
		for _, b := range buffer[:n] {
			if b != 0 {
				return errors.New("non-padding data after end of tar archive")
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

type dockerSaveManifestEntry struct {
	Config   string
	RepoTags []string
	Layers   []string
}

// validateDockerArchive hashes every regular member in one sequential pass.
// Only manifest.json and referenced configs are read into bounded buffers. The
// recorded body offsets permit small config reads even when manifest.json is
// last, without retaining layers or rereading multi-gigabyte layer payloads.
func validateDockerArchive(archivePath string, expected []string) error {
	file, err := openArchiveSource(archivePath, nil)
	if err != nil {
		return err
	}
	defer file.Close()
	tr := tar.NewReader(file)
	entries := make(map[string]dockerArchiveFile)
	var manifest []byte
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read docker-save archive: %w", err)
		}
		name, err := checkArchiveHeader(header)
		if err != nil {
			return err
		}
		if _, exists := entries[name]; exists {
			return fmt.Errorf("duplicate docker-save entry %q", name)
		}
		entry := dockerArchiveFile{size: header.Size, directory: header.Typeflag == tar.TypeDir}
		if name == "." && !entry.directory {
			return errors.New("docker-save root must be a directory")
		}
		if !entry.directory {
			entry.offset, err = file.Seek(0, io.SeekCurrent)
			if err != nil {
				return err
			}
			hash := sha256.New()
			if name == "manifest.json" {
				if header.Size > archiveJSONLimit {
					return errors.New("docker-save manifest.json exceeds 16 MiB")
				}
				manifest, err = io.ReadAll(io.TeeReader(tr, hash))
			} else {
				_, err = io.Copy(hash, tr)
			}
			if err != nil {
				return err
			}
			entry.digest = hex.EncodeToString(hash.Sum(nil))
		}
		entries[name] = entry
	}
	if err := consumeArchivePadding(file); err != nil {
		return err
	}
	if err := checkArchiveParents(entries); err != nil {
		return err
	}
	if len(manifest) == 0 {
		return errors.New("docker-save archive is missing manifest.json")
	}
	var images []dockerSaveManifestEntry
	if err := json.Unmarshal(manifest, &images); err != nil {
		return fmt.Errorf("parse docker-save manifest.json: %w", err)
	}
	if len(images) == 0 {
		return errors.New("docker-save manifest contains no images")
	}
	tags := make(map[string]bool)
	layerDiffIDs := make(map[string]string)
	for _, image := range images {
		configName, err := safeArchiveName(image.Config)
		if err != nil {
			return err
		}
		config, exists := entries[configName]
		if !exists || config.directory || config.size == 0 || config.size > archiveJSONLimit {
			return fmt.Errorf("missing, empty or oversized docker config %q", image.Config)
		}
		if digest := dockerConfigFilenameDigest(configName); digest != "" && digest != config.digest {
			return fmt.Errorf("docker config filename digest mismatch: %q", configName)
		}
		contents, err := io.ReadAll(io.NewSectionReader(file, config.offset, config.size))
		if err != nil {
			return err
		}
		digest := sha256.Sum256(contents)
		if int64(len(contents)) != config.size || hex.EncodeToString(digest[:]) != config.digest {
			return fmt.Errorf("docker config changed while validating: %q", configName)
		}
		var metadata struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			RootFS       *struct {
				DiffIDs []string `json:"diff_ids"`
			} `json:"rootfs"`
		}
		if err := json.Unmarshal(contents, &metadata); err != nil {
			return fmt.Errorf("parse docker config %q: %w", configName, err)
		}
		if metadata.OS != "linux" || metadata.Architecture != "amd64" {
			return fmt.Errorf("docker config %q must target linux/amd64", configName)
		}
		if metadata.RootFS == nil || len(metadata.RootFS.DiffIDs) != len(image.Layers) {
			return fmt.Errorf("docker config %q rootfs.diff_ids does not match Layers", configName)
		}
		for index, layer := range image.Layers {
			name, err := safeArchiveName(layer)
			if err != nil {
				return err
			}
			entry, exists := entries[name]
			if !exists || entry.directory || entry.size == 0 {
				return fmt.Errorf("missing or empty docker layer %q", layer)
			}
			diffID, checked := layerDiffIDs[name]
			if !checked {
				diffID = "sha256:" + entry.digest
				// containerd-backed Docker may export gzip-compressed OCI
				// blobs. Their content digest is not their uncompressed DiffID.
				var magic [2]byte
				if _, err := file.ReadAt(magic[:], entry.offset); err != nil {
					return err
				}
				if magic == [2]byte{0x1f, 0x8b} {
					compressed, err := gzip.NewReader(io.NewSectionReader(file, entry.offset, entry.size))
					if err != nil {
						return fmt.Errorf("open compressed docker layer %q: %w", layer, err)
					}
					hash := sha256.New()
					_, readErr := io.Copy(hash, compressed)
					closeErr := compressed.Close()
					if readErr != nil || closeErr != nil {
						return fmt.Errorf("invalid compressed docker layer %q", layer)
					}
					diffID = "sha256:" + hex.EncodeToString(hash.Sum(nil))
				}
				if strings.HasPrefix(name, "blobs/sha256/") && path.Base(name) != entry.digest {
					return fmt.Errorf("docker layer blob filename digest mismatch: %q", layer)
				}
				layerDiffIDs[name] = diffID
			}
			if metadata.RootFS.DiffIDs[index] != diffID {
				return fmt.Errorf("docker layer %q digest does not match rootfs.diff_ids", layer)
			}
		}
		for _, tag := range image.RepoTags {
			tags[tag] = true
		}
	}
	for _, reference := range expected {
		if reference == "" || !tags[reference] {
			return fmt.Errorf("docker-save archive is missing expected tag %q", reference)
		}
	}
	return nil
}

func dockerConfigFilenameDigest(name string) string {
	base := path.Base(name)
	if strings.HasSuffix(base, ".json") {
		base = strings.TrimSuffix(base, ".json")
	} else if !strings.HasPrefix(name, "blobs/sha256/") {
		return ""
	}
	if len(base) != sha256.Size*2 || strings.ToLower(base) != base {
		return ""
	}
	if _, err := hex.DecodeString(base); err != nil {
		return ""
	}
	return base
}
