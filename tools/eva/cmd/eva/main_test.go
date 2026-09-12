package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"eva-deployer/tools/eva/internal/runtime"
)

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

	environment, err := shellEnvironment([]string{"PATH=/usr/bin", "KEEP=value"}, resolvedRuntime, "", workspaceRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	values := environmentMap(environment)
	if values["EVA_RUNTIME_ROOT"] != resolvedRuntime.Root || values["EVA_SITE_ID"] != "customer-a" || values["EVA_WORKSPACE_ROOT"] != workspaceRoot || values["KEEP"] != "value" {
		t.Fatalf("shell environment = %#v", values)
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
