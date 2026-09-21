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
	ResolveBackend  func(string) (string, error)
	Run             ProcessRunner
	Clock           Clock
}
type PrepareOptions struct {
	Release  release.Resolved
	Registry string
	Project  string
	Streams  Streams
}

func NewPrepareService() PrepareService {
	return PrepareService{PreparationRoot: DefaultPreparationRoot, ResolveBackend: ResolveBackend, Run: runProcess}
}

func (service PrepareService) Prepare(ctx context.Context, options PrepareOptions) (string, error) {
	if options.Streams.Stdout == nil {
		options.Streams.Stdout = io.Discard
	}
	if options.Project == "" {
		options.Project = defaultRemoteProject
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
			if err := ValidatePreparation(root, options.Release, identity, manifest); err != nil {
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
	if service.ResolveBackend == nil || service.Run == nil {
		return manifestPath, errors.New("Remote prepare service is not configured")
	}
	steps, err := service.steps(root, options.Release, identity, &manifest, options.Streams)
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
	for _, name := range []string{"cache", "reports", "work"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o750); err != nil {
			return fmt.Errorf("create preparation %s: %w", name, err)
		}
	}
	return nil
}

func (service PrepareService) steps(root string, resolved release.Resolved, identity PreparationIdentity, manifest *Manifest, streams Streams) ([]PreparationStep, error) {
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
			return StepResult{Evidence: evidence}, nil
		}, ValidateEvidence: func(context.Context, []string) error {
			if err := validate(); err != nil {
				return err
			}
			fmt.Fprintf(streams.Stdout, "[OK] %s\n", step)
			return nil
		}}
	}
	base := map[string]string{"EVA_CACHE_ROOT": filepath.Join(root, "cache"), "COMPONENTS": "all", "EVA_AGENT_QDRANT_SNAPSHOT_SOURCE": "harbor", "EVA_AGENT_QDRANT_VALUES_FILE": "values-k3s.harbor.yaml", "PULL_PLATFORM": "linux/amd64", "REPOSITORY_REGISTRY": identity.RepositoryRegistry, "REPOSITORY_PROJECT": identity.RepositoryProject}
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
	steps := []PreparationStep{
		{Name: "validate-release", Run: func(context.Context) (StepResult, error) {
			if err := release.ValidateRemotePublish(resolved); err != nil {
				return StepResult{}, err
			}
			report := map[string]string{"release_version": identity.ReleaseVersion, "release_yaml_sha256": identity.ReleaseYAMLSHA256, "checksums_sha256": identity.ChecksumsSHA256, "platform": resolved.Metadata.Platform.OS + "/" + resolved.Metadata.Platform.Arch, "offline_artifact": offlineArtifactName(resolved), "offline_artifact_sha256": offlineArtifactSHA256(resolved)}
			if err := writeYAMLReport(root, "reports/release-validation.yaml", report); err != nil {
				return StepResult{}, err
			}
			return StepResult{Evidence: []string{"reports/release-validation.yaml"}}, nil
		}, ValidateEvidence: func(context.Context, []string) error { return nonEmptyRegular(root, "reports/release-validation.yaml") }},
		command("prepare-offline-assets", env(nil), []string{"cache/apt/debs/manifest.txt", "cache/docker/debs/manifest.txt", "cache/manifest.txt", "cache/nvidia/container-toolkit-debs/manifest.txt", "cache/tools/oras"}, func() error { return ValidateOfflineAssets(root) }),
		command("download-product-images", env(map[string]string{"PULL_SOURCE_IMAGES": "true"}), []string{"cache/images/images-all.txt", "cache/images/images-missing.txt", "cache/images/images-pulled.txt"}, func() error {
			return ValidateImageLists(root, "images-all.txt", "images-pulled.txt", "images-missing.txt")
		}),
		command("download-infra-images", env(map[string]string{"PULL_SOURCE_IMAGES": "true"}), []string{"cache/images/infra-images-all.txt", "cache/images/infra-images-missing.txt", "cache/images/infra-images-pulled.txt"}, func() error {
			return ValidateImageLists(root, "infra-images-all.txt", "infra-images-pulled.txt", "infra-images-missing.txt")
		}),
		command("download-models", env(nil), []string{"cache/models/manifest.txt"}, func() error { return ValidateModels(root) }),
		command("download-qdrant-snapshots", env(nil), []string{"cache/qdrant-snapshots/manifest.txt"}, func() error { return ValidateQdrantSnapshots(root) }),
		command("publish-product-images", env(map[string]string{"PULL_SOURCE_IMAGES": "false", "IMAGE_LIST": filepath.Join(root, "cache/images/images-pulled.txt"), "REPOSITORY_MAPPING_FILE": filepath.Join(root, "reports/repository-mapping-product.txt"), "REPOSITORY_MIRROR_PATH_IMAGES": "false"}), []string{"reports/repository-mapping-product.txt"}, func() error {
			return ValidateRepositoryMapping(root, "cache/images/images-pulled.txt", "reports/repository-mapping-product.txt", identity.RepositoryRegistry, identity.RepositoryProject)
		}),
		command("publish-infra-images", env(map[string]string{"PULL_SOURCE_IMAGES": "false", "IMAGE_LIST": filepath.Join(root, "cache/images/infra-images-pulled.txt"), "REPOSITORY_MAPPING_FILE": filepath.Join(root, "reports/repository-mapping-infra.txt"), "REPOSITORY_MIRROR_PATH_IMAGES": "false"}), []string{"reports/repository-mapping-infra.txt"}, func() error {
			return ValidateRepositoryMapping(root, "cache/images/infra-images-pulled.txt", "reports/repository-mapping-infra.txt", identity.RepositoryRegistry, identity.RepositoryProject)
		}),
		command("publish-qdrant-snapshots", env(map[string]string{"SNAPSHOT_DIR": filepath.Join(root, "cache/qdrant-snapshots"), "HARBOR_ARTIFACT_MANIFEST": filepath.Join(root, "reports/qdrant-artifacts.txt")}), []string{"reports/qdrant-artifacts.txt"}, func() error {
			return ValidateQdrantArtifacts(root, identity.RepositoryRegistry, identity.RepositoryProject)
		}),
		{Name: "write-manifest", Run: func(context.Context) (StepResult, error) {
			if err := ValidatePreparation(root, resolved, identity, *manifest); err != nil {
				return StepResult{}, err
			}
			summary := map[string]any{"release_version": identity.ReleaseVersion, "repository": identity.RepositoryRegistry + "/" + identity.RepositoryProject, "assets": []string{"offline", "product-images", "infra-images", "models", "qdrant-snapshots"}}
			if err := writeYAMLReport(root, "reports/preparation-summary.yaml", summary); err != nil {
				return StepResult{}, err
			}
			return StepResult{Evidence: []string{"reports/preparation-summary.yaml"}}, nil
		}, ValidateEvidence: func(context.Context, []string) error {
			return nonEmptyRegular(root, "reports/preparation-summary.yaml")
		}},
		{Name: "verify", Run: func(context.Context) (StepResult, error) {
			if err := ValidatePreparation(root, resolved, identity, *manifest); err != nil {
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
