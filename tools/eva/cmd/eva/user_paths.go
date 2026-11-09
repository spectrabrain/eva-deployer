package main

import (
	"os"
	"path/filepath"

	"eva-deployer/tools/eva/internal/apply"
	"eva-deployer/tools/eva/internal/argocd"
	"eva-deployer/tools/eva/internal/operation"
	"eva-deployer/tools/eva/internal/plan"
)

func userPath(system, relative string) string {
	if os.Geteuid() == 0 {
		return system
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return system
	}
	return filepath.Join(home, relative)
}
func userStateRoot() string { return userPath(operation.DefaultRoot, ".local/state/eva/operations") }
func userLogRoot() string   { return userPath(apply.DefaultLogRoot, ".local/state/eva/logs") }
func userArgoCDReceiptRoot() string {
	if defaultArgoCDReceiptRoot != argocd.DefaultReceiptRoot {
		return defaultArgoCDReceiptRoot
	}
	return userPath(defaultArgoCDReceiptRoot, ".local/state/eva/sites")
}
func argoCDReceiptRoot(document plan.Document) string {
	for _, setting := range document.AnsibleExtraVars {
		if setting == "eva_managed_cluster=true" {
			return userArgoCDReceiptRoot()
		}
	}
	return defaultArgoCDReceiptRoot
}
func userWorkspaceCacheRoot() string {
	if defaultWorkspaceCacheRoot != "/var/lib/eva/workspaces" {
		return defaultWorkspaceCacheRoot
	}
	return userPath(defaultWorkspaceCacheRoot, ".local/share/eva/workspaces")
}
