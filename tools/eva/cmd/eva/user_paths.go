package main

import (
	"os"
	"path/filepath"

	"eva-deployer/tools/eva/internal/apply"
	"eva-deployer/tools/eva/internal/operation"
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
func userWorkspaceCacheRoot() string {
	if defaultWorkspaceCacheRoot != "/var/lib/eva/workspaces" {
		return defaultWorkspaceCacheRoot
	}
	return userPath(defaultWorkspaceCacheRoot, ".local/share/eva/workspaces")
}
