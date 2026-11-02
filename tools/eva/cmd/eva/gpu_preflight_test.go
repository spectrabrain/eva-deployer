package main

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"

	"eva-deployer/tools/eva/internal/workspace"
)

func TestGPUPreflightInvokingHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := gpuPreflightInvokingHome(1000, "")
	if err != nil || got != home {
		t.Fatalf("ordinary user home = %q: %v", got, err)
	}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	got, err = gpuPreflightInvokingHome(0, account.Username)
	if err != nil || got != account.HomeDir {
		t.Fatalf("sudo must use the original account home, got %q: %v", got, err)
	}
	if _, err := gpuPreflightInvokingHome(0, "eva-no-such-preflight-user"); err == nil {
		t.Fatal("unknown sudo account accepted")
	}
	t.Setenv("HOME", "relative/home")
	if _, err := gpuPreflightInvokingHome(1000, ""); err == nil {
		t.Fatal("relative home accepted")
	}
}

func TestGPUPreflightDefaultInputRequiresAnInstallerConfig(t *testing.T) {
	home := t.TempDir()
	previous := gpuPreflightHome
	t.Cleanup(func() { gpuPreflightHome = previous })
	gpuPreflightHome = func() (string, error) { return home, nil }
	if _, err := gpuPreflightWorkspaceInput(workspace.Options{}); err == nil {
		t.Fatal("missing default config accepted")
	}
	path := filepath.Join(home, "eva-install.yaml")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := gpuPreflightWorkspaceInput(workspace.Options{}); err == nil {
		t.Fatal("default config directory accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("schema_version: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	options, err := gpuPreflightWorkspaceInput(workspace.Options{CacheRoot: "/test-cache"})
	if err != nil || options.ConfigFile != path || options.CacheRoot != "/test-cache" {
		t.Fatalf("default input = %#v: %v", options, err)
	}
}

func TestGPUPreflightExplicitInputSkipsHomeLookup(t *testing.T) {
	previous := gpuPreflightHome
	t.Cleanup(func() { gpuPreflightHome = previous })
	gpuPreflightHome = func() (string, error) { t.Fatal("explicit input must not look up a default config"); return "", nil }
	for _, options := range []workspace.Options{{ConfigFile: "/custom/config.yaml"}, {SiteID: "site-a"}, {Workspace: "/custom/workspace"}} {
		got, err := gpuPreflightWorkspaceInput(options)
		if err != nil || got != options {
			t.Fatalf("explicit input changed: %#v %v", got, err)
		}
	}
}
