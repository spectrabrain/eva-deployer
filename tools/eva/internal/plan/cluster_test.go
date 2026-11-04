package plan

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"eva-deployer/tools/eva/internal/release"
	"eva-deployer/tools/eva/internal/workspace"
)

func TestManagedClusterUsesExistingSolutionPlaybooksAndPinnedWorkspace(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "kubeconfig")
	raw := `apiVersion: v1
kind: Config
current-context: unrelated
contexts:
  - name: nks
    context: {cluster: nks, user: ncp}
  - name: unrelated
    context: {cluster: other, user: other}
clusters:
  - name: nks
    cluster: {server: "https://nks.example.test", certificate-authority: ca.crt}
users:
  - name: ncp
    user:
      exec: {apiVersion: client.authentication.k8s.io/v1beta1, command: /home/eva/bin/ncp-iam-authenticator}
`
	if err := os.WriteFile(original, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	input := workspace.Installer{SchemaVersion: "v1"}
	input.Site.ID = "nks-test"
	input.Repository.Mode = "online"
	input.Deployment = &workspace.Deployment{Environment: "nks", Kubeconfig: original, Context: "nks"}
	input.Components = map[string]bool{"infra": false, "iam": true, "agent": true, "vision": true, "app": true}
	input.Targets = map[string]workspace.InstallerTarget{"controller": {Ansible: map[string]any{"ansible_connection": "local"}}}
	resolved, err := input.Materialize(filepath.Join(root, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	document := Build(resolved, release.Resolved{}, time.Now())
	var components []string
	for _, step := range document.Steps {
		components = append(components, step.Component)
	}
	if !reflect.DeepEqual(components, []string{"precondition", "config", "iam", "agent", "vision", "app"}) {
		t.Fatalf("steps=%v", components)
	}
	if document.ExecutionMode != "" {
		t.Fatal("managed cluster must use common Apply, not an independent deployment engine")
	}
	if document.Steps[2].Playbook != "src/solution/playbooks/site_eva_iam.yaml" {
		t.Fatal("Solution role bypassed")
	}
	pinned := document.Environment["KUBECONFIG"]
	if pinned == "" || pinned == original {
		t.Fatal("kubeconfig not snapshotted")
	}
	contents, err := os.ReadFile(pinned)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "current-context: nks") || strings.Contains(string(contents), "unrelated") || !strings.Contains(string(contents), filepath.Join(root, "ca.crt")) {
		t.Fatalf("unexpected snapshot: %s", contents)
	}
	if err := os.WriteFile(original, []byte("current-context: other\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Resolve(workspace.Options{Workspace: resolved.Root}); err != nil {
		t.Fatalf("retry must not re-read original config: %v", err)
	}
	for _, invalid := range []string{"infra", "n8n"} {
		input.Components[invalid] = true
		if _, err := input.Materialize(filepath.Join(root, "invalid-cache")); err == nil {
			t.Fatalf("managed cluster accepted %s", invalid)
		}
		input.Components[invalid] = false
	}
}
