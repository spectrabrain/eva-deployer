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
