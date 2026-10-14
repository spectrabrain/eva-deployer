package inspect

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"eva-deployer/tools/eva/internal/operation"
	"eva-deployer/tools/eva/internal/plan"
)

func fixture(t *testing.T, state, site, releaseRoot string, when time.Time) operation.Record {
	t.Helper()
	record, err := operation.Create(state, plan.Document{
		SchemaVersion: plan.SchemaVersion, SiteID: site, ReleaseVersion: "3.2.0",
		ReleaseRoot: releaseRoot, Workspace: "/etc/eva/sites/" + site,
		RepositoryMode: "remote_repository",
		Steps:          []plan.Step{{Component: "app", Playbook: "src/solution/playbooks/site_eva_app.yaml"}},
	}, when)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func execute(t *testing.T, arguments ...string) (string, string, error) {
	t.Helper()
	var output, diagnostics bytes.Buffer
	err := Run(arguments, &output, &diagnostics)
	return output.String(), diagnostics.String(), err
}

func TestValuesPrintSecretsVerbatimWithoutWrites(t *testing.T) {
	root := t.TempDir()
	state, releaseRoot := filepath.Join(root, "state"), filepath.Join(root, "release")
	record := fixture(t, state, "customer-a", releaseRoot, time.Now())
	path := filepath.Join(releaseRoot, "out/work/rendered/customer-a/10.0.0.21/app/resolved-values.yaml")
	content := "app:\n  sso:\n    adminClientSecret: test-secret\ndatabase:\n  password: test-password\n"
	write(t, path, content)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	out, diagnostics, err := execute(t, "values", "--state-root", state, "--component", "app")
	if err != nil || out != content || !strings.Contains(diagnostics, record.ID) || !strings.Contains(diagnostics, path) {
		t.Fatalf("output=%q diagnostics=%q error=%v", out, diagnostics, err)
	}
	after, err := os.Stat(path)
	if err != nil || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("inspection changed artifact metadata: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(state, record.ID))
	if err != nil || len(entries) != 2 {
		t.Fatalf("inspection wrote operation data: entries=%v err=%v", entries, err)
	}
}

func TestValuesSourcesAndNoSilentFallback(t *testing.T) {
	root := t.TempDir()
	state, releaseRoot := filepath.Join(root, "state"), filepath.Join(root, "release")
	fixture(t, state, "site-a", releaseRoot, time.Now())
	directory := filepath.Join(releaseRoot, "out/work/rendered/site-a/node-a/app")
	for source, name := range map[string]string{
		"effective": "effective-input-values.yaml", "defaults": "chart-defaults.yaml",
		"override": "app-k3s-override.yaml", "sources": "values-sources.yaml",
	} {
		write(t, filepath.Join(directory, name), "source: "+source+"\n")
		out, _, err := execute(t, "values", "--state-root", state, "--component", "app", "--source", source)
		if err != nil || out != "source: "+source+"\n" {
			t.Fatalf("source %s: out=%q err=%v", source, out, err)
		}
	}
	out, _, err := execute(t, "values", "--state-root", state, "--component", "app")
	if err == nil || out != "" || !strings.Contains(err.Error(), "artifact is not available") {
		t.Fatalf("resolved lookup fell back: out=%q err=%v", out, err)
	}
	write(t, filepath.Join(filepath.Dir(directory), "iam/eva-iam-k3s-override.yaml"), "iam: true\n")
	if out, _, err := execute(t, "values", "--state-root", state, "--component", "iam", "--source", "override"); err != nil || out != "iam: true\n" {
		t.Fatalf("IAM source: out=%q err=%v", out, err)
	}
	if _, _, err := execute(t, "values", "--state-root", state, "--component", "iam"); err == nil || !strings.Contains(err.Error(), "artifact is not available") {
		t.Fatalf("IAM resolved should be unavailable: %v", err)
	}
}

func TestOperationSelectionUsesCreationTimeAndSiteFilter(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	old := fixture(t, state, "site-a", filepath.Join(root, "old"), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	newer := fixture(t, state, "site-a", filepath.Join(root, "new"), old.CreatedAt.Add(time.Hour))
	other := fixture(t, state, "site-b", filepath.Join(root, "other"), old.CreatedAt.Add(2*time.Hour))
	old.UpdatedAt = old.CreatedAt.Add(24 * time.Hour)
	if err := operation.Update(state, old); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		arguments []string
		want      string
	}{
		{[]string{"paths", "--state-root", state}, other.ID},
		{[]string{"paths", "--state-root", state, "--site", "site-a"}, newer.ID},
		{[]string{"paths", "--state-root", state, "--operation", old.ID}, old.ID},
	} {
		out, _, err := execute(t, test.arguments...)
		if err != nil || !strings.Contains(out, test.want) {
			t.Fatalf("args=%v out=%q err=%v", test.arguments, out, err)
		}
	}
	out, _, err := execute(t, "operations", "--state-root", state, "--site", "site-a")
	if err != nil || strings.Contains(out, other.ID) || !strings.Contains(out, newer.ID) || !strings.Contains(out, old.ID) || strings.Index(out, newer.ID) >= strings.Index(out, old.ID) {
		t.Fatalf("operation list: out=%q err=%v", out, err)
	}
	if _, _, err := execute(t, "paths", "--state-root", state, "--operation", old.ID, "--site", "site-b"); err == nil {
		t.Fatal("accepted mismatched site")
	}
}

func TestTargetAmbiguityFilesAndConfig(t *testing.T) {
	root := t.TempDir()
	state, releaseRoot := filepath.Join(root, "state"), filepath.Join(root, "release")
	fixture(t, state, "site-a", releaseRoot, time.Now())
	for _, target := range []string{"node-a", "node-b"} {
		write(t, filepath.Join(releaseRoot, "out/work/rendered/site-a", target, "app/resolved-values.yaml"), "target: "+target+"\n")
	}
	write(t, filepath.Join(releaseRoot, "out/work/config/site-a/node-a/eva.yaml"), "gpu_count: 2\n")
	if _, _, err := execute(t, "values", "--state-root", state, "--component", "app"); err == nil || !strings.Contains(err.Error(), "--target") {
		t.Fatalf("ambiguous target error=%v", err)
	}
	out, _, err := execute(t, "values", "--state-root", state, "--component", "app", "--target", "node-b")
	if err != nil || out != "target: node-b\n" {
		t.Fatalf("target selection: out=%q err=%v", out, err)
	}
	out, _, err = execute(t, "files", "--state-root", state)
	if err != nil || !strings.Contains(out, "node-a") || !strings.Contains(out, "node-b") || !strings.Contains(out, "eva.yaml") || !strings.Contains(out, "0600") {
		t.Fatalf("files: out=%q err=%v", out, err)
	}
	out, _, err = execute(t, "config", "--state-root", state)
	if err != nil || out != "gpu_count: 2\n" {
		t.Fatalf("config: out=%q err=%v", out, err)
	}
}

func TestDirectSourceAndCustomGeneratedDirectories(t *testing.T) {
	root := t.TempDir()
	rendered := filepath.Join(root, "custom-values")
	write(t, filepath.Join(rendered, "node-a/app/custom.yaml"), "password: raw-test-value\n")
	out, _, err := execute(t, "values", "--release-root", root, "--site", "site-a", "--rendered-root", rendered, "--component", "app", "--file", "custom.yaml")
	if err != nil || out != "password: raw-test-value\n" {
		t.Fatalf("direct source: out=%q err=%v", out, err)
	}
	out, _, err = execute(t, "files", "--release-root", root, "--site", "site-a")
	if err != nil || !strings.Contains(out, "No generated YAML files") {
		t.Fatalf("missing artifacts: out=%q err=%v", out, err)
	}
}

func TestPlanResultLogs(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	record := fixture(t, state, "site-a", root, time.Now())
	out, _, err := execute(t, "plan", "--state-root", state)
	if err != nil || !strings.Contains(out, "release_root:") || !strings.Contains(out, "site-a") {
		t.Fatalf("plan: out=%q err=%v", out, err)
	}
	if _, _, err := execute(t, "result", "--state-root", state); err == nil {
		t.Fatal("accepted missing result")
	}
	if _, err := operation.WriteResult(state, record, operation.Result{Status: operation.Succeeded}); err != nil {
		t.Fatal(err)
	}
	out, _, err = execute(t, "result", "--state-root", state)
	if err != nil || !strings.Contains(out, "status: succeeded") {
		t.Fatalf("result: out=%q err=%v", out, err)
	}
	if _, _, err := execute(t, "logs", "--state-root", state); err == nil {
		t.Fatal("accepted missing log directory")
	}
	record.LogDirectory = filepath.Join(root, "logs", record.ID)
	write(t, filepath.Join(record.LogDirectory, "ansible.log"), "one\ntwo\nthree\n")
	if err := operation.Update(state, record); err != nil {
		t.Fatal(err)
	}
	out, _, err = execute(t, "logs", "--state-root", state, "--tail", "2")
	if err != nil || out != "two\nthree\n" {
		t.Fatalf("logs: out=%q err=%v", out, err)
	}
	out, _, err = execute(t, "logs", "--state-root", state, "--tail", "0")
	if err != nil || out != "one\ntwo\nthree\n" {
		t.Fatalf("all logs: out=%q err=%v", out, err)
	}
}

func TestRejectInvalidOptionsAndSymlinkEscape(t *testing.T) {
	for _, arguments := range [][]string{
		{"unknown"}, {"values"}, {"values", "--component", "other"},
		{"values", "--component", "app", "--source", "unknown"},
		{"files", "--target", "../node"}, {"files", "--site", ".."},
		{"values", "--component", "app", "--file", "../secret.yaml"},
		{"values", "--component", "app", "--file", "secret.txt"},
		{"paths", "--release-root", "/tmp"}, {"plan", "--release-root", "/tmp", "--site", "a"},
		{"paths", "--operation", "../escape"}, {"logs", "--tail", "-1"},
		{"logs", "--tail", "100001"}, {"paths", "extra"},
		{"logs", "--component", "app"}, {"config", "--source", "resolved"},
		{"paths", "--tail", "1"}, {"values", "--component", "app", "--config-root", "/tmp"},
	} {
		if _, _, err := execute(t, arguments...); err == nil {
			t.Fatalf("accepted %v", arguments)
		}
	}
	root := t.TempDir()
	directory := filepath.Join(root, "release/out/work/rendered/site-a/node-a/app")
	write(t, filepath.Join(root, "outside.yaml"), "secret: outside\n")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "outside.yaml"), filepath.Join(directory, "resolved-values.yaml")); err != nil {
		t.Fatal(err)
	}
	out, _, err := execute(t, "values", "--release-root", filepath.Join(root, "release"), "--site", "site-a", "--component", "app")
	if err == nil || out != "" || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("symlink escape: out=%q err=%v", out, err)
	}
}

func TestHelp(t *testing.T) {
	for _, arguments := range [][]string{{"help"}, {"--help"}, {"values", "--help"}} {
		out, _, err := execute(t, arguments...)
		if err != nil || !strings.Contains(out, "WITHOUT secret masking") {
			t.Fatalf("help: out=%q err=%v", out, err)
		}
	}
}
