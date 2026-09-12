package workspace

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestResolveExternalWorkspace(t *testing.T) {
	root := writeWorkspace(t, `
site:
  id: customer-a
repository:
  mode: remote
  registry: harbor.customer.example:32080
components:
  infra: true
  app: true
`)

	resolved, err := Resolve(Options{Workspace: root})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.SiteID != "customer-a" || resolved.AnsibleMode != "remote_repository" {
		t.Fatalf("unexpected resolved workspace: %#v", resolved)
	}
	want := []string{
		"eva_workspace_root=" + root,
		"eva_site_id=customer-a",
		"repository_mode=remote_repository",
		"repository_registry=harbor.customer.example:32080",
		"repository_project=eva",
	}
	if got := resolved.AnsibleExtraVars(); !reflect.DeepEqual(got, want) {
		t.Fatalf("AnsibleExtraVars() = %#v, want %#v", got, want)
	}
	if got, want := resolved.Environment(), map[string]string{
		"EVA_SITE_ID":        "customer-a",
		"EVA_WORKSPACE_ROOT": root,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Environment() = %#v, want %#v", got, want)
	}
}

func TestResolveRejectsMismatchedSite(t *testing.T) {
	root := writeWorkspace(t, `
site:
  id: customer-a
repository:
  mode: cloud
components:
  app: true
`)
	if _, err := Resolve(Options{SiteID: "customer-b", Workspace: root}); err == nil {
		t.Fatal("Resolve() accepted mismatched site ID")
	}
}

func TestResolveRejectsLoopbackRegistry(t *testing.T) {
	root := writeWorkspace(t, `
site:
  id: customer-a
repository:
  mode: local
  registry: localhost:32080
components:
  infra: true
`)
	if _, err := Resolve(Options{Workspace: root}); err == nil {
		t.Fatal("Resolve() accepted loopback registry")
	}
}

func TestResolveRequiresSiteForDefaultWorkspace(t *testing.T) {
	if _, err := Resolve(Options{}); err == nil {
		t.Fatal("Resolve() accepted an implicit default workspace")
	}
}

func TestResolveRequiresEnabledComponent(t *testing.T) {
	root := writeWorkspace(t, `
site:
  id: customer-a
repository:
  mode: cloud
components:
  app: false
`)
	if _, err := Resolve(Options{Workspace: root}); err == nil {
		t.Fatal("Resolve() accepted a workspace without enabled components")
	}
}

func TestSelectComponentsScopesToEnabledComponent(t *testing.T) {
	resolved := Resolved{Config: Config{Components: map[string]bool{
		"infra": true,
		"app":   true,
		"n8n":   false,
	}}}

	selected, err := resolved.SelectComponents([]string{"app"})
	if err != nil {
		t.Fatalf("SelectComponents() error = %v", err)
	}
	want := map[string]bool{"infra": false, "app": true, "n8n": false}
	if !reflect.DeepEqual(selected.Config.Components, want) {
		t.Fatalf("selected components = %#v, want %#v", selected.Config.Components, want)
	}
	if !resolved.Config.Components["infra"] {
		t.Fatal("SelectComponents() mutated the original workspace")
	}
}

func TestSelectComponentsRejectsDisabledAndInvalidSelections(t *testing.T) {
	resolved := Resolved{Config: Config{Components: map[string]bool{"app": true, "n8n": false}}}
	for _, requested := range [][]string{{"n8n"}, {"unknown"}, {"all", "app"}, {"app", "app"}} {
		if _, err := resolved.SelectComponents(requested); err == nil {
			t.Fatalf("SelectComponents(%v) succeeded", requested)
		}
	}
}

func TestSelectComponentsAllKeepsEnabledComponents(t *testing.T) {
	resolved := Resolved{Config: Config{Components: map[string]bool{"infra": true, "app": true, "n8n": false}}}
	selected, err := resolved.SelectComponents([]string{"all"})
	if err != nil {
		t.Fatalf("SelectComponents() error = %v", err)
	}
	if !reflect.DeepEqual(selected.Config.Components, resolved.Config.Components) {
		t.Fatalf("selected components = %#v, want %#v", selected.Config.Components, resolved.Config.Components)
	}
}

func writeWorkspace(t *testing.T, content string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "site-values")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "site.yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}
