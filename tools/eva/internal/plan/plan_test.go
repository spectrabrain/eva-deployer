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
	want := []string{"precondition", "infra", "gpu", "config", "iam", "agent", "vision", "app"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("step order = %v, want %v", got, want)
	}
	if document.GeneratedAt.Location() != time.UTC {
		t.Fatalf("GeneratedAt location = %s, want UTC", document.GeneratedAt.Location())
	}
}

func TestBuildOrdersScopedComponentPlans(t *testing.T) {
	tests := []struct {
		name       string
		components map[string]bool
		want       []string
	}{
		{name: "IAM only", components: map[string]bool{"iam": true}, want: []string{"precondition", "iam"}},
		{name: "Agent only", components: map[string]bool{"agent": true}, want: []string{"precondition", "gpu", "config", "agent"}},
		{name: "Vision only", components: map[string]bool{"vision": true}, want: []string{"precondition", "gpu", "config", "vision"}},
		{name: "App only", components: map[string]bool{"app": true}, want: []string{"precondition", "config", "app"}},
	}

	releaseResolved := release.Resolved{Root: "/releases/3.2.0"}
	releaseResolved.Metadata.Version = "3.2.0"
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspaceResolved := workspace.Resolved{
				SiteID: "customer-a",
				Root:   "/etc/eva/sites/customer-a",
				Config: workspace.Config{Components: test.components},
			}
			document := Build(workspaceResolved, releaseResolved, time.Now())
			got := make([]string, 0, len(document.Steps))
			for _, step := range document.Steps {
				got = append(got, step.Component)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("step order = %v, want %v", got, test.want)
			}
		})
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

func TestScopedStepsIntersectCLISelection(t *testing.T) {
	resolved := workspace.Resolved{Config: workspace.Config{
		Components: map[string]bool{"infra": true, "iam": true, "app": true, "agent": true},
		TargetComponents: map[string]map[string]bool{
			"infra-only":   {"infra": true},
			"iam-app":      {"infra": true, "iam": true, "app": true, "agent": true},
			"external-app": {"infra": true, "app": true},
		},
	}}
	for component, want := range map[string][]string{
		"app":   {"external-app", "iam-app"},
		"iam":   {"iam-app"},
		"agent": {"iam-app"},
		"infra": {"external-app", "iam-app", "infra-only"},
	} {
		selected, err := resolved.SelectComponents([]string{component})
		if err != nil {
			t.Fatal(err)
		}
		document := Build(selected, release.Resolved{}, time.Now())
		for _, step := range document.Steps {
			if !reflect.DeepEqual(step.Targets, want) {
				t.Fatalf("%s selection, %s targets = %v, want %v", component, step.Component, step.Targets, want)
			}
		}
		if !resolved.Config.Components["app"] || !resolved.Config.Components["infra"] {
			t.Fatal("component selection mutated source config")
		}
	}
}

func TestGPUPreflightIsOrderedAndScopedInEveryRepositoryMode(t *testing.T) {
	for _, mode := range []string{"online_repository", "remote_repository", "local_repository"} {
		t.Run(mode, func(t *testing.T) {
			resolved := workspace.Resolved{AnsibleMode: mode, Config: workspace.Config{
				Components: map[string]bool{"infra": true, "agent": true, "vision": true, "app": true},
				TargetComponents: map[string]map[string]bool{
					"gpu-a": {"infra": true, "agent": true},
					"gpu-b": {"vision": true},
					"app":   {"app": true},
					"infra": {"infra": true},
				},
			}}
			document := Build(resolved, release.Resolved{}, time.Now())
			if document.Steps[1].Component != "infra" || document.Steps[2].Component != "gpu" || document.Steps[3].Component != "config" {
				t.Fatal("GPU preparation must run after Infra and before Config")
			}
			if !reflect.DeepEqual(document.Steps[2].Targets, []string{"gpu-a", "gpu-b"}) {
				t.Fatalf("GPU target selection includes unrelated hosts: %v", document.Steps[2].Targets)
			}
			gpu, err := GPUPreflight(document)
			if err != nil {
				t.Fatal(err)
			}
			if !gpu.GPUPreflightOnly || len(gpu.Steps) != 2 || gpu.Steps[1].Component != "gpu" || !reflect.DeepEqual(gpu.Steps[0].Targets, gpu.Steps[1].Targets) {
				t.Fatalf("explicit GPU preparation must only check and prepare GPU hosts: %#v", gpu)
			}
			if !reflect.DeepEqual(gpu.GPUPreflightComponents, []string{"agent", "vision"}) {
				t.Fatal("GPU preparation lost its source component selection")
			}
			if !reflect.DeepEqual(document.Steps[0].Targets, []string{"app", "gpu-a", "gpu-b", "infra"}) {
				t.Fatal("GPU-only plan mutated the source installation plan")
			}
		})
	}
	if _, err := GPUPreflight(Build(workspace.Resolved{Config: workspace.Config{Components: map[string]bool{"app": true}}}, release.Resolved{}, time.Now())); err == nil {
		t.Fatal("GPU preparation accepted a config without GPU components")
	}
}
