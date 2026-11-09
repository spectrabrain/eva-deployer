package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"eva-deployer/tools/eva/internal/workspace"
	"gopkg.in/yaml.v3"
)

func TestInstallConfigurePRO6000MIGSelection(t *testing.T) {
	for _, mode := range []string{"1", "2", "3"} {
		for _, selection := range []string{"1", "2", "3"} {
			t.Run(mode+"/gpu-"+selection, func(t *testing.T) {
				answers := mode + "\n"
				if mode != "3" {
					answers += "1\n"
				}
				answers += "3\n1\n3\n1\n1\n" + selection + "\n"
				input, messages, path := configureTest(t, answers)
				value, exists := input.Targets["server-a"].Ansible["eva_gpu_mig_enabled"]
				if exists != (selection != "3") || exists && value != (selection == "2") {
					t.Fatalf("MIG selection lost: value=%v exists=%t", value, exists)
				}
				if input.Components["infra"] || !strings.Contains(messages, "PRO6000 GPU 구성") {
					t.Fatal("Solution-only must collect GPU preparation choices")
				}
				contents, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				contents = []byte(strings.ReplaceAll(string(contents), "<TARGET_IP>", "192.0.2.10"))
				if err := os.WriteFile(path, fillInstallerSample(contents), 0o600); err != nil {
					t.Fatal(err)
				}
				resolved, err := workspace.Resolve(workspace.Options{ConfigFile: path, CacheRoot: filepath.Join(t.TempDir(), "cache")})
				if err != nil {
					t.Fatal(err)
				}
				hostVars, err := os.ReadFile(filepath.Join(resolved.Root, "inventory/host_vars/server-a.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				var variables map[string]any
				if err := yaml.Unmarshal(hostVars, &variables); err != nil {
					t.Fatal(err)
				}
				if variables["eva_gpu_mig_enabled"] != value {
					t.Fatal("workspace lost the target's MIG selection")
				}
			})
		}
	}
}

func TestInstallConfigureReusesPRO6000SelectionAcrossTargets(t *testing.T) {
	answers := "1\n2\n1\nfirst\n192.0.2.10\neva\n3\n1\n3\n1\n1\n1\n1\nsecond\n192.0.2.11\n\n1\n2\n"
	input, messages, _ := configureTest(t, answers, "first-private", "second-private")
	for _, name := range []string{"first", "second"} {
		if input.Targets[name].Ansible["eva_gpu_mig_enabled"] != false {
			t.Fatal("reusing installation options lost MIG opt-out")
		}
	}
	if strings.Count(messages, "PRO6000 GPU 구성") != 1 {
		t.Fatal("reused MIG choice must not be prompted again")
	}
}
