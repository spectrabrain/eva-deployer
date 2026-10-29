package airgap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"eva-deployer/tools/eva/internal/release"
	"eva-deployer/tools/eva/internal/remote"
	managedruntime "eva-deployer/tools/eva/internal/runtime"
	"gopkg.in/yaml.v3"
)

var serviceTestSteps = []string{
	"scripts/download/download_offline_assets.sh",
	"scripts/download/download_python_venv_debs.sh",
	"scripts/install/setup_harbor.sh",
	"scripts/download/download_eva_images.sh",
	"scripts/download/download_infra_images.sh",
	"scripts/download/download_n8n_images.sh",
	"scripts/download/download_eva_models.sh",
	"scripts/download/download_qdrant_snapshots.sh",
	"scripts/download/download_vision_models.sh",
}

const serviceTestBackendRoot = "/trusted-airgap-test-backends"

// Every host-facing boundary is installed explicitly, rather than falling back
// to NewService. Only asset validation and Runtime export use production code.
type serviceTestFixture struct {
	t               *testing.T
	service         Service
	options         Options
	stdout          bytes.Buffer
	calls           []remote.ProcessOptions
	resolved        []string
	preflights      int
	runtimes        int
	validations     int
	exports         int
	infraCatalog    []byte
	solutionCatalog []byte
	dockerArchive   []byte
	failStep        string
	resolveFailure  string
	afterStep       func(string, string)
}

func serviceTestYAML(t *testing.T, value any) []byte {
	t.Helper()
	contents, err := yaml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func serviceTestRuntime(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	descriptor := managedruntime.Descriptor{
		SchemaVersion: "v1", Version: "1.0.1", Tools: map[string]string{},
	}
	for _, name := range []string{"ansible-playbook", "helm", "kubectl", "kustomize", "oras"} {
		descriptor.Tools[name] = "bin/" + name
		path := filepath.Join(root, "bin", name)
		archiveTestWriteFile(t, path, []byte("#!/bin/sh\nexit 97\n"))
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	archiveTestWriteFile(t, filepath.Join(root, "runtime.yaml"), serviceTestYAML(t, descriptor))
	archiveTestWriteFile(t, filepath.Join(root, "collections/ansible_collections/ansible/posix/MANIFEST.json"), []byte("{}\n"))
	if _, err := managedruntime.Resolve(root); err != nil {
		t.Fatalf("synthetic Runtime must pass production Resolve without executing tools: %v", err)
	}
	return root
}

func newServiceTestFixture(t *testing.T) *serviceTestFixture {
	t.Helper()
	f := &serviceTestFixture{t: t}
	f.infraCatalog = serviceTestYAML(t, map[string]string{"harbor_version": "2.12.2", "k3s_version": "v1.32.0+k3s1"})
	f.solutionCatalog = serviceTestYAML(t, map[string]string{"eva_agent_release": "3.2.0"})
	infra := []archiveTestEntry{
		archiveTestRegular("ansible.cfg", []byte("[defaults]\n")),
		archiveTestRegular("src/playbook-preflight.yaml", []byte("---\n[]\n")),
		archiveTestRegular("src/playbook-vars.yaml", []byte("---\n[]\n")),
		archiveTestRegular("src/infra/playbooks/site_precondition.yaml", []byte("---\n[]\n")),
		archiveTestRegular("src/infra/playbooks/site_infra.yaml", []byte("---\n[]\n")),
		archiveTestRegular("src/infra/version.yaml", f.infraCatalog),
	}
	solution := []archiveTestEntry{archiveTestRegular("src/solution/version.yaml", f.solutionCatalog)}
	root := t.TempDir()
	metadata := release.Metadata{Version: "3.2.0", Platform: release.Platform{OS: "linux", Arch: "amd64"}}
	for _, artifact := range []struct {
		name, file string
		contents   []byte
	}{
		{"eva-tool", "eva-tool.tar.gz", serviceTestCompressedTar(t, []archiveTestEntry{archiveTestRegular("bin/eva", []byte("synthetic tool"))})},
		{"eva-tool-installer", "eva-tool-installer.sh", []byte("#!/bin/sh\nexit 97\n")},
		{"eva-infra", "eva-infra.tar.gz", serviceTestCompressedTar(t, infra)},
		{"eva-solution", "eva-solution.tar.gz", serviceTestCompressedTar(t, solution)},
	} {
		archiveTestWriteFile(t, filepath.Join(root, artifact.file), artifact.contents)
		metadata.Artifacts = append(metadata.Artifacts, release.Artifact{
			Name: artifact.name, File: artifact.file, SHA256: archiveTestDigest(artifact.contents),
		})
	}
	archiveTestWriteFile(t, filepath.Join(root, "release.yaml"), serviceTestYAML(t, metadata))
	resolved, err := release.Resolve(root)
	if err != nil {
		t.Fatalf("synthetic original Release must be verified: %v", err)
	}
	f.options = Options{
		Release: resolved, Output: filepath.Join(t.TempDir(), "bundle.tar.gz"),
		VLLMProfile: "L40sx1", DriverPackage: "nvidia-driver-580",
		AWSCredential: remote.AWSCredential{
			AccessKeyID: "AKIA_SERVICE_TEST_DO_NOT_TRANSPORT", SecretAccessKey: "service-test-secret-do-not-transport", Region: "ap-northeast-2",
		},
		Stdout: &f.stdout,
	}
	f.dockerArchive = archiveTestTarBytes(t, archiveTestDockerEntries(t))
	f.service = Service{
		RuntimeRoot: serviceTestRuntime(t),
		Preflight: func(context.Context) error {
			f.preflights++
			return nil
		},
		ResolveBackend: func(step string) (string, error) {
			f.resolved = append(f.resolved, step)
			if step == f.resolveFailure {
				return "", errors.New("trusted backend unavailable")
			}
			for _, allowed := range serviceTestSteps {
				if step == allowed {
					return filepath.Join(serviceTestBackendRoot, step), nil
				}
			}
			t.Fatalf("unexpected backend resolution (no real resolver allowed): %q", step)
			return "", errors.New("unexpected backend")
		},
		Run: f.run,
		EnsureRuntime: func(root string) (managedruntime.Resolved, error) {
			f.runtimes++
			if root != f.service.RuntimeRoot {
				t.Fatalf("unexpected Runtime root %q", root)
			}
			return managedruntime.Resolve(root) // Never online bootstrap.
		},
		ExportRuntime: func(root, destination string) error {
			f.exports++
			return remote.ExportRuntimeArchive(root, destination)
		},
		ValidateAssets: func(cache string) error {
			f.validations++
			return validateAssets(cache)
		},
	}
	return f
}

func serviceTestCompressedTar(t *testing.T, entries []archiveTestEntry) []byte {
	t.Helper()
	contents, err := os.ReadFile(archiveTestWriteTar(t, entries, true))
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func (f *serviceTestFixture) run(_ context.Context, options remote.ProcessOptions) error {
	f.t.Helper()
	// Preserve snapshots even if the implementation subsequently mutates args/env.
	copyOptions := options
	copyOptions.Args = append([]string(nil), options.Args...)
	copyOptions.Env = make(map[string]string, len(options.Env))
	for key, value := range options.Env {
		copyOptions.Env[key] = value
	}
	f.calls = append(f.calls, copyOptions)
	if options.Streams.Stdout != io.Discard || options.Streams.Stderr != io.Discard {
		f.t.Fatal("backend diagnostics must not be connected to public output")
	}
	cache := options.Env["EVA_CACHE_ROOT"]
	if cache == "" || cache != filepath.Join(options.Dir, "cache") {
		f.t.Fatalf("backend must use its private work/cache: dir=%q cache=%q", options.Dir, cache)
	}
	if options.Path == "docker" {
		if len(options.Args) < 4 || !reflect.DeepEqual(options.Args[:3], []string{"save", "--output", filepath.Join(cache, "images/eva-images.tar")}) {
			f.t.Fatalf("unexpected Docker operation (only fake save allowed): %q", options.Args)
		}
		if f.failStep == "docker" {
			return errors.New("docker export private diagnostic: " + f.options.AWSCredential.SecretAccessKey)
		}
		return os.WriteFile(options.Args[2], f.dockerArchive, 0o600)
	}
	step := strings.TrimPrefix(options.Path, serviceTestBackendRoot+"/")
	allowed := false
	for _, candidate := range serviceTestSteps {
		allowed = allowed || step == candidate
	}
	if !allowed || options.Path != filepath.Join(serviceTestBackendRoot, step) {
		f.t.Fatalf("untrusted backend invocation: %q", options.Path)
	}
	if step == f.failStep {
		fmt.Fprintln(options.Streams.Stdout, f.options.AWSCredential.AccessKeyID)
		fmt.Fprintln(options.Streams.Stderr, f.options.AWSCredential.SecretAccessKey)
		return errors.New("backend private diagnostic: " + f.options.AWSCredential.SecretAccessKey)
	}
	f.writeStepAssets(step, options)
	if f.afterStep != nil {
		f.afterStep(step, cache)
	}
	return nil
}

func (f *serviceTestFixture) writeStepAssets(step string, options remote.ProcessOptions) {
	f.t.Helper()
	cache := options.Env["EVA_CACHE_ROOT"]
	write := func(name, contents string) {
		f.t.Helper()
		archiveTestWriteFile(f.t, filepath.Join(cache, name), []byte(contents))
	}
	switch step {
	case "scripts/download/download_offline_assets.sh":
		for _, name := range []string{
			"manifest.txt", "apt/debs/manifest.txt", "apt/debs/a.deb", "docker/debs/manifest.txt", "docker/debs/a.deb",
			"nvidia/container-toolkit-debs/manifest.txt", "nvidia/container-toolkit-debs/a.deb", "tools/oras",
			"k3s/k3s-v1-linux-amd64", "k3s/k3s-airgap-images-amd64.tar.zst",
			"eva-app/app.tgz", "eva-vision/vision.tgz", "eva-iam/iam.tgz", "eva-agent/agent.tgz",
			"eva-agent/eva-agent-init-3.2.0.tgz", "eva-agent/eva-agent-vllm-3.2.0.tgz", "qdrant/qdrant.tgz",
			"eva-agent/release/3.2.0/plugins/eva-agent-qdrant/post-renderer.sh",
			"eva-agent/release/3.2.0/plugins/eva-agent-qdrant/plugin.yaml",
			"nvidia/nvidia-driver-repo.tar.gz", "nvidia/nvidia-driver-repo.manifest.txt",
		} {
			write(name, "synthetic offline asset\n")
		}
		if options.Env["DISPLAY_MODE_SELECTOR_ZIP_PATH"] != "" {
			write("display_mode_selector/mode_selector_1.72.0/linux/x64/displaymodeselector", "synthetic display mode selector\n")
		}
		// Credentials generated outside the cache must not cross the packaging
		// boundary; rendered Helm secrets inside it must be excluded entirely.
		secret := f.options.AWSCredential.AccessKeyID + "\n" + f.options.AWSCredential.SecretAccessKey
		archiveTestWriteFile(f.t, filepath.Join(options.Env["HOME"], ".aws/credentials"), []byte(secret))
		archiveTestWriteFile(f.t, filepath.Join(options.Env["DOCKER_CONFIG"], "config.json"), []byte(secret))
		write("images/rendered/secrets/secret.yaml", secret)
	case "scripts/download/download_python_venv_debs.sh":
		write("python-debs/python3-venv.deb", "synthetic deb\n")
	case "scripts/install/setup_harbor.sh":
		write("harbor/harbor-offline-installer-v2.12.2.tgz", "synthetic Harbor installer\n")
	case "scripts/download/download_eva_images.sh", "scripts/download/download_infra_images.sh":
		prefix := ""
		if strings.HasSuffix(step, "download_infra_images.sh") {
			prefix = "infra-"
		}
		write("images/"+prefix+"images-all.txt", "example.test/eva:1\n")
		write("images/"+prefix+"images-pulled.txt", "example.test/eva:1\n")
		write("images/"+prefix+"images-missing.txt", "")
	case "scripts/download/download_n8n_images.sh":
		write("images/n8n-images.txt", "example.test/eva:1\n")
	case "scripts/download/download_eva_models.sh":
		write("models/agent/hf/model.bin", "agent weights\n")
		write("models/vllm/hf/model.bin", "vllm weights\n")
		write("models/manifest.txt", "files:\n"+filepath.Join(cache, "models/agent/hf/model.bin")+"\n"+filepath.Join(cache, "models/vllm/hf/model.bin")+"\n")
	case "scripts/download/download_qdrant_snapshots.sh":
		write("qdrant-snapshots/snapshot.bin", "snapshot\n")
		write("qdrant-snapshots/manifest.txt", "source: harbor\nsnapshot_specs:\ndir|snapshot.bin|collection\nfiles:\n"+filepath.Join(cache, "qdrant-snapshots/snapshot.bin")+"\n")
	case "scripts/download/download_vision_models.sh":
		archiveTestVisionCache(f.t, cache)
	default:
		f.t.Fatalf("no fake asset generator for %q", step)
	}
}

func (f *serviceTestFixture) assertClean() {
	f.t.Helper()
	if _, err := os.Lstat(f.options.Output); !errors.Is(err, os.ErrNotExist) {
		f.t.Fatalf("failed preparation exposed output: %v", err)
	}
	if matches, err := filepath.Glob(filepath.Join(filepath.Dir(f.options.Output), ".eva-airgap-*")); err != nil || len(matches) != 0 {
		f.t.Fatalf("temporary preparation/build files remain: %q, %v", matches, err)
	}
	for _, call := range f.calls {
		if _, err := os.Lstat(call.Dir); !errors.Is(err, os.ErrNotExist) {
			f.t.Fatalf("private work directory was not removed: %q: %v", call.Dir, err)
		}
	}
	if strings.Contains(f.stdout.String(), "[OK]") {
		f.t.Fatal("failed preparation reported success")
	}
}

func TestNewServiceDefaults(t *testing.T) {
	service := NewService()
	if service.RuntimeRoot != managedruntime.DefaultRoot || service.Preflight == nil || service.ResolveBackend == nil ||
		service.Run == nil || service.EnsureRuntime == nil || service.ExportRuntime == nil || service.ValidateAssets == nil {
		t.Fatalf("NewService has incomplete defaults: %#v", service)
	}
}

func TestServicePrepareVerifiedImportableBundle(t *testing.T) {
	for _, key := range []string{"EVA_K3S_VERSION", "EVA_AGENT_VERSION", "EVA_HARBOR_VERSION", "EVA_AGENT_RELEASE", "AWS_PROFILE", "AWS_DEFAULT_PROFILE", "DOCKER_CONFIG", "PYTHONPATH", "BASH_ENV"} {
		t.Setenv(key, "inherited-private-override")
	}
	t.Setenv("EVA_INFRA_VERSION_FILE", "/untrusted/infra.yaml")
	t.Setenv("EVA_SOLUTION_VERSION_FILE", "/untrusted/solution.yaml")
	t.Setenv("AWS_ACCESS_KEY_ID", "inherited-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "inherited-secret-key")
	t.Setenv("HTTPS_PROXY", "https://proxy.example.test:8443")
	f := newServiceTestFixture(t)
	before := serviceTestReleaseContents(t, f.options.Release)
	result, err := f.service.Prepare(context.Background(), f.options)
	if err != nil {
		t.Fatalf("Prepare with production validators/exporter: %v", err)
	}
	if result != f.options.Output || f.preflights != 1 || f.runtimes != 1 || f.validations != 1 || f.exports != 1 {
		t.Fatalf("pipeline boundaries: result=%q preflight=%d runtime=%d validate=%d export=%d", result, f.preflights, f.runtimes, f.validations, f.exports)
	}
	if !reflect.DeepEqual(f.resolved, serviceTestSteps) || len(f.calls) != len(serviceTestSteps)+1 {
		t.Fatalf("unexpected pipeline: resolved=%q calls=%d", f.resolved, len(f.calls))
	}
	for i, call := range f.calls {
		if i < len(serviceTestSteps) {
			if call.Path != filepath.Join(serviceTestBackendRoot, serviceTestSteps[i]) {
				t.Fatalf("step %d = %q", i, call.Path)
			}
			if serviceTestSteps[i] == "scripts/install/setup_harbor.sh" {
				if !reflect.DeepEqual(call.Args, []string{"--download-only"}) {
					t.Fatalf("Harbor must only download, got %q", call.Args)
				}
			} else if len(call.Args) != 0 {
				t.Fatalf("unexpected backend arguments: %q", call.Args)
			}
		} else if !reflect.DeepEqual(call.Args[3:], []string{"example.test/eva:1"}) {
			t.Fatalf("transport union was not deduplicated: %q", call.Args)
		}
		serviceTestCheckEnvironment(t, f, call)
		if _, err := os.Lstat(call.Dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("successful preparation retained private staging: %v", err)
		}
	}
	if got := serviceTestReleaseContents(t, f.options.Release); !reflect.DeepEqual(got, before) {
		t.Fatal("original verified Release was mutated")
	}
	metadata, err := release.VerifyAirgapBundle(result)
	if err != nil {
		t.Fatalf("VerifyAirgapBundle: %v", err)
	}
	imported, err := release.ImportAirgapBundle(result, filepath.Join(t.TempDir(), "imports"))
	if err != nil {
		t.Fatalf("ImportAirgapBundle: %v", err)
	}
	if !reflect.DeepEqual(metadata, imported.Metadata) || metadata.Version != f.options.Release.Metadata.Version {
		t.Fatal("verified/imported metadata differs")
	}
	if len(metadata.Artifacts) != len(f.options.Release.Metadata.Artifacts)+1 {
		t.Fatalf("unexpected derived artifacts: %#v", metadata.Artifacts)
	}
	for _, artifact := range metadata.Artifacts {
		if artifact.File != filepath.Base(artifact.File) {
			t.Fatalf("Bundle must have a flat first-install layout: %q", artifact.File)
		}
	}
	for _, name := range []string{"release.yaml", "checksums.sha256", "eva-tool-installer.sh", "eva-tool_3.2.0_linux_amd64.tar.gz"} {
		if _, err := os.Stat(filepath.Join(imported.Root, name)); err != nil {
			t.Fatalf("missing first-install file %q: %v", name, err)
		}
	}
	offline, err := imported.ArtifactPath("eva-offline")
	if err != nil {
		t.Fatal(err)
	}
	entries := archiveTestReadOffline(t, offline)
	for _, name := range []string{
		"runtime/runtime.yaml", "runtime/bin/ansible-playbook", "runtime/bin/helm", "runtime/bin/kubectl", "runtime/bin/kustomize", "runtime/bin/oras",
		"out/cache/manifest.txt", "out/cache/python-debs/python3-venv.deb", "out/cache/harbor/harbor-offline-installer-v2.12.2.tgz",
		"out/cache/models/agent/hf/model.bin", "out/cache/models/vllm/hf/model.bin", "out/cache/qdrant-snapshots/snapshot.bin",
		"out/cache/vision-models/manifest.json", "out/cache/vision-models/model_dir/Owl-v2/weights/model.bin",
		"out/cache/images/eva-images.tar", "out/cache/images/airgap-images.txt", "out/cache/airgap-preparation.yaml",
	} {
		if len(entries[name].body) == 0 {
			t.Errorf("required nonempty Offline entry missing: %q", name)
		}
	}
	if !bytes.Equal(entries["out/cache/images/eva-images.tar"].body, f.dockerArchive) {
		t.Fatal("Docker save payload changed during packaging")
	}
	if got := string(entries["out/cache/images/airgap-images.txt"].body); got != "example.test/eva:1\n" {
		t.Fatalf("transport image manifest = %q", got)
	}
	for name, entry := range entries {
		if !strings.HasPrefix(name, "runtime/") && !strings.HasPrefix(name, "out/cache/") {
			t.Errorf("unexpected Offline entry: %q", name)
		}
		if strings.Contains(name, "images/rendered") || strings.Contains(name, "docker-auth") || strings.Contains(name, ".aws") {
			t.Errorf("private/rendered path transported: %q", name)
		}
		for _, secret := range []string{f.options.AWSCredential.AccessKeyID, f.options.AWSCredential.SecretAccessKey, "inherited-private-override", "inherited-secret-key"} {
			if bytes.Contains(entry.body, []byte(secret)) || strings.Contains(f.stdout.String(), secret) {
				t.Errorf("private value leaked in %q or public output", name)
			}
		}
	}
	for _, name := range []string{"out/cache/models/manifest.txt", "out/cache/qdrant-snapshots/manifest.txt"} {
		body := string(entries[name].body)
		if strings.Contains(body, f.calls[0].Dir) || !strings.Contains(body, "out/cache/") {
			t.Errorf("manifest not portable: %s = %q", name, body)
		}
	}
	var report map[string]string
	if err := yaml.Unmarshal(entries["out/cache/airgap-preparation.yaml"].body, &report); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"schema_version": "v1", "release": "3.2.0", "platform": "ubuntu24.04/amd64", "vllm_profile": "L40sx1",
		"driver_package": "nvidia-driver-580", "qdrant_snapshot_source": "harbor",
		"infra_catalog_sha256": archiveTestDigest(f.infraCatalog), "solution_catalog_sha256": archiveTestDigest(f.solutionCatalog),
		"source_release_sha256": archiveTestDigest(before["release.yaml"]),
	} {
		if report[key] != want {
			t.Errorf("report[%q] = %q, want %q", key, report[key], want)
		}
	}
	if _, err := managedruntime.BootstrapOffline(offline, filepath.Join(t.TempDir(), "relocated-runtime")); err != nil {
		t.Fatalf("produced Offline Runtime is not relocatable: %v", err)
	}
	if !strings.Contains(f.stdout.String(), "[OK] Airgap Bundle verified:") {
		t.Fatalf("missing completion output: %s", &f.stdout)
	}
	if matches, err := filepath.Glob(filepath.Join(filepath.Dir(result), ".eva-airgap-*")); err != nil || len(matches) != 0 {
		t.Fatalf("successful preparation left staging files: %q, %v", matches, err)
	}
}

func serviceTestReleaseContents(t *testing.T, resolved release.Resolved) map[string][]byte {
	t.Helper()
	contents := map[string][]byte{}
	names := []string{"release.yaml"}
	for _, artifact := range resolved.Metadata.Artifacts {
		names = append(names, artifact.File)
	}
	for _, name := range names {
		body, err := os.ReadFile(filepath.Join(resolved.Root, name))
		if err != nil {
			t.Fatal(err)
		}
		contents[name] = body
	}
	return contents
}

func serviceTestCheckEnvironment(t *testing.T, f *serviceTestFixture, call remote.ProcessOptions) {
	t.Helper()
	env := call.Env
	for _, key := range []string{"EVA_K3S_VERSION", "EVA_AGENT_VERSION", "EVA_HARBOR_VERSION", "EVA_AGENT_RELEASE", "AWS_PROFILE", "AWS_DEFAULT_PROFILE", "PYTHONPATH", "BASH_ENV"} {
		if _, present := env[key]; present {
			t.Errorf("inherited configuration passed to backend: %s", key)
		}
	}
	for key, want := range map[string]string{
		"HOME": filepath.Join(call.Dir, "home"), "DOCKER_CONFIG": filepath.Join(call.Dir, "docker-auth"), "TMPDIR": filepath.Join(call.Dir, "tmp"),
		"EVA_CACHE_ROOT": filepath.Join(call.Dir, "cache"), "AWS_CONFIG_FILE": "/dev/null", "AWS_SHARED_CREDENTIALS_FILE": "/dev/null",
		"AWS_EC2_METADATA_DISABLED": "true", "AWS_ACCESS_KEY_ID": f.options.AWSCredential.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY": f.options.AWSCredential.SecretAccessKey, "AWS_REGION": f.options.AWSCredential.Region, "AWS_DEFAULT_REGION": f.options.AWSCredential.Region,
		"COMPONENTS": "all", "PULL_PLATFORM": "linux/amd64", "STRICT_MISSING": "true", "DRIVER_REPO_MODE": "full",
		"NVIDIA_DRIVER_PACKAGE": f.options.DriverPackage, "EVA_AGENT_QDRANT_SNAPSHOT_SOURCE": "harbor",
		"EVA_AGENT_QDRANT_VALUES_FILE": "values-k3s.harbor.yaml", "EVA_AGENT_VLLM_VALUES_FILE": "values-k3s.L40sx1.docker.yaml",
		"HARBOR_ASSET_DIR": filepath.Join(call.Dir, "cache/harbor"), "HARBOR_INSTALL_ROOT": filepath.Join(call.Dir, "harbor"),
		"HTTPS_PROXY": "https://proxy.example.test:8443",
	} {
		if env[key] != want {
			t.Errorf("backend env[%q] = %q, want %q", key, env[key], want)
		}
	}
	if !strings.HasPrefix(env["PATH"], filepath.Join(f.service.RuntimeRoot, "bin")+string(os.PathListSeparator)) {
		t.Errorf("managed Runtime tools are not first in PATH: %q", env["PATH"])
	}
	for _, key := range []string{"HOME", "DOCKER_CONFIG", "TMPDIR"} {
		if env[key] == env["EVA_CACHE_ROOT"] || strings.HasPrefix(env[key], env["EVA_CACHE_ROOT"]+string(os.PathSeparator)) {
			t.Errorf("private backend directory is inside cache: %s=%s", key, env[key])
		}
	}
	// Catalog contents are captured during execution below; after Prepare returns
	// staging must be gone, so the persisted environment records prove provenance.
	for key, relative := range map[string]string{"EVA_INFRA_VERSION_FILE": "src/infra/version.yaml", "EVA_SOLUTION_VERSION_FILE": "src/solution/version.yaml"} {
		want := filepath.Join(call.Dir, "releases", f.options.Release.Metadata.Version, relative)
		if env[key] != want {
			t.Errorf("backend catalog not extracted from the verified Release: %s=%q, want %q", key, env[key], want)
		}
	}
}

func TestServicePrepareReadsExtractedPinnedCatalogs(t *testing.T) {
	f := newServiceTestFixture(t)
	f.afterStep = func(_ string, cache string) {
		call := f.calls[len(f.calls)-1]
		for key, want := range map[string][]byte{"EVA_INFRA_VERSION_FILE": f.infraCatalog, "EVA_SOLUTION_VERSION_FILE": f.solutionCatalog} {
			got, err := os.ReadFile(call.Env[key])
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("backend catalog %s does not contain Release pins: %q, %v", key, got, err)
			}
		}
		for _, name := range []string{call.Dir, cache, call.Env["HOME"], call.Env["DOCKER_CONFIG"], call.Env["TMPDIR"]} {
			info, err := os.Stat(name)
			if err != nil {
				t.Fatal(err)
			}
			if !info.IsDir() || info.Mode().Perm() != 0o700 {
				t.Errorf("preparation directory must be private: %s mode=%v", name, info.Mode())
			}
		}
	}
	if _, err := f.service.Prepare(context.Background(), f.options); err != nil {
		t.Fatal(err)
	}
}

func TestServicePrepareRejectsSourceMetadataChangedDuringDownloads(t *testing.T) {
	f := newServiceTestFixture(t)
	f.afterStep = func(step, _ string) {
		if step != serviceTestSteps[len(serviceTestSteps)-1] {
			return
		}
		metadata := f.options.Release.Metadata
		metadata.Version = "3.2.1"
		archiveTestWriteFile(t, f.options.Release.MetadataPath, serviceTestYAML(t, metadata))
	}
	if output, err := f.service.Prepare(context.Background(), f.options); err == nil || output != "" || !strings.Contains(err.Error(), "metadata changed") {
		t.Fatalf("changed source identity accepted: output=%s error=%v", output, err)
	}
	f.assertClean()
}

func TestServicePrepareRevalidatesOnDiskArtifactsBeforeDownloads(t *testing.T) {
	f := newServiceTestFixture(t)
	archiveTestWriteFile(t, filepath.Join(f.options.Release.Root, "eva-tool.tar.gz"), []byte("corrupt"))
	if output, err := f.service.Prepare(context.Background(), f.options); err == nil || output != "" {
		t.Fatal("stale verified source was accepted")
	}
	if f.preflights != 0 || len(f.calls) != 0 {
		t.Fatal("corrupt source started host preparation")
	}
	f.assertClean()
}

func TestHarborDownloadOnlyBackendDoesNotDeploy(t *testing.T) {
	backend, err := filepath.Abs("../../../../scripts/install/setup_harbor.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backend); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	archiveTestWriteFile(t, filepath.Join(bin, "curl"), []byte("#!/bin/sh\nwhile [ $# -gt 0 ]; do\n if [ \"$1\" = -o ]; then shift; printf 'fake downloaded installer' > \"$1\"; exit 0; fi\n shift\ndone\nexit 98\n"))
	archiveTestWriteFile(t, filepath.Join(bin, "docker"), []byte("#!/bin/sh\nprintf invoked > \"$HOME/docker-invoked\"\nexit 99\n"))
	for _, name := range []string{"curl", "docker"} {
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cache := filepath.Join(root, "cache")
	err = runProcess(context.Background(), remote.ProcessOptions{
		Path: "/bin/bash", Args: []string{backend, "--download-only"}, Dir: root,
		Env:     map[string]string{"PATH": bin + ":/usr/bin:/bin", "HOME": root, "EVA_SITE_ID": "airgap-test", "EVA_CACHE_ROOT": cache, "HARBOR_INSTALL_ROOT": filepath.Join(root, "harbor-runtime")},
		Streams: remote.Streams{Stdout: io.Discard, Stderr: io.Discard},
	})
	if err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(cache, "harbor/harbor-offline-installer-*.tgz"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("installer not downloaded: %v %v", matches, err)
	}
	if _, err := os.Stat(filepath.Join(root, "docker-invoked")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("download-only backend invoked Docker")
	}
	if _, err := os.Stat(filepath.Join(root, "harbor-runtime/harbor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("download-only backend extracted/deployed Harbor")
	}
}

func TestServicePrepareStopsAtEveryFailedDownload(t *testing.T) {
	for i, step := range serviceTestSteps {
		t.Run(filepath.Base(step), func(t *testing.T) {
			f := newServiceTestFixture(t)
			f.failStep = step
			result, err := f.service.Prepare(context.Background(), f.options)
			if err == nil || result != "" || !strings.Contains(err.Error(), filepath.Base(step)) {
				t.Fatalf("failed backend result=%q error=%v", result, err)
			}
			if len(f.calls) != i+1 || !reflect.DeepEqual(f.resolved, serviceTestSteps[:i+1]) || f.validations != 0 || f.exports != 0 {
				t.Fatalf("work continued after failure: calls=%d resolved=%q validations=%d exports=%d", len(f.calls), f.resolved, f.validations, f.exports)
			}
			for _, private := range []string{"backend private diagnostic", f.options.AWSCredential.AccessKeyID, f.options.AWSCredential.SecretAccessKey} {
				if strings.Contains(err.Error(), private) || strings.Contains(f.stdout.String(), private) {
					t.Fatal("backend error or diagnostic leaked to public output")
				}
			}
			f.assertClean()
		})
	}
}

func TestServicePrepareStopsAtUnresolvableBackend(t *testing.T) {
	for i, step := range serviceTestSteps {
		t.Run(filepath.Base(step), func(t *testing.T) {
			f := newServiceTestFixture(t)
			f.resolveFailure = step
			result, err := f.service.Prepare(context.Background(), f.options)
			if err == nil || result != "" || !strings.Contains(err.Error(), "trusted backend unavailable") {
				t.Fatalf("unresolvable backend result=%q error=%v", result, err)
			}
			if len(f.calls) != i || !reflect.DeepEqual(f.resolved, serviceTestSteps[:i+1]) || f.validations != 0 || f.exports != 0 {
				t.Fatal("pipeline continued after backend resolution failure")
			}
			f.assertClean()
		})
	}
}

func TestServicePrepareRejectsIncompleteAssets(t *testing.T) {
	for _, relative := range []string{
		"manifest.txt", "apt/debs/a.deb", "docker/debs/a.deb", "nvidia/container-toolkit-debs/a.deb", "tools/oras",
		"eva-app/app.tgz", "eva-vision/vision.tgz", "eva-agent/release/3.2.0/plugins/eva-agent-qdrant/plugin.yaml",
		"python-debs/python3-venv.deb", "harbor/harbor-offline-installer-v2.12.2.tgz", "nvidia/nvidia-driver-repo.tar.gz",
		"nvidia/nvidia-driver-repo.manifest.txt", "eva-iam/iam.tgz", "eva-agent/eva-agent-init-3.2.0.tgz",
		"eva-agent/eva-agent-vllm-3.2.0.tgz", "qdrant/qdrant.tgz", "k3s/k3s-airgap-images-amd64.tar.zst",
		"models/agent/hf/model.bin", "models/vllm/hf/model.bin", "models/manifest.txt", "qdrant-snapshots/snapshot.bin", "qdrant-snapshots/manifest.txt",
		"vision-models/manifest.json", "vision-models/model_dir/Owl-v2/weights/model.bin",
		"images/images-all.txt", "images/infra-images-pulled.txt",
	} {
		t.Run(relative, func(t *testing.T) {
			f := newServiceTestFixture(t)
			f.afterStep = func(step, cache string) {
				if step == serviceTestSteps[len(serviceTestSteps)-1] {
					if err := os.Remove(filepath.Join(cache, relative)); err != nil {
						t.Fatal(err)
					}
				}
			}
			result, err := f.service.Prepare(context.Background(), f.options)
			if err == nil || result != "" || !strings.Contains(err.Error(), "validate downloaded assets") {
				t.Fatalf("incomplete assets accepted: result=%q error=%v", result, err)
			}
			if len(f.calls) != len(serviceTestSteps) || f.validations != 1 || f.exports != 0 {
				t.Fatal("asset validation failure did not stop before Docker/export")
			}
			f.assertClean()
		})
	}
	for _, tc := range []struct{ name, path, contents string }{
		{"empty Harbor installer", "harbor/harbor-offline-installer-v2.12.2.tgz", ""},
		{"missing product image", "images/images-missing.txt", "example.test/eva:1\n"},
		{"different infra image lists", "images/infra-images-pulled.txt", "example.test/eva:latest\n"},
		{"model manifest outside cache", "models/manifest.txt", "files:\n/does-not-exist/model.bin\n"},
		{"invalid snapshot specs", "qdrant-snapshots/manifest.txt", "snapshot_specs:\ninvalid\nfiles:\n"},
		{"corrupt Vision model", "vision-models/model_dir/Owl-v2/weights/model.bin", "wrong weights\n"},
		{"extra Vision file", "vision-models/model_dir/Owl-v2/extra.bin", "extra\n"},
		{"invalid Vision manifest", "vision-models/manifest.json", "{}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServiceTestFixture(t)
			f.afterStep = func(step, cache string) {
				if step == serviceTestSteps[len(serviceTestSteps)-1] {
					archiveTestWriteFile(t, filepath.Join(cache, tc.path), []byte(tc.contents))
				}
			}
			if result, err := f.service.Prepare(context.Background(), f.options); err == nil || result != "" {
				t.Fatalf("invalid downloaded assets accepted: %q, %v", result, err)
			}
			if len(f.calls) != len(serviceTestSteps) || f.exports != 0 {
				t.Fatal("invalid assets reached Docker/export")
			}
			f.assertClean()
		})
	}
}

func TestServicePrepareRejectsIncompleteDockerTransport(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *serviceTestFixture)
	}{
		{"save failure", func(_ *testing.T, f *serviceTestFixture) { f.failStep = "docker" }},
		{"empty save", func(_ *testing.T, f *serviceTestFixture) { f.dockerArchive = nil }},
		{"missing layer", func(t *testing.T, f *serviceTestFixture) {
			var entries []archiveTestEntry
			for _, entry := range archiveTestDockerEntries(t) {
				if entry.header.Name != "second/layer.tar" {
					entries = append(entries, entry)
				}
			}
			f.dockerArchive = archiveTestTarBytes(t, entries)
		}},
		{"missing requested tag", func(t *testing.T, f *serviceTestFixture) {
			entries := archiveTestChangeManifest(t, archiveTestDockerEntries(t), func(manifest []dockerSaveManifestEntry) {
				manifest[0].RepoTags = []string{"example.test/eva:latest"}
			})
			f.dockerArchive = archiveTestTarBytes(t, entries)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServiceTestFixture(t)
			tc.mutate(t, f)
			result, err := f.service.Prepare(context.Background(), f.options)
			if err == nil || result != "" || !strings.Contains(err.Error(), "Docker") {
				t.Fatalf("bad Docker transport accepted: %q, %v", result, err)
			}
			if f.validations != 1 || f.exports != 0 || len(f.calls) != len(serviceTestSteps)+1 {
				t.Fatal("bad Docker transport reached Runtime packaging")
			}
			if strings.Contains(err.Error(), f.options.AWSCredential.SecretAccessKey) || strings.Contains(f.stdout.String(), f.options.AWSCredential.SecretAccessKey) {
				t.Fatal("Docker diagnostic leaked")
			}
			f.assertClean()
		})
	}
}

func TestServicePrepareRejectsUnsafeCacheCredentials(t *testing.T) {
	for _, name := range []string{"aws_key.ini", "tools/credentials/aws.ini", "harbor/.docker/config.json", "tools/server.key", "tools/.env", "images/.aws/credentials"} {
		t.Run(name, func(t *testing.T) {
			f := newServiceTestFixture(t)
			f.afterStep = func(step, cache string) {
				if step == serviceTestSteps[len(serviceTestSteps)-1] {
					archiveTestWriteFile(t, filepath.Join(cache, name), []byte(f.options.AWSCredential.SecretAccessKey))
				}
			}
			if result, err := f.service.Prepare(context.Background(), f.options); err == nil || result != "" {
				t.Fatalf("unsafe cache credential accepted: %q, %v", result, err)
			}
			f.assertClean()
		})
	}
}

func TestServicePrepareRejectsOptionsBeforePreflight(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *Options)
	}{
		{"missing output", func(_ *testing.T, o *Options) { o.Output = "" }},
		{"wrong output suffix", func(_ *testing.T, o *Options) { o.Output += ".zip" }},
		{"missing profile", func(_ *testing.T, o *Options) { o.VLLMProfile = "" }},
		{"unsupported profile", func(_ *testing.T, o *Options) { o.VLLMProfile = "all" }},
		{"wrong case profile", func(_ *testing.T, o *Options) { o.VLLMProfile = "l40sx1" }},
		{"missing driver", func(_ *testing.T, o *Options) { o.DriverPackage = "" }},
		{"driver injection", func(_ *testing.T, o *Options) { o.DriverPackage = "nvidia-driver-580; touch /tmp/forbidden" }},
		{"driver options", func(_ *testing.T, o *Options) { o.DriverPackage = "nvidia-driver-580 --extra" }},
		{"driver suffix", func(_ *testing.T, o *Options) { o.DriverPackage = "nvidia-driver-580-server" }},
		{"existing output", func(t *testing.T, o *Options) { archiveTestWriteFile(t, o.Output, []byte("preserve me")) }},
		{"output directory", func(t *testing.T, o *Options) {
			if err := os.Mkdir(o.Output, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"dangling output symlink", func(t *testing.T, o *Options) {
			if err := os.Symlink(filepath.Join(t.TempDir(), "absent"), o.Output); err != nil {
				t.Fatal(err)
			}
		}},
		{"output symlink", func(t *testing.T, o *Options) {
			target := filepath.Join(t.TempDir(), "target")
			archiveTestWriteFile(t, target, []byte("preserve me"))
			if err := os.Symlink(target, o.Output); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing selector", func(t *testing.T, o *Options) { o.DisplayModeSelector = filepath.Join(t.TempDir(), "missing.zip") }},
		{"selector directory", func(t *testing.T, o *Options) { o.DisplayModeSelector = t.TempDir() }},
		{"selector symlink", func(t *testing.T, o *Options) {
			target := filepath.Join(t.TempDir(), "selector.zip")
			archiveTestWriteFile(t, target, []byte("synthetic approved asset"))
			o.DisplayModeSelector = filepath.Join(t.TempDir(), "link.zip")
			if err := os.Symlink(target, o.DisplayModeSelector); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServiceTestFixture(t)
			tc.mutate(t, &f.options)
			before, beforeErr := os.Lstat(f.options.Output)
			result, err := f.service.Prepare(context.Background(), f.options)
			if err == nil || result != "" || f.preflights != 0 || f.runtimes != 0 || len(f.calls) != 0 || len(f.resolved) != 0 {
				t.Fatalf("invalid options were not rejected before any host work: %q, %v", result, err)
			}
			if beforeErr == nil {
				after, err := os.Lstat(f.options.Output)
				if err != nil || !os.SameFile(before, after) {
					t.Fatalf("existing output changed: %v", err)
				}
				if !after.IsDir() {
					if got, err := os.ReadFile(f.options.Output); err == nil && string(got) != "preserve me" {
						t.Fatalf("existing output contents changed: %q", got)
					}
				}
			} else if f.options.Output != "" {
				if _, err := os.Lstat(f.options.Output); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("invalid request created an output: %v", err)
				}
			}
		})
	}
}

func TestValidateOptionsAcceptsSupportedProfilesAndDrivers(t *testing.T) {
	for _, profile := range []string{"A6000x1", "L40sx1", "PRO5000x3", "PRO6000-MIGx4"} {
		for _, driver := range []string{"nvidia-driver-580", "nvidia-driver-580-open", "nvidia-driver-535"} {
			t.Run(profile+"/"+driver, func(t *testing.T) {
				options := Options{Output: filepath.Join(t.TempDir(), "bundle.tar.gz"), VLLMProfile: profile, DriverPackage: driver}
				if err := ValidateOptions(options); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestServicePrepareDisplayModeSelectorAndOpenDriver(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("download-missing=%t", missing), func(t *testing.T) {
			f := newServiceTestFixture(t)
			f.options.DriverPackage = "nvidia-driver-580-open"
			f.options.VLLMProfile = "PRO6000-MIGx4"
			f.options.DisplayModeSelector = filepath.Join(t.TempDir(), "approved-selector.zip")
			archiveTestWriteFile(t, f.options.DisplayModeSelector, []byte("operator-approved synthetic ZIP"))
			f.afterStep = func(step, cache string) {
				env := f.calls[len(f.calls)-1].Env
				if env["DISPLAY_MODE_SELECTOR_ZIP_PATH"] != f.options.DisplayModeSelector || env["NVIDIA_DRIVER_PACKAGE"] != "nvidia-driver-580-open" || env["EVA_AGENT_VLLM_VALUES_FILE"] != "values-k3s.PRO6000-MIGx4.docker.yaml" {
					t.Fatal("approved selector/profile/driver flags not passed to backend")
				}
				if missing && step == serviceTestSteps[len(serviceTestSteps)-1] {
					if err := os.Remove(filepath.Join(cache, "display_mode_selector/mode_selector_1.72.0/linux/x64/displaymodeselector")); err != nil {
						t.Fatal(err)
					}
				}
			}
			result, err := f.service.Prepare(context.Background(), f.options)
			if missing {
				if err == nil || result != "" || len(f.calls) != len(serviceTestSteps) {
					t.Fatalf("missing selector executable accepted: %q, %v", result, err)
				}
				f.assertClean()
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestServicePrepareRejectsInvalidInputsBeforePreflight(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *serviceTestFixture)
	}{
		{"prepared Release", func(_ *testing.T, f *serviceTestFixture) { f.options.Release.Prepared = true }},
		{"imported Bundle", func(t *testing.T, f *serviceTestFixture) {
			archiveTestWriteFile(t, filepath.Join(f.options.Release.Root, ".eva-airgap-bundle"), []byte("bundle\n"))
		}},
		{"missing installer", func(_ *testing.T, f *serviceTestFixture) {
			var artifacts []release.Artifact
			for _, artifact := range f.options.Release.Metadata.Artifacts {
				if artifact.Name != "eva-tool-installer" {
					artifacts = append(artifacts, artifact)
				}
			}
			f.options.Release.Metadata.Artifacts = artifacts
		}},
		{"missing AWS access key", func(_ *testing.T, f *serviceTestFixture) { f.options.AWSCredential.AccessKeyID = "" }},
		{"missing AWS secret", func(_ *testing.T, f *serviceTestFixture) { f.options.AWSCredential.SecretAccessKey = "" }},
		{"missing AWS region", func(_ *testing.T, f *serviceTestFixture) { f.options.AWSCredential.Region = "" }},
		{"no preflight", func(_ *testing.T, f *serviceTestFixture) { f.service.Preflight = nil }},
		{"no resolver", func(_ *testing.T, f *serviceTestFixture) { f.service.ResolveBackend = nil }},
		{"no runner", func(_ *testing.T, f *serviceTestFixture) { f.service.Run = nil }},
		{"no Runtime preparation", func(_ *testing.T, f *serviceTestFixture) { f.service.EnsureRuntime = nil }},
		{"no Runtime export", func(_ *testing.T, f *serviceTestFixture) { f.service.ExportRuntime = nil }},
		{"no asset validator", func(_ *testing.T, f *serviceTestFixture) { f.service.ValidateAssets = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServiceTestFixture(t)
			tc.mutate(t, f)
			if result, err := f.service.Prepare(context.Background(), f.options); err == nil || result != "" {
				t.Fatalf("invalid preparation input accepted: %q, %v", result, err)
			}
			if f.preflights != 0 || f.runtimes != 0 || len(f.calls) != 0 || len(f.resolved) != 0 {
				t.Fatal("invalid input triggered host-facing work")
			}
			f.assertClean()
		})
	}
}

func TestServicePrepareBoundaryFailuresAndCancellation(t *testing.T) {
	for _, boundary := range []string{"preflight", "Runtime preparation", "Runtime export", "offline Runtime validation", "cancelled before publication", "corrupted original Release"} {
		t.Run(boundary, func(t *testing.T) {
			f := newServiceTestFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("injected boundary failure")
			switch boundary {
			case "preflight":
				f.service.Preflight = func(context.Context) error { f.preflights++; return failure }
			case "Runtime preparation":
				f.service.EnsureRuntime = func(string) (managedruntime.Resolved, error) { f.runtimes++; return managedruntime.Resolved{}, failure }
			case "Runtime export":
				f.service.ExportRuntime = func(string, string) error { f.exports++; return failure }
			case "offline Runtime validation":
				f.service.ExportRuntime = func(_ string, output string) error {
					body, err := os.ReadFile(archiveTestRuntime(t)) // Safe tar, but intentionally invalid descriptor.
					if err != nil {
						return err
					}
					return os.WriteFile(output, body, 0o600)
				}
			case "cancelled before publication":
				export := f.service.ExportRuntime
				f.service.ExportRuntime = func(root, output string) error {
					err := export(root, output)
					cancel()
					return err
				}
			case "corrupted original Release":
				archiveTestWriteFile(t, filepath.Join(f.options.Release.Root, "eva-tool.tar.gz"), []byte("changed after Resolve"))
			}
			result, err := f.service.Prepare(ctx, f.options)
			if err == nil || result != "" {
				t.Fatalf("failed boundary published a Bundle: %q, %v", result, err)
			}
			if boundary == "cancelled before publication" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation not propagated: %v", err)
			}
			if (boundary == "preflight" || boundary == "Runtime preparation") && len(f.calls) != 0 {
				t.Fatal("download began after early boundary failure")
			}
			if boundary == "offline Runtime validation" && !strings.Contains(err.Error(), "validate offline Runtime") {
				t.Fatalf("invalid exported Runtime failed at unexpected boundary: %v", err)
			}
			f.assertClean()
		})
	}
}

func TestVLLMValuesFilePinnedReleaseLayouts(t *testing.T) {
	for _, tc := range []struct {
		name, version, fallback, want string
	}{
		{"legacy", "3.1.9", "", "values-k3s.A6000x1.yaml"},
		{"older major", "2.9.0", "", "values-k3s.A6000x1.yaml"},
		{"split", "3.2.0", "", "values-k3s.A6000x1.docker.yaml"},
		{"v prefix", "v3.2.0", "", "values-k3s.A6000x1.docker.yaml"},
		{"later minor", "3.10.0", "", "values-k3s.A6000x1.docker.yaml"},
		{"later major", "4.0.0", "", "values-k3s.A6000x1.docker.yaml"},
		{"legacy catalog key", "", "3.1.0", "values-k3s.A6000x1.yaml"},
		{"split fallback", "", "3.2.0", "values-k3s.A6000x1.docker.yaml"},
		{"release key wins", "3.1.0", "4.0.0", "values-k3s.A6000x1.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := filepath.Join(t.TempDir(), "version.yaml")
			archiveTestWriteFile(t, catalog, serviceTestYAML(t, map[string]string{"eva_agent_release": tc.version, "eva_agent_deploy_version": tc.fallback}))
			got, err := vllmValuesFile(catalog, "A6000x1")
			if err != nil || got != tc.want {
				t.Fatalf("vllmValuesFile = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, contents := range []string{"{}\n", "eva_agent_release: 3.2\n", "eva_agent_release: invalid.2.0\n", "eva_agent_release: 3.invalid.0\n", "[invalid yaml"} {
		t.Run("invalid/"+contents, func(t *testing.T) {
			catalog := filepath.Join(t.TempDir(), "version.yaml")
			archiveTestWriteFile(t, catalog, []byte(contents))
			if got, err := vllmValuesFile(catalog, "A6000x1"); err == nil || got != "" {
				t.Fatalf("invalid catalog accepted: %q, %v", got, err)
			}
		})
	}
	if got, err := vllmValuesFile(filepath.Join(t.TempDir(), "missing.yaml"), "L40sx1"); err == nil || got != "" {
		t.Fatalf("missing catalog accepted: %q, %v", got, err)
	}
}

func TestTransportImageListUnionAndValidation(t *testing.T) {
	root := t.TempDir()
	for name, contents := range map[string]string{
		"images-pulled.txt":       " example.test/eva:latest \nexample.test/eva:1\n\n",
		"infra-images-pulled.txt": "example.test/eva:1\n", "n8n-images.txt": "example.test/eva:latest\n",
	} {
		archiveTestWriteFile(t, filepath.Join(root, "images", name), []byte(contents))
	}
	got, err := transportImageList(root)
	if err != nil || !reflect.DeepEqual(got, []string{"example.test/eva:1", "example.test/eva:latest"}) {
		t.Fatalf("transport union = %q, %v", got, err)
	}
	for _, contents := range []string{"", "example.test/eva:1 --flag\n", "$(touch /tmp/not-allowed)\n"} {
		archiveTestWriteFile(t, filepath.Join(root, "images/n8n-images.txt"), []byte(contents))
		if _, err := transportImageList(root); err == nil {
			t.Errorf("empty/unsafe transport list accepted: %q", contents)
		}
	}
	if err := os.Remove(filepath.Join(root, "images/n8n-images.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := transportImageList(root); err == nil {
		t.Fatal("missing n8n transport list accepted")
	}
}

func TestRunProcessUsesOnlyExplicitEnvironment(t *testing.T) {
	t.Setenv("AIRGAP_AMBIENT_SENTINEL", "must-not-be-inherited")
	t.Setenv("AWS_PROFILE", "must-not-be-inherited")
	t.Setenv("PYTHONPATH", "must-not-be-inherited")
	t.Setenv("BASH_ENV", "/must-not-be-sourced")
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	err := runProcess(context.Background(), remote.ProcessOptions{
		Path: "/bin/sh", Args: []string{"-c", `read -r input; printf '%s|%s|%s|%s|%s|%s|%s' "$EXPLICIT" "${AIRGAP_AMBIENT_SENTINEL-unset}" "${AWS_PROFILE-unset}" "${PYTHONPATH-unset}" "${BASH_ENV-unset}" "$PWD" "$input"; printf 'private stderr' >&2`},
		Dir: root, Env: map[string]string{"EXPLICIT": "only-provided-value"},
		Streams: remote.Streams{Stdin: strings.NewReader("stdin value\n"), Stdout: &stdout, Stderr: &stderr},
	})
	want := "only-provided-value|unset|unset|unset|unset|" + root + "|stdin value"
	if err != nil || stdout.String() != want || stderr.String() != "private stderr" {
		t.Fatalf("explicit process environment/streams: stdout=%q stderr=%q err=%v", &stdout, &stderr, err)
	}
	stdout.Reset()
	err = runProcess(context.Background(), remote.ProcessOptions{
		Path: "/bin/sh", Args: []string{"-c", `printf '%s' "${AIRGAP_AMBIENT_SENTINEL-unset}"`},
		Streams: remote.Streams{Stdout: &stdout, Stderr: io.Discard},
	})
	if err != nil || stdout.String() != "unset" {
		t.Fatalf("nil environment inherited host state: %q, %v", &stdout, err)
	}
	var exitError *exec.ExitError
	err = runProcess(context.Background(), remote.ProcessOptions{Path: "/bin/sh", Args: []string{"-c", "exit 7"}})
	if !errors.As(err, &exitError) || exitError.ExitCode() != 7 {
		t.Fatalf("process exit status not propagated: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runProcess(ctx, remote.ProcessOptions{Path: "/bin/sh", Args: []string{"-c", "exit 0"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled process ran or lost cancellation: %v", err)
	}
}

func TestEnsureRuntimeReusesValidAndRefusesInvalidExistingWithoutBootstrap(t *testing.T) {
	root := serviceTestRuntime(t)
	resolved, err := ensureRuntime(root)
	if err != nil || resolved.Root != root || resolved.Descriptor.SchemaVersion != "v1" {
		t.Fatalf("valid Runtime not reused: %#v, %v", resolved, err)
	}
	// Every negative path deliberately exists (including dangling links). Never
	// call ensureRuntime with an absent root: that would invoke host bootstrap.
	for _, kind := range []string{"empty directory", "bad descriptor", "regular file", "dangling symlink"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			path := filepath.Join(parent, "runtime")
			switch kind {
			case "empty directory", "bad descriptor":
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
				if kind == "bad descriptor" {
					archiveTestWriteFile(t, filepath.Join(path, "runtime.yaml"), []byte("schema_version: invalid\n"))
				}
			case "regular file":
				archiveTestWriteFile(t, path, []byte("preserve invalid Runtime\n"))
			case "dangling symlink":
				if err := os.Symlink("absent-target", path); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ensureRuntime(path); err == nil || !strings.Contains(err.Error(), "existing managed Runtime is invalid") {
				t.Fatalf("damaged existing Runtime was not refused: %v", err)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("damaged existing Runtime was replaced: %v", err)
			}
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != 1 || entries[0].Name() != "runtime" {
				t.Fatalf("bootstrap/staging files appeared: %v, %v", entries, err)
			}
		})
	}
}
