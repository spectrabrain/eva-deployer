package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"eva-deployer/tools/eva/internal/operation"
	"eva-deployer/tools/eva/internal/release"
	"eva-deployer/tools/eva/internal/remote"
)

func TestBareGPUPreflightIgnoresInstallerConfigAndUsesCurrentServer(t *testing.T) {
	for _, test := range []struct {
		name, config    string
		explicitRelease bool
	}{
		{"invalid-home-config", "invalid: [yaml", true},
		{"ssh-home-config", "schema_version: v1\nrepository:\n  mode: remote\ntargets:\n  ssh-target:\n    ansible:\n      ansible_host: 192.0.2.10\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			if err := os.WriteFile(filepath.Join(home, "eva-install.yaml"), []byte(test.config), 0o600); err != nil {
				t.Fatal(err)
			}
			previousCache, previousStat := defaultWorkspaceCacheRoot, stdinStat
			defaultWorkspaceCacheRoot = filepath.Join(t.TempDir(), "workspaces")
			t.Cleanup(func() { defaultWorkspaceCacheRoot, stdinStat = previousCache, previousStat })
			stdinStat = func() (os.FileInfo, error) { t.Fatal("preflight must not prompt"); return nil, nil }
			mockRemoteInstaller(t, func(remote.TargetAction) error { t.Fatal("bare preflight must not dispatch SSH targets"); return nil })
			runtimeRoot := writeShellRuntime(t)
			if err := os.WriteFile(filepath.Join(runtimeRoot, "venv/bin/ansible-playbook"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			stateRoot := t.TempDir()
			args := []string{"preflight", "gpu", "--install-root", t.TempDir(), "--runtime-root", runtimeRoot, "--state-root", stateRoot, "--log-root", t.TempDir()}
			releaseRoot := writePreparableRelease(t)
			if test.explicitRelease {
				args = append(args, "--release", releaseRoot)
			} else {
				previousReceipt := defaultCurrentReleaseReceiptPath
				t.Cleanup(func() { defaultCurrentReleaseReceiptPath = previousReceipt })
				defaultCurrentReleaseReceiptPath = filepath.Join(t.TempDir(), "current.yaml")
				source, err := release.Resolve(releaseRoot)
				if err != nil {
					t.Fatal(err)
				}
				if err := release.WriteCurrentReceipt(defaultCurrentReleaseReceiptPath, source, "eva-tool-installer", time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			output := captureRemoteTargetStdout(t, func() error {
				return run(args)
			})
			if !strings.Contains(output, "target=current-server (local)") || !strings.Contains(output, "--limit\ncurrent-server") || strings.Contains(output, "site_precondition.yaml") {
				t.Fatalf("wrong execution: %s", output)
			}
			records, err := operation.List(stateRoot, "gpu-current-host")
			if err != nil || len(records) != 1 || records[0].Status != operation.Succeeded {
				t.Fatalf("operation not recorded: %v %v", records, err)
			}
			document, err := operation.LoadPlan(stateRoot, records[0])
			if err != nil {
				t.Fatal(err)
			}
			if !document.GPUPreflightOnly || document.ExecutionMode != "" || len(document.Steps) != 1 || document.Steps[0].Component != "gpu" {
				t.Fatalf("wrong local plan: %#v", document)
			}
			inventory, err := os.ReadFile(filepath.Join(document.Workspace, "inventory/inventory.ini"))
			if err != nil || string(inventory) != "[eva]\ncurrent-server ansible_connection=local ansible_become=true\n" {
				t.Fatalf("wrong inventory: %q %v", inventory, err)
			}
			for _, path := range []string{document.Workspace, filepath.Join(document.Workspace, "inventory/inventory.ini")} {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm()&0o077 != 0 {
					t.Fatalf("workspace is not private: %s %v", path, err)
				}
			}
		})
	}
}

func TestGPUPreflightHostSelectionRequiresConfig(t *testing.T) {
	for _, args := range [][]string{{"--site", "remote-a"}, {"--workspace", "/custom/workspace"}, {"--component", "agent"}} {
		if err := run(append([]string{"preflight", "gpu"}, args...)); err == nil {
			t.Fatalf("host selection without --config accepted: %v", args)
		}
	}
}

func TestCurrentHostGPUSupply(t *testing.T) {
	source := release.Resolved{Root: t.TempDir()}
	mode, _, _, err := currentHostGPUSupply(source)
	if err != nil || mode != "online" {
		t.Fatalf("ordinary release: %s %v", mode, err)
	}
	source.Metadata.Artifacts = append(source.Metadata.Artifacts, release.Artifact{Name: "eva-offline"})
	mode, _, _, err = currentHostGPUSupply(source)
	if err != nil || mode != "local" {
		t.Fatalf("offline release: %s %v", mode, err)
	}
	if err := os.WriteFile(filepath.Join(source.Root, ".eva-remote-release"), []byte("invalid: marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := currentHostGPUSupply(source); err == nil {
		t.Fatal("invalid Remote delivery fell back to local assets")
	}
}

func TestCurrentHostGPUPlanRejectsSymlinkAncestor(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := currentHostGPUPlan(release.Resolved{}, "online", "", filepath.Join(root, "link", "cache")); err == nil {
		t.Fatal("symlink ancestor accepted")
	}
}
