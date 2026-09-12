package plan

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"eva-deployer/tools/eva/internal/release"
	"eva-deployer/tools/eva/internal/workspace"
)

func TestBuildOrdersSelectedComponentsAndConfig(t *testing.T) {
	workspaceResolved := workspace.Resolved{
		SiteID:      "customer-a",
		Root:        "/etc/eva/sites/customer-a",
		AnsibleMode: "remote_repository",
		Config: workspace.Config{Components: map[string]bool{
			"infra":  true,
			"iam":    true,
			"agent":  true,
			"vision": true,
			"app":    true,
		}},
	}
	workspaceResolved.Config.Repository.Registry = "harbor.customer.example:32080"
	workspaceResolved.Config.Repository.Project = "eva"
	releaseResolved := release.Resolved{Root: "/releases/3.2.0"}
	releaseResolved.Metadata.Version = "3.2.0"

	document := Build(workspaceResolved, releaseResolved, time.Date(2026, 9, 12, 1, 2, 3, 0, time.UTC))
	got := make([]string, 0, len(document.Steps))
	for _, step := range document.Steps {
		got = append(got, step.Component)
	}
	want := []string{"infra", "config", "iam", "agent", "vision", "app"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("step order = %v, want %v", got, want)
	}
	if document.GeneratedAt.Location() != time.UTC {
		t.Fatalf("GeneratedAt location = %s, want UTC", document.GeneratedAt.Location())
	}
}

func TestWriteUsesOwnerOnlyPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "plan.yaml")
	if err := Write(path, []byte("schema_version: v1\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("plan permissions = %o, want 600", got)
	}
}
