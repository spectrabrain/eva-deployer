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
	"eva-deployer/tools/eva/internal/runtime"
	"gopkg.in/yaml.v3"
)

const runtimeArtifactDirectory = "runtime"

type RuntimeArtifactManifest struct {
	SchemaVersion string              `yaml:"schema_version"`
	Release       PreparationIdentity `yaml:"release"`
	Repository    RepositoryIdentity  `yaml:"repository"`
	Runtime       RuntimeArtifactInfo `yaml:"runtime"`
}

type RuntimeArtifactInfo struct {
	Version          string `yaml:"version"`
	Platform         string `yaml:"platform"`
	Archive          string `yaml:"archive"`
	ArchiveSHA256    string `yaml:"archive_sha256"`
	DescriptorSHA256 string `yaml:"descriptor_sha256"`
}

type RuntimeArtifactSource struct {
	Directory string
	Manifest  RuntimeArtifactManifest
}

type remoteDeliveryMarker struct {
	SchemaVersion         string `yaml:"schema_version"`
	ReleaseVersion        string `yaml:"release_version"`
	ReleaseYAMLSHA256     string `yaml:"release_yaml_sha256"`
	ChecksumsSHA256       string `yaml:"checksums_sha256"`
	PayloadManifestSHA256 string `yaml:"payload_manifest_sha256"`
	RuntimeManifestSHA256 string `yaml:"runtime_manifest_sha256"`
	Registry              string `yaml:"registry"`
	Project               string `yaml:"project"`
}

func RuntimeArtifactPath(root string, identity PreparationIdentity) string {
	return filepath.Join(root, runtimeArtifactDirectory, payloadIdentityKey(identity))
}

// BootstrapTargetRuntime supplies a missing Remote Target Runtime exclusively
// from the verified delivery artifact. It intentionally has no online or
// eva-offline Release fallback.
func BootstrapTargetRuntime(resolved release.Resolved, registry, project, runtimeRoot string) (runtime.Resolved, error) {
	artifact, _, err := ValidatePublishedRemoteDelivery(resolved, registry, project)
	if err != nil {
		return runtime.Resolved{}, fmt.Errorf("load published Remote Runtime artifact: %w", err)
	}
	installed, err := runtime.BootstrapOffline(filepath.Join(artifact.Directory, artifact.Manifest.Runtime.Archive), runtimeRoot)
	if err != nil {
		return runtime.Resolved{}, fmt.Errorf("bootstrap published Remote Runtime artifact: %w", err)
	}
	if installed.Descriptor.Version != artifact.Manifest.Runtime.Version {
		return runtime.Resolved{}, errors.New("published Remote Runtime version does not match installed Runtime")
	}
	return installed, nil
}

// ValidatePublishedRemoteDelivery is the Target-side, read-only trust-boundary
// check for the marker plus the separately published Runtime and payload.
func ValidatePublishedRemoteDelivery(resolved release.Resolved, registry, project string) (RuntimeArtifactSource, PayloadSource, error) {
	identity, err := BuildPreparationIdentity(resolved, registry, project)
	if err != nil {
		return RuntimeArtifactSource{}, PayloadSource{}, err
	}
	runtimeArtifact, err := LoadRuntimeArtifact(filepath.Join(resolved.Root, "remote-runtime"), identity)
	if err != nil {
		return RuntimeArtifactSource{}, PayloadSource{}, fmt.Errorf("load published Remote Runtime artifact: %w", err)
	}
	payload, err := LoadTargetPayload(filepath.Join(resolved.Root, targetPayloadDirectory), identity)
	if err != nil {
		return RuntimeArtifactSource{}, PayloadSource{}, fmt.Errorf("load published Target payload: %w", err)
	}
	markerPath := filepath.Join(resolved.Root, ".eva-remote-release")
	info, err := os.Lstat(markerPath)
	if err != nil {
		return RuntimeArtifactSource{}, PayloadSource{}, fmt.Errorf("read Remote delivery marker: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return RuntimeArtifactSource{}, PayloadSource{}, errors.New("Remote delivery marker must be a regular non-symlink file")
	}
	contents, err := os.ReadFile(markerPath)
	if err != nil {
		return RuntimeArtifactSource{}, PayloadSource{}, err
	}
	if err := rejectSecretContent(strings.Split(string(contents), "\n")); err != nil {
		return RuntimeArtifactSource{}, PayloadSource{}, errors.New("Remote delivery marker contains sensitive content")
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(contents)))
	decoder.KnownFields(true)
	var marker remoteDeliveryMarker
	if err := decoder.Decode(&marker); err != nil {
		return RuntimeArtifactSource{}, PayloadSource{}, fmt.Errorf("parse Remote delivery marker: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return RuntimeArtifactSource{}, PayloadSource{}, errors.New("Remote delivery marker must contain exactly one YAML document")
	}
	runtimeDigest, err := regularFileSHA256(filepath.Join(runtimeArtifact.Directory, "manifest.yaml"))
	if err != nil {
		return RuntimeArtifactSource{}, PayloadSource{}, err
	}
	payloadDigest, err := regularFileSHA256(filepath.Join(payload.Directory, "manifest.yaml"))
	if err != nil {
		return RuntimeArtifactSource{}, PayloadSource{}, err
	}
	if marker.SchemaVersion != "v1" || marker.ReleaseVersion != identity.ReleaseVersion || marker.ReleaseYAMLSHA256 != identity.ReleaseYAMLSHA256 || marker.ChecksumsSHA256 != identity.ChecksumsSHA256 || marker.Registry != identity.RepositoryRegistry || marker.Project != identity.RepositoryProject || marker.RuntimeManifestSHA256 != runtimeDigest || marker.PayloadManifestSHA256 != payloadDigest {
		return RuntimeArtifactSource{}, PayloadSource{}, errors.New("Remote delivery marker does not match published artifacts")
	}
	return runtimeArtifact, payload, nil
}

// BuildRuntimeArtifact packages only the validated managed Runtime. It never
// overwrites an existing result, including a corrupt one.
func BuildRuntimeArtifact(root string, identity PreparationIdentity, runtimeRoot string) (RuntimeArtifactSource, error) {
	if err := validateIdentity(identity); err != nil {
		return RuntimeArtifactSource{}, err
	}
	resolved, err := runtime.Resolve(runtimeRoot)
	if err != nil {
		return RuntimeArtifactSource{}, fmt.Errorf("validate managed Runtime: %w", err)
	}
	if err := validateRuntimeSource(resolved.Root); err != nil {
		return RuntimeArtifactSource{}, err
	}

	final := RuntimeArtifactPath(root, identity)
	if _, err := os.Lstat(final); err == nil {
		artifact, loadErr := LoadRuntimeArtifact(final, identity)
		if loadErr != nil {
			return RuntimeArtifactSource{}, fmt.Errorf("existing Runtime artifact is invalid: %w", loadErr)
		}
		return artifact, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return RuntimeArtifactSource{}, fmt.Errorf("read Runtime artifact destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o750); err != nil {
		return RuntimeArtifactSource{}, err
	}
	staging, err := os.MkdirTemp(filepath.Dir(final), ".runtime-")
	if err != nil {
		return RuntimeArtifactSource{}, err
	}
	defer os.RemoveAll(staging)

	archiveName := fmt.Sprintf("eva-runtime_%s_linux_amd64.tar.gz", resolved.Descriptor.Version)
	archivePath := filepath.Join(staging, archiveName)
	if err := writeRuntimeArchive(resolved.Root, archivePath); err != nil {
		return RuntimeArtifactSource{}, err
	}
	archiveDigest, err := regularFileSHA256(archivePath)
	if err != nil {
		return RuntimeArtifactSource{}, fmt.Errorf("digest Runtime artifact: %w", err)
	}
	descriptorDigest, err := regularFileSHA256(resolved.DescriptorPath)
	if err != nil {
		return RuntimeArtifactSource{}, fmt.Errorf("digest Runtime descriptor: %w", err)
	}
	manifest := RuntimeArtifactManifest{
		SchemaVersion: "v1",
		Release:       identity,
		Repository:    RepositoryIdentity{Registry: identity.RepositoryRegistry, Project: identity.RepositoryProject},
		Runtime: RuntimeArtifactInfo{
			Version: resolved.Descriptor.Version, Platform: "linux/amd64", Archive: archiveName,
			ArchiveSHA256: archiveDigest, DescriptorSHA256: descriptorDigest,
		},
	}
	contents, err := yaml.Marshal(manifest)
	if err != nil {
		return RuntimeArtifactSource{}, err
	}
	if err := writePayloadFile(filepath.Join(staging, "manifest.yaml"), contents); err != nil {
		return RuntimeArtifactSource{}, err
	}
	if err := writePayloadChecksums(filepath.Join(staging, "checksums.sha256"), archiveDigest, archiveName); err != nil {
		return RuntimeArtifactSource{}, err
	}
	if _, err := LoadRuntimeArtifact(staging, identity); err != nil {
		return RuntimeArtifactSource{}, fmt.Errorf("validate staged Runtime artifact: %w", err)
	}
	stagingDirectory, err := os.Open(staging)
	if err != nil {
		return RuntimeArtifactSource{}, fmt.Errorf("open staged Runtime artifact: %w", err)
	}
	if err := stagingDirectory.Sync(); err != nil {
		stagingDirectory.Close()
		return RuntimeArtifactSource{}, fmt.Errorf("sync staged Runtime artifact: %w", err)
	}
	if err := stagingDirectory.Close(); err != nil {
		return RuntimeArtifactSource{}, fmt.Errorf("close staged Runtime artifact: %w", err)
	}
	if err := os.Rename(staging, final); err != nil {
		return RuntimeArtifactSource{}, fmt.Errorf("publish Runtime artifact: %w", err)
	}
	parent, err := os.Open(filepath.Dir(final))
	if err != nil {
		return RuntimeArtifactSource{}, fmt.Errorf("open Runtime artifact parent: %w", err)
	}
	defer parent.Close()
	if err := parent.Sync(); err != nil {
		return RuntimeArtifactSource{}, fmt.Errorf("sync Runtime artifact parent: %w", err)
	}
	return LoadRuntimeArtifact(final, identity)
}

func LoadRuntimeArtifact(directory string, identity PreparationIdentity) (RuntimeArtifactSource, error) {
	manifestPath := filepath.Join(directory, "manifest.yaml")
	info, err := os.Lstat(manifestPath)
	if err != nil {
		return RuntimeArtifactSource{}, fmt.Errorf("read Runtime artifact manifest: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return RuntimeArtifactSource{}, errors.New("Runtime artifact manifest must be a regular non-symlink file")
	}
	contents, err := os.ReadFile(manifestPath)
	if err != nil {
		return RuntimeArtifactSource{}, err
	}
	if err := rejectSecretContent(strings.Split(string(contents), "\n")); err != nil {
		return RuntimeArtifactSource{}, errors.New("Runtime artifact manifest contains sensitive content")
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(contents)))
	decoder.KnownFields(true)
	var manifest RuntimeArtifactManifest
	if err := decoder.Decode(&manifest); err != nil {
		return RuntimeArtifactSource{}, fmt.Errorf("parse Runtime artifact manifest: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return RuntimeArtifactSource{}, errors.New("Runtime artifact manifest must contain exactly one YAML document")
		}
		return RuntimeArtifactSource{}, err
	}
	if err := validateRuntimeArtifactManifest(manifest, identity); err != nil {
		return RuntimeArtifactSource{}, err
	}
	if err := validatePayloadDirectory(directory, manifest.Runtime.Archive); err != nil {
		return RuntimeArtifactSource{}, fmt.Errorf("Runtime artifact directory: %w", err)
	}
	checksums := PayloadManifest{Archive: manifest.Runtime.Archive, ArchiveSHA256: manifest.Runtime.ArchiveSHA256}
	if err := validatePayloadChecksums(directory, checksums); err != nil {
		return RuntimeArtifactSource{}, fmt.Errorf("Runtime artifact checksums: %w", err)
	}
	archivePath := filepath.Join(directory, manifest.Runtime.Archive)
	if digest, err := regularFileSHA256(archivePath); err != nil || digest != manifest.Runtime.ArchiveSHA256 {
		return RuntimeArtifactSource{}, errors.New("Runtime artifact checksum mismatch")
	}
	if err := validateRuntimeArchive(archivePath, manifest.Runtime.DescriptorSHA256); err != nil {
		return RuntimeArtifactSource{}, err
	}
	validationRoot, err := os.MkdirTemp("", ".eva-runtime-verify-")
	if err != nil {
		return RuntimeArtifactSource{}, fmt.Errorf("create Runtime artifact validation directory: %w", err)
	}
	defer os.RemoveAll(validationRoot)
	validated, err := runtime.BootstrapOffline(archivePath, filepath.Join(validationRoot, "runtime"))
	if err != nil || validated.Descriptor.Version != manifest.Runtime.Version {
		return RuntimeArtifactSource{}, errors.New("Runtime artifact does not contain the declared valid managed Runtime")
	}
	return RuntimeArtifactSource{Directory: directory, Manifest: manifest}, nil
}

func validateRuntimeArtifactManifest(manifest RuntimeArtifactManifest, identity PreparationIdentity) error {
	actual := manifest.Release
	actual.RepositoryRegistry = manifest.Repository.Registry
	actual.RepositoryProject = manifest.Repository.Project
	if manifest.SchemaVersion != "v1" || actual != identity || manifest.Runtime.Platform != "linux/amd64" ||
		manifest.Runtime.Version == "" || filepath.Base(manifest.Runtime.Archive) != manifest.Runtime.Archive ||
		!sha256Pattern.MatchString(manifest.Runtime.ArchiveSHA256) || !sha256Pattern.MatchString(manifest.Runtime.DescriptorSHA256) {
		return errors.New("Runtime artifact manifest is invalid")
	}
	return nil
}

func validateRuntimeSource(root string) error {
	entries, err := runtimeEntries(root)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return errors.New("Runtime source is empty")
	}
	return nil
}

func writeRuntimeArchive(root, archive string) (resultErr error) {
	entries, err := runtimeEntries(root)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(archive, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); resultErr == nil && closeErr != nil {
			resultErr = closeErr
		}
	}()
	gz, err := gzip.NewWriterLevel(file, gzip.BestCompression)
	if err != nil {
		return err
	}
	gz.Header.ModTime, gz.Header.OS = time.Unix(0, 0), 255
	tw := tar.NewWriter(gz)
	for _, relative := range entries {
		full := filepath.Join(root, relative)
		info, err := os.Lstat(full)
		if err != nil {
			tw.Close()
			gz.Close()
			return err
		}
		header := &tar.Header{Name: path.Join("runtime", filepath.ToSlash(relative)), Mode: int64(info.Mode().Perm()), ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		if info.IsDir() {
			header.Typeflag, header.Name = tar.TypeDir, header.Name+"/"
			if err := tw.WriteHeader(header); err != nil {
				tw.Close()
				gz.Close()
				return err
			}
			continue
		}
		header.Typeflag, header.Size = tar.TypeReg, info.Size()
		if err := tw.WriteHeader(header); err != nil {
			tw.Close()
			gz.Close()
			return err
		}
		in, err := os.Open(full)
		if err != nil {
			tw.Close()
			gz.Close()
			return err
		}
		_, copyErr := io.Copy(tw, in)
		closeErr := in.Close()
		if copyErr != nil {
			tw.Close()
			gz.Close()
			return copyErr
		}
		if closeErr != nil {
			tw.Close()
			gz.Close()
			return closeErr
		}
	}
	if err := tw.Close(); err != nil {
		gz.Close()
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return file.Sync()
}

func runtimeEntries(root string) ([]string, error) {
	var entries []string
	err := filepath.Walk(root, func(name string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if !runtimeAllowedPath(relative) || isSecretLikePath(relative) || info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("unsafe Runtime source: %s", relative)
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && info.Mode().IsRegular() && stat.Nlink > 1 {
			return fmt.Errorf("hard-linked Runtime source: %s", relative)
		}
		entries = append(entries, relative)
		return nil
	})
	sort.Strings(entries)
	return entries, err
}

func runtimeAllowedPath(relative string) bool {
	relative = filepath.ToSlash(relative)
	return relative == "runtime.yaml" || relative == "bin" || relative == "venv" || strings.HasPrefix(relative, "bin/") || strings.HasPrefix(relative, "venv/")
}

func validateRuntimeArchive(archive, descriptorDigest string) error {
	file, err := os.Open(archive)
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
	seen := map[string]bool{}
	var descriptor string
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(header.Name, "/")
		if (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir) || !safeRuntimeArchiveName(name) || seen[name] {
			return errors.New("unsafe Runtime archive entry")
		}
		seen[name] = true
		if header.Typeflag == tar.TypeReg && name == "runtime/runtime.yaml" {
			contents, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(contents)
			descriptor = hex.EncodeToString(sum[:])
		}
	}
	if descriptor == "" || descriptor != descriptorDigest {
		return errors.New("Runtime artifact descriptor mismatch")
	}
	if !seen["runtime/runtime.yaml"] {
		return errors.New("Runtime archive descriptor missing")
	}
	return nil
}

func safeRuntimeArchiveName(name string) bool {
	if name == "runtime" {
		return true
	}
	if !strings.HasPrefix(name, "runtime/") || strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
		return false
	}
	return runtimeAllowedPath(strings.TrimPrefix(name, "runtime/")) && !isSecretLikePath(name)
}
