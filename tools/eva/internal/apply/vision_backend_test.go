package apply

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"eva-deployer/tools/eva/internal/plan"
	"eva-deployer/tools/eva/internal/runtime"
)

func TestCommandEnvironmentUsesInstalledVisionHelperRoot(t *testing.T) {
	root := t.TempDir()
	backend := filepath.Join(root, "scripts/publish/push_vision_models_to_harbor.sh")
	if err := os.MkdirAll(filepath.Dir(backend), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backend, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EVA_REMOTE_LIBEXEC_ROOT", root)
	document := plan.Document{Environment: map[string]string{"EVA_REMOTE_LIBEXEC_ROOT": "/untrusted-plan-helper"}}
	environment := commandEnvironment(document, "/prepared-release-without-scripts", os.DevNull, runtime.Resolved{})
	found := false
	for _, entry := range environment {
		if strings.HasPrefix(entry, "EVA_REMOTE_LIBEXEC_ROOT=") {
			found = true
			if entry != "EVA_REMOTE_LIBEXEC_ROOT="+root {
				t.Fatalf("wrong helper root: %s", entry)
			}
		}
	}
	if !found {
		t.Fatal("installed helper root was not provided to Ansible")
	}
	if err := os.Chmod(backend, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, entry := range commandEnvironment(document, "/prepared-release", os.DevNull, runtime.Resolved{}) {
		if strings.HasPrefix(entry, "EVA_REMOTE_LIBEXEC_ROOT=") {
			t.Fatal("invalid helper or plan override was passed to Ansible")
		}
	}
}
