package operation

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

func testPlan() plan.Document {
	return plan.Document{
		SchemaVersion:  plan.SchemaVersion,
		SiteID:         "customer-a",
		ReleaseVersion: "3.2.0",
	}
}
