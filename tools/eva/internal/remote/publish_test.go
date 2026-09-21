package remote

import (
	"crypto/sha256"
	"errors"
	"fmt"
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

func TestPublishRejectsPreparedAndOfflineMissingReleases(t *testing.T) {
	service := NewService()
	if err := service.Publish(PublishOptions{Release: release.Resolved{Prepared: true}, Target: "eva@target"}); err == nil || !strings.Contains(err.Error(), "prepared") {
		t.Fatalf("Publish(prepared) error = %v", err)
	}
	missingOffline := writeOriginalRelease(t, false)
	if err := service.Publish(PublishOptions{Release: missingOffline, Target: "eva@target"}); err == nil || !strings.Contains(err.Error(), "eva-offline") {
		t.Fatalf("Publish(missing offline) error = %v", err)
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
		ResolvePayload: func(release.Resolved) (PayloadSource, error) {
			return PayloadSource{Directory: "/preparation/target-payload"}, nil
		},
		Run: func(path string, arguments []string, _ Streams) error {
			gotPath = path
			gotArguments = append([]string(nil), arguments...)
			return nil
		},
	}
	if err := service.Publish(PublishOptions{Release: resolved, Target: "eva@target.example.internal"}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if gotPath == "" {
		t.Fatal("backend was not invoked")
	}
	want := []string{"--release-dir", resolved.Root, "--payload-dir", "/preparation/target-payload", "--target", "eva@target.example.internal"}
	if strings.Join(gotArguments, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("arguments = %#v, want %#v", gotArguments, want)
	}
}

func TestPublishPreservesBackendFailure(t *testing.T) {
	backendFailure := errors.New("backend failed")
	service := Service{
		ResolveBackend: func() (string, error) { return "/backend", nil },
		ResolvePayload: func(release.Resolved) (PayloadSource, error) { return PayloadSource{Directory: "/payload"}, nil },
		Run:            func(string, []string, Streams) error { return backendFailure },
	}
	err := service.Publish(PublishOptions{Release: writeOriginalRelease(t, true), Target: "target.example.internal"})
	if !errors.Is(err, backendFailure) {
		t.Fatalf("Publish() error = %v, want wrapped backend failure", err)
	}
}

func writeOriginalRelease(t *testing.T, includeOffline bool) release.Resolved {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"eva-tool.tar.gz":     "tool",
		"eva-infra.tar.gz":    "infra",
		"eva-solution.tar.gz": "solution",
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
