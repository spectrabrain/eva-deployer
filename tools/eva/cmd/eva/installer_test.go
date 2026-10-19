package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"eva-deployer/tools/eva/internal/apply"
	"eva-deployer/tools/eva/internal/operation"
	"eva-deployer/tools/eva/internal/plan"
	"eva-deployer/tools/eva/internal/workspace"
)

func installerTestInput(t *testing.T) string {
	t.Helper()
	previous := defaultWorkspaceCacheRoot
	defaultWorkspaceCacheRoot = filepath.Join(t.TempDir(), "workspaces")
	t.Cleanup(func() { defaultWorkspaceCacheRoot = previous })
	contents, err := workspace.InstallerSample("cloud")
	if err != nil {
		t.Fatal(err)
	}
	contents = fillInstallerSample(contents)
	path := filepath.Join(t.TempDir(), "install.yaml")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fillInstallerSample(contents []byte) []byte {
	return []byte(strings.NewReplacer(
		"<SITE_ID>", "customer-a", "<TARGET_USER>", "eva",
		"<HARBOR_HOST>", "harbor.example.com",
		"<PUBLIC_HOST>", "eva.example.com", "<APP_HOST>", "app.example.com", "<IAM_HOST>", "iam.example.com",
		"<TLS_CERT_DIRECTORY>", "/etc/eva/certs", "<APP_TLS_CERT_DIRECTORY>", "/etc/eva/app-certs", "<IAM_TLS_CERT_DIRECTORY>", "/etc/eva/iam-certs",
		"<AWS_ACCESS_KEY_ID>", "test-access", "<AWS_SECRET_ACCESS_KEY>", "test-secret",
		"<SUDO_PASSWORD>", "test-sudo", "<REALM_ADMIN_PASSWORD>", "test-admin",
		"<LICENSE_PRODUCT_CODE>", "eva-test", "<LICENSE_API_KEY>", "test-license", "<LICENSE_SHARED_KEY>", "test-shared",
		"<EXTERNAL_IAM_BASE_URL>", "https://iam.external.example.com/iam",
		"<SSO_REALM>", "eva-iam", "<SSO_CLIENT_ID>", "eva-app", "<SSO_ADMIN_CLIENT_ID>", "eva-admin-api",
		"<SSO_ADMIN_CLIENT_SECRET>", "test-external-secret",
	).Replace(string(contents)))
}

func TestConfigSamplesAndValidationDoNotWriteWorkspace(t *testing.T) {
	input := installerTestInput(t)
	for _, mode := range []string{"cloud", "remote", "local"} {
		output := captureRemoteTargetStdout(t, func() error { return run([]string{"config", "sample", "--mode", mode, "--non-interactive"}) })
		if err := os.WriteFile(input, fillInstallerSample([]byte(output)), 0o600); err != nil {
			t.Fatal(err)
		}
		parsed, err := workspace.LoadInstaller(input, "customer-a")
		if err != nil || parsed.Repository.Mode != mode {
			t.Fatalf("sample for %s did not validate: %v", mode, err)
		}
		if mode != "cloud" && (strings.Contains(output, "aws_access_key_id") || strings.Contains(output, "imagePullSecrets")) {
			t.Fatal("offline sample contains AWS inputs")
		}
		output = captureRemoteTargetStdout(t, func() error { return run([]string{"workspace", "validate", "--config", input}) })
		if !strings.Contains(output, "no Workspace was written") {
			t.Fatal("validation did not explain read-only behavior")
		}
		if _, err := os.Stat(defaultWorkspaceCacheRoot); !os.IsNotExist(err) {
			t.Fatal("validation wrote a workspace")
		}
	}
	for _, args := range [][]string{{"config", "unknown"}, {"config", "sample", "--mode", "unknown"}, {"config", "sample", "extra"}, {"workspace", "validate", "--config", input, "--workspace", "/unused"}} {
		if err := run(args); err == nil {
			t.Fatalf("invalid arguments accepted: %q", args)
		}
	}
}

func TestExternalIAMConfigPlanExcludesIAMAndKeepsSecretsPrivate(t *testing.T) {
	input := installerTestInput(t)
	contents, err := workspace.InstallerSampleFor(workspace.SampleOptions{Mode: "cloud", Protocol: "https", IAM: "external"})
	if err != nil {
		t.Fatal(err)
	}
	contents = []byte(strings.ReplaceAll(string(contents), "eva-redis-pass", "test-redis-password"))
	if err := os.WriteFile(input, fillInstallerSample(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "operations")
	if err := run([]string{"plan", writeRemotePublishRelease(t), "--config", input, "--save", "--state-root", state}); err != nil {
		t.Fatal(err)
	}
	record, err := operation.Latest(state)
	if err != nil {
		t.Fatal(err)
	}
	document, err := operation.LoadPlan(state, record)
	if err != nil {
		t.Fatal(err)
	}
	foundApp := false
	for _, step := range document.Steps {
		if step.Component == "iam" {
			t.Fatal("external IAM plan must not install IAM")
		}
		foundApp = foundApp || step.Component == "app"
	}
	if !foundApp {
		t.Fatal("external IAM plan lost App installation")
	}
	planContents, err := os.ReadFile(record.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(planContents), "test-external-secret") || strings.Contains(string(planContents), "test-redis-password") {
		t.Fatal("external SSO credentials leaked into public plan")
	}
	appValues, err := os.ReadFile(filepath.Join(document.Workspace, "site-values/app.yaml"))
	if err != nil || !strings.Contains(string(appValues), "test-external-secret") || !strings.Contains(string(appValues), "test-redis-password") {
		t.Fatal("external IAM credentials must remain in private workspace inputs")
	}
}

func TestConfigFlagNormalization(t *testing.T) {
	for _, normalize := range []func([]string) ([]string, error){normalizeInstallArgs, normalizePlanArgs} {
		for _, input := range [][]string{
			{"/release", "--config", "/inputs/install.yaml"},
			{"--config", "/inputs/install.yaml", "/release"},
		} {
			got, err := normalize(input)
			if err != nil || !reflect.DeepEqual(got, []string{"--config", "/inputs/install.yaml", "/release"}) {
				t.Fatalf("config normalization failed: %q, %v", got, err)
			}
		}
		if _, err := normalize([]string{"--config"}); err == nil {
			t.Fatal("missing config value accepted")
		}
	}
}

func TestPlanConfigPersistsWorkspaceForRetry(t *testing.T) {
	input := installerTestInput(t)
	state := filepath.Join(t.TempDir(), "operations")
	releaseRoot := writeRemotePublishRelease(t)
	if err := run([]string{"plan", releaseRoot, "--config", input, "--component", "app", "--save", "--state-root", state}); err != nil {
		t.Fatal(err)
	}
	record, err := operation.Latest(state)
	if err != nil {
		t.Fatal(err)
	}
	document, err := operation.LoadPlan(state, record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(document.Workspace, defaultWorkspaceCacheRoot+string(filepath.Separator)) {
		t.Fatal("plan did not persist generated workspace")
	}
	if got := document.Environment["EVA_WORKSPACE_ROOT"]; got != document.Workspace {
		t.Fatal("workspace environment not updated")
	}
	contents, err := os.ReadFile(record.PlanPath)
	if err != nil || strings.Contains(string(contents), "test-license") || strings.Contains(string(contents), "test-sudo") {
		t.Fatal("plan leaked raw config values")
	}
	original := document.Workspace
	updated, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	updated = []byte(strings.Replace(string(updated), "test-license", "changed-api-key", 1))
	if err := os.WriteFile(input, updated, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"plan", "--release", releaseRoot, "--config", input, "--output", filepath.Join(t.TempDir(), "plan.yaml")}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(input); err != nil {
		t.Fatal(err)
	}
	record.Status = operation.Failed
	if err := operation.Update(state, record); err != nil {
		t.Fatal(err)
	}
	retry, err := operation.Retry(state, record, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	retried, err := operation.LoadPlan(state, retry)
	if err != nil || retried.Workspace != original {
		t.Fatalf("retry did not preserve old generated workspace: %v", err)
	}
	values, err := os.ReadFile(filepath.Join(original, "site-values/app.yaml"))
	if err != nil || strings.Contains(string(values), "changed-api-key") {
		t.Fatal("retry input mutated after config edit")
	}
}

func TestInstallConfigUsesGeneratedWorkspaceBeforeRuntimeValidation(t *testing.T) {
	input := installerTestInput(t)
	missingRuntime := filepath.Join(t.TempDir(), "missing-runtime")
	err := run([]string{"install", writeRemotePublishRelease(t), "--config", input, "--runtime-root", missingRuntime, "--yes"})
	if err == nil || !strings.Contains(err.Error(), missingRuntime) {
		t.Fatalf("install did not reach Runtime validation with config input: %v", err)
	}
	if _, err := os.Stat(filepath.Join(defaultWorkspaceCacheRoot, "customer-a")); err != nil {
		t.Fatal("install did not generate its Workspace")
	}
}

func TestArgoCDPreflightAcceptsConfig(t *testing.T) {
	input := installerTestInput(t)
	previousPrecondition, previousHandoff := runArgoCDPrecondition, runArgoCDHandoff
	t.Cleanup(func() { runArgoCDPrecondition, runArgoCDHandoff = previousPrecondition, previousHandoff })
	var selected plan.Document
	runArgoCDPrecondition = func(_ apply.Options, document plan.Document) (string, error) {
		selected = document
		return document.ReleaseRoot, nil
	}
	handoff := false
	runArgoCDHandoff = func(plan.Document, string) error { handoff = true; return nil }
	if err := run([]string{"preflight", "argocd", "--config", input, "--release", writePreparableRelease(t), "--install-root", t.TempDir(), "--runtime-root", writeRetryRuntime(t)}); err != nil {
		t.Fatal(err)
	}
	if !handoff || !strings.HasPrefix(selected.Workspace, defaultWorkspaceCacheRoot+string(filepath.Separator)) {
		t.Fatal("Argo CD preflight did not use generated workspace")
	}
}
