package remote

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"eva-deployer/tools/eva/internal/release"
)

func TestPublishRejectsInvalidTarget(t *testing.T) {
	resolved := writeOriginalRelease(t, true)
	service := NewService()
	for _, target := range []string{"", "eva target", "eva@target\nnext", "-oProxyCommand=bad"} {
		if err := service.Publish(PublishOptions{Release: resolved, Target: target}); err == nil {
			t.Fatalf("Publish() accepted invalid target %q", target)
		}
	}
}

func TestPublishRejectsPreparedAndAcceptsBaseReleaseContract(t *testing.T) {
	service := NewService()
	if err := service.Publish(PublishOptions{Release: release.Resolved{Prepared: true}, Target: "eva@target"}); err == nil || !strings.Contains(err.Error(), "prepared") {
		t.Fatalf("Publish(prepared) error = %v", err)
	}
	baseRelease := writeOriginalRelease(t, false)
	if err := release.ValidateRemotePreparationInput(baseRelease); err != nil {
		t.Fatalf("ValidateRemotePreparationInput(base release) error = %v", err)
	}
	airgapImported := writeOriginalRelease(t, true)
	if err := os.WriteFile(filepath.Join(airgapImported.Root, ".eva-airgap-bundle"), []byte("bundle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.Publish(PublishOptions{Release: airgapImported, Target: "eva@target"}); err == nil || !strings.Contains(err.Error(), "Airgap") {
		t.Fatalf("Publish(imported Airgap) error = %v", err)
	}
}

func TestPublishForwardsReleaseAndVerifiedPayload(t *testing.T) {
	resolved := writeOriginalRelease(t, true)
	var gotPath string
	var gotArguments []string
	service := Service{
		ResolveBackend: func() (string, error) {
			return "/tool/libexec/remote-root/scripts/remote/publish_release_to_target.sh", nil
		},
		ResolvePayload: func(release.Resolved, string, string) (PayloadSource, error) {
			return PayloadSource{Directory: "/preparation/target-payload"}, nil
		},
		ResolveRuntime: func(release.Resolved, string, string) (RuntimeArtifactSource, error) {
			return RuntimeArtifactSource{Directory: "/preparation/runtime"}, nil
		},
		Run: func(path string, arguments []string, _ Streams) error {
			gotPath = path
			gotArguments = append([]string(nil), arguments...)
			return nil
		},
	}
	if err := service.Publish(PublishOptions{Release: resolved, Target: "eva@target.example.internal", Registry: "harbor.example.internal:32080"}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if gotPath == "" {
		t.Fatal("backend was not invoked")
	}
	want := []string{"--release-dir", resolved.Root, "--runtime-dir", "/preparation/runtime", "--payload-dir", "/preparation/target-payload", "--target", "eva@target.example.internal"}
	if strings.Join(gotArguments, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("arguments = %#v, want %#v", gotArguments, want)
	}
}

func TestPublishPreservesBackendFailure(t *testing.T) {
	backendFailure := errors.New("backend failed")
	service := Service{
		ResolveBackend: func() (string, error) { return "/backend", nil },
		ResolvePayload: func(release.Resolved, string, string) (PayloadSource, error) {
			return PayloadSource{Directory: "/payload"}, nil
		},
		ResolveRuntime: func(release.Resolved, string, string) (RuntimeArtifactSource, error) {
			return RuntimeArtifactSource{Directory: "/runtime"}, nil
		},
		Run: func(string, []string, Streams) error { return backendFailure },
	}
	err := service.Publish(PublishOptions{Release: writeOriginalRelease(t, true), Target: "eva@target.example.internal", Registry: "harbor.example.internal:32080"})
	if !errors.Is(err, backendFailure) {
		t.Fatalf("Publish() error = %v, want wrapped backend failure", err)
	}
}

func TestPublishMultipleTargetsContinuesAfterFailure(t *testing.T) {
	var calls []string
	backendCalls := 0
	service := Service{
		ResolveBackend: func() (string, error) { backendCalls++; return "/backend", nil },
		ResolvePayload: func(release.Resolved, string, string) (PayloadSource, error) {
			return PayloadSource{Directory: "/payload"}, nil
		},
		ResolveRuntime: func(release.Resolved, string, string) (RuntimeArtifactSource, error) {
			return RuntimeArtifactSource{Directory: "/runtime"}, nil
		},
		Run: func(_ string, arguments []string, _ Streams) error {
			target := arguments[len(arguments)-1]
			calls = append(calls, target)
			if target == "eva@second.example.internal" {
				return errors.New("ssh key=private-value failed")
			}
			return nil
		},
	}
	result, err := service.PublishWithResult(PublishOptions{Release: writeOriginalRelease(t, true), Targets: []string{"eva@first.example.internal", "eva@second.example.internal", "eva@third.example.internal"}, Registry: "harbor.example.internal:32080"})
	if err == nil || result.Succeeded != 2 || result.Failed != 1 || backendCalls != 1 {
		t.Fatalf("result=%#v err=%v backendCalls=%d", result, err, backendCalls)
	}
	if got, want := strings.Join(calls, ","), "eva@first.example.internal,eva@second.example.internal,eva@third.example.internal"; got != want {
		t.Fatalf("calls=%s, want %s", got, want)
	}
	if result.Targets[1].Err == nil || strings.Contains(result.Targets[1].Err.Error(), "private-value") {
		t.Fatalf("target error is not sanitized: %v", result.Targets[1].Err)
	}
}

func TestPublishValidatesAllTargetsBeforeBackend(t *testing.T) {
	calls := 0
	service := Service{ResolveBackend: func() (string, error) { calls++; return "/backend", nil }}
	for _, targets := range [][]string{{"eva@one.example.internal", "eva@ONE.example.internal"}, {"eva@one.example.internal", "not a target"}} {
		if _, err := service.PublishWithResult(PublishOptions{Release: writeOriginalRelease(t, true), Targets: targets, Registry: "harbor.example.internal:32080"}); err == nil {
			t.Fatalf("invalid targets accepted: %q", targets)
		}
	}
	if calls != 0 {
		t.Fatalf("backend resolved after invalid target: %d", calls)
	}
}

func TestManagedTargetPreflightPrecedesTransport(t *testing.T) {
	resolved := writeOriginalRelease(t, true)
	artifactRoot := t.TempDir()
	payload := filepath.Join(artifactRoot, "payload")
	runtimeArtifact := filepath.Join(artifactRoot, "runtime")
	if err := os.Mkdir(payload, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(runtimeArtifact, 0o700); err != nil {
		t.Fatal(err)
	}
	backendCalls := 0
	service := Service{
		ResolveBackend: func() (string, error) { return "/backend", nil },
		ResolvePayload: func(release.Resolved, string, string) (PayloadSource, error) {
			return PayloadSource{Directory: payload}, nil
		},
		ResolveRuntime: func(release.Resolved, string, string) (RuntimeArtifactSource, error) {
			return RuntimeArtifactSource{Directory: runtimeArtifact}, nil
		},
		ResolveManagedTarget: func(string, int64) (TargetConnection, error) {
			return TargetConnection{}, errors.New("Target sudo authentication failed")
		},
		Run: func(string, []string, Streams) error { backendCalls++; return nil },
	}
	err := service.Publish(PublishOptions{Release: resolved, Target: "site-dev-196", Registry: "harbor.example.internal:32080"})
	var preflight ManagedTargetPreflightError
	if !errors.As(err, &preflight) || backendCalls != 0 {
		t.Fatalf("err=%v backendCalls=%d; preflight must prevent transport", err, backendCalls)
	}
}

func TestManagedTargetPassesSudoPasswordByFileDescriptor(
	t *testing.T,
) {
	resolved := writeOriginalRelease(t, true)
	artifactRoot := t.TempDir()
	payload := filepath.Join(artifactRoot, "payload")
	runtimeArtifact := filepath.Join(artifactRoot, "runtime")

	if err := os.Mkdir(payload, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(runtimeArtifact, 0o700); err != nil {
		t.Fatal(err)
	}

	var arguments []string
	var sudoPassword string

	service := Service{
		ResolveBackend: func() (string, error) {
			return "/backend", nil
		},
		ResolvePayload: func(
			release.Resolved,
			string,
			string,
		) (PayloadSource, error) {
			return PayloadSource{Directory: payload}, nil
		},
		ResolveRuntime: func(
			release.Resolved,
			string,
			string,
		) (RuntimeArtifactSource, error) {
			return RuntimeArtifactSource{
				Directory: runtimeArtifact,
			}, nil
		},
		ResolveManagedTarget: func(
			string,
			int64,
		) (TargetConnection, error) {
			return TargetConnection{
				Target:       "eva@10.159.56.197",
				SSHOptions:   []string{"ControlPath=/run/eva/p/control"},
				sudoMethod:   "password",
				sudoPassword: "sudo-secret",
			}, nil
		},
		Run: func(
			_ string,
			got []string,
			streams Streams,
		) error {
			arguments = append([]string(nil), got...)

			if len(streams.ExtraFiles) != 1 {
				t.Fatalf(
					"ExtraFiles=%d, want 1",
					len(streams.ExtraFiles),
				)
			}

			contents, err := io.ReadAll(
				streams.ExtraFiles[0],
			)
			if err != nil {
				t.Fatal(err)
			}

			sudoPassword = strings.TrimSpace(
				string(contents),
			)
			return nil
		},
	}

	if err := service.Publish(PublishOptions{
		Release:  resolved,
		Target:   "site-dev-196",
		Registry: "harbor.example.internal:32080",
	}); err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(arguments, "\n")

	for _, expected := range []string{
		"--sudo-mode",
		"password",
		"--sudo-password-fd",
		"3",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf(
				"missing transport argument %q: %#v",
				expected,
				arguments,
			)
		}
	}

	if sudoPassword != "sudo-secret" {
		t.Fatal(
			"sudo password was not delivered through fd 3",
		)
	}

	if strings.Contains(joined, "sudo-secret") {
		t.Fatal("sudo password leaked into arguments")
	}
}

func TestManagedPasswordlessTargetDoesNotPassSecretFile(
	t *testing.T,
) {
	resolved := writeOriginalRelease(t, true)
	artifactRoot := t.TempDir()
	payload := filepath.Join(artifactRoot, "payload")
	runtimeArtifact := filepath.Join(artifactRoot, "runtime")

	if err := os.Mkdir(payload, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(runtimeArtifact, 0o700); err != nil {
		t.Fatal(err)
	}

	var arguments []string

	service := Service{
		ResolveBackend: func() (string, error) {
			return "/backend", nil
		},
		ResolvePayload: func(
			release.Resolved,
			string,
			string,
		) (PayloadSource, error) {
			return PayloadSource{Directory: payload}, nil
		},
		ResolveRuntime: func(
			release.Resolved,
			string,
			string,
		) (RuntimeArtifactSource, error) {
			return RuntimeArtifactSource{
				Directory: runtimeArtifact,
			}, nil
		},
		ResolveManagedTarget: func(
			string,
			int64,
		) (TargetConnection, error) {
			return TargetConnection{
				Target:     "eva@10.159.56.197",
				SSHOptions: []string{"ControlPath=/run/eva/p/control"},
				sudoMethod: "passwordless",
			}, nil
		},
		Run: func(
			_ string,
			got []string,
			streams Streams,
		) error {
			arguments = append([]string(nil), got...)

			if len(streams.ExtraFiles) != 0 {
				t.Fatal(
					"passwordless sudo received a secret fd",
				)
			}
			return nil
		},
	}

	if err := service.Publish(PublishOptions{
		Release:  resolved,
		Target:   "site-dev-196",
		Registry: "harbor.example.internal:32080",
	}); err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(arguments, "\n")

	if !strings.Contains(joined, "passwordless") {
		t.Fatalf(
			"passwordless sudo mode is missing: %#v",
			arguments,
		)
	}
	if strings.Contains(joined, "--sudo-password-fd") {
		t.Fatalf(
			"passwordless sudo received a password fd: %#v",
			arguments,
		)
	}
}

func TestManagedTargetPassesStrictConnectionOptionsToTransport(t *testing.T) {
	resolved := writeOriginalRelease(t, true)
	artifactRoot := t.TempDir()
	payload := filepath.Join(artifactRoot, "payload")
	runtimeArtifact := filepath.Join(artifactRoot, "runtime")
	if err := os.Mkdir(payload, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(runtimeArtifact, 0o700); err != nil {
		t.Fatal(err)
	}
	var arguments []string
	var extraFileCount int

	service := Service{
		ResolveBackend: func() (string, error) { return "/backend", nil },
		ResolvePayload: func(release.Resolved, string, string) (PayloadSource, error) {
			return PayloadSource{Directory: payload}, nil
		},
		ResolveRuntime: func(release.Resolved, string, string) (RuntimeArtifactSource, error) {
			return RuntimeArtifactSource{Directory: runtimeArtifact}, nil
		},
		ResolveManagedTarget: func(string, int64) (TargetConnection, error) {
			return TargetConnection{
				Target: "eva@10.159.56.197",
				SSHOptions: []string{
					"Port=22",
					"StrictHostKeyChecking=yes",
					"ControlPath=/run/eva/p/control",
				},
				sudoMethod: "passwordless",
			}, nil
		},
		Run: func(
			_ string,
			got []string,
			streams Streams,
		) error {
			arguments = append(
				[]string(nil),
				got...,
			)
			extraFileCount = len(streams.ExtraFiles)
			return nil
		},
	}
	if err := service.Publish(PublishOptions{Release: resolved, Target: "site-dev-196", Registry: "harbor.example.internal:32080"}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(arguments, "\n")

	required := []string{
		"--target",
		"eva@10.159.56.197",
		"--ssh-option",
		"Port=22",
		"StrictHostKeyChecking=yes",
		"ControlPath=/run/eva/p/control",
		"--sudo-mode",
		"passwordless",
	}

	for _, expected := range required {
		if !strings.Contains(joined, expected) {
			t.Fatalf(
				"managed connection argument %q is missing: %#v",
				expected,
				arguments,
			)
		}
	}

	forbidden := []string{
		"--sudo-password-fd",
		"ssh-secret",
		"sudo-secret",
	}

	for _, value := range forbidden {
		if strings.Contains(joined, value) {
			t.Fatalf(
				"managed connection arguments contain forbidden value %q: %#v",
				value,
				arguments,
			)
		}
	}

	if extraFileCount != 0 {
		t.Fatalf(
			"passwordless managed Target received %d secret file descriptors",
			extraFileCount,
		)
	}
}

func writeOriginalRelease(t *testing.T, includeOffline bool) release.Resolved {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"eva-tool.tar.gz":       "tool",
		"eva-tool-installer.sh": "installer",
		"eva-infra.tar.gz":      "infra",
		"eva-solution.tar.gz":   "solution",
	}
	if includeOffline {
		files["eva-offline.tar.gz"] = "offline"
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	artifacts := []string{
		artifactYAML("eva-tool", "eva-tool.tar.gz", files["eva-tool.tar.gz"]),
		artifactYAML("eva-tool-installer", "eva-tool-installer.sh", files["eva-tool-installer.sh"]),
		artifactYAML("eva-infra", "eva-infra.tar.gz", files["eva-infra.tar.gz"]),
		artifactYAML("eva-solution", "eva-solution.tar.gz", files["eva-solution.tar.gz"]),
	}
	if includeOffline {
		artifacts = append(artifacts, artifactYAML("eva-offline", "eva-offline.tar.gz", files["eva-offline.tar.gz"]))
	}
	metadata := fmt.Sprintf("version: 3.2.0\nplatform:\n  os: %s\n  arch: %s\nartifacts:\n%s", runtime.GOOS, runtime.GOARCH, strings.Join(artifacts, ""))
	if err := os.WriteFile(filepath.Join(root, "release.yaml"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := release.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func artifactYAML(name, file, contents string) string {
	digest := sha256.Sum256([]byte(contents))
	return fmt.Sprintf("  - name: %s\n    file: %s\n    sha256: %x\n", name, file, digest)
}
