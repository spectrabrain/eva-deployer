package argocd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"eva-deployer/tools/eva/internal/workspace"
)

func TestTargetDetectionDoesNotCombineIndependentClustersOrStaleReports(t *testing.T) {
	root := t.TempDir()
	writeReport(t, root, testWorkspaceSite, "host-a", "ok", []string{"legacy-a-eva-app"})
	writeReport(t, root, testWorkspaceSite, "host-b", "ok", []string{"legacy-b-eva-app"})
	writeReport(t, root, testWorkspaceSite, "stale", "error", nil)
	detection, err := LoadTargetDetection(root, testWorkspaceSite, "host-a")
	if err != nil || !reflect.DeepEqual(detection.Applications, []string{"legacy-a-eva-app"}) {
		t.Fatalf("target detection: %#v %v", detection, err)
	}
	if _, err := LoadTargetDetection(root, testWorkspaceSite, "missing"); !os.IsNotExist(err) {
		t.Fatalf("missing report accepted: %v", err)
	}
	if _, err := LoadTargetDetection(root, testWorkspaceSite, "../host-a"); err == nil {
		t.Fatal("invalid target report path accepted")
	}
	writeReport(t, root, testWorkspaceSite, "host-a", "", nil)
	if _, err := LoadTargetDetection(root, testWorkspaceSite, "host-a"); err == nil {
		t.Fatal("report with missing tracking status accepted")
	}
}

func TestTargetStateSeparatesReceiptsAndPendingWithNormalizedSSHPort(t *testing.T) {
	root := t.TempDir()
	target := workspace.InstallerTarget{Ansible: map[string]any{"ansible_host": "192.0.2.10"}}
	want := TargetStateRoot(root, "node-a", target)
	for _, port := range []any{nil, 22, "22", float64(22)} {
		target.Ansible["ansible_port"] = port
		if got := TargetStateRoot(root, "node-a", target); got != want {
			t.Fatalf("equivalent SSH port moved state: %s != %s", got, want)
		}
	}
	if TargetStateRoot(root, "node-b", target) == want {
		t.Fatal("different target alias shared receipt and pending state")
	}
	target.Ansible["ansible_port"] = 2222
	if TargetStateRoot(root, "node-a", target) == want {
		t.Fatal("different SSH port inherited state")
	}
	target.Ansible["ansible_port"], target.Ansible["ansible_host"] = 22, "192.0.2.11"
	if TargetStateRoot(root, "node-a", target) == want {
		t.Fatal("different target address inherited state")
	}
	if ReceiptPath(want, testWorkspaceSite) == ReceiptPath(root, testWorkspaceSite) || filepath.Dir(PendingPath(want, testWorkspaceSite)) != filepath.Dir(ReceiptPath(want, testWorkspaceSite)) {
		t.Fatal("receipt/pending state isolation is inconsistent")
	}
}
