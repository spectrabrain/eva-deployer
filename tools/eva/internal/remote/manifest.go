package remote

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"eva-deployer/tools/eva/internal/release"
	"gopkg.in/yaml.v3"
)

const DefaultPreparationRoot = "/var/lib/eva/preparation/remote"
const DefaultRemoteCacheRoot = "/var/lib/eva/cache/remote"
const manifestFileName = "manifest.yaml"
const manifestSchemaVersion = "v1"

var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var registryHostPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
var projectPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var platformPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

type ManifestStatus string

const (
	ManifestPending   ManifestStatus = "pending"
	ManifestRunning   ManifestStatus = "running"
	ManifestFailed    ManifestStatus = "failed"
	ManifestSucceeded ManifestStatus = "succeeded"
)

type StepStatus string

const (
	StepPending   StepStatus = "pending"
	StepRunning   StepStatus = "running"
	StepFailed    StepStatus = "failed"
	StepSucceeded StepStatus = "succeeded"
	StepSkipped   StepStatus = "skipped"
)

var DefaultStepNames = []string{
	"validate-release",
	"main-preflight",
	"build-runtime-artifact",
	"prepare-offline-assets",
	"download-product-images",
	"download-infra-images",
	"download-models",
	"download-qdrant-snapshots",
	"publish-product-images",
	"publish-infra-images",
	"publish-qdrant-snapshots",
	"write-manifest",
	"verify",
}

type PreparationIdentity struct {
	ReleaseVersion     string `yaml:"version"`
	Platform           string `yaml:"platform"`
	ReleaseYAMLSHA256  string `yaml:"release_yaml_sha256"`
	ChecksumsSHA256    string `yaml:"checksums_sha256"`
	RepositoryRegistry string `yaml:"-"`
	RepositoryProject  string `yaml:"-"`
}

type RepositoryIdentity struct {
	Registry string `yaml:"registry"`
	Project  string `yaml:"project"`
}

type ManifestStep struct {
	Name        string     `yaml:"name"`
	Status      StepStatus `yaml:"status"`
	StartedAt   time.Time  `yaml:"started_at,omitempty"`
	CompletedAt time.Time  `yaml:"completed_at,omitempty"`
	Evidence    []string   `yaml:"evidence,omitempty"`
	Error       string     `yaml:"error,omitempty"`
}

type Manifest struct {
	SchemaVersion string              `yaml:"schema_version"`
	Status        ManifestStatus      `yaml:"status"`
	Release       PreparationIdentity `yaml:"release"`
	Repository    RepositoryIdentity  `yaml:"repository"`
	CreatedAt     time.Time           `yaml:"created_at"`
	UpdatedAt     time.Time           `yaml:"updated_at"`
	CompletedAt   time.Time           `yaml:"completed_at,omitempty"`
	Steps         []ManifestStep      `yaml:"steps"`
}

type Clock func() time.Time

type ManifestStore struct {
	Root         string
	Clock        Clock
	beforeRename func() error
}

func NewManifestStore(root string, clock Clock) ManifestStore {
	if root == "" {
		root = DefaultPreparationRoot
	}
	if clock == nil {
		clock = time.Now
	}
	return ManifestStore{Root: root, Clock: clock}
}

func BuildPreparationIdentity(resolved release.Resolved, registry, project string) (PreparationIdentity, error) {
	if err := release.ValidateRemotePreparationInput(resolved); err != nil {
		return PreparationIdentity{}, err
	}
	registry, err := ValidateRegistry(registry)
	if err != nil {
		return PreparationIdentity{}, err
	}
	project, err = ValidateProject(project)
	if err != nil {
		return PreparationIdentity{}, err
	}
	releaseDigest, err := regularFileSHA256(resolved.MetadataPath)
	if err != nil {
		return PreparationIdentity{}, fmt.Errorf("digest release.yaml: %w", err)
	}
	checksumsDigest, err := regularFileSHA256(filepath.Join(resolved.Root, "checksums.sha256"))
	if err != nil {
		return PreparationIdentity{}, fmt.Errorf("digest checksums.sha256: %w", err)
	}
	return PreparationIdentity{
		ReleaseVersion:     resolved.Metadata.Version,
		Platform:           resolved.Metadata.Platform.OS + "/" + resolved.Metadata.Platform.Arch,
		ReleaseYAMLSHA256:  releaseDigest,
		ChecksumsSHA256:    checksumsDigest,
		RepositoryRegistry: registry,
		RepositoryProject:  project,
	}, nil
}

func ValidateRegistry(registry string) (string, error) {
	if registry == "" {
		return "", errors.New("repository registry is required")
	}
	if strings.HasPrefix(registry, "-") || strings.IndexFunc(registry, unicode.IsSpace) >= 0 {
		return "", fmt.Errorf("invalid repository registry %q", registry)
	}
	if strings.Contains(registry, "://") || strings.HasSuffix(registry, "/") || strings.ContainsAny(registry, "/?#@[]") {
		return "", fmt.Errorf("invalid repository registry %q", registry)
	}
	host := registry
	if strings.Count(registry, ":") == 1 {
		var port string
		host, port, _ = strings.Cut(registry, ":")
		if host == "" || port == "" {
			return "", fmt.Errorf("invalid repository registry %q", registry)
		}
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return "", fmt.Errorf("invalid repository registry port %q", registry)
		}
	} else if strings.Count(registry, ":") > 1 {
		return "", fmt.Errorf("IPv6 repository registries are not supported: %q", registry)
	}
	if !registryHostPattern.MatchString(host) || strings.EqualFold(host, "localhost") {
		return "", fmt.Errorf("invalid repository registry %q", registry)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return "", fmt.Errorf("repository registry must not use loopback address %q", registry)
	}
	return registry, nil
}

func ValidateProject(project string) (string, error) {
	if project == "" {
		project = "eva"
	}
	lower := strings.ToLower(project)
	if strings.IndexFunc(project, unicode.IsSpace) >= 0 || !projectPattern.MatchString(project) ||
		strings.Contains(lower, "password") || strings.Contains(lower, "secret") ||
		strings.Contains(lower, "token") || strings.Contains(lower, "credential") ||
		strings.Contains(lower, "authorization") {
		return "", fmt.Errorf("invalid repository project %q", project)
	}
	return project, nil
}

func NewManifest(identity PreparationIdentity, stepNames []string, clock Clock) (Manifest, error) {
	if clock == nil {
		clock = time.Now
	}
	if err := validateIdentity(identity); err != nil {
		return Manifest{}, err
	}
	steps := make([]ManifestStep, len(stepNames))
	for index, name := range stepNames {
		steps[index] = ManifestStep{Name: name, Status: StepPending}
	}
	now := clock().UTC()
	manifest := Manifest{
		SchemaVersion: manifestSchemaVersion,
		Status:        ManifestPending,
		Release:       identity,
		Repository: RepositoryIdentity{
			Registry: identity.RepositoryRegistry,
			Project:  identity.RepositoryProject,
		},
		CreatedAt: now,
		UpdatedAt: now,
		Steps:     steps,
	}
	return manifest, ValidateManifest(manifest)
}

func (store ManifestStore) ManifestPath(version string) (string, error) {
	if version == "" || filepath.Base(version) != version || version == "." || version == ".." {
		return "", fmt.Errorf("invalid preparation release version %q", version)
	}
	root, err := filepath.Abs(store.Root)
	if err != nil {
		return "", fmt.Errorf("resolve preparation root: %w", err)
	}
	return filepath.Join(root, version, manifestFileName), nil
}

func (store ManifestStore) LoadOrCreate(identity PreparationIdentity, stepNames []string) (Manifest, bool, error) {
	path, err := store.ManifestPath(identity.ReleaseVersion)
	if err != nil {
		return Manifest{}, false, err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		manifest, err := NewManifest(identity, stepNames, store.Clock)
		return manifest, true, err
	} else if err != nil {
		return Manifest{}, false, fmt.Errorf("read preparation manifest: %w", err)
	}
	manifest, err := store.Load(identity.ReleaseVersion)
	if err != nil {
		return Manifest{}, false, err
	}
	if err := EnsureIdentity(manifest, identity); err != nil {
		return Manifest{}, false, err
	}
	if err := validateStepNames(manifest.Steps, stepNames); err != nil {
		return Manifest{}, false, err
	}
	return manifest, false, nil
}

func (store ManifestStore) Save(manifest Manifest) error {
	if err := ValidateManifest(manifest); err != nil {
		return err
	}
	path, err := store.ManifestPath(manifest.Release.ReleaseVersion)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("create preparation directory: %w", err)
	}
	if err := os.Chmod(directory, 0o750); err != nil {
		return fmt.Errorf("set preparation directory mode: %w", err)
	}
	contents, err := yaml.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshal preparation manifest: %w", err)
	}
	if containsSensitiveManifestKey(contents) {
		return errors.New("preparation manifest contains a forbidden sensitive field")
	}
	temporary, err := os.CreateTemp(directory, ".manifest-")
	if err != nil {
		return fmt.Errorf("create preparation manifest temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o640); err != nil {
		temporary.Close()
		return fmt.Errorf("set preparation manifest mode: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return fmt.Errorf("write preparation manifest: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync preparation manifest: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close preparation manifest: %w", err)
	}
	if store.beforeRename != nil {
		if err := store.beforeRename(); err != nil {
			return fmt.Errorf("prepare preparation manifest publish: %w", err)
		}
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("publish preparation manifest: %w", err)
	}
	if parent, err := os.Open(directory); err == nil {
		defer parent.Close()
		if err := parent.Sync(); err != nil {
			return fmt.Errorf("sync preparation manifest directory: %w", err)
		}
	}
	return nil
}

func (store ManifestStore) Load(version string) (Manifest, error) {
	path, err := store.ManifestPath(version)
	if err != nil {
		return Manifest{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read preparation manifest: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Manifest{}, errors.New("preparation manifest must be a regular non-symlink file")
	}
	file, err := os.Open(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("open preparation manifest: %w", err)
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("parse preparation manifest: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Manifest{}, errors.New("preparation manifest must contain exactly one YAML document")
		}
		return Manifest{}, fmt.Errorf("parse trailing preparation manifest content: %w", err)
	}
	if err := ValidateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func EnsureIdentity(manifest Manifest, expected PreparationIdentity) error {
	if err := validateIdentity(expected); err != nil {
		return err
	}
	actual := manifest.Release
	actual.RepositoryRegistry = manifest.Repository.Registry
	actual.RepositoryProject = manifest.Repository.Project
	if actual != expected {
		return errors.New("preparation manifest identity does not match the requested Release or repository")
	}
	return nil
}

func ValidateManifest(manifest Manifest) error {
	if manifest.SchemaVersion != manifestSchemaVersion {
		return fmt.Errorf("unsupported preparation manifest schema version %q", manifest.SchemaVersion)
	}
	identity := manifest.Release
	identity.RepositoryRegistry = manifest.Repository.Registry
	identity.RepositoryProject = manifest.Repository.Project
	if err := validateIdentity(identity); err != nil {
		return err
	}
	if !validManifestStatus(manifest.Status) {
		return fmt.Errorf("invalid preparation manifest status %q", manifest.Status)
	}
	if manifest.CreatedAt.IsZero() || manifest.UpdatedAt.IsZero() || manifest.UpdatedAt.Before(manifest.CreatedAt) {
		return errors.New("preparation manifest timestamps are invalid")
	}
	if manifest.CreatedAt.Location() != time.UTC || manifest.UpdatedAt.Location() != time.UTC {
		return errors.New("preparation manifest timestamps must use UTC")
	}
	if !manifest.CompletedAt.IsZero() && manifest.CompletedAt.Before(manifest.CreatedAt) {
		return errors.New("preparation manifest completed_at is invalid")
	}
	if !manifest.CompletedAt.IsZero() && manifest.CompletedAt.Location() != time.UTC {
		return errors.New("preparation manifest completed_at must use UTC")
	}
	if err := validateStepNames(manifest.Steps, nil); err != nil {
		return err
	}
	failed := false
	for _, step := range manifest.Steps {
		if !validStepStatus(step.Status) {
			return fmt.Errorf("invalid preparation step status %q", step.Status)
		}
		if (step.Status == StepRunning || step.Status == StepSucceeded || step.Status == StepFailed) && step.StartedAt.IsZero() {
			return fmt.Errorf("preparation step %q is missing started_at", step.Name)
		}
		if (step.Status == StepSucceeded || step.Status == StepFailed) && step.CompletedAt.IsZero() {
			return fmt.Errorf("preparation step %q is missing completed_at", step.Name)
		}
		if !step.StartedAt.IsZero() && step.StartedAt.Location() != time.UTC || !step.CompletedAt.IsZero() && step.CompletedAt.Location() != time.UTC {
			return fmt.Errorf("preparation step %q timestamps must use UTC", step.Name)
		}
		if !step.CompletedAt.IsZero() && step.CompletedAt.Before(step.StartedAt) {
			return fmt.Errorf("preparation step %q completed before it started", step.Name)
		}
		if step.Status == StepSucceeded && len(step.Evidence) == 0 {
			return fmt.Errorf("preparation step %q succeeded without evidence", step.Name)
		}
		if step.Status != StepFailed && step.Error != "" {
			return fmt.Errorf("preparation step %q has an error outside failed state", step.Name)
		}
		if step.Status == StepFailed {
			failed = true
			if step.Error == "" {
				return fmt.Errorf("preparation step %q failed without an error", step.Name)
			}
			if sanitizeStepError(errors.New(step.Error)) != step.Error {
				return fmt.Errorf("preparation step %q error is not safe to persist", step.Name)
			}
		}
		if err := ValidateEvidence(step.Evidence); err != nil {
			return fmt.Errorf("preparation step %q evidence: %w", step.Name, err)
		}
	}
	switch manifest.Status {
	case ManifestPending:
		if !manifest.CompletedAt.IsZero() {
			return errors.New("pending preparation manifest has completed_at")
		}
	case ManifestRunning:
		if !manifest.CompletedAt.IsZero() {
			return errors.New("running preparation manifest has completed_at")
		}
	case ManifestFailed:
		if manifest.CompletedAt.IsZero() || !failed {
			return errors.New("failed preparation manifest must have completed_at and a failed step")
		}
	case ManifestSucceeded:
		if manifest.CompletedAt.IsZero() {
			return errors.New("succeeded preparation manifest is missing completed_at")
		}
		for _, step := range manifest.Steps {
			if step.Status != StepSucceeded && step.Status != StepSkipped {
				return errors.New("succeeded preparation manifest has incomplete steps")
			}
		}
	}
	return nil
}

func ValidateEvidence(evidence []string) error {
	seen := make(map[string]struct{}, len(evidence))
	for _, path := range evidence {
		if path == "" || filepath.IsAbs(path) || strings.Contains(path, "://") || strings.ContainsAny(path, "?#\\") {
			return fmt.Errorf("unsafe evidence path %q", path)
		}
		clean := filepath.Clean(path)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean != path {
			return fmt.Errorf("unsafe evidence path %q", path)
		}
		if isSecretLikePath(path) {
			return fmt.Errorf("sensitive evidence path %q", path)
		}
		if _, duplicate := seen[path]; duplicate {
			return fmt.Errorf("duplicate evidence path %q", path)
		}
		seen[path] = struct{}{}
	}
	ordered := stableEvidence(evidence)
	for index := range evidence {
		if evidence[index] != ordered[index] {
			return errors.New("evidence paths must use stable sorted order")
		}
	}
	return nil
}

func validateIdentity(identity PreparationIdentity) error {
	if identity.ReleaseVersion == "" || filepath.Base(identity.ReleaseVersion) != identity.ReleaseVersion {
		return errors.New("preparation identity release version is invalid")
	}
	if !platformPattern.MatchString(identity.Platform) {
		return errors.New("preparation identity platform is invalid")
	}
	if !sha256Pattern.MatchString(identity.ReleaseYAMLSHA256) || !sha256Pattern.MatchString(identity.ChecksumsSHA256) {
		return errors.New("preparation identity requires SHA-256 digests")
	}
	if _, err := ValidateRegistry(identity.RepositoryRegistry); err != nil {
		return err
	}
	if _, err := ValidateProject(identity.RepositoryProject); err != nil {
		return err
	}
	return nil
}

func validateStepNames(steps []ManifestStep, expected []string) error {
	if len(steps) == 0 {
		return errors.New("preparation manifest has no steps")
	}
	if expected != nil && len(steps) != len(expected) {
		return errors.New("preparation manifest step list does not match requested steps")
	}
	seen := make(map[string]struct{}, len(steps))
	for index, step := range steps {
		if step.Name == "" {
			return errors.New("preparation step name is required")
		}
		if _, duplicate := seen[step.Name]; duplicate {
			return fmt.Errorf("duplicate preparation step %q", step.Name)
		}
		seen[step.Name] = struct{}{}
		if expected != nil && step.Name != expected[index] {
			return errors.New("preparation manifest step list does not match requested steps")
		}
	}
	return nil
}

func validManifestStatus(status ManifestStatus) bool {
	return status == ManifestPending || status == ManifestRunning || status == ManifestFailed || status == ManifestSucceeded
}

func validStepStatus(status StepStatus) bool {
	return status == StepPending || status == StepRunning || status == StepFailed || status == StepSucceeded || status == StepSkipped
}

func regularFileSHA256(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("must be a regular non-symlink file: %s", path)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:]), nil
}

func isSecretLikePath(path string) bool {
	lower := strings.ToLower(filepath.ToSlash(path))
	base := filepath.Base(lower)
	if base == ".env" || strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") || base == "id_rsa" || base == "authorized_keys" {
		return true
	}
	for _, segment := range strings.Split(lower, "/") {
		if segment == ".aws" || segment == ".docker" || segment == "credentials" || segment == "secrets" || segment == "private" {
			return true
		}
	}
	return strings.Contains(lower, "password") || strings.Contains(lower, "token") || strings.Contains(lower, "secret")
}

func containsSensitiveManifestKey(contents []byte) bool {
	lower := strings.ToLower(string(contents))
	for _, key := range []string{"aws_access_key", "aws_secret", "session_token", "harbor_password", "docker_auth", "authorization:", "private_key:"} {
		if strings.Contains(lower, key) {
			return true
		}
	}
	return false
}

func stableEvidence(evidence []string) []string {
	values := append([]string(nil), evidence...)
	sort.Strings(values)
	return values
}
