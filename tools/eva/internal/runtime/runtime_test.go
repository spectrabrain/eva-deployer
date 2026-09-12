package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveValidatesManagedTools(t *testing.T) {
	root := createRuntime(t)
	resolved, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Descriptor.Version != "3.2.0" {
		t.Fatalf("runtime version = %q", resolved.Descriptor.Version)
	}
	path, err := resolved.ToolPath("ansible-playbook")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "venv", "bin", "ansible-playbook"); path != want {
		t.Fatalf("ToolPath() = %q, want %q", path, want)
	}
	if _, err := resolved.ToolPath("sh"); err == nil {
		t.Fatal("ToolPath(sh) unexpectedly succeeded")
	}
}

func TestResolveRejectsToolEscapingRuntimeRoot(t *testing.T) {
	root := createRuntime(t)
	descriptor := filepath.Join(root, descriptorName)
	contents, err := os.ReadFile(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	contents = []byte(strings.Replace(string(contents), "bin/helm", "../helm", 1))
	if err := os.WriteFile(descriptor, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(root); err == nil || !strings.Contains(err.Error(), "escapes the runtime root") {
		t.Fatalf("Resolve() error = %v, want root escape error", err)
	}
}

func TestResolveRejectsSymlinkOutsideRuntimeRoot(t *testing.T) {
	root := createRuntime(t)
	outside := filepath.Join(t.TempDir(), "helm")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "bin", "helm")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "bin", "helm")); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(root); err == nil || !strings.Contains(err.Error(), "resolves outside the runtime root") {
		t.Fatalf("Resolve() error = %v, want external symlink error", err)
	}
}

func TestInstallPublishesValidatedRuntime(t *testing.T) {
	source := createRuntime(t)
	destination := filepath.Join(t.TempDir(), "runtime")
	installed, err := Install(source, destination)
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if installed.Root != destination {
		t.Fatalf("installed root = %q, want %q", installed.Root, destination)
	}
	contents, err := os.ReadFile(filepath.Join(destination, "runtime.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "version: 3.2.0") {
		t.Fatalf("installed descriptor = %q", contents)
	}
}

func TestInstallLeavesExistingRuntimeWhenSourceIsInvalid(t *testing.T) {
	destination := createRuntime(t)
	source := t.TempDir()
	if _, err := Install(source, destination); err == nil {
		t.Fatal("Install() unexpectedly succeeded")
	}
	if _, err := Resolve(destination); err != nil {
		t.Fatalf("existing runtime was not preserved: %v", err)
	}
}

func TestInstallRejectsIdenticalSourceAndDestination(t *testing.T) {
	root := createRuntime(t)
	if _, err := Install(root, root); err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("Install() error = %v, want source/destination error", err)
	}
}

func createRuntime(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	paths := map[string]string{
		"ansible-playbook": "venv/bin/ansible-playbook",
		"helm":             "bin/helm",
		"kubectl":          "bin/kubectl",
		"kustomize":        "bin/kustomize",
		"oras":             "bin/oras",
	}
	for _, path := range paths {
		fullPath := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	contents := "schema_version: v1\nversion: 3.2.0\ntools:\n"
	for name, path := range paths {
		contents += "  " + name + ": " + path + "\n"
	}
	if err := os.WriteFile(filepath.Join(root, descriptorName), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}
