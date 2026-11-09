package main

import (
	"os"
	"path/filepath"
	"testing"

	"eva-deployer/tools/eva/internal/argocd"
	"eva-deployer/tools/eva/internal/plan"
)

func TestArgoCDReceiptRootKeepsServerPathsAndUsesTheNKSAccount(t *testing.T) {
	previous := defaultArgoCDReceiptRoot
	defaultArgoCDReceiptRoot = argocd.DefaultReceiptRoot
	t.Cleanup(func() { defaultArgoCDReceiptRoot = previous })
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := argoCDReceiptRoot(plan.Document{}); got != argocd.DefaultReceiptRoot {
		t.Fatalf("on-premise receipt path changed: %s", got)
	}
	document := plan.Document{AnsibleExtraVars: []string{"eva_managed_cluster=true", "ansible_become=false"}}
	want := filepath.Join(home, ".local/state/eva/sites")
	if os.Geteuid() == 0 {
		want = argocd.DefaultReceiptRoot
	}
	if got := argoCDReceiptRoot(document); got != want {
		t.Fatalf("NKS receipt root = %s, want %s", got, want)
	}
	defaultArgoCDReceiptRoot = t.TempDir()
	if got := argoCDReceiptRoot(document); got != defaultArgoCDReceiptRoot {
		t.Fatalf("explicit receipt root was overridden: %s", got)
	}
}
