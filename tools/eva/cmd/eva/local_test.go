package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"eva-deployer/tools/eva/internal/airgap"
	"eva-deployer/tools/eva/internal/release"
)

func TestRunLocalPrepareUsesConfigAndCurrentRelease(t *testing.T) {
	root := writeRemotePublishRelease(t)
	resolved, err := release.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	previousPrepare, previousReceipt := prepareLocalRepository, defaultCurrentReleaseReceiptPath
	t.Cleanup(func() { prepareLocalRepository, defaultCurrentReleaseReceiptPath = previousPrepare, previousReceipt })
	defaultCurrentReleaseReceiptPath = filepath.Join(t.TempDir(), "current.yaml")
	if err := release.WriteCurrentReceipt(defaultCurrentReleaseReceiptPath, resolved, "eva-tool-installer", time.Now()); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "install.yaml")
	if err := os.WriteFile(config, []byte("schema_version: v1\nsite:\n  id: local-a\nrepository:\n  mode: local\n  registry: harbor.example.test:32080\n  project: eva\ncomponents:\n  infra: true\ntargets:\n  server-a:\n    ansible:\n      ansible_connection: local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	prepareLocalRepository = func(_ context.Context, options airgap.LocalOptions) (string, error) {
		called = true
		if options.Release.Root != root || options.Config.Repository.Registry != "harbor.example.test:32080" || !options.Yes || options.RequestCredential == nil {
			t.Fatal("Local CLI dropped selected config, Release or approval")
		}
		return root, nil
	}
	if err := run([]string{"local", "prepare", "--config", config, "--yes"}); err != nil || !called {
		t.Fatalf("Local CLI: called=%t error=%v", called, err)
	}
	called = false
	if err := run([]string{"local", "prepare", "--config", config}); err == nil || called {
		t.Fatal("Local preparation started without approval")
	}
}

func TestRunLocalHelpAndInvalidArguments(t *testing.T) {
	for _, args := range [][]string{{"local"}, {"local", "help"}, {"local", "prepare", "--help"}} {
		if err := run(args); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"local", "unknown"}, {"local", "prepare", "--yes"}, {"local", "prepare", "--config", "missing", "extra", "--yes"}} {
		if err := run(args); err == nil {
			t.Fatalf("invalid arguments accepted: %q", args)
		}
	}
}
