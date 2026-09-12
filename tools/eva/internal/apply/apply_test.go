package apply

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"eva-deployer/tools/eva/internal/fieldoverride"
	"eva-deployer/tools/eva/internal/operation"
	"eva-deployer/tools/eva/internal/plan"
)

func TestExecuteRunsPlanAndRecordsResult(t *testing.T) {
	stateRoot := t.TempDir()
	logRoot := t.TempDir()
	workspace := createWorkspace(t)
	releaseRoot := createReleaseSource(t, "")
	runtimeRoot := createRuntime(t)
	record := createOperation(t, stateRoot, workspace, releaseRoot, []plan.Step{{Component: "infra", Playbook: expectedPlaybooks["infra"]}})
	var output bytes.Buffer
	now := fixedClock()

	completed, err := Execute(Options{
		StateRoot: stateRoot, LogRoot: logRoot, RuntimeRoot: runtimeRoot,
		Stdout: &output, Stderr: &output, Now: now,
	}, record)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if completed.Status != operation.Succeeded || completed.ResultPath == "" {
		t.Fatalf("completed operation = %#v", completed)
	}
	if !strings.Contains(output.String(), "EVA_REPO_ROOT="+releaseRoot) {
		t.Fatalf("managed Ansible output = %q", output.String())
	}
	for _, path := range []string{completed.ResultPath, filepath.Join(completed.LogDirectory, "ansible.log"), filepath.Join(completed.LogDirectory, "ansible-internal.log")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("permissions for %s = %o, want 600", path, got)
		}
	}
}

func TestExecuteStopsAfterFailedStep(t *testing.T) {
	stateRoot := t.TempDir()
	workspace := createWorkspace(t)
	releaseRoot := createReleaseSource(t, "fail")
	runtimeRoot := createRuntime(t)
	record := createOperation(t, stateRoot, workspace, releaseRoot, []plan.Step{
		{Component: "infra", Playbook: expectedPlaybooks["infra"]},
		{Component: "app", Playbook: expectedPlaybooks["app"]},
	})
	completed, err := Execute(Options{StateRoot: stateRoot, LogRoot: t.TempDir(), RuntimeRoot: runtimeRoot, Now: fixedClock()}, record)
	if err == nil || !strings.Contains(err.Error(), "infra") {
		t.Fatalf("Execute() error = %v, want failed infra step", err)
	}
	if completed.Status != operation.Failed {
		t.Fatalf("operation status = %q, want failed", completed.Status)
	}
	contents, err := os.ReadFile(completed.ResultPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(contents), "component:") != 1 {
		t.Fatalf("failed result should contain one step: %s", contents)
	}
}

func TestExecutePassesOnlyStagedAppOverrideVars(t *testing.T) {
	stateRoot := t.TempDir()
	workspace := createWorkspace(t)
	releaseRoot := createReleaseSource(t, "")
	runtimeRoot := createRuntime(t)
	valuesPath := filepath.Join(t.TempDir(), "app.yaml")
	if err := os.WriteFile(valuesPath, []byte("token: secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	overrides, err := fieldoverride.Parse(nil, []string{"app=" + valuesPath}, []string{"app:replicaCount=2"}, map[string]bool{"app": true})
	if err != nil {
		t.Fatal(err)
	}
	document := plan.Document{
		SchemaVersion: plan.SchemaVersion, SiteID: "customer-a", Workspace: workspace,
		ReleaseVersion: "3.2.0", ReleaseRoot: releaseRoot,
		Steps:     []plan.Step{{Component: "app", Playbook: expectedPlaybooks["app"]}},
		Overrides: overrides.Public(), OverrideInputs: overrides,
		AnsibleExtraVars: []string{"eva_site_id=customer-a"},
		Environment:      map[string]string{"EVA_SITE_ID": "customer-a", "EVA_WORKSPACE_ROOT": workspace},
	}
	record, err := operation.Create(stateRoot, document, fixedClock()())
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if _, err := Execute(Options{StateRoot: stateRoot, LogRoot: t.TempDir(), RuntimeRoot: runtimeRoot, Stdout: &output, Stderr: &output, Now: fixedClock()}, record); err != nil {
		t.Fatal(err)
	}
	loaded, err := operation.LoadPlan(stateRoot, record)
	if err != nil {
		t.Fatal(err)
	}
	varsPath := loaded.Overrides["app"].AnsibleVarsPath
	if !strings.Contains(output.String(), "@"+varsPath) {
		t.Fatalf("managed Ansible arguments = %q, want staged vars %q", output.String(), varsPath)
	}
	if strings.Contains(output.String(), valuesPath) {
		t.Fatalf("managed Ansible arguments leaked source path: %q", output.String())
	}
}

func createOperation(t *testing.T, stateRoot, workspace, releaseRoot string, steps []plan.Step) operation.Record {
	t.Helper()
	record, err := operation.Create(stateRoot, plan.Document{
		SchemaVersion: plan.SchemaVersion, SiteID: "customer-a", Workspace: workspace,
		ReleaseVersion: "3.2.0", ReleaseRoot: releaseRoot, Steps: steps,
		AnsibleExtraVars: []string{"eva_site_id=customer-a"},
		Environment:      map[string]string{"EVA_SITE_ID": "customer-a", "EVA_WORKSPACE_ROOT": workspace},
	}, time.Date(2026, 9, 12, 1, 2, 3, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func createWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "inventory")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "inventory.ini"), []byte("[local]\nlocalhost ansible_connection=local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func createReleaseSource(t *testing.T, behavior string) string {
	t.Helper()
	root := t.TempDir()
	for component, path := range expectedPlaybooks {
		fullPath := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte("# "+component+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "ansible.cfg"), []byte("[defaults]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if behavior != "" {
		if err := os.WriteFile(filepath.Join(root, "fail"), []byte(behavior), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func createRuntime(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	paths := map[string]string{
		"ansible-playbook": "venv/bin/ansible-playbook", "helm": "bin/helm", "kubectl": "bin/kubectl", "kustomize": "bin/kustomize", "oras": "bin/oras",
	}
	for name, path := range paths {
		fullPath := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		contents := "#!/bin/sh\nprintf 'EVA_REPO_ROOT=%s\\n' \"$EVA_REPO_ROOT\"\nprintf 'ARGS=%s\\n' \"$*\"\n"
		if name == "ansible-playbook" {
			contents += "if [ -f \"$EVA_REPO_ROOT/fail\" ]; then exit 7; fi\n"
		}
		if err := os.WriteFile(fullPath, []byte(contents), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	contents := "schema_version: v1\nversion: 3.2.0\ntools:\n"
	for name, path := range paths {
		contents += "  " + name + ": " + path + "\n"
	}
	if err := os.WriteFile(filepath.Join(root, "runtime.yaml"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func fixedClock() func() time.Time {
	now := time.Date(2026, 9, 12, 1, 2, 4, 0, time.UTC)
	return func() time.Time { return now }
}
