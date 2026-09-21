package remote

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePublishBackendFromInstalledTool(t *testing.T) {
	executable, backendRoot := writeBackendLayout(t)
	backend, err := resolvePublishBackend(executable, "")
	if err != nil {
		t.Fatalf("resolvePublishBackend() error = %v", err)
	}
	want := filepath.Join(backendRoot, filepath.FromSlash(publishBackendRelativePath))
	if backend != want {
		t.Fatalf("backend = %q, want %q", backend, want)
	}
}

func TestResolvePublishBackendResolvesExecutableSymlink(t *testing.T) {
	executable, backendRoot := writeBackendLayout(t)
	link := filepath.Join(t.TempDir(), "eva")
	if err := os.Symlink(executable, link); err != nil {
		t.Fatal(err)
	}
	backend, err := resolvePublishBackend(link, "")
	if err != nil {
		t.Fatalf("resolvePublishBackend() error = %v", err)
	}
	if want := filepath.Join(backendRoot, filepath.FromSlash(publishBackendRelativePath)); backend != want {
		t.Fatalf("backend = %q, want %q", backend, want)
	}
}

func TestResolvePublishBackendUsesOverride(t *testing.T) {
	_, backendRoot := writeBackendLayout(t)
	backend, err := resolvePublishBackend(filepath.Join(t.TempDir(), "unused-eva"), backendRoot)
	if err != nil {
		t.Fatalf("resolvePublishBackend() error = %v", err)
	}
	if want := filepath.Join(backendRoot, filepath.FromSlash(publishBackendRelativePath)); backend != want {
		t.Fatalf("backend = %q, want %q", backend, want)
	}
}

func TestResolvePublishBackendRejectsInvalidRootsAndBackends(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		setup func(t *testing.T, executable, backendRoot, root string)
	}{
		{
			name: "missing override directory",
			setup: func(t *testing.T, executable, _, root string) {
				if _, err := resolvePublishBackend(executable, filepath.Join(root, "missing")); err == nil {
					t.Fatal("missing override directory was accepted")
				}
			},
		},
		{
			name: "override is file",
			setup: func(t *testing.T, executable, _, root string) {
				path := filepath.Join(root, "not-a-directory")
				if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				if _, err := resolvePublishBackend(executable, path); err == nil {
					t.Fatal("file override was accepted")
				}
			},
		},
		{
			name: "missing publish backend",
			setup: func(t *testing.T, executable, backendRoot, _ string) {
				if err := os.Remove(filepath.Join(backendRoot, filepath.FromSlash(publishBackendRelativePath))); err != nil {
					t.Fatal(err)
				}
				if _, err := resolvePublishBackend(executable, ""); err == nil {
					t.Fatal("missing publish backend was accepted")
				}
			},
		},
		{
			name: "publish backend symlink",
			setup: func(t *testing.T, executable, backendRoot, _ string) {
				path := filepath.Join(backendRoot, filepath.FromSlash(publishBackendRelativePath))
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(executable, path); err != nil {
					t.Fatal(err)
				}
				if _, err := resolvePublishBackend(executable, ""); err == nil {
					t.Fatal("publish backend symlink was accepted")
				}
			},
		},
		{
			name: "publish backend non executable",
			setup: func(t *testing.T, executable, backendRoot, _ string) {
				path := filepath.Join(backendRoot, filepath.FromSlash(publishBackendRelativePath))
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
				if _, err := resolvePublishBackend(executable, ""); err == nil {
					t.Fatal("non-executable publish backend was accepted")
				}
			},
		},
		{
			name: "publish backend escapes root through parent symlink",
			setup: func(t *testing.T, executable, backendRoot, root string) {
				scripts := filepath.Join(backendRoot, "scripts")
				if err := os.RemoveAll(scripts); err != nil {
					t.Fatal(err)
				}
				outsideBackend := filepath.Join(root, filepath.FromSlash("remote/publish_release_to_target.sh"))
				if err := os.MkdirAll(filepath.Dir(outsideBackend), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(outsideBackend, []byte("#!/bin/sh\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(root, scripts); err != nil {
					t.Fatal(err)
				}
				if _, err := resolvePublishBackend(executable, ""); err == nil {
					t.Fatal("backend outside root was accepted")
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			executable, backendRoot := writeBackendLayout(t)
			testCase.setup(t, executable, backendRoot, t.TempDir())
		})
	}
}

func writeBackendLayout(t *testing.T) (string, string) {
	t.Helper()
	toolRoot := filepath.Join(t.TempDir(), "tool")
	executable := filepath.Join(toolRoot, "bin", "eva")
	backendRoot := filepath.Join(toolRoot, "libexec", "remote-root")
	backend := filepath.Join(backendRoot, filepath.FromSlash(publishBackendRelativePath))
	if err := os.MkdirAll(filepath.Dir(backend), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backend, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return executable, backendRoot
}
