package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"eva-deployer/tools/eva/internal/release"
	"eva-deployer/tools/eva/internal/runtime"
)

const defaultRemoteProject = "eva"

var remoteBackendPaths = map[string]string{
	"prepare-offline-assets":    "scripts/download/download_offline_assets.sh",
	"download-product-images":   "scripts/download/download_eva_images.sh",
	"download-infra-images":     "scripts/download/download_infra_images.sh",
	"download-models":           "scripts/download/download_eva_models.sh",
	"download-qdrant-snapshots": "scripts/download/download_qdrant_snapshots.sh",
	"publish-product-images":    "scripts/publish/push_images_to_repository.sh",
	"publish-infra-images":      "scripts/publish/push_images_to_repository.sh",
	"publish-qdrant-snapshots":  "scripts/publish/push_qdrant_snapshots_to_harbor.sh",
}

type ProcessOptions struct {
	Path    string
	Args    []string
	Env     map[string]string
	Dir     string
	Streams Streams
}
type ProcessRunner func(context.Context, ProcessOptions) error

type PrepareService struct {
	PreparationRoot string
	CacheRoot       string
	ResolveBackend  func(string) (string, error)
	Run             ProcessRunner
	Preflight       Preflight
	Clock           Clock
	RuntimeRoot     string
	PrepareFunc     func(context.Context, PrepareOptions) (string, error) // test boundary; nil uses the production implementation
}
type PrepareOptions struct {
	Release       release.Resolved
	Registry      string
	Project       string
	AWSCredential AWSCredential
	Streams       Streams
}

func NewPrepareService() PrepareService {
	return PrepareService{PreparationRoot: DefaultPreparationRoot, CacheRoot: DefaultRemoteCacheRoot, ResolveBackend: ResolveBackend, Run: runProcess, Preflight: NewPreflight(), RuntimeRoot: runtime.DefaultRoot}
}

func (service PrepareService) Prepare(ctx context.Context, options PrepareOptions) (string, error) {
	if service.PrepareFunc != nil {
		return service.PrepareFunc(ctx, options)
	}
	if options.Streams.Stdout == nil {
		options.Streams.Stdout = io.Discard
	}
	if options.Project == "" {
		options.Project = defaultRemoteProject
	}
	if service.PreparationRoot == "" {
		service.PreparationRoot = DefaultPreparationRoot
	}
	if service.CacheRoot == "" {
		service.CacheRoot = DefaultRemoteCacheRoot
	}
	if err := ensureRemoteCacheRoot(service.PreparationRoot, service.CacheRoot); err != nil {
		return "", err
	}
	identity, err := BuildPreparationIdentity(options.Release, options.Registry, options.Project)
	if err != nil {
		return "", err
	}
	store := NewManifestStore(service.PreparationRoot, service.Clock)
	manifestPath, err := store.ManifestPath(identity.ReleaseVersion)
	if err != nil {
		return "", err
	}
	root := filepath.Dir(manifestPath)
	if err := makePreparationLayout(root); err != nil {
		return manifestPath, err
	}
	manifest, created, err := store.LoadOrCreate(identity, DefaultStepNames)
	if err != nil {
		return manifestPath, err
	}
	if !created {
		switch manifest.Status {
		case ManifestSucceeded:
			if err := ValidateCompletedPreparation(root, service.CacheRoot, options.Release, identity, manifest); err != nil {
				return manifestPath, fmt.Errorf("existing Remote preparation failed validation: %w", err)
			}
			fmt.Fprintf(options.Streams.Stdout, "[OK] Remote preparation already completed\n[INFO] manifest=%s\n", manifestPath)
			return manifestPath, nil
		case ManifestRunning:
			return manifestPath, errors.New("Remote preparation is already running or requires operator inspection")
		case ManifestFailed:
			return manifestPath, errors.New("Remote preparation previously failed; retry is not implemented")
		case ManifestPending:
			return manifestPath, errors.New("Remote preparation has a pending manifest; resume is not implemented")
		}
	}
	if service.ResolveBackend == nil || service.Run == nil || service.Preflight.Run == nil {
		return manifestPath, errors.New("Remote prepare service is not configured")
	}
	steps, err := service.steps(root, service.CacheRoot, options.Release, identity, &manifest, options.Streams, options.AWSCredential)
	if err != nil {
		return manifestPath, err
	}
	runner := StepRunner{Store: store, Clock: service.Clock}
	if err := runner.Run(ctx, &manifest, identity, steps); err != nil {
		return manifestPath, err
	}
	fmt.Fprintf(options.Streams.Stdout, "[OK] Remote preparation completed\n[INFO] manifest=%s\n", manifestPath)
	return manifestPath, nil
}

func makePreparationLayout(root string) error {
	for _, name := range []string{"reports", "work"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o750); err != nil {
			return fmt.Errorf("create preparation %s: %w", name, err)
		}
	}
	return nil
}

func ensureRemoteCacheRoot(preparationRoot, cacheRoot string) error {
	preparation, err := filepath.Abs(preparationRoot)
	if err != nil {
		return err
	}
	cache, err := filepath.Abs(cacheRoot)
	if err != nil {
		return err
	}
	if preparation == cache || strings.HasPrefix(cache+string(os.PathSeparator), preparation+string(os.PathSeparator)) || strings.HasPrefix(preparation+string(os.PathSeparator), cache+string(os.PathSeparator)) {
		return errors.New("Remote preparation root and cache root must not overlap")
	}
	if info, err := os.Lstat(cache); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("Remote cache root must be a directory and not a symlink")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(cache, 0o750); err != nil {
		return err
	}
	return os.Chmod(cache, 0o750)
}

func (service PrepareService) steps(root, cacheRoot string, resolved release.Resolved, identity PreparationIdentity, manifest *Manifest, streams Streams, awsCredential AWSCredential) ([]PreparationStep, error) {
	if streams.Stdout == nil {
		streams.Stdout = io.Discard
	}
	backend := map[string]string{}
	for step, relative := range remoteBackendPaths {
		path, err := service.ResolveBackend(relative)
		if err != nil {
			return nil, fmt.Errorf("resolve backend for %s: %w", step, err)
		}
		backend[step] = path
	}
	command := func(step string, environment map[string]string, evidence []string, validate func() error) PreparationStep {
		return PreparationStep{Name: step, Run: func(ctx context.Context) (StepResult, error) {
			fmt.Fprintf(streams.Stdout, "[INFO] Step %d/%d %s\n", stepIndex(step), len(DefaultStepNames), step)
			if err := service.Run(ctx, ProcessOptions{Path: backend[step], Env: environment, Dir: filepath.Join(root, "work"), Streams: streams}); err != nil {
				return StepResult{}, fmt.Errorf("run %s: %w", step, err)
			}
			if len(evidence) == 1 && strings.HasPrefix(evidence[0], "reports/cache-") {
				if err := validate(); err != nil {
					return StepResult{}, err
				}
				if err := writeYAMLReport(root, evidence[0], map[string]string{"schema_version": "v1", "cache_scope": "remote", "asset_type": step, "status": "validated"}); err != nil {
					return StepResult{}, err
				}
			}
			return StepResult{Evidence: evidence}, nil
		}, ValidateEvidence: func(context.Context, []string) error {
			if len(evidence) == 1 && strings.HasPrefix(evidence[0], "reports/cache-") {
				return nonEmptyRegular(root, evidence[0])
			}
			if err := validate(); err != nil {
				return err
			}
			fmt.Fprintf(streams.Stdout, "[OK] %s\n", step)
			return nil
		}}
	}
	base := map[string]string{"EVA_CACHE_ROOT": cacheRoot, "COMPONENTS": "all", "EVA_AGENT_QDRANT_SNAPSHOT_SOURCE": "harbor", "EVA_AGENT_QDRANT_VALUES_FILE": "values-k3s.harbor.yaml", "PULL_PLATFORM": "linux/amd64", "REPOSITORY_REGISTRY": identity.RepositoryRegistry, "REPOSITORY_PROJECT": identity.RepositoryProject}
	env := func(values map[string]string) map[string]string {
		copy := map[string]string{}
		for k, v := range base {
			copy[k] = v
		}
		for k, v := range values {
			copy[k] = v
		}
		return copy
	}
	awsEnv := func(values map[string]string) map[string]string {
		result := env(values)
		if awsCredential.AccessKeyID != "" {
			result["AWS_ACCESS_KEY_ID"] = awsCredential.AccessKeyID
			result["AWS_SECRET_ACCESS_KEY"] = awsCredential.SecretAccessKey
			result["AWS_DEFAULT_REGION"] = awsCredential.Region
			result["AWS_REGION"] = awsCredential.Region
		}
		return result
	}
	steps := []PreparationStep{
		{Name: "validate-release", Run: func(context.Context) (StepResult, error) {
			if err := release.ValidateRemotePreparationInput(resolved); err != nil {
				return StepResult{}, err
			}
			report := map[string]string{"release_version": identity.ReleaseVersion, "release_yaml_sha256": identity.ReleaseYAMLSHA256, "checksums_sha256": identity.ChecksumsSHA256, "platform": resolved.Metadata.Platform.OS + "/" + resolved.Metadata.Platform.Arch, "offline_artifact": offlineArtifactName(resolved), "offline_artifact_sha256": offlineArtifactSHA256(resolved)}
			if err := writeYAMLReport(root, "reports/release-validation.yaml", report); err != nil {
				return StepResult{}, err
			}
			return StepResult{Evidence: []string{"reports/release-validation.yaml"}}, nil
		}, ValidateEvidence: func(context.Context, []string) error { return nonEmptyRegular(root, "reports/release-validation.yaml") }},
		{Name: "main-preflight", Run: func(ctx context.Context) (StepResult, error) {
			fmt.Fprintln(streams.Stdout, "[INFO] Checking Main preparation prerequisites")
			report, err := service.Preflight.Check(ctx, PreflightOptions{ReleaseRoot: resolved.Root, PreparationRoot: root, Registry: identity.RepositoryRegistry, Project: identity.RepositoryProject, AWSCredential: awsCredential})
			if err != nil {
				return StepResult{}, fmt.Errorf("Main preparation preflight failed: %w", err)
			}
			if err := writeYAMLReport(root, "reports/main-preflight.yaml", report); err != nil {
				return StepResult{}, err
			}
			for _, category := range report.Categories {
				fmt.Fprintf(streams.Stdout, "[OK] %s\n", category)
			}
			fmt.Fprintln(streams.Stdout, "[OK] Main preparation prerequisites")
			return StepResult{Evidence: []string{"reports/main-preflight.yaml"}}, nil
		}, ValidateEvidence: func(context.Context, []string) error { return nonEmptyRegular(root, "reports/main-preflight.yaml") }},
		{Name: "build-runtime-artifact", Run: func(context.Context) (StepResult, error) {
			artifact, err := BuildRuntimeArtifact(root, identity, service.RuntimeRoot)
			if err != nil {
				return StepResult{}, err
			}
			relative := filepath.ToSlash(filepath.Join(runtimeArtifactDirectory, payloadIdentityKey(identity)))
			return StepResult{Evidence: stableEvidence([]string{relative + "/manifest.yaml", relative + "/checksums.sha256", relative + "/" + artifact.Manifest.Runtime.Archive})}, nil
		}, ValidateEvidence: func(context.Context, []string) error {
			_, err := LoadRuntimeArtifact(RuntimeArtifactPath(root, identity), identity)
			return err
		}},
		command("prepare-offline-assets", awsEnv(nil), []string{"reports/cache-offline-assets.yaml"}, func() error { return ValidateOfflineAssets(cacheRoot) }),
		command("download-product-images", awsEnv(map[string]string{"PULL_SOURCE_IMAGES": "true"}), []string{"reports/cache-product-images.yaml"}, func() error {
			return ValidateImageLists(cacheRoot, "images-all.txt", "images-pulled.txt", "images-missing.txt")
		}),
		command("download-infra-images", env(map[string]string{"PULL_SOURCE_IMAGES": "true"}), []string{"reports/cache-infra-images.yaml"}, func() error {
			return ValidateImageLists(cacheRoot, "infra-images-all.txt", "infra-images-pulled.txt", "infra-images-missing.txt")
		}),
		command("download-models", awsEnv(nil), []string{"reports/cache-models.yaml"}, func() error { return ValidateModels(cacheRoot) }),
		command("download-qdrant-snapshots", awsEnv(nil), []string{"reports/cache-qdrant-snapshots.yaml"}, func() error { return ValidateQdrantSnapshots(cacheRoot) }),
		command("publish-product-images", env(map[string]string{"PULL_SOURCE_IMAGES": "false", "IMAGE_LIST": filepath.Join(cacheRoot, "images/images-pulled.txt"), "REPOSITORY_MAPPING_FILE": filepath.Join(root, "reports/repository-mapping-product.txt"), "REPOSITORY_MIRROR_PATH_IMAGES": "false"}), []string{"reports/repository-mapping-product.txt"}, func() error {
			return ValidateRepositoryMapping(cacheRoot, root, "images/images-pulled.txt", "reports/repository-mapping-product.txt", identity.RepositoryRegistry, identity.RepositoryProject)
		}),
		command("publish-infra-images", env(map[string]string{"PULL_SOURCE_IMAGES": "false", "IMAGE_LIST": filepath.Join(cacheRoot, "images/infra-images-pulled.txt"), "REPOSITORY_MAPPING_FILE": filepath.Join(root, "reports/repository-mapping-infra.txt"), "REPOSITORY_MIRROR_PATH_IMAGES": "false"}), []string{"reports/repository-mapping-infra.txt"}, func() error {
			return ValidateRepositoryMapping(cacheRoot, root, "images/infra-images-pulled.txt", "reports/repository-mapping-infra.txt", identity.RepositoryRegistry, identity.RepositoryProject)
		}),
		command("publish-qdrant-snapshots", env(map[string]string{"SNAPSHOT_DIR": filepath.Join(cacheRoot, "qdrant-snapshots"), "HARBOR_ARTIFACT_MANIFEST": filepath.Join(root, "reports/qdrant-artifacts.txt")}), []string{"reports/qdrant-artifacts.txt"}, func() error {
			return ValidateQdrantArtifacts(cacheRoot, root, identity.RepositoryRegistry, identity.RepositoryProject)
		}),
		{Name: "write-manifest", Run: func(context.Context) (StepResult, error) {
			if err := ValidatePreparationAssets(root, cacheRoot, resolved, identity, *manifest); err != nil {
				return StepResult{}, err
			}
			payload, err := BuildTargetPayload(root, cacheRoot, identity, resolved.Metadata.Platform.OS+"/"+resolved.Metadata.Platform.Arch)
			if err != nil {
				return StepResult{}, err
			}
			runtimeArtifact, err := LoadRuntimeArtifact(RuntimeArtifactPath(root, identity), identity)
			if err != nil {
				return StepResult{}, err
			}
			summary := map[string]any{"release_version": identity.ReleaseVersion, "repository": identity.RepositoryRegistry + "/" + identity.RepositoryProject, "assets": []string{"offline", "product-images", "infra-images", "models", "qdrant-snapshots", "runtime-artifact", "target-payload"}, "runtime_artifact": "prepared", "runtime_version": runtimeArtifact.Manifest.Runtime.Version, "runtime_archive_sha256": runtimeArtifact.Manifest.Runtime.ArchiveSHA256, "runtime_manifest": filepath.ToSlash(filepath.Join(runtimeArtifactDirectory, payloadIdentityKey(identity), "manifest.yaml"))}
			if err := writeYAMLReport(root, "reports/preparation-summary.yaml", summary); err != nil {
				return StepResult{}, err
			}
			return StepResult{Evidence: stableEvidence([]string{"reports/preparation-summary.yaml", filepath.ToSlash(filepath.Join(targetPayloadDirectory, payload.Manifest.Identity, "manifest.yaml"))})}, nil
		}, ValidateEvidence: func(context.Context, []string) error {
			return nonEmptyRegular(root, "reports/preparation-summary.yaml")
		}},
		{Name: "verify", Run: func(context.Context) (StepResult, error) {
			if err := ValidatePreparation(root, cacheRoot, resolved, identity, *manifest); err != nil {
				return StepResult{}, err
			}
			if err := writeYAMLReport(root, "reports/verification.yaml", map[string]string{"release_version": identity.ReleaseVersion, "repository": identity.RepositoryRegistry + "/" + identity.RepositoryProject, "status": "validated"}); err != nil {
				return StepResult{}, err
			}
			return StepResult{Evidence: []string{"reports/verification.yaml"}}, nil
		}, ValidateEvidence: func(context.Context, []string) error { return nonEmptyRegular(root, "reports/verification.yaml") }},
	}
	return steps, nil
}

func stepIndex(name string) int {
	for i, step := range DefaultStepNames {
		if step == name {
			return i + 1
		}
	}
	return 0
}
func offlineArtifactName(resolved release.Resolved) string {
	path, err := resolved.ArtifactPath("eva-offline")
	if err != nil {
		return ""
	}
	return filepath.Base(path)
}
func offlineArtifactSHA256(resolved release.Resolved) string {
	for _, artifact := range resolved.Metadata.Artifacts {
		if artifact.Name == "eva-offline" {
			return artifact.SHA256
		}
	}
	return ""
}

func runProcess(ctx context.Context, options ProcessOptions) error {
	if options.Path == "" {
		return errors.New("process path is required")
	}
	command := exec.CommandContext(ctx, options.Path, options.Args...)
	command.Dir, command.Stdin, command.Stdout, command.Stderr = options.Dir, options.Streams.Stdin, options.Streams.Stdout, options.Streams.Stderr
	command.Env = mergedEnvironment(os.Environ(), options.Env)
	if err := command.Run(); err != nil {
		return fmt.Errorf("Remote backend process failed: %w", err)
	}
	return nil
}
func mergedEnvironment(base []string, additions map[string]string) []string {
	values := map[string]string{}
	order := []string{}
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			if _, exists := values[key]; !exists {
				order = append(order, key)
			}
			values[key] = value
		}
	}
	for key, value := range additions {
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = value
	}
	result := make([]string, 0, len(order))
	for _, key := range order {
		result = append(result, key+"="+values[key])
	}
	return result
}
