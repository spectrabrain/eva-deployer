package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"eva-deployer/tools/eva/internal/apt"
	"eva-deployer/tools/eva/internal/health"
	"eva-deployer/tools/eva/internal/operation"
	"eva-deployer/tools/eva/internal/plan"
	"eva-deployer/tools/eva/internal/release"
	"eva-deployer/tools/eva/internal/runtime"
)

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

func TestAPTPriorToInfraSkipsLocalRepository(t *testing.T) {
	called := false
	previousService := newAPTService
	t.Cleanup(func() { newAPTService = previousService })
	newAPTService = func() apt.Service {
		called = true
		return apt.Service{}
	}
	if err := aptPrerequisite(plan.Document{
		RepositoryMode: "local_repository",
		Steps:          []plan.Step{{Component: "infra"}},
	}); err != nil {
		t.Fatalf("aptPrerequisite() error = %v", err)
	}
	if called {
		t.Fatal("local repository unexpectedly ran APT diagnostic")
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
