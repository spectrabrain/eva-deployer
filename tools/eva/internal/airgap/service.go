// Package airgap prepares transportable Local repository assets on an online
// host. It does not deploy Harbor or publish anything to a customer repository.
package airgap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"eva-deployer/tools/eva/internal/release"
	"eva-deployer/tools/eva/internal/remote"
	managedruntime "eva-deployer/tools/eva/internal/runtime"
	"gopkg.in/yaml.v3"
)

type Options struct {
	Release             release.Resolved
	Output              string
	VLLMProfile         string
	DriverPackage       string
	DisplayModeSelector string
	AWSCredential       remote.AWSCredential
	Stdout              io.Writer
}

// Service boundaries allow complete pipeline tests without touching Docker,
// apt, the managed Runtime, internet sources or customer repositories.
type Service struct {
	RuntimeRoot    string
	Preflight      func(context.Context) error
	ResolveBackend func(string) (string, error)
	Run            remote.ProcessRunner
	EnsureRuntime  func(string) (managedruntime.Resolved, error)
	ExportRuntime  func(string, string) error
	ValidateAssets func(string) error
}

func NewService() Service {
	return Service{
		RuntimeRoot:    managedruntime.DefaultRoot,
		Preflight:      preflight,
		ResolveBackend: remote.ResolveBackend,
		Run:            runProcess,
		EnsureRuntime:  ensureRuntime,
		ExportRuntime:  remote.ExportRuntimeArchive,
		ValidateAssets: validateAssets,
	}
}

var driverPackagePattern = regexp.MustCompile(`^nvidia-driver-[0-9]+(-open)?$`)
var imagePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/:@-]*$`)

func ValidateOptions(options Options) error {
	if options.Output == "" || !strings.HasSuffix(options.Output, ".tar.gz") {
		return errors.New("airgap prepare requires --output FILE.tar.gz")
	}
	switch options.VLLMProfile {
	case "A6000x1", "L40sx1", "PRO5000x3", "PRO6000-MIGx4":
	default:
		return errors.New("--vllm-profile must be A6000x1, L40sx1, PRO5000x3 or PRO6000-MIGx4")
	}
	if !driverPackagePattern.MatchString(options.DriverPackage) {
		return errors.New("--driver-package must be nvidia-driver-NNN or nvidia-driver-NNN-open")
	}
	if _, err := os.Lstat(options.Output); err == nil {
		return errors.New("Airgap Bundle output already exists; choose a new output path")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if options.DisplayModeSelector != "" {
		file, err := openArchiveSource(options.DisplayModeSelector, nil)
		if err != nil {
			return fmt.Errorf("approved Display Mode Selector archive: %w", err)
		}
		file.Close()
	}
	return nil
}

func (service Service) Prepare(ctx context.Context, options Options) (string, error) {
	if err := ValidateOptions(options); err != nil {
		return "", err
	}
	if service.Preflight == nil || service.Run == nil || service.ResolveBackend == nil || service.EnsureRuntime == nil || service.ExportRuntime == nil || service.ValidateAssets == nil {
		return "", errors.New("Airgap preparation service is not configured")
	}
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}
	if err := release.ValidateRemotePreparationInput(options.Release); err != nil {
		return "", fmt.Errorf("Airgap preparation requires an original Release: %w", err)
	}
	verified, err := release.Resolve(options.Release.Root)
	if err != nil {
		return "", fmt.Errorf("verify original Release before preparation: %w", err)
	}
	options.Release = verified
	if err := release.ValidateRemotePreparationInput(options.Release); err != nil {
		return "", fmt.Errorf("Airgap preparation requires a verified original Release: %w", err)
	}
	sourceMetadata, err := os.ReadFile(verified.MetadataPath)
	if err != nil {
		return "", err
	}
	sourceDigest := sha256.Sum256(sourceMetadata)
	if options.AWSCredential.AccessKeyID == "" || options.AWSCredential.SecretAccessKey == "" || options.AWSCredential.Region == "" {
		return "", errors.New("AWS credential is required for Airgap preparation")
	}
	if err := service.Preflight(ctx); err != nil {
		return "", err
	}
	output, err := filepath.Abs(options.Output)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(filepath.Dir(output), ".eva-airgap-prepare-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	// Staging, credentials and Docker authentication are never placed below cache.
	cache := filepath.Join(work, "cache")
	for _, name := range []string{"cache", "home", "docker-auth", "tmp"} {
		if err := os.Mkdir(filepath.Join(work, name), 0o700); err != nil {
			return "", err
		}
	}
	prepared, err := release.Prepare(options.Release, filepath.Join(work, "releases"))
	if err != nil {
		return "", err
	}
	infraCatalog := filepath.Join(prepared.Root, "src/infra/version.yaml")
	solutionCatalog := filepath.Join(prepared.Root, "src/solution/version.yaml")
	for _, file := range []string{infraCatalog, solutionCatalog} {
		if err := requireNonempty(file); err != nil {
			return "", err
		}
	}
	vllmFile, err := vllmValuesFile(solutionCatalog, options.VLLMProfile)
	if err != nil {
		return "", err
	}
	fmt.Fprintln(options.Stdout, "[INFO] Preparing managed Runtime (online bootstrap if absent)")
	rt, err := service.EnsureRuntime(service.RuntimeRoot)
	if err != nil {
		return "", fmt.Errorf("prepare managed Runtime: %w", err)
	}
	env := preparationEnvironment(work, cache, infraCatalog, solutionCatalog, rt, options, vllmFile)
	if options.DisplayModeSelector != "" {
		asset, err := filepath.Abs(options.DisplayModeSelector)
		if err != nil {
			return "", err
		}
		env["DISPLAY_MODE_SELECTOR_ZIP_PATH"] = asset
	}
	steps := []string{
		"scripts/download/download_offline_assets.sh",
		"scripts/download/download_python_venv_debs.sh",
		"scripts/install/setup_harbor.sh",
		"scripts/download/download_eva_images.sh",
		"scripts/download/download_infra_images.sh",
		"scripts/download/download_n8n_images.sh",
		"scripts/download/download_eva_models.sh",
		"scripts/download/download_qdrant_snapshots.sh",
	}
	for _, step := range steps {
		backend, err := service.ResolveBackend(step)
		if err != nil {
			return "", err
		}
		args := []string{}
		if step == "scripts/install/setup_harbor.sh" {
			args = []string{"--download-only"}
		}
		fmt.Fprintf(options.Stdout, "[INFO] %s\n", filepath.Base(step))
		// Backend output can contain upstream rendered values or authentication
		// diagnostics. Do not send it to public logs or into the Bundle.
		if err := service.Run(ctx, remote.ProcessOptions{Path: backend, Args: args, Dir: work, Env: env, Streams: remote.Streams{Stdout: io.Discard, Stderr: io.Discard}}); err != nil {
			return "", fmt.Errorf("Airgap preparation stopped at %s; check host prerequisites, source access and AWS permissions; no Bundle was published", filepath.Base(step))
		}
	}
	if err := service.ValidateAssets(cache); err != nil {
		return "", fmt.Errorf("validate downloaded assets: %w", err)
	}
	if options.DisplayModeSelector != "" {
		if err := requireNonempty(filepath.Join(cache, "display_mode_selector/mode_selector_1.72.0/linux/x64/displaymodeselector")); err != nil {
			return "", err
		}
	}
	images, err := transportImageList(cache)
	if err != nil {
		return "", err
	}
	imageArchive := filepath.Join(cache, "images/eva-images.tar")
	fmt.Fprintln(options.Stdout, "[INFO] Exporting and verifying linux/amd64 image layers")
	args := append([]string{"save", "--output", imageArchive}, images...)
	if err := service.Run(ctx, remote.ProcessOptions{Path: "docker", Args: args, Dir: work, Env: env, Streams: remote.Streams{Stdout: io.Discard, Stderr: io.Discard}}); err != nil {
		return "", errors.New("Docker image export failed; no Bundle was published")
	}
	if err := validateDockerArchive(imageArchive, images); err != nil {
		return "", fmt.Errorf("Docker transport archive is incomplete or unsupported (do not change/restart the daemon automatically): %w", err)
	}
	if err := os.WriteFile(filepath.Join(cache, "images/airgap-images.txt"), []byte(strings.Join(images, "\n")+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := writePreparationReport(cache, options, infraCatalog, solutionCatalog); err != nil {
		return "", err
	}
	runtimeArchive := filepath.Join(work, "runtime.tar.gz")
	if err := service.ExportRuntime(rt.Root, runtimeArchive); err != nil {
		return "", err
	}
	offline := filepath.Join(work, "offline.tar.gz")
	fmt.Fprintln(options.Stdout, "[INFO] Packaging Runtime, cache, models, snapshots and image archive")
	if err := buildOfflineArchive(runtimeArchive, cache, offline); err != nil {
		return "", err
	}
	// Validate real offline relocation before exposing the completed result.
	if _, err := managedruntime.BootstrapOffline(offline, filepath.Join(work, "runtime-check")); err != nil {
		return "", fmt.Errorf("validate offline Runtime: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	currentMetadata, err := os.ReadFile(verified.MetadataPath)
	if err != nil || sha256.Sum256(currentMetadata) != sourceDigest {
		return "", errors.New("original Release metadata changed during preparation; no Bundle was published")
	}
	if err := release.BuildAirgapBundle(options.Release, offline, output); err != nil {
		return "", err
	}
	fmt.Fprintf(options.Stdout, "[OK] Airgap Bundle verified: %s\n", output)
	return output, nil
}

func preparationEnvironment(work, cache, infra, solution string, rt managedruntime.Resolved, options Options, vllm string) map[string]string {
	env := map[string]string{
		"PATH": strings.Join(append(rt.ToolDirectories(), "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"), string(os.PathListSeparator)),
		"HOME": filepath.Join(work, "home"), "TMPDIR": filepath.Join(work, "tmp"), "LC_ALL": "C", "DEBIAN_FRONTEND": "noninteractive",
		"DOCKER_CONFIG": filepath.Join(work, "docker-auth"), "AWS_CONFIG_FILE": "/dev/null", "AWS_SHARED_CREDENTIALS_FILE": "/dev/null", "AWS_EC2_METADATA_DISABLED": "true",
		"AWS_ACCESS_KEY_ID": options.AWSCredential.AccessKeyID, "AWS_SECRET_ACCESS_KEY": options.AWSCredential.SecretAccessKey,
		"AWS_DEFAULT_REGION": options.AWSCredential.Region, "AWS_REGION": options.AWSCredential.Region,
		"EVA_INFRA_VERSION_FILE": infra, "EVA_SOLUTION_VERSION_FILE": solution, "EVA_CACHE_ROOT": cache,
		"EVA_SITE_ID": "airgap-preparation", "HARBOR_ASSET_DIR": filepath.Join(cache, "harbor"), "HARBOR_INSTALL_ROOT": filepath.Join(work, "harbor"),
		"COMPONENTS": "all", "PULL_PLATFORM": "linux/amd64", "STRICT_MISSING": "true", "DRIVER_REPO_MODE": "full", "NVIDIA_DRIVER_PACKAGE": options.DriverPackage,
		"EVA_AGENT_QDRANT_SNAPSHOT_SOURCE": "harbor", "EVA_AGENT_QDRANT_VALUES_FILE": "values-k3s.harbor.yaml", "EVA_AGENT_VLLM_VALUES_FILE": vllm,
	}
	// Proxy and public CA settings are transport configuration, not version or
	// component overrides. User AWS/Docker/Python configuration is not inherited.
	for _, key := range []string{"http_proxy", "https_proxy", "no_proxy", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR", "AWS_CA_BUNDLE"} {
		if value := os.Getenv(key); value != "" {
			env[key] = value
		}
	}
	return env
}

func runProcess(ctx context.Context, options remote.ProcessOptions) error {
	command := exec.CommandContext(ctx, options.Path, options.Args...)
	command.Dir, command.Stdin, command.Stdout, command.Stderr = options.Dir, options.Streams.Stdin, options.Streams.Stdout, options.Streams.Stderr
	command.Env = []string{}
	for key, value := range options.Env {
		command.Env = append(command.Env, key+"="+value)
	}
	return command.Run()
}

func ensureRuntime(root string) (managedruntime.Resolved, error) {
	if resolved, err := managedruntime.Resolve(root); err == nil {
		return resolved, nil
	}
	// Do not silently replace an existing damaged Runtime.
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		return managedruntime.Resolved{}, errors.New("existing managed Runtime is invalid; repair it before Airgap preparation")
	}
	return managedruntime.BootstrapOnline(root)
}

func preflight(ctx context.Context) error {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" || os.Geteuid() != 0 {
		return errors.New("Airgap preparation requires sudo on Ubuntu 24.04 linux/amd64")
	}
	osRelease, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return err
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(osRelease), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			fields[key] = strings.Trim(value, "\"")
		}
	}
	if fields["ID"] != "ubuntu" || fields["VERSION_ID"] != "24.04" {
		return errors.New("Airgap preparation supports Ubuntu 24.04 only; match the target package repositories and kernel")
	}
	for _, name := range []string{"bash", "curl", "docker", "aws", "sudo", "apt-get", "apt-cache", "apt", "dpkg", "dpkg-scanpackages", "gpg", "tar", "gzip", "unzip", "sha256sum", "find", "awk", "sed", "sort", "python3"} {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("Airgap preparation prerequisite missing: %s", name)
		}
	}
	command := exec.CommandContext(ctx, "docker", "info", "--format", "{{.OSType}}/{{.Architecture}}")
	output, err := command.Output()
	if value := strings.TrimSpace(string(output)); err != nil || value != "linux/x86_64" && value != "linux/amd64" {
		return errors.New("a working linux/amd64 Docker daemon is required for image transport")
	}
	return nil
}

func vllmValuesFile(catalog, profile string) (string, error) {
	contents, err := os.ReadFile(catalog)
	if err != nil {
		return "", err
	}
	var values map[string]string
	if err := yaml.Unmarshal(contents, &values); err != nil {
		return "", errors.New("invalid Release solution version catalog")
	}
	version := values["eva_agent_release"]
	if version == "" {
		version = values["eva_agent_deploy_version"]
	}
	parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
	if len(parts) < 3 {
		return "", errors.New("Release catalog must pin eva_agent_release")
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return "", errors.New("invalid pinned Agent version")
	}
	if major > 3 || major == 3 && minor >= 2 {
		return "values-k3s." + profile + ".docker.yaml", nil
	}
	return "values-k3s." + profile + ".yaml", nil
}

func validateAssets(cache string) error {
	for _, validate := range []func(string) error{remote.ValidateOfflineAssets, remote.ValidateModels, remote.ValidateQdrantSnapshots} {
		if err := validate(cache); err != nil {
			return err
		}
	}
	for _, prefix := range []string{"", "infra-"} {
		if err := remote.ValidateImageLists(cache, prefix+"images-all.txt", prefix+"images-pulled.txt", prefix+"images-missing.txt"); err != nil {
			return err
		}
	}
	for _, pattern := range []string{"python-debs/*.deb", "harbor/harbor-offline-installer-*.tgz", "nvidia/nvidia-driver-repo.tar.gz", "nvidia/nvidia-driver-repo.manifest.txt", "eva-iam/*.tgz", "eva-agent/eva-agent-init-*.tgz", "eva-agent/eva-agent-vllm-*.tgz", "qdrant/*.tgz", "k3s/k3s-airgap-images-amd64.tar.zst"} {
		matches, err := filepath.Glob(filepath.Join(cache, pattern))
		if err != nil || len(matches) == 0 {
			return fmt.Errorf("required offline asset is missing: %s", pattern)
		}
		for _, match := range matches {
			if err := requireNonempty(match); err != nil {
				return err
			}
		}
	}
	return nil
}

func requireNonempty(name string) error {
	file, err := openArchiveSource(name, nil)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return fmt.Errorf("required asset is empty: %s", filepath.Base(name))
	}
	return nil
}

func transportImageList(cache string) ([]string, error) {
	seen := map[string]bool{}
	for _, name := range []string{"images-pulled.txt", "infra-images-pulled.txt", "n8n-images.txt"} {
		file := filepath.Join(cache, "images", name)
		if err := requireNonempty(file); err != nil {
			return nil, err
		}
		contents, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(contents), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if !imagePattern.MatchString(line) {
				return nil, errors.New("invalid image reference in transport list")
			}
			seen[line] = true
		}
	}
	images := make([]string, 0, len(seen))
	for image := range seen {
		images = append(images, image)
	}
	sort.Strings(images)
	return images, nil
}

func writePreparationReport(cache string, options Options, infra, solution string) error {
	report := map[string]string{"schema_version": "v1", "release": options.Release.Metadata.Version, "platform": "ubuntu24.04/amd64", "vllm_profile": options.VLLMProfile, "driver_package": options.DriverPackage, "qdrant_snapshot_source": "harbor"}
	for key, file := range map[string]string{"infra_catalog_sha256": infra, "solution_catalog_sha256": solution, "source_release_sha256": options.Release.MetadataPath} {
		contents, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(contents)
		report[key] = hex.EncodeToString(digest[:])
	}
	contents, err := yaml.Marshal(report)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cache, "airgap-preparation.yaml"), contents, 0o600)
}
