package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	gort "runtime"
	"strings"
	"testing"
	"time"

	"eva-deployer/tools/eva/internal/apt"
	"eva-deployer/tools/eva/internal/health"
	"eva-deployer/tools/eva/internal/operation"
	"eva-deployer/tools/eva/internal/plan"
	"eva-deployer/tools/eva/internal/release"
	remotecommand "eva-deployer/tools/eva/internal/remote"
	"eva-deployer/tools/eva/internal/runtime"
	"eva-deployer/tools/eva/internal/workspace"
)

func TestRunRemoteHelpAndUnknownCommand(t *testing.T) {
	for _, arguments := range [][]string{{"remote"}, {"remote", "help"}, {"remote", "--help"}, {"remote", "bootstrap", "--help"}, {"remote", "publish", "--help"}, {"remote", "prepare", "--help"}, {"remote", "verify", "--help"}} {
		if err := run(arguments); err != nil {
			t.Fatalf("run(%q) error = %v", arguments, err)
		}
	}
	if err := run([]string{"remote", "unknown"}); err == nil || !strings.Contains(err.Error(), "unknown remote command") {
		t.Fatalf("run(remote unknown) error = %v", err)
	}
}

func TestRunRemoteBootstrapRequiresRegistryAndConfirmation(t *testing.T) {
	previous := newRemoteBootstrapService
	previousReceiptPath := defaultRemoteBootstrapReceiptPath
	defer func() { newRemoteBootstrapService = previous; defaultRemoteBootstrapReceiptPath = previousReceiptPath }()
	defaultRemoteBootstrapReceiptPath = filepath.Join(t.TempDir(), "harbor.yaml")
	called := false
	newRemoteBootstrapService = func() remotecommand.BootstrapService {
		return remotecommand.BootstrapService{EnsureRuntime: func(context.Context) error { return nil }, EnsureDocker: func(context.Context) error { return nil }, EnsureHarbor: func(context.Context, string, string, bool) (remotecommand.HarborReceipt, error) {
			called = true
			return remotecommand.HarborReceipt{SchemaVersion: "v1", ManagedBy: "eva", Registry: "harbor.example.internal:32080", Project: "eva", HarborVersion: "2.15.2", InstallRoot: "/opt/eva/harbor", DataRoot: "/var/lib/eva/harbor", Protocol: "http"}, nil
		}, CheckHarbor: func(context.Context, remotecommand.HarborReceipt) error { return nil }, Login: func(context.Context, string, string, string) error { return nil }, Credential: func(string) bool { return true }, Password: func(remotecommand.HarborReceipt) (string, error) { return "test-password", nil }}
	}
	if err := run([]string{"remote", "bootstrap", "--registry", "harbor.example.internal:32080", "--yes"}); err != nil {
		t.Fatalf("bootstrap error = %v", err)
	}
	if !called {
		t.Fatal("bootstrap Harbor service was not called")
	}
	if err := run([]string{"remote", "bootstrap", "--yes"}); err != nil {
		t.Fatalf("bootstrap did not revalidate configured receipt: %v", err)
	}
	if err := run([]string{"remote", "bootstrap", "--registry", "harbor.example.internal:32080"}); err == nil {
		t.Fatal("bootstrap accepted missing confirmation")
	}
}

func TestRunRemotePublishForwardsPositionalReleasePath(t *testing.T) {
	releaseRoot := writeRemotePublishRelease(t)
	var gotArguments []string
	restore := replaceRemoteService(t, func() remotecommand.Service {
		return remotecommand.Service{
			ResolveBackend: func() (string, error) { return "/fixture/publish", nil },
			ResolvePayload: func(release.Resolved, string, string) (remotecommand.PayloadSource, error) {
				return remotecommand.PayloadSource{Directory: "/fixture/payload"}, nil
			},
			ResolveRuntime: func(release.Resolved, string, string) (remotecommand.RuntimeArtifactSource, error) {
				return remotecommand.RuntimeArtifactSource{Directory: "/fixture/runtime"}, nil
			},
			Run: func(_ string, arguments []string, _ remotecommand.Streams) error {
				gotArguments = append([]string(nil), arguments...)
				return nil
			},
		}
	})
	defer restore()

	if err := run([]string{"remote", "publish", releaseRoot, "--registry", "harbor.example.internal:32080", "--target", "eva@target.example.internal"}); err != nil {
		t.Fatalf("run(remote publish) error = %v", err)
	}
	want := []string{"--release-dir", releaseRoot, "--runtime-dir", "/fixture/runtime", "--payload-dir", "/fixture/payload", "--target", "eva@target.example.internal"}
	if !reflect.DeepEqual(gotArguments, want) {
		t.Fatalf("forwarded arguments = %#v, want %#v", gotArguments, want)
	}
}

func TestRunRemotePublishAcceptsPathAfterTargetAndDefaultsToCurrentDirectory(t *testing.T) {
	releaseRoot := writeRemotePublishRelease(t)
	restore := replaceRemoteService(t, func() remotecommand.Service {
		return remotecommand.Service{
			ResolveBackend: func() (string, error) { return "/fixture/publish", nil },
			ResolvePayload: func(release.Resolved, string, string) (remotecommand.PayloadSource, error) {
				return remotecommand.PayloadSource{Directory: "/fixture/payload"}, nil
			},
			ResolveRuntime: func(release.Resolved, string, string) (remotecommand.RuntimeArtifactSource, error) {
				return remotecommand.RuntimeArtifactSource{Directory: "/fixture/runtime"}, nil
			},
			Run: func(string, []string, remotecommand.Streams) error { return nil },
		}
	})
	defer restore()

	if err := run([]string{"remote", "publish", "--registry", "harbor.example.internal:32080", "--target", "eva@10.159.56.196", releaseRoot}); err != nil {
		t.Fatalf("run(remote publish path after target) error = %v", err)
	}
	previousDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(releaseRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previousDirectory) })
	if err := run([]string{"remote", "publish", "--registry", "harbor.example.internal:32080", "--target", "eva@target.example.internal"}); err != nil {
		t.Fatalf("run(remote publish default path) error = %v", err)
	}
}

func TestRunRemotePublishAcceptsRepeatedTargets(t *testing.T) {
	releaseRoot := writeRemotePublishRelease(t)
	var targets []string
	restore := replaceRemoteService(t, func() remotecommand.Service {
		return remotecommand.Service{
			ResolveBackend: func() (string, error) { return "/fixture/publish", nil },
			ResolvePayload: func(release.Resolved, string, string) (remotecommand.PayloadSource, error) {
				return remotecommand.PayloadSource{Directory: "/fixture/payload"}, nil
			},
			ResolveRuntime: func(release.Resolved, string, string) (remotecommand.RuntimeArtifactSource, error) {
				return remotecommand.RuntimeArtifactSource{Directory: "/fixture/runtime"}, nil
			},
			Run: func(_ string, arguments []string, _ remotecommand.Streams) error {
				targets = append(targets, arguments[len(arguments)-1])
				return nil
			},
		}
	})
	defer restore()
	if err := run([]string{"remote", "publish", releaseRoot, "--registry", "harbor.example.internal:32080", "--target", "eva@first.example.internal", "--target", "eva@second.example.internal"}); err != nil {
		t.Fatalf("run(remote publish repeated targets) error = %v", err)
	}
	if got, want := strings.Join(targets, ","), "eva@first.example.internal,eva@second.example.internal"; got != want {
		t.Fatalf("targets=%s, want %s", got, want)
	}
}

func TestRunRemotePublishRejectsInvalidArguments(t *testing.T) {
	for _, arguments := range [][]string{
		{"remote", "publish"},
		{"remote", "publish", "--target"},
		{"remote", "publish", "--unknown", "value"},
		{"remote", "publish", "one", "two", "--target", "target"},
	} {
		if err := run(arguments); err == nil {
			t.Fatalf("run(%q) succeeded", arguments)
		}
	}
}

func TestNormalizeRemotePrepareArgsAcceptsBothPositionalPlacements(t *testing.T) {
	for _, input := range [][]string{
		{"/release", "--registry", "harbor.example.internal:32080"},
		{"--registry", "harbor.example.internal:32080", "/release"},
	} {
		got, err := normalizeRemotePrepareArgs(input)
		if err != nil {
			t.Fatalf("normalizeRemotePrepareArgs(%q) error = %v", input, err)
		}
		want := []string{"--registry", "harbor.example.internal:32080", "/release"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("normalizeRemotePrepareArgs(%q) = %q, want %q", input, got, want)
		}
	}
	for _, input := range [][]string{{"--registry"}, {"--unknown", "x"}, {"one", "two", "--registry", "registry"}} {
		if _, err := normalizeRemotePrepareArgs(input); err == nil {
			t.Fatalf("normalizeRemotePrepareArgs(%q) succeeded", input)
		}
	}
}

func TestNormalizeRemoteVerifyArgsMatchesPrepareContract(t *testing.T) {
	for _, input := range [][]string{{"/release", "--registry", "harbor.example.internal:32080"}, {"--registry", "harbor.example.internal:32080", "/release"}} {
		prepare, prepareErr := normalizeRemotePrepareArgs(input)
		verify, verifyErr := normalizeRemoteRepositoryArgs(input, "verify")
		if prepareErr != nil || verifyErr != nil || !reflect.DeepEqual(prepare, verify) {
			t.Fatalf("normalization mismatch for %q: prepare=%q/%v verify=%q/%v", input, prepare, prepareErr, verify, verifyErr)
		}
	}
	for _, input := range [][]string{{"--registry"}, {"--unknown", "x"}, {"one", "two", "--registry", "registry"}} {
		if _, err := normalizeRemoteRepositoryArgs(input, "verify"); err == nil {
			t.Fatalf("verify normalization accepted %q", input)
		}
	}
}

func TestRunRemoteVerifyForwardsResolvedReleaseWithoutBackend(t *testing.T) {
	releaseRoot := writeRemotePublishRelease(t)
	previous := newRemoteVerifyService
	defer func() { newRemoteVerifyService = previous }()
	var received remotecommand.VerifyOptions
	newRemoteVerifyService = func() remotecommand.VerifyService {
		return remotecommand.VerifyService{VerifyFunc: func(options remotecommand.VerifyOptions) (remotecommand.VerifyResult, error) {
			received = options
			return remotecommand.VerifyResult{ReleaseVersion: options.Release.Metadata.Version, Registry: options.Registry, Project: "eva", ManifestPath: "/fixture/manifest.yaml"}, nil
		}}
	}
	if err := run([]string{"remote", "verify", "--registry", "harbor.example.internal:32080", releaseRoot}); err != nil {
		t.Fatalf("run(remote verify) error = %v", err)
	}
	if received.Release.Root != releaseRoot || received.Registry != "harbor.example.internal:32080" {
		t.Fatalf("verify options = %#v", received)
	}
}

func TestRunRemoteVerifyUsesCurrentReleaseWithoutPath(t *testing.T) {
	releaseRoot := writeRemotePublishRelease(t)
	directory := t.TempDir()
	previousBootstrapReceipt, previousCurrentReceipt := defaultRemoteBootstrapReceiptPath, defaultCurrentReleaseReceiptPath
	previousVerify := newRemoteVerifyService
	defer func() {
		defaultRemoteBootstrapReceiptPath, defaultCurrentReleaseReceiptPath = previousBootstrapReceipt, previousCurrentReceipt
		newRemoteVerifyService = previousVerify
	}()
	defaultRemoteBootstrapReceiptPath = filepath.Join(directory, "harbor.yaml")
	defaultCurrentReleaseReceiptPath = filepath.Join(directory, "releases", "current.yaml")
	if err := remotecommand.WriteHarborReceipt(defaultRemoteBootstrapReceiptPath, remotecommand.HarborReceipt{SchemaVersion: "v1", ManagedBy: "eva", Registry: "harbor.example.internal:32080", Project: "eva", HarborVersion: "2.15.2", InstallRoot: "/opt/eva/harbor", DataRoot: "/var/lib/eva/harbor", Protocol: "http"}); err != nil {
		t.Fatal(err)
	}
	resolved, err := release.Resolve(releaseRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := release.WriteCurrentReceipt(defaultCurrentReleaseReceiptPath, resolved, "eva-tool-installer", time.Now()); err != nil {
		t.Fatal(err)
	}
	var received remotecommand.VerifyOptions
	newRemoteVerifyService = func() remotecommand.VerifyService {
		return remotecommand.VerifyService{VerifyFunc: func(options remotecommand.VerifyOptions) (remotecommand.VerifyResult, error) {
			received = options
			return remotecommand.VerifyResult{ReleaseVersion: options.Release.Metadata.Version, Registry: options.Registry, Project: options.Project, ManifestPath: "/fixture/manifest.yaml"}, nil
		}}
	}
	if err := run([]string{"remote", "verify"}); err != nil {
		t.Fatalf("remote verify with Current Release: %v", err)
	}
	if received.Release.Root != releaseRoot {
		t.Fatalf("remote verify resolved %q, want Current Release %q", received.Release.Root, releaseRoot)
	}
}

func TestRunVerifyUsesCurrentReleaseReceiptAndExplicitOverride(t *testing.T) {
	currentRoot := writeRemotePublishRelease(t)
	overrideRoot := writeRemotePublishRelease(t)
	previousReceiptPath := defaultCurrentReleaseReceiptPath
	defer func() { defaultCurrentReleaseReceiptPath = previousReceiptPath }()
	defaultCurrentReleaseReceiptPath = filepath.Join(t.TempDir(), "releases", "current.yaml")
	current, err := release.Resolve(currentRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := release.WriteCurrentReceipt(defaultCurrentReleaseReceiptPath, current, "eva-tool-installer", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify"}); err != nil {
		t.Fatalf("verify with Current Release: %v", err)
	}
	if err := run([]string{"verify", "--release", overrideRoot}); err != nil {
		t.Fatalf("verify with explicit Release: %v", err)
	}
	receipt, err := release.LoadCurrentReceipt(defaultCurrentReleaseReceiptPath)
	if err != nil || receipt.ReleaseRoot != currentRoot {
		t.Fatalf("explicit verify altered Current Release: %#v, %v", receipt, err)
	}
}

func TestInternalRegisterCurrentReleaseUsesManagedReceipt(t *testing.T) {
	releaseRoot := writeRemotePublishRelease(t)
	previousReceiptPath, previousNow := defaultCurrentReleaseReceiptPath, currentReleaseNow
	defer func() {
		defaultCurrentReleaseReceiptPath, currentReleaseNow = previousReceiptPath, previousNow
	}()
	defaultCurrentReleaseReceiptPath = filepath.Join(t.TempDir(), "releases", "current.yaml")
	currentReleaseNow = func() time.Time { return time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC) }
	if err := run([]string{"internal", "register-current-release", "--release", releaseRoot, "--selected-by", "eva-tool-installer"}); err != nil {
		t.Fatalf("register Current Release: %v", err)
	}
	receipt, resolved, err := release.LoadCurrentRelease(defaultCurrentReleaseReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.SelectedBy != "eva-tool-installer" || resolved.Root != releaseRoot {
		t.Fatalf("registered receipt=%#v resolved=%#v", receipt, resolved)
	}
	if err := run([]string{"internal", "validate-current-release", "--release", releaseRoot}); err != nil {
		t.Fatalf("validate Current Release: %v", err)
	}
}

func TestRemoteCommandsResolveRegistryFromBootstrapReceipt(t *testing.T) {
	releaseRoot := writeRemotePublishRelease(t)
	previousReceiptPath := defaultRemoteBootstrapReceiptPath
	previousVerify := newRemoteVerifyService
	defer func() {
		defaultRemoteBootstrapReceiptPath = previousReceiptPath
		newRemoteVerifyService = previousVerify
	}()
	defaultRemoteBootstrapReceiptPath = filepath.Join(t.TempDir(), "harbor.yaml")
	receipt := remotecommand.HarborReceipt{SchemaVersion: "v1", ManagedBy: "eva", Registry: "harbor.example.internal:32080", Project: "eva", HarborVersion: "2.15.2", InstallRoot: "/opt/eva/harbor", DataRoot: "/var/lib/eva/harbor", Protocol: "http"}
	if err := remotecommand.WriteHarborReceipt(defaultRemoteBootstrapReceiptPath, receipt); err != nil {
		t.Fatal(err)
	}
	var received remotecommand.VerifyOptions
	newRemoteVerifyService = func() remotecommand.VerifyService {
		return remotecommand.VerifyService{VerifyFunc: func(options remotecommand.VerifyOptions) (remotecommand.VerifyResult, error) {
			received = options
			return remotecommand.VerifyResult{ReleaseVersion: options.Release.Metadata.Version, Registry: options.Registry, Project: options.Project}, nil
		}}
	}
	if err := run([]string{"remote", "verify", releaseRoot}); err != nil {
		t.Fatalf("verify using receipt: %v", err)
	}
	if received.Registry != receipt.Registry || received.Project != receipt.Project {
		t.Fatalf("verify context = %#v", received)
	}
	if err := run([]string{"remote", "verify", releaseRoot, "--registry", "other.example.internal:32080"}); err == nil {
		t.Fatal("verify accepted conflicting registry")
	}
}

func TestRemotePrepareFailsBeforeServiceWithoutManagedAWSCredentialInNonInteractiveMode(t *testing.T) {
	releaseRoot := writeRemotePublishRelease(t)
	previousReceiptPath, previousCredentialPath := defaultRemoteBootstrapReceiptPath, defaultRemoteAWSCredentialPath
	previousPrepare, previousStat := newRemotePrepareService, stdinStat
	defer func() {
		defaultRemoteBootstrapReceiptPath, defaultRemoteAWSCredentialPath = previousReceiptPath, previousCredentialPath
		newRemotePrepareService, stdinStat = previousPrepare, previousStat
	}()
	directory := t.TempDir()
	defaultRemoteBootstrapReceiptPath = filepath.Join(directory, "harbor.yaml")
	defaultRemoteAWSCredentialPath = filepath.Join(directory, "credentials", "aws_key.ini")
	receipt := remotecommand.HarborReceipt{SchemaVersion: "v1", ManagedBy: "eva", Registry: "harbor.example.internal:32080", Project: "eva", HarborVersion: "2.15.2", InstallRoot: "/opt/eva/harbor", DataRoot: "/var/lib/eva/harbor", Protocol: "http"}
	if err := remotecommand.WriteHarborReceipt(defaultRemoteBootstrapReceiptPath, receipt); err != nil {
		t.Fatal(err)
	}
	called := false
	newRemotePrepareService = func() remotecommand.PrepareService {
		return remotecommand.PrepareService{PrepareFunc: func(context.Context, remotecommand.PrepareOptions) (string, error) { called = true; return "", nil }}
	}
	stdinStat = func() (os.FileInfo, error) { return os.Stat(filepath.Join(directory, "harbor.yaml")) }
	err := run([]string{"remote", "prepare", releaseRoot})
	if err == nil || !strings.Contains(err.Error(), defaultRemoteAWSCredentialPath) || called {
		t.Fatalf("prepare error=%v called=%v", err, called)
	}
}

func replaceRemoteService(t *testing.T, factory func() remotecommand.Service) func() {
	t.Helper()
	previous := newRemoteService
	newRemoteService = factory
	return func() { newRemoteService = previous }
}

func writeRemotePublishRelease(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"eva-tool.tar.gz":       "tool",
		"eva-tool-installer.sh": "installer",
		"eva-infra.tar.gz":      "infra",
		"eva-solution.tar.gz":   "solution",
		"eva-offline.tar.gz":    "offline",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	metadata := fmt.Sprintf(`version: 3.2.0
platform:
  os: %s
  arch: %s
artifacts:
  - name: eva-tool
    file: eva-tool.tar.gz
    sha256: %x
  - name: eva-tool-installer
    file: eva-tool-installer.sh
    sha256: %x
  - name: eva-infra
    file: eva-infra.tar.gz
    sha256: %x
  - name: eva-solution
    file: eva-solution.tar.gz
    sha256: %x
  - name: eva-offline
    file: eva-offline.tar.gz
    sha256: %x
`, gort.GOOS, gort.GOARCH,
		sha256.Sum256([]byte(files["eva-tool.tar.gz"])),
		sha256.Sum256([]byte(files["eva-tool-installer.sh"])),
		sha256.Sum256([]byte(files["eva-infra.tar.gz"])),
		sha256.Sum256([]byte(files["eva-solution.tar.gz"])),
		sha256.Sum256([]byte(files["eva-offline.tar.gz"])))
	if err := os.WriteFile(filepath.Join(root, "release.yaml"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "checksums.sha256"), []byte("release checksums\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestReleaseEnvironmentUsesExpectedPreparedRoot(t *testing.T) {
	resolved := release.Resolved{Root: "/tmp/eva-base-release", Metadata: release.Metadata{Version: "v3.2.0"}}
	got := releaseEnvironment(resolved)
	want := map[string]string{
		"RELEASE_DIR":     "/tmp/eva-base-release",
		"RELEASE_VERSION": "v3.2.0",
		"RELEASE_ROOT":    "/opt/eva/releases/v3.2.0",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("releaseEnvironment() = %#v, want %#v", got, want)
	}

	resolved.Root = "/opt/eva/releases/v3.2.0"
	resolved.Prepared = true
	if got := releaseEnvironment(resolved)["RELEASE_ROOT"]; got != resolved.Root {
		t.Fatalf("prepared RELEASE_ROOT = %q, want %q", got, resolved.Root)
	}
}

func TestNormalizeInstallArgsKeepsRepeatableComponentFlags(t *testing.T) {
	got, err := normalizeInstallArgs([]string{
		"/releases/3.2.0", "--component", "app", "--component", "iam", "--yes",
	})
	if err != nil {
		t.Fatalf("normalizeInstallArgs() error = %v", err)
	}
	want := []string{"--component", "app", "--component", "iam", "--yes", "/releases/3.2.0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeInstallArgs() = %v, want %v", got, want)
	}
}

func TestNormalizePlanArgsKeepsComponentFlagAfterReleasePath(t *testing.T) {
	got, err := normalizePlanArgs([]string{"/releases/3.2.0", "--component", "agent", "--save"})
	if err != nil {
		t.Fatalf("normalizePlanArgs() error = %v", err)
	}
	want := []string{"--component", "agent", "--save", "/releases/3.2.0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizePlanArgs() = %v, want %v", got, want)
	}
}

func TestCheckComponentsUsesOnlySelectedProductSteps(t *testing.T) {
	components := checkComponents(plan.Document{Steps: []plan.Step{
		{Component: "precondition"}, {Component: "infra"}, {Component: "config"},
		{Component: "iam"}, {Component: "app"},
	}})
	want := []health.Component{
		{Name: "iam", Namespace: "eva-iam"},
		{Name: "app", Namespace: "eva-app"},
	}
	if !reflect.DeepEqual(components, want) {
		t.Fatalf("checkComponents() = %#v, want %#v", components, want)
	}
}

func TestInspectGPUPreflightReportsGPUsAndMIGInstances(t *testing.T) {
	result, err := inspectGPUPreflight(func(args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "--query-gpu=name,driver_version --format=csv,noheader":
			return "NVIDIA RTX PRO 6000 Blackwell Server Edition, 570.42.01\n", nil
		case "-L":
			return "GPU 0: NVIDIA RTX PRO 6000 Blackwell Server Edition\n  MIG 1g.24gb Device 0\n  MIG 1g.24gb Device 1\n", nil
		default:
			return "", errors.New("unexpected nvidia-smi arguments")
		}
	})
	if err != nil {
		t.Fatalf("inspectGPUPreflight() error = %v", err)
	}
	if got, want := result.GPUs, []string{"NVIDIA RTX PRO 6000 Blackwell Server Edition, 570.42.01"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("GPUs = %v, want %v", got, want)
	}
	if result.MIGInstances != 2 {
		t.Fatalf("MIGInstances = %d, want 2", result.MIGInstances)
	}
}

func TestInspectGPUPreflightFailsWithoutGPU(t *testing.T) {
	_, err := inspectGPUPreflight(func(args ...string) (string, error) {
		return "", nil
	})
	if err == nil {
		t.Fatal("inspectGPUPreflight() succeeded without GPUs")
	}
}

func TestShellEnvironmentPrependsRuntimeAndWorkspace(t *testing.T) {
	runtimeRoot := writeShellRuntime(t)
	resolvedRuntime, err := runtime.Resolve(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspaceRoot, "site-values"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "site-values", "site.yaml"), []byte("site:\n  id: customer-a\nrepository:\n  mode: cloud\ncomponents:\n  app: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	environment, err := shellEnvironment([]string{"PATH=/usr/bin", "KEEP=value", "ANSIBLE_COLLECTIONS_PATH=/operator/collections"}, resolvedRuntime, "", workspaceRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	values := environmentMap(environment)
	if values["EVA_RUNTIME_ROOT"] != resolvedRuntime.Root || values["EVA_SITE_ID"] != "customer-a" || values["EVA_WORKSPACE_ROOT"] != workspaceRoot || values["KEEP"] != "value" {
		t.Fatalf("shell environment = %#v", values)
	}
	wantCollectionPath := resolvedRuntime.CollectionPath() + string(os.PathListSeparator) + "/operator/collections"
	if values["ANSIBLE_COLLECTIONS_PATH"] != wantCollectionPath {
		t.Fatalf("ANSIBLE_COLLECTIONS_PATH = %q, want %q", values["ANSIBLE_COLLECTIONS_PATH"], wantCollectionPath)
	}
	for _, directory := range resolvedRuntime.ToolDirectories() {
		if !strings.Contains(values["PATH"], directory) {
			t.Fatalf("PATH = %q, missing %q", values["PATH"], directory)
		}
	}
	if !strings.HasSuffix(values["PATH"], string(os.PathListSeparator)+"/usr/bin") {
		t.Fatalf("PATH = %q, want original PATH last", values["PATH"])
	}
}

func TestRunRetryClonesLatestFailedOperationAndAppliesIt(t *testing.T) {
	stateRoot := t.TempDir()
	workspaceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspaceRoot, "inventory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "inventory", "inventory.ini"), []byte("[local]\nlocalhost ansible_connection=local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	releaseRoot := t.TempDir()
	playbook := filepath.Join(releaseRoot, "src", "infra", "playbooks", "site_infra.yaml")
	if err := os.MkdirAll(filepath.Dir(playbook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(playbook, []byte("---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(releaseRoot, "ansible.cfg"), []byte("[defaults]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runtimeRoot := writeRetryRuntime(t)
	source, err := operation.Create(stateRoot, plan.Document{
		SchemaVersion: plan.SchemaVersion, SiteID: "customer-a", Workspace: workspaceRoot,
		ReleaseVersion: "3.2.0", ReleaseRoot: releaseRoot, RepositoryMode: "local_repository",
		Steps: []plan.Step{{Component: "infra", Playbook: "src/infra/playbooks/site_infra.yaml"}},
	}, time.Date(2026, 9, 12, 1, 2, 3, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	source.Status = operation.Failed
	if err := operation.Update(stateRoot, source); err != nil {
		t.Fatal(err)
	}
	if err := runRetry([]string{
		"--yes", "--state-root", stateRoot, "--log-root", t.TempDir(), "--runtime-root", runtimeRoot,
	}); err != nil {
		t.Fatalf("runRetry() error = %v", err)
	}
	loadedSource, err := operation.Load(stateRoot, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loadedSource.Status != operation.Failed || loadedSource.RetryOperationID == "" {
		t.Fatalf("source after retry = %#v", loadedSource)
	}
	retry, err := operation.Load(stateRoot, loadedSource.RetryOperationID)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Status != operation.Succeeded || retry.SourceOperationID != source.ID {
		t.Fatalf("retry operation = %#v", retry)
	}
}

func TestAPTPriorToInfraRequiresSeparateInteractiveApproval(t *testing.T) {
	previousService := newAPTService
	t.Cleanup(func() { newAPTService = previousService })
	nonTerminalInput, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer nonTerminalInput.Close()
	nonTerminalInfo, err := nonTerminalInput.Stat()
	if err != nil {
		t.Fatal(err)
	}
	previousStdinStat := stdinStat
	t.Cleanup(func() { stdinStat = previousStdinStat })
	stdinStat = func() (os.FileInfo, error) { return nonTerminalInfo, nil }
	newAPTService = func() apt.Service {
		return apt.Service{Run: func(command apt.Command) (apt.Result, error) {
			return apt.Result{
				ExitCode: 100,
				Output:   "Err: https://pkg.jenkins.io/debian-stable binary/ Release\nNO_PUBKEY 7198F4B714ABFC68\n",
			}, nil
		}}
	}
	err = aptPrerequisite(plan.Document{
		RepositoryMode: "cloud_repository",
		Steps:          []plan.Step{{Component: "infra"}},
	})
	var displayed *displayedError
	if !errors.As(err, &displayed) {
		t.Fatalf("aptPrerequisite() error = %v, want displayed approval error", err)
	}
	if !strings.Contains(displayed.message, "Interactive approval is required") || !strings.Contains(displayed.message, "troubleshoot apt --fix-known --yes") {
		t.Fatalf("approval error = %q", displayed.message)
	}
}

func TestAPTPriorToInfraSkipsOfflineRepositories(t *testing.T) {
	for _, repositoryMode := range []string{
		"remote_repository",
		"local_repository",
	} {
		t.Run(repositoryMode, func(t *testing.T) {
			called := false
			previousService := newAPTService
			t.Cleanup(func() { newAPTService = previousService })
			newAPTService = func() apt.Service {
				called = true
				return apt.Service{}
			}
			if err := aptPrerequisite(plan.Document{
				RepositoryMode: repositoryMode,
				Steps:          []plan.Step{{Component: "infra"}},
			}); err != nil {
				t.Fatalf("aptPrerequisite() error = %v", err)
			}
			if called {
				t.Fatalf("%s unexpectedly ran APT diagnostic", repositoryMode)
			}
		})
	}
}

func TestOfflineRepositoryModeAcceptsWorkspaceAndAnsibleModes(t *testing.T) {
	for _, repositoryMode := range []string{
		"remote",
		"local",
		"remote_repository",
		"local_repository",
	} {
		if !offlineRepositoryMode(repositoryMode) {
			t.Fatalf("offlineRepositoryMode(%q) = false", repositoryMode)
		}
	}
	if offlineRepositoryMode("cloud") ||
		offlineRepositoryMode("cloud_repository") {
		t.Fatal("cloud mode was classified as offline")
	}
}

func TestMaterializedRemoteCacheOnlyUsesRemoteRepository(t *testing.T) {
	previous := materializeRemotePayload
	defer func() { materializeRemotePayload = previous }()
	calls := 0
	materializeRemotePayload = func(resolved release.Resolved, registry, project, artifactRoot string) (string, error) {
		calls++
		if registry != "harbor.example.internal:32080" || project != "eva" || artifactRoot != "" {
			t.Fatalf("materialize arguments = %q, %q, %q", registry, project, artifactRoot)
		}
		return "/var/lib/eva/artifacts/remote/3.2.0/identity/cache", nil
	}
	resolved := release.Resolved{Metadata: release.Metadata{Version: "3.2.0"}}
	remoteWorkspace := workspace.Resolved{}
	remoteWorkspace.Config.Repository.Mode = "remote"
	remoteWorkspace.Config.Repository.Registry = "harbor.example.internal:32080"
	remoteWorkspace.Config.Repository.Project = "eva"
	cache, err := materializedRemoteCache(resolved, remoteWorkspace)
	if err != nil || cache == "" || calls != 1 {
		t.Fatalf("remote materialization = %q, %v, calls=%d", cache, err, calls)
	}
	for _, mode := range []string{"cloud", "local"} {
		workspaceResolved := remoteWorkspace
		workspaceResolved.Config.Repository.Mode = mode
		cache, err := materializedRemoteCache(resolved, workspaceResolved)
		if err != nil || cache != "" || calls != 1 {
			t.Fatalf("%s materialization = %q, %v, calls=%d", mode, cache, err, calls)
		}
	}
}

func TestEnsureInstallRuntimeFailsClosedForPreparedRemoteRelease(t *testing.T) {
	releaseResolved := release.Resolved{
		Prepared: true,
		Metadata: release.Metadata{
			Version: "3.2.0",
		},
	}
	err := ensureInstallRuntime(
		filepath.Join(t.TempDir(), "runtime"),
		releaseResolved,
		"remote",
	)
	if err == nil ||
		!strings.Contains(err.Error(), "original Release") {
		t.Fatalf("ensureInstallRuntime() error = %v", err)
	}
}

func writeShellRuntime(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	tools := map[string]string{
		"ansible-playbook": "venv/bin/ansible-playbook", "helm": "bin/helm", "kubectl": "bin/kubectl", "kustomize": "bin/kustomize", "oras": "bin/oras",
	}
	for _, path := range tools {
		fullPath := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	contents := "schema_version: v1\nversion: 3.2.0\ntools:\n"
	for name, path := range tools {
		contents += "  " + name + ": " + path + "\n"
	}
	if err := os.WriteFile(filepath.Join(root, "runtime.yaml"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeRetryRuntime(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	tools := map[string]string{
		"ansible-playbook": "venv/bin/ansible-playbook", "helm": "bin/helm", "kubectl": "bin/kubectl", "kustomize": "bin/kustomize", "oras": "bin/oras",
	}
	for _, path := range tools {
		fullPath := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	contents := "schema_version: v1\nversion: 3.2.0\ntools:\n"
	for name, path := range tools {
		contents += "  " + name + ": " + path + "\n"
	}
	if err := os.WriteFile(filepath.Join(root, "runtime.yaml"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func environmentMap(environment []string) map[string]string {
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, found := strings.Cut(entry, "=")
		if found {
			values[name] = value
		}
	}
	return values
}
