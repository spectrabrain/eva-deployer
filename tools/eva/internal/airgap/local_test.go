package airgap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"eva-deployer/tools/eva/internal/release"
	"eva-deployer/tools/eva/internal/remote"
	"eva-deployer/tools/eva/internal/workspace"
	"gopkg.in/yaml.v3"
)

func localTestBundle(t *testing.T) (release.Resolved, string) {
	t.Helper()
	f := newServiceTestFixture(t)
	bundle, err := f.service.Prepare(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	source, err := release.ImportAirgapBundle(bundle, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return source, f.service.RuntimeRoot
}

func localTestConfig() workspace.Installer {
	input := workspace.Installer{SchemaVersion: "v1"}
	input.Site.ID = "local-test"
	input.Repository.Mode, input.Repository.Registry, input.Repository.Project = "local", "harbor.example.test:32080", "eva"
	input.Components = map[string]bool{"infra": true, "agent": true}
	input.Targets = map[string]workspace.InstallerTarget{"server-a": {Ansible: map[string]any{"ansible_connection": "local"}}}
	return input
}

func TestLocalPrepareOfflineAndExistingHarbor(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", external), func(t *testing.T) {
			source, runtimeRoot := localTestBundle(t)
			service := NewLocalService()
			service.InstallRoot, service.RuntimeRoot = t.TempDir(), runtimeRoot
			service.RegistryConfigPath = filepath.Join(t.TempDir(), "registries.yaml")
			archiveTestWriteFile(t, service.RegistryConfigPath, []byte("mirrors:\n  other.test:443:\n    endpoint: [https://other.test:443]\nconfigs:\n  other.test:443:\n    tls:\n      ca_file: /etc/other-ca.pem\n"))
			service.Preflight = func(context.Context) error { return nil }
			service.ValidateNewRegistry = func(string) error { return nil }
			service.ResolveBackend = func(name string) (string, error) { return "/trusted/" + name, nil }
			dockerReady := false
			service.DockerReady = func(context.Context) bool { return dockerReady }
			service.ComposeReady = func(context.Context) bool { return true }
			var calls []remote.ProcessOptions
			service.Run = func(_ context.Context, options remote.ProcessOptions) error {
				if options.Streams.Stdout != nil && options.Streams.Stdout != io.Discard {
					t.Fatal("backend output exposed")
				}
				for _, key := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_PROFILE", "BASH_ENV", "PYTHONPATH"} {
					if options.Env[key] != "" {
						t.Fatalf("inherited source/auth override %s", key)
					}
				}
				if options.Env["PULL_SOURCE_IMAGES"] != "false" || options.Env["AIRGAP_MODE"] != "true" || options.Env["REPOSITORY_RECOVER_MISSING_CONTENT"] != "false" {
					t.Fatal("Local pipeline permits source downloads")
				}
				calls = append(calls, options)
				if strings.HasSuffix(options.Path, "install_docker.sh") {
					dockerReady = true
				}
				return nil
			}
			protocol := "http"
			if external {
				protocol = "https"
			}
			service.NewBootstrap = func() remote.BootstrapService {
				return remote.BootstrapService{
					DetectExistingHarbor:     func(context.Context, string, string) (string, bool, error) { return protocol, external, nil },
					EnsureExistingHarborHTTP: func(context.Context, string) error { t.Fatal("existing TLS changed"); return nil },
					EnsureRegistryTransport:  func(context.Context, string) error { return nil },
					ProbeManagedHarbor:       func(context.Context, remote.HarborReceipt, time.Duration) error { return nil },
					EnsureHarbor: func(_ context.Context, registry, project string, existing bool) (remote.HarborReceipt, error) {
						if !existing {
							t.Fatal("online Harbor bootstrap used")
						}
						return remote.HarborReceipt{SchemaVersion: "v1", ManagedBy: "external", Registry: registry, Project: project, Protocol: protocol, HarborVersion: "external", InstallRoot: "/opt/eva/harbor", DataRoot: "/var/lib/eva/harbor"}, nil
					},
					CheckHarbor:       func(context.Context, remote.HarborReceipt) error { return nil },
					Credential:        func(string) bool { return true },
					Password:          func(remote.HarborReceipt) (string, error) { return "fixture-password", nil },
					Login:             func(context.Context, string, string, string) error { return nil },
					EnsurePublication: func(context.Context, remote.HarborReceipt, remote.HarborCredential) error { return nil },
					ReadCredential: func(context.Context, string) (remote.HarborCredential, error) {
						return remote.HarborCredential{Username: "fixture-user", Password: "fixture-password"}, nil
					},
				}
			}
			t.Setenv("AWS_ACCESS_KEY_ID", "must-not-inherit")
			t.Setenv("BASH_ENV", "must-not-execute")
			var output bytes.Buffer
			root, err := service.Prepare(context.Background(), LocalOptions{Release: source, Config: localTestConfig(), Yes: true, Stdout: &output})
			if err != nil {
				t.Fatal(err)
			}
			paths := []string{}
			for _, call := range calls {
				paths = append(paths, filepath.Base(call.Path))
			}
			want := []string{"install_docker.sh", "docker", "push_images_to_repository.sh", "push_qdrant_snapshots_to_harbor.sh"}
			if !external {
				want = append(want[:1], append([]string{"setup_harbor.sh"}, want[1:]...)...)
			}
			if strings.Join(paths, ",") != strings.Join(want, ",") {
				t.Fatalf("pipeline=%q want=%q", paths, want)
			}
			if !strings.Contains(output.String(), "[OK] Local preparation completed") || strings.Contains(output.String(), "fixture-password") {
				t.Fatalf("bad completion output %q", output.String())
			}
			if _, err := os.Stat(filepath.Join(root, "runtime")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("Runtime mixed into prepared source")
			}
			contents, err := os.ReadFile(service.RegistryConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			var registry map[string]any
			if err := yaml.Unmarshal(contents, &registry); err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(contents, []byte("other.test:443")) || !bytes.Contains(contents, []byte("/etc/other-ca.pem")) || !bytes.Contains(contents, []byte("fixture-password")) || !bytes.Contains(contents, []byte(protocol+"://harbor.example.test:32080")) {
				t.Fatal("registry auth/transport lost unrelated settings")
			}
			info, _ := os.Stat(service.RegistryConfigPath)
			if info.Mode().Perm() != 0o600 {
				t.Fatal("registry credentials are not private")
			}
			originalRun := service.Run
			calls = nil
			service.Run = func(ctx context.Context, options remote.ProcessOptions) error {
				if strings.HasSuffix(options.Path, "push_images_to_repository.sh") {
					return errors.New("private backend failure")
				}
				return originalRun(ctx, options)
			}
			output.Reset()
			if _, err := service.Prepare(context.Background(), LocalOptions{Release: source, Config: localTestConfig(), Yes: true, Stdout: &output}); err == nil || strings.Contains(output.String(), "[OK]") || strings.Contains(err.Error(), "private backend failure") {
				t.Fatal("failed image publication was hidden or leaked diagnostics")
			}
			if len(calls) != 1 || calls[0].Path != "docker" {
				t.Fatal("failed publication continued to later steps or reinstalled Harbor")
			}
			service.Run = originalRun
			cachedFile := filepath.Join(root, "out/cache/models/agent/hf/model.bin")
			if err := os.WriteFile(cachedFile, []byte("corrupted"), 0o600); err != nil {
				t.Fatal(err)
			}
			before := len(calls)
			if _, err := service.Prepare(context.Background(), LocalOptions{Release: source, Config: localTestConfig(), Yes: true}); err == nil || len(calls) != before {
				t.Fatal("corrupted cache reached a host mutation")
			}
		})
	}
}

func TestLocalCacheFailureDoesNotPublishPartialContents(t *testing.T) {
	for _, kind := range []string{"traversal", "symlink", "gzip-checksum"} {
		t.Run(kind, func(t *testing.T) {
			entry := archiveTestRegular("out/cache/models/file.txt", []byte("cache"))
			if kind == "traversal" {
				entry.header.Name = "out/cache/../../escape.txt"
			}
			if kind == "symlink" {
				entry.header.Typeflag, entry.header.Size, entry.header.Linkname, entry.body = tar.TypeSymlink, 0, "/tmp/escape", nil
			}
			var buffer bytes.Buffer
			compressed := gzip.NewWriter(&buffer)
			if _, err := compressed.Write(archiveTestTarBytes(t, []archiveTestEntry{entry})); err != nil {
				t.Fatal(err)
			}
			if err := compressed.Close(); err != nil {
				t.Fatal(err)
			}
			body := append([]byte(nil), buffer.Bytes()...)
			if kind == "gzip-checksum" {
				body[len(body)-8] ^= 0xff
			}
			sourceRoot := t.TempDir()
			archiveTestWriteFile(t, filepath.Join(sourceRoot, "offline.tar.gz"), body)
			source := release.Resolved{Root: sourceRoot, Metadata: release.Metadata{Artifacts: []release.Artifact{{Name: "eva-offline", File: "offline.tar.gz", SHA256: archiveTestDigest(body)}}}}
			prepared := t.TempDir()
			if _, err := MaterializeLocalCache(source, prepared); err == nil {
				t.Fatal("unsafe or corrupt offline cache accepted")
			}
			if entries, _ := os.ReadDir(filepath.Join(prepared, "out")); len(entries) != 0 {
				t.Fatal("failed extraction left a published/partial cache")
			}
			if _, err := os.Stat(filepath.Join(prepared, "escape.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("archive escaped cache staging")
			}
		})
	}
}

func TestLocalCacheIsolationAndReuse(t *testing.T) {
	source, _ := localTestBundle(t)
	prepared, err := release.Prepare(source, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := MaterializeLocalCache(source, prepared.Root)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := MaterializeLocalCache(source, prepared.Root); err != nil || root != again {
		t.Fatalf("cache reuse: %q %v", again, err)
	}
	if _, err := os.Stat(filepath.Join(root, "runtime")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Runtime extracted into cache")
	}
	unknown := filepath.Join(t.TempDir(), "out/cache")
	archiveTestWriteFile(t, filepath.Join(unknown, "preserve.txt"), []byte("existing"))
	if _, err := MaterializeLocalCache(source, filepath.Dir(filepath.Dir(unknown))); err == nil {
		t.Fatal("unknown cache overwritten")
	}
	if contents, _ := os.ReadFile(filepath.Join(unknown, "preserve.txt")); string(contents) != "existing" {
		t.Fatal("existing data changed")
	}
	target := filepath.Join(root, "models/agent/hf/model.bin")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(source.MetadataPath, target); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeLocalCache(source, prepared.Root); err == nil {
		t.Fatal("symlink cache reused")
	}
}

func TestLocalCacheReuseChecksLayoutWithoutRehashing(t *testing.T) {
	source, _ := localTestBundle(t)
	prepared, err := release.Prepare(source, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := MaterializeLocalCache(source, prepared.Root)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "models/agent/hf/model.bin")
	original, err := os.ReadFile(target)
	if err != nil || len(original) == 0 {
		t.Fatalf("fixture cache file: %q %v", original, err)
	}
	contents, err := os.ReadFile(filepath.Join(root, localCacheReceipt))
	if err != nil || !strings.Contains(string(contents), "size: ") {
		t.Fatalf("cache receipt must record file sizes: %s %v", contents, err)
	}

	// Content was hashed during extraction; reuse must not read it again.
	sameSize := bytes.Repeat([]byte("x"), len(original))
	archiveTestWriteFile(t, target, sameSize)
	if _, err := MaterializeLocalCache(source, prepared.Root); err != nil {
		t.Fatalf("reuse re-hashed cache content: %v", err)
	}

	archiveTestWriteFile(t, target, append(sameSize, 'x'))
	if _, err := MaterializeLocalCache(source, prepared.Root); err == nil {
		t.Fatal("cache file with a changed size reused")
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeLocalCache(source, prepared.Root); err == nil {
		t.Fatal("cache with a missing file reused")
	}
}

func TestLocalCacheReuseAcceptsReceiptWithoutSizes(t *testing.T) {
	source, _ := localTestBundle(t)
	prepared, err := release.Prepare(source, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := MaterializeLocalCache(source, prepared.Root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, localCacheReceipt)
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt cacheReceipt
	if err := yaml.Unmarshal(contents, &receipt); err != nil {
		t.Fatal(err)
	}
	for index := range receipt.Files {
		receipt.Files[index].Size = nil
	}
	legacy, err := yaml.Marshal(receipt)
	if err != nil || strings.Contains(string(legacy), "size:") {
		t.Fatalf("legacy receipt fixture: %s %v", legacy, err)
	}
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeLocalCache(source, prepared.Root); err != nil {
		t.Fatalf("cache from an older receipt rejected: %v", err)
	}
}

func TestLocalDirectoryRejectsSymlinkBeforeCreatingChildren(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := makeLocalDirectory(filepath.Join(root, "link", "child"), 0o700); err == nil {
		t.Fatal("symlink parent accepted")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatal("created directories through a symlink before rejecting it")
	}
}

func TestLocalMirrorProjectsIncludesUnqualifiedDockerHubPaths(t *testing.T) {
	cache := t.TempDir()
	for name, images := range map[string]string{
		"images-pulled.txt":       "example.test/eva-app:3.3.0\nbusybox:latest\nbitnami/kubectl:latest\n",
		"infra-images-pulled.txt": "rancher/local-path-provisioner:latest\nbusybox:latest\n",
		"n8n-images.txt":          "docker.n8n.io/n8nio/n8n:2.32.7\n",
	} {
		archiveTestWriteFile(t, filepath.Join(cache, "images", name), []byte(images))
	}
	projects, err := localMirrorProjects(cache)
	if err != nil || strings.Join(projects, ",") != "bitnami,library,rancher" {
		t.Fatalf("Docker Hub project preparation: %v %v", projects, err)
	}
}

func TestLocalPrepareRequiresApprovalAndLocalTarget(t *testing.T) {
	service := NewLocalService()
	service.Preflight = func(context.Context) error { t.Fatal("invalid request reached host preflight"); return nil }
	input := localTestConfig()
	for _, options := range []LocalOptions{{Config: input}, {Config: workspace.Installer{}, Yes: true}} {
		if _, err := service.Prepare(context.Background(), options); err == nil {
			t.Fatal("invalid Local preparation accepted")
		}
	}
	input.Targets["server-a"] = workspace.InstallerTarget{Ansible: map[string]any{"ansible_connection": "ssh"}}
	if _, err := service.Prepare(context.Background(), LocalOptions{Config: input, Yes: true}); err == nil {
		t.Fatal("SSH controller prepared itself")
	}
}
