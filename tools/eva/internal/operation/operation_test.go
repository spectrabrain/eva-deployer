package operation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"eva-deployer/tools/eva/internal/fieldoverride"
	"eva-deployer/tools/eva/internal/plan"
	"gopkg.in/yaml.v3"
)

func TestCreateAndLoad(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 12, 1, 2, 3, 0, time.UTC)
	record, err := Create(root, testPlan(), now)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if record.Status != Planned || record.SiteID != "customer-a" {
		t.Fatalf("unexpected operation record: %#v", record)
	}
	for _, path := range []string{record.PlanPath, filepath.Join(root, record.ID, "operation.yaml")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("permissions for %s = %o, want 600", path, got)
		}
	}

	loaded, err := Load(root, record.ID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded != record {
		t.Fatalf("Load() = %#v, want %#v", loaded, record)
	}

	contents, err := os.ReadFile(record.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	var savedPlan plan.Document
	if err := yaml.Unmarshal(contents, &savedPlan); err != nil {
		t.Fatalf("unmarshal saved plan: %v", err)
	}
	if savedPlan.OperationID != record.ID {
		t.Fatalf("saved plan operation ID = %q, want %q", savedPlan.OperationID, record.ID)
	}
}

func TestCreateStagesPrivateOverrideInputs(t *testing.T) {
	root := t.TempDir()
	inputRoot := t.TempDir()
	valuesPath := filepath.Join(inputRoot, "app.yaml")
	if err := os.WriteFile(valuesPath, []byte("token: very-secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	overrides, err := fieldoverride.Parse(
		nil, []string{"app=" + valuesPath}, []string{"app:replicaCount=2", "app:api.token=very-secret-value"}, map[string]bool{"app": true},
	)
	if err != nil {
		t.Fatal(err)
	}
	document := testPlan()
	document.Overrides = overrides.Public()
	document.OverrideInputs = overrides
	record, err := Create(root, document, time.Date(2026, 9, 12, 1, 2, 3, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !record.HasOverrides {
		t.Fatal("operation does not identify field overrides")
	}
	planContents, err := os.ReadFile(record.PlanPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(planContents), "very-secret-value") {
		t.Fatalf("plan leaks --set value: %s", planContents)
	}
	loaded, err := LoadPlan(root, record)
	if err != nil {
		t.Fatal(err)
	}
	stagedValues := loaded.Overrides["app"].Values.StagedPath
	contents, err := os.ReadFile(stagedValues)
	if err != nil || string(contents) != "token: very-secret-value\n" {
		t.Fatalf("staged values = %q, %v", contents, err)
	}
	for path, wantMode := range map[string]os.FileMode{
		stagedValues:                            0o644,
		loaded.Overrides["app"].AnsibleVarsPath: 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != wantMode {
			t.Fatalf("permissions for %s = %o, want %o", path, got, wantMode)
		}
	}
	variables, err := os.ReadFile(loaded.Overrides["app"].AnsibleVarsPath)
	if err != nil || !strings.Contains(string(variables), "very-secret-value") {
		t.Fatalf("private override variables = %q, %v", variables, err)
	}
}

func TestLatestUsesUpdatedAt(t *testing.T) {
	root := t.TempDir()
	first, err := Create(root, testPlan(), time.Date(2026, 9, 12, 1, 2, 3, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	secondPlan := testPlan()
	secondPlan.SiteID = "customer-b"
	second, err := Create(root, secondPlan, time.Date(2026, 9, 12, 1, 2, 4, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	latest, err := Latest(root)
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID != second.ID || latest.ID == first.ID {
		t.Fatalf("Latest() = %#v, want %q", latest, second.ID)
	}
}

func TestRetryClonesFailedOperationAndStagedOverrides(t *testing.T) {
	root := t.TempDir()
	inputRoot := t.TempDir()
	valuesPath := filepath.Join(inputRoot, "app.yaml")
	if err := os.WriteFile(valuesPath, []byte("token: retry-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	overrides, err := fieldoverride.Parse(
		nil, []string{"app=" + valuesPath}, []string{"app:api.token=retry-secret"}, map[string]bool{"app": true},
	)
	if err != nil {
		t.Fatal(err)
	}
	document := testPlan()
	document.Overrides = overrides.Public()
	document.OverrideInputs = overrides
	source, err := Create(root, document, time.Date(2026, 9, 12, 1, 2, 3, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	source.Status = Failed
	source.CompletedAt = time.Date(2026, 9, 12, 1, 3, 0, 0, time.UTC)
	if err := Update(root, source); err != nil {
		t.Fatal(err)
	}

	retry, err := Retry(root, source, time.Date(2026, 9, 12, 1, 4, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Retry() error = %v", err)
	}
	if retry.ID == source.ID || retry.Status != Planned || retry.SourceOperationID != source.ID {
		t.Fatalf("retry record = %#v", retry)
	}
	loadedSource, err := Load(root, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loadedSource.Status != Failed || loadedSource.RetryOperationID != retry.ID {
		t.Fatalf("source after retry = %#v", loadedSource)
	}

	sourcePlan, err := LoadPlan(root, source)
	if err != nil {
		t.Fatal(err)
	}
	retryPlan, err := LoadPlan(root, retry)
	if err != nil {
		t.Fatal(err)
	}
	if retryPlan.OperationID != retry.ID || retryPlan.GeneratedAt != sourcePlan.GeneratedAt {
		t.Fatalf("retry plan = %#v", retryPlan)
	}
	sourceValues := sourcePlan.Overrides["app"].Values.StagedPath
	retryOverride := retryPlan.Overrides["app"]
	retryValues := retryOverride.Values.StagedPath
	if sourceValues == retryValues || !strings.Contains(retryValues, retry.ID) {
		t.Fatalf("retry staged values = %q, source = %q", retryValues, sourceValues)
	}
	contents, err := os.ReadFile(retryValues)
	if err != nil || string(contents) != "token: retry-secret\n" {
		t.Fatalf("retry staged values = %q, %v", contents, err)
	}
	variables, err := os.ReadFile(retryOverride.AnsibleVarsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(variables), "retry-secret") || !strings.Contains(string(variables), retryValues) || strings.Contains(string(variables), sourceValues) {
		t.Fatalf("retry variables = %q", variables)
	}
	for _, path := range []string{retry.PlanPath, retryOverride.AnsibleVarsPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("permissions for %s = %o, want 600", path, info.Mode().Perm())
		}
	}
}

func TestRetryRejectsNonFailedOperation(t *testing.T) {
	root := t.TempDir()
	source, err := Create(root, testPlan(), time.Date(2026, 9, 12, 1, 2, 3, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Retry(root, source, time.Date(2026, 9, 12, 1, 3, 0, 0, time.UTC)); err == nil || !strings.Contains(err.Error(), "only failed") {
		t.Fatalf("Retry() error = %v, want failed status error", err)
	}
	loaded, err := Load(root, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RetryOperationID != "" {
		t.Fatalf("source retry relationship = %q, want empty", loaded.RetryOperationID)
	}
}

func testPlan() plan.Document {
	return plan.Document{
		SchemaVersion:  plan.SchemaVersion,
		SiteID:         "customer-a",
		ReleaseVersion: "3.2.0",
	}
}
