package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"eva-deployer/tools/eva/internal/workspace"
)

var gpuPreflightHome = func() (string, error) {
	return gpuPreflightInvokingHome(os.Geteuid(), os.Getenv("SUDO_USER"))
}

// sudo commonly changes HOME to /root. Use the account that created the
// installer scaffold rather than looking for another user's config.
func gpuPreflightInvokingHome(euid int, sudoUser string) (string, error) {
	var home string
	if euid == 0 && sudoUser != "" {
		account, err := user.Lookup(sudoUser)
		if err != nil {
			return "", fmt.Errorf("resolve GPU preflight invoking user: %w", err)
		}
		home = account.HomeDir
	} else {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve GPU preflight home: %w", err)
		}
	}
	if !filepath.IsAbs(home) {
		return "", errors.New("GPU preflight user home must be an absolute path")
	}
	return home, nil
}

func gpuPreflightWorkspaceInput(options workspace.Options) (workspace.Options, error) {
	if options.ConfigFile != "" || options.SiteID != "" || options.Workspace != "" {
		return options, nil
	}
	home, err := gpuPreflightHome()
	if err != nil {
		return options, err
	}
	options.ConfigFile = filepath.Join(home, "eva-install.yaml")
	info, err := os.Lstat(options.ConfigFile)
	if errors.Is(err, os.ErrNotExist) {
		return options, fmt.Errorf("GPU preflight 설정 파일이 없습니다: %s; eva install configure로 작성하거나 --config FILE로 지정하세요", options.ConfigFile)
	}
	if err != nil {
		return options, fmt.Errorf("inspect default GPU preflight config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return options, fmt.Errorf("default GPU preflight config must be a regular file: %s", options.ConfigFile)
	}
	// LoadInstaller still performs the normal private-file and YAML validation.
	fmt.Printf("[INFO] GPU preflight config=%s\n", options.ConfigFile)
	return options, nil
}
