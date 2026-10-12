package remote

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"eva-deployer/tools/eva/internal/release"
	"gopkg.in/yaml.v3"
)

const payloadSchemaVersion = "v1"
const targetPayloadDirectory = "remote-payload"
const DefaultTargetPayloadRoot = "/var/lib/eva/artifacts/remote"

var payloadCacheRoots = []string{
	"apt", "docker", "nvidia", "cuda", "helm", "k3s", "k8s", "nfs",
	"eva-app", "eva-vision", "eva-agent", "eva-iam", "qdrant", "tools",
	"display_mode_selector", "models", "python-debs", "wheels",
}

type PayloadManifest struct {
	SchemaVersion string              `yaml:"schema_version"`
	Release       PreparationIdentity `yaml:"release"`
	Repository    RepositoryIdentity  `yaml:"repository"`
	Platform      string              `yaml:"platform"`
	Identity      string              `yaml:"identity"`
	Archive       string              `yaml:"archive"`
	ArchiveSHA256 string              `yaml:"archive_sha256"`
	ContentSHA256 string              `yaml:"content_sha256"`
	Categories    []string            `yaml:"categories"`
	CreatedAt     time.Time           `yaml:"created_at"`
}

type PayloadSource struct {
	Directory string
	Manifest  PayloadManifest
}

// BuildTargetPayload creates a deterministic, verified archive from the
// managed preparation cache. Image and Qdrant snapshot payloads deliberately
// remain outside this archive because Main Harbor supplies them.
func BuildTargetPayload(
	preparationRoot string,
	cacheRoot string,
	identity PreparationIdentity,
	platform string,
) (PayloadSource, error) {
	return BuildTargetPayloadWithProgress(
		preparationRoot,
		cacheRoot,
		identity,
		platform,
		io.Discard,
	)
}

func BuildTargetPayloadWithProgress(
	preparationRoot string,
	cacheRoot string,
	identity PreparationIdentity,
	platform string,
	progress io.Writer,
) (PayloadSource, error) {
	if progress == nil {
		progress = io.Discard
	}
	if err := validateIdentity(identity); err != nil {
		return PayloadSource{}, err
	}
	if platform == "" {
		return PayloadSource{}, errors.New("payload platform is required")
	}
	if err := ValidateOfflineAssets(cacheRoot); err != nil {
		return PayloadSource{}, fmt.Errorf("validate payload offline assets: %w", err)
	}
	if err := ValidateModels(cacheRoot); err != nil {
		return PayloadSource{}, fmt.Errorf("validate payload models: %w", err)
	}
	key := payloadIdentityKey(identity)
	final := filepath.Join(preparationRoot, targetPayloadDirectory, key)
	if _, err := os.Lstat(final); err == nil {
		payload, loadErr := LoadTargetPayload(final, identity)
		if loadErr != nil {
			return PayloadSource{}, fmt.Errorf("existing target payload is invalid: %w", loadErr)
		}
		return payload, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return PayloadSource{}, fmt.Errorf("read target payload destination: %w", err)
	}
	parent := filepath.Dir(final)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return PayloadSource{}, fmt.Errorf("create target payload directory: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".payload-")
	if err != nil {
		return PayloadSource{}, fmt.Errorf("create target payload staging: %w", err)
	}
	defer os.RemoveAll(staging)
	archiveName := fmt.Sprintf("remote-payload_%s_%s.tar.gz", identity.ReleaseVersion, key)
	archivePath := filepath.Join(staging, archiveName)
	contentDigest, err := writePayloadArchive(
		cacheRoot,
		archivePath,
		progress,
	)
	if err != nil {
		return PayloadSource{}, err
	}
	archiveInfo, err := os.Stat(archivePath)
	if err != nil {
		return PayloadSource{}, fmt.Errorf(
			"inspect target payload archive: %w",
			err,
		)
	}
	fmt.Fprintf(
		progress,
		"[OK] Target payload archive written: bytes=%d\n",
		archiveInfo.Size(),
	)
	fmt.Fprintln(
		progress,
		"[INFO] Validating Target payload archive",
	)
	archiveDigest, err := regularFileSHA256(archivePath)
	if err != nil {
		return PayloadSource{}, fmt.Errorf("digest target payload archive: %w", err)
	}
	manifest := PayloadManifest{SchemaVersion: payloadSchemaVersion, Release: identity, Repository: RepositoryIdentity{Registry: identity.RepositoryRegistry, Project: identity.RepositoryProject}, Platform: platform, Identity: key, Archive: archiveName, ArchiveSHA256: archiveDigest, ContentSHA256: contentDigest, Categories: []string{"offline-assets", "models"}, CreatedAt: time.Now().UTC()}
	if err := writePayloadManifest(filepath.Join(staging, "manifest.yaml"), manifest); err != nil {
		return PayloadSource{}, err
	}
	if err := writePayloadChecksums(filepath.Join(staging, "checksums.sha256"), archiveDigest, archiveName); err != nil {
		return PayloadSource{}, err
	}
	if _, err := LoadTargetPayload(staging, identity); err != nil {
		return PayloadSource{}, fmt.Errorf(
			"validate staged target payload: %w",
			err,
		)
	}
	fmt.Fprintln(
		progress,
		"[OK] Target payload archive validated",
	)
	if err := os.Rename(staging, final); err != nil {
		return PayloadSource{}, fmt.Errorf("publish target payload: %w", err)
	}
	// The staged payload was fully verified above; rename does not change it.
	return InspectTargetPayload(final, identity)
}

func TargetPayloadPath(preparationRoot string, identity PreparationIdentity) string {
	return filepath.Join(preparationRoot, targetPayloadDirectory, payloadIdentityKey(identity))
}

// LoadTargetPayload fully verifies a Target payload, including both archive
// digests. Prepare calls it once when it builds the payload; later consumers
// use InspectTargetPayload instead of rehashing tens of gigabytes.
func LoadTargetPayload(directory string, identity PreparationIdentity) (PayloadSource, error) {
	source, err := InspectTargetPayload(directory, identity)
	if err != nil {
		return PayloadSource{}, err
	}
	manifest := source.Manifest
	archivePath := filepath.Join(directory, manifest.Archive)
	if digest, err := regularFileSHA256(archivePath); err != nil || digest != manifest.ArchiveSHA256 {
		if err != nil {
			return PayloadSource{}, err
		}
		return PayloadSource{}, errors.New("target payload archive digest does not match manifest")
	}
	if digest, err := payloadArchiveDigest(archivePath); err != nil || digest != manifest.ContentSHA256 {
		if err != nil {
			return PayloadSource{}, err
		}
		return PayloadSource{}, errors.New("target payload content digest does not match manifest")
	}
	return source, nil
}

// InspectTargetPayload checks the recorded payload evidence without reading
// the archive: manifest, identity, directory layout and checksum manifest.
// The archive digest itself was verified by prepare and is verified again by
// the publish backend and on the Target after transfer.
func InspectTargetPayload(directory string, identity PreparationIdentity) (PayloadSource, error) {
	manifestPath := filepath.Join(directory, "manifest.yaml")
	info, err := os.Lstat(manifestPath)
	if err != nil {
		return PayloadSource{}, fmt.Errorf("read target payload manifest: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return PayloadSource{}, errors.New("target payload manifest must be a regular non-symlink file")
	}
	contents, err := os.ReadFile(manifestPath)
	if err != nil {
		return PayloadSource{}, err
	}
	if err := rejectSecretContent(strings.Split(string(contents), "\n")); err != nil {
		return PayloadSource{}, err
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(contents)))
	decoder.KnownFields(true)
	var manifest PayloadManifest
	if err := decoder.Decode(&manifest); err != nil {
		return PayloadSource{}, fmt.Errorf("parse target payload manifest: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return PayloadSource{}, errors.New("target payload manifest must contain exactly one YAML document")
		}
		return PayloadSource{}, err
	}
	if err := validatePayloadManifest(manifest, identity); err != nil {
		return PayloadSource{}, err
	}
	if err := validatePayloadDirectory(directory, manifest.Archive); err != nil {
		return PayloadSource{}, err
	}
	if err := validatePayloadChecksums(directory, manifest); err != nil {
		return PayloadSource{}, err
	}
	if err := requireRecordedArchive(filepath.Join(directory, manifest.Archive)); err != nil {
		return PayloadSource{}, fmt.Errorf("target payload archive: %w", err)
	}
	return PayloadSource{Directory: directory, Manifest: manifest}, nil
}

func requireRecordedArchive(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("archive must be a non-empty regular non-symlink file")
	}
	return nil
}

func validatePayloadDirectory(directory, archive string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	want := map[string]bool{"manifest.yaml": true, "checksums.sha256": true, archive: true}
	if len(entries) != len(want) {
		return errors.New("target payload directory has unexpected entries")
	}
	for _, entry := range entries {
		if !want[entry.Name()] {
			return errors.New("target payload directory has unexpected entries")
		}
		info, err := os.Lstat(filepath.Join(directory, entry.Name()))
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("target payload directory entries must be regular non-symlink files")
		}
	}
	return nil
}

// MaterializeTargetPayload safely extracts a verified payload into the cache
// consumed by existing offline roles. It never overwrites a different identity.
// MaterializeTargetPayload extracts the published payload into the managed
// Target cache once. The archive is read a single time: extraction computes
// both the archive and the content digest, and the staged tree is published
// only when both match the manifest bound to the delivery marker.
func MaterializeTargetPayload(releaseResolved release.Resolved, registry, project, artifactRoot string, progress io.Writer) (string, error) {
	identity, err := BuildPreparationIdentity(releaseResolved, registry, project)
	if err != nil {
		return "", err
	}
	_, payload, err := checkPublishedRemoteDelivery(releaseResolved, registry, project, false)
	if err != nil {
		return "", err
	}
	if progress == nil {
		progress = io.Discard
	}
	if artifactRoot == "" {
		artifactRoot = DefaultTargetPayloadRoot
	}
	destination := filepath.Join(artifactRoot, identity.ReleaseVersion, payload.Manifest.Identity)
	cacheDestination := filepath.Join(destination, "cache")
	archive := filepath.Join(payload.Directory, payload.Manifest.Archive)
	if _, err := os.Lstat(destination); err == nil {
		if err := validateMaterializedCache(destination, payload.Manifest); err != nil {
			return "", fmt.Errorf("existing materialized payload is invalid: %w", err)
		}
		if err := requireMaterializedSource(destination, archive, payload.Manifest); err != nil {
			return "", err
		}
		return cacheDestination, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(filepath.Dir(destination), ".materialize-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	fmt.Fprintln(progress, "[INFO] Verifying and extracting the published Target payload")
	archiveDigest, contentDigest, err := extractPayloadArchive(archive, staging, progress)
	if err != nil {
		return "", err
	}
	if archiveDigest != payload.Manifest.ArchiveSHA256 {
		return "", errors.New("published target payload archive digest does not match manifest")
	}
	if contentDigest != payload.Manifest.ContentSHA256 {
		return "", errors.New("published target payload content digest does not match manifest")
	}
	fmt.Fprintln(progress, "[OK] Target payload verified and extracted")
	if err := validateMaterializedCache(staging, payload.Manifest); err != nil {
		return "", err
	}
	if err := writeMaterializedSource(staging, archive, archiveDigest); err != nil {
		return "", err
	}
	if err := os.Rename(staging, destination); err != nil {
		return "", fmt.Errorf("publish materialized target payload: %w", err)
	}
	return cacheDestination, nil
}

func payloadIdentityKey(identity PreparationIdentity) string {
	value := strings.Join([]string{identity.ReleaseVersion, identity.Platform, identity.ReleaseYAMLSHA256, identity.ChecksumsSHA256, identity.RepositoryRegistry, identity.RepositoryProject}, "\x00")
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:16])
}

func writePayloadArchive(
	cacheRoot string,
	archivePath string,
	progress io.Writer,
) (string, error) {
	entries, err := payloadEntries(cacheRoot)
	if err != nil {
		return "", err
	}
	if progress == nil {
		progress = io.Discard
	}

	totalFiles := 0
	var totalBytes int64
	for _, entry := range entries {
		info, err := os.Lstat(
			filepath.Join(cacheRoot, entry),
		)
		if err != nil {
			return "", err
		}
		if info.Mode().IsRegular() {
			totalFiles++
			totalBytes += info.Size()
		}
	}

	fmt.Fprintf(
		progress,
		"[INFO] Building Target payload: files=%d bytes=%d compression=gzip-best-speed\n",
		totalFiles,
		totalBytes,
	)

	file, err := os.OpenFile(
		archivePath,
		os.O_WRONLY|os.O_CREATE|os.O_EXCL,
		0o640,
	)
	if err != nil {
		return "", err
	}
	gzipWriter, err := gzip.NewWriterLevel(
		file,
		gzip.BestSpeed,
	)
	if err != nil {
		file.Close()
		return "", err
	}
	gzipWriter.Header.ModTime = time.Unix(0, 0)
	gzipWriter.Header.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	hash := sha256.New()
	processedFiles := 0
	var processedBytes int64
	var lastReportedBytes int64
	const progressByteInterval int64 = 1 << 30

	for _, entry := range entries {
		info, err := os.Lstat(
			filepath.Join(cacheRoot, entry),
		)
		if err != nil {
			tarWriter.Close()
			gzipWriter.Close()
			file.Close()
			return "", err
		}

		if err := writePayloadEntry(
			tarWriter,
			hash,
			cacheRoot,
			entry,
		); err != nil {
			tarWriter.Close()
			gzipWriter.Close()
			file.Close()
			return "", err
		}

		if !info.Mode().IsRegular() {
			continue
		}

		processedFiles++
		processedBytes += info.Size()

		if processedBytes-lastReportedBytes >=
			progressByteInterval ||
			processedFiles == totalFiles {
			percent := float64(100)
			if totalBytes > 0 {
				percent = float64(processedBytes) *
					100 /
					float64(totalBytes)
			}
			fmt.Fprintf(
				progress,
				"[INFO] Target payload progress: files=%d/%d bytes=%d/%d percent=%.1f\n",
				processedFiles,
				totalFiles,
				processedBytes,
				totalBytes,
				percent,
			)
			lastReportedBytes = processedBytes
		}
	}
	if err := tarWriter.Close(); err != nil {
		gzipWriter.Close()
		file.Close()
		return "", err
	}
	if err := gzipWriter.Close(); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func payloadEntries(cacheRoot string) ([]string, error) {
	entries := []string{}
	// The top-level offline manifest is consumed alongside the package and
	// host-asset directories, but is not itself a category directory.
	manifest := filepath.Join(cacheRoot, "manifest.txt")
	if info, err := os.Lstat(manifest); err != nil {
		return nil, fmt.Errorf("read target payload offline manifest: %w", err)
	} else if info.Mode()&os.ModeSymlink != 0 ||
		!info.Mode().IsRegular() {
		return nil, errors.New("unsafe target payload offline manifest")
	} else if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink > 1 {
		return nil, errors.New("hard-linked target payload offline manifest")
	}
	entries = append(entries, "manifest.txt")
	for _, root := range payloadCacheRoots {
		path := filepath.Join(cacheRoot, root)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		err := filepath.Walk(path, func(name string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(cacheRoot, name)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 ||
				!info.IsDir() && !info.Mode().IsRegular() {
				return fmt.Errorf(
					"unsafe target payload source: %s",
					relative,
				)
			}
			if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink > 1 && info.Mode().IsRegular() {
				return fmt.Errorf("hard-linked target payload source: %s", relative)
			}
			entries = append(entries, relative)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(entries)
	if len(entries) == 0 {
		return nil, errors.New("target payload has no allowed cache entries")
	}
	return entries, nil
}

func writePayloadEntry(writer *tar.Writer, digest io.Writer, cacheRoot, relative string) error {
	full := filepath.Join(cacheRoot, relative)
	info, err := os.Lstat(full)
	if err != nil {
		return err
	}
	name := path.Join("cache", filepath.ToSlash(relative))
	if strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
		return errors.New("unsafe target payload archive entry")
	}
	header := &tar.Header{Name: name, Mode: int64(info.Mode().Perm()), ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
	if info.IsDir() {
		header.Typeflag = tar.TypeDir
		header.Name += "/"
		return writer.WriteHeader(header)
	}
	contents, rewritten, err := targetPayloadContents(cacheRoot, relative)
	if err != nil {
		return err
	}
	header.Typeflag, header.Size = tar.TypeReg, info.Size()
	if rewritten {
		header.Size = int64(len(contents))
	}
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	if rewritten {
		if _, err := writer.Write(contents); err != nil {
			return err
		}
		fileDigest := sha256.Sum256(contents)
		_, err := fmt.Fprintf(digest, "%x %s\n", fileDigest, name)
		return err
	}
	file, err := os.Open(full)
	if err != nil {
		return err
	}
	defer file.Close()
	fileDigest, err := regularFileSHA256(full)
	if err != nil {
		return err
	}
	if _, err := io.Copy(writer, file); err != nil {
		return err
	}
	_, err = fmt.Fprintf(digest, "%s %s\n", fileDigest, name)
	return err
}

// The preparation-time model manifest lists absolute Main-host paths. Rewrite
// only its file references so it remains valid below the Target cache root;
// no other preparation report or host path is transported.
func targetPayloadContents(cacheRoot, relative string) ([]byte, bool, error) {
	if filepath.ToSlash(relative) != "models/manifest.txt" {
		return nil, false, nil
	}
	contents, err := os.ReadFile(filepath.Join(cacheRoot, relative))
	if err != nil {
		return nil, false, err
	}
	lines := strings.Split(string(contents), "\n")
	inFiles := false
	for index, line := range lines {
		if line == "files:" {
			inFiles = true
			continue
		}
		if !inFiles || strings.TrimSpace(line) == "" {
			continue
		}
		modelPath, err := manifestPathRelative(cacheRoot, line)
		if err != nil {
			return nil, false, fmt.Errorf("rewrite model manifest: %w", err)
		}
		if !strings.HasPrefix(filepath.ToSlash(modelPath), "models/") {
			return nil, false, errors.New("rewrite model manifest: model file is outside model cache")
		}
		lines[index] = "cache/" + filepath.ToSlash(modelPath)
	}
	return []byte(strings.Join(lines, "\n")), true, nil
}

func validatePayloadManifest(manifest PayloadManifest, identity PreparationIdentity) error {
	if manifest.SchemaVersion != payloadSchemaVersion || manifest.Platform != identity.Platform || manifest.Identity != payloadIdentityKey(identity) || manifest.Archive == "" || filepath.Base(manifest.Archive) != manifest.Archive || !sha256Pattern.MatchString(manifest.ArchiveSHA256) || !sha256Pattern.MatchString(manifest.ContentSHA256) || len(manifest.Categories) != 2 || manifest.Categories[0] != "offline-assets" || manifest.Categories[1] != "models" || manifest.CreatedAt.IsZero() {
		return errors.New("target payload manifest is invalid")
	}
	actual := manifest.Release
	actual.RepositoryRegistry, actual.RepositoryProject = manifest.Repository.Registry, manifest.Repository.Project
	if actual != identity {
		return errors.New("target payload identity does not match Release or repository")
	}
	return nil
}
func writePayloadManifest(path string, manifest PayloadManifest) error {
	contents, err := yaml.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := rejectSecretContent(strings.Split(string(contents), "\n")); err != nil {
		return err
	}
	return writePayloadFile(path, contents)
}
func writePayloadChecksums(path, digest, archive string) error {
	return writePayloadFile(path, []byte(digest+"  "+archive+"\n"))
}
func writePayloadFile(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	if _, err = file.Write(contents); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}
func validatePayloadChecksums(directory string, manifest PayloadManifest) error {
	lines, err := readLines(directory, "checksums.sha256", true)
	if err != nil {
		return err
	}
	if len(lines) != 2 || strings.TrimSpace(lines[1]) != "" {
		return errors.New("target payload checksum manifest is invalid")
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 2 || fields[0] != manifest.ArchiveSHA256 || fields[1] != manifest.Archive {
		return errors.New("target payload checksum manifest does not match archive")
	}
	return nil
}

func payloadArchiveDigest(archive string) (string, error) {
	reader, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	gzipReader, err := gzip.NewReader(reader)
	if err != nil {
		return "", err
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	digest := sha256.New()
	saw := false
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if err := validatePayloadArchiveHeader(header); err != nil {
			return "", err
		}
		if header.Typeflag == tar.TypeReg {
			content := sha256.New()
			if _, err := io.Copy(content, tarReader); err != nil {
				return "", err
			}
			if _, err := fmt.Fprintf(digest, "%s %s\n", hex.EncodeToString(content.Sum(nil)), header.Name); err != nil {
				return "", err
			}
			saw = true
		}
	}
	if !saw {
		return "", errors.New("target payload archive has no files")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
func validatePayloadArchiveHeader(header *tar.Header) error {
	if header.Typeflag == tar.TypeDir && header.Name == "cache/" {
		return nil
	}
	name := path.Clean(header.Name)
	if name == "." ||
		strings.HasPrefix(name, "/") ||
		strings.HasPrefix(name, "../") ||
		name != header.Name && name+"/" != header.Name ||
		!strings.HasPrefix(name, "cache/") ||
		(header.Typeflag != tar.TypeReg &&
			header.Typeflag != tar.TypeDir) {
		return fmt.Errorf(
			"unsafe target payload archive entry %q",
			header.Name,
		)
	}
	return nil
}

// materializedSourceFile records which published archive a materialized cache
// was verified from. Reuse compares the archive's size and modification time
// instead of rehashing it, so a replaced or rewritten source still fails closed.
const materializedSourceFile = ".eva-payload-source.yaml"

type materializedSource struct {
	ArchiveSHA256 string `yaml:"archive_sha256"`
	Size          int64  `yaml:"size"`
	ModifiedUnix  int64  `yaml:"modified_unix_nano"`
}

func archiveStamp(archive string) (int64, int64, error) {
	info, err := os.Lstat(archive)
	if err != nil {
		return 0, 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return 0, 0, errors.New("published target payload archive must be a regular non-symlink file")
	}
	return info.Size(), info.ModTime().UnixNano(), nil
}

func writeMaterializedSource(root, archive, archiveDigest string) error {
	size, modified, err := archiveStamp(archive)
	if err != nil {
		return err
	}
	contents, err := yaml.Marshal(materializedSource{ArchiveSHA256: archiveDigest, Size: size, ModifiedUnix: modified})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, materializedSourceFile), contents, 0o640)
}

func requireMaterializedSource(root, archive string, manifest PayloadManifest) error {
	contents, err := os.ReadFile(filepath.Join(root, materializedSourceFile))
	if errors.Is(err, os.ErrNotExist) {
		// Materialized by an earlier Tool that kept no source record: verify
		// the published archive in full once, then record it.
		if digest, err := regularFileSHA256(archive); err != nil || digest != manifest.ArchiveSHA256 {
			return errors.New("published target payload archive does not match the materialized payload")
		}
		if digest, err := payloadArchiveDigest(archive); err != nil || digest != manifest.ContentSHA256 {
			return errors.New("published target payload content does not match the materialized payload")
		}
		return writeMaterializedSource(root, archive, manifest.ArchiveSHA256)
	}
	if err != nil {
		return fmt.Errorf("read materialized payload source record: %w", err)
	}
	var source materializedSource
	if err := yaml.Unmarshal(contents, &source); err != nil {
		return fmt.Errorf("parse materialized payload source record: %w", err)
	}
	size, modified, err := archiveStamp(archive)
	if err != nil {
		return err
	}
	if source.ArchiveSHA256 != manifest.ArchiveSHA256 || source.Size != size || source.ModifiedUnix != modified {
		return errors.New("published target payload archive changed since it was verified and materialized")
	}
	return nil
}

// extractPayloadArchive extracts a payload archive into destination and
// returns the SHA-256 of the archive file and the payload content digest (the
// same value payloadArchiveDigest computes) from one read of the archive.
func extractPayloadArchive(archive, destination string, progress io.Writer) (string, string, error) {
	file, err := os.Open(archive)
	if err != nil {
		return "", "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", "", err
	}
	if progress == nil {
		progress = io.Discard
	}
	archiveHash := sha256.New()
	counted := &progressReader{reader: io.TeeReader(file, archiveHash), total: info.Size(), progress: progress}
	gzipReader, err := gzip.NewReader(counted)
	if err != nil {
		return "", "", err
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	contentDigest := sha256.New()
	saw := false
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", "", err
		}
		if err := validatePayloadArchiveHeader(header); err != nil {
			return "", "", err
		}
		target := filepath.Join(destination, filepath.FromSlash(path.Clean(header.Name)))
		if header.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return "", "", err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return "", "", err
		}
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(header.Mode)&0o755)
		if err != nil {
			return "", "", err
		}
		content := sha256.New()
		if _, err := io.Copy(io.MultiWriter(output, content), tarReader); err != nil {
			output.Close()
			return "", "", err
		}
		if err := output.Close(); err != nil {
			return "", "", err
		}
		if _, err := fmt.Fprintf(contentDigest, "%s %s\n", hex.EncodeToString(content.Sum(nil)), header.Name); err != nil {
			return "", "", err
		}
		saw = true
	}
	if !saw {
		return "", "", errors.New("target payload archive has no files")
	}
	// gzip may stop before trailing bytes; the archive digest covers the file.
	if _, err := io.Copy(io.Discard, counted); err != nil {
		return "", "", err
	}
	counted.report(true)
	return hex.EncodeToString(archiveHash.Sum(nil)), hex.EncodeToString(contentDigest.Sum(nil)), nil
}

// progressReader reports how much of a large archive has been read, at most
// once per progressReportStep bytes, so a long extraction is visibly alive.
type progressReader struct {
	reader   io.Reader
	total    int64
	read     int64
	reported int64
	progress io.Writer
}

const progressReportStep = int64(1) << 30

func (reader *progressReader) Read(buffer []byte) (int, error) {
	count, err := reader.reader.Read(buffer)
	reader.read += int64(count)
	reader.report(false)
	return count, err
}

func (reader *progressReader) report(final bool) {
	if reader.total <= 0 || (!final && reader.read-reader.reported < progressReportStep) || reader.reported == reader.read {
		return
	}
	reader.reported = reader.read
	fmt.Fprintf(reader.progress, "[INFO] Target payload progress: bytes=%d/%d percent=%.1f\n", reader.read, reader.total, float64(reader.read)*100/float64(reader.total))
}

func validateMaterializedCache(root string, manifest PayloadManifest) error {
	cacheRoot := filepath.Join(root, "cache")
	if err := ValidateOfflineAssets(cacheRoot); err != nil {
		return err
	}
	if err := ValidateModels(cacheRoot); err != nil {
		return err
	}
	return nil
}
