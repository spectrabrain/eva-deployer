package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"eva-deployer/tools/eva/internal/operation"
	"eva-deployer/tools/eva/internal/plan"
)

func TestCheckPrintsLiveAccessURLs(t *testing.T) {
	runtimeRoot := writeRetryRuntime(t)
	writeAccessFixture(t, filepath.Join(runtimeRoot, "bin/systemctl"), "#!/bin/sh\necho active\n", 0o755)
	t.Setenv("PATH", filepath.Join(runtimeRoot, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	writeAccessFixture(t, filepath.Join(runtimeRoot, "bin/kubectl"), `#!/bin/sh
case "$*" in
  'get nodes -o json')
    echo '{"items":[{"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}' ;;
  *' -n eva-app '*)
    echo '{"items":[{"kind":"Deployment","spec":{"replicas":1},"status":{"readyReplicas":1}},{"kind":"ConfigMap","data":{"BACKEND_HOST":"10.159.56.197","BACKEND_SECURE":"true","BACKEND_SECURE_PORT":"32443"}},{"kind":"Ingress","spec":{"rules":[{"http":{"paths":[{"path":"/"}]}}]}}]}' ;;
  *' -n eva-iam '*)
    echo '{"items":[{"kind":"Deployment","spec":{"replicas":1},"status":{"readyReplicas":1}},{"kind":"ConfigMap","data":{"ISSUER":"https://10.159.56.197/iam/realms/eva-iam"}}]}' ;;
  *' -n n8n '*)
    echo '{"items":[{"kind":"Deployment","spec":{"replicas":1,"template":{"spec":{"containers":[{"env":[{"name":"N8N_EDITOR_BASE_URL","value":"http://10.186.0.75:30678/"}]}]}}},"status":{"readyReplicas":1}}]}' ;;
  *) exit 1 ;;
esac
`, 0o755)
	stateRoot, _, _ := accessOperationFixture(t)
	output := captureRemoteTargetStdout(t, func() error {
		return runCheck([]string{"--state-root", stateRoot, "--runtime-root", runtimeRoot})
	})
	for _, expected := range []string{"[OK] EVA installation is healthy", "Access URLs:", "https://10.159.56.197:32443/", "https://10.159.56.197/iam/", "http://10.159.56.124:32080/", "http://10.186.0.75:30678/"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("check output missing %q:\n%s", expected, output)
		}
	}
}

func TestCompletedInstallationPrintsAccessURLsOnlyOnSuccess(t *testing.T) {
	stateRoot, record, document := accessOperationFixture(t)
	writeAccessFixture(t, filepath.Join(document.Workspace, "inventory/host_vars/server-a.yaml"), "ansible_host: 10.186.0.75\nansible_connection: ssh\n", 0o600)
	writeAccessFixture(t, filepath.Join(document.ReleaseRoot, "out/work/rendered/customer-a/10.186.0.75/app/resolved-values.yaml"), "app:\n  backendHost: 10.186.0.75\n  backendPublicPort: 30080\n", 0o600)
	writeAccessFixture(t, filepath.Join(document.ReleaseRoot, "out/work/rendered/customer-a/10.186.0.75/iam/resolved-values.yaml"), "config:\n  host: 10.186.0.75\n  publicPort: 30081\ningress:\n  path: /iam\n", 0o600)
	writeAccessFixture(t, filepath.Join(document.ReleaseRoot, "out/work/config/customer-a/10.186.0.75/n8n.yaml"), "endpoint: http://10.186.0.75:30678\n", 0o600)
	output := captureRemoteTargetStdout(t, func() error {
		printCompletedAccess(record, stateRoot, t.TempDir())
		return nil
	})
	for _, expected := range []string{"http://10.186.0.75:30080/", "http://10.186.0.75:30081/iam/", "http://10.159.56.124:32080/", "http://10.186.0.75:30678/"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("installation output missing %q:\n%s", expected, output)
		}
	}
	record.Status = operation.Failed
	output = captureRemoteTargetStdout(t, func() error {
		printCompletedAccess(record, stateRoot, t.TempDir())
		return nil
	})
	if output != "" {
		t.Fatalf("failed installation printed success addresses: %q", output)
	}
}

func accessOperationFixture(t *testing.T) (string, operation.Record, plan.Document) {
	t.Helper()
	previousReceipt := defaultRemoteBootstrapReceiptPath
	t.Cleanup(func() { defaultRemoteBootstrapReceiptPath = previousReceipt })
	defaultRemoteBootstrapReceiptPath = filepath.Join(t.TempDir(), "harbor.yaml")
	writeAccessFixture(t, defaultRemoteBootstrapReceiptPath, "registry: 10.159.56.124:32080\nprotocol: http\n", 0o600)
	document := plan.Document{SchemaVersion: plan.SchemaVersion, SiteID: "customer-a", Workspace: t.TempDir(), ReleaseRoot: t.TempDir(), RepositoryMode: "remote_repository", Repository: "10.159.56.124:32080", Steps: []plan.Step{{Component: "iam"}, {Component: "app"}, {Component: "n8n"}}}
	stateRoot := t.TempDir()
	record, err := operation.Create(stateRoot, document, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	record.Status = operation.Succeeded
	if err := operation.Update(stateRoot, record); err != nil {
		t.Fatal(err)
	}
	return stateRoot, record, document
}

func writeAccessFixture(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}
