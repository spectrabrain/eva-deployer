package workspace

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConfigureSampleProfileMatrix(t *testing.T) {
	for _, mode := range []string{"online", "remote", "local"} {
		for _, address := range []string{"shared", "separate", "ip"} {
			for _, protocol := range []string{"http", "https"} {
				for _, iam := range []string{"install", "external"} {
					if iam == "external" && address == "shared" {
						continue
					}
					t.Run(mode+"/"+address+"/"+protocol+"/"+iam, func(t *testing.T) {
						contents, err := ConfigureSample(mode, []ConfigureTarget{{Name: "node-a", Ansible: map[string]any{"ansible_connection": "ssh", "ansible_user": "eva", "ansible_host": "192.0.2.10"}, Solution: true, Address: address, Protocol: protocol, IAM: iam}})
						if err != nil {
							t.Fatal(err)
						}
						assertPlaceholderExamples(t, contents)
						path := writeInstaller(t, string(fillSample(contents)))
						input, err := LoadInstaller(path, "")
						if err != nil {
							t.Fatal(err)
						}
						if address == "ip" && nestedSampleValue(input.Targets["node-a"].Values["app"], "app", "backendHost") != "192.0.2.10" {
							t.Fatal("IP profile must prefill the target IP")
						}
						if iam == "install" && nestedSampleValue(input.Targets["node-a"].Values["iam"], "config", "publicPort") != "" {
							t.Fatal("wizard must retain the optional empty IAM publicPort")
						}
						assertSampleLicense(t, contents, input.Targets["node-a"].Values["app"], mode)
						resolved, err := Resolve(Options{ConfigFile: path, CacheRoot: filepath.Join(t.TempDir(), "cache")})
						if err != nil {
							t.Fatal(err)
						}
						if resolved.Config.TargetComponents != nil {
							t.Fatal("single-target sample should inherit global components without redundant target scope")
						}
						if resolved.Config.Components["iam"] != (iam == "install") {
							t.Fatal("global IAM scope lost")
						}
					})
				}
			}
		}
	}
}

func TestConfigureSampleIndependentTargetPlaceholders(t *testing.T) {
	targets := []ConfigureTarget{
		{Name: "node-a", Solution: true, IAM: "install", Protocol: "https", Address: "shared"},
		{Name: "node_a", Solution: true, IAM: "external", Protocol: "http", Address: "ip"},
	}
	contents, err := ConfigureSample("online", targets)
	if err != nil {
		t.Fatal(err)
	}
	assertPlaceholderExamples(t, contents)
	var input Installer
	if err := yaml.Unmarshal(contents, &input); err != nil {
		t.Fatal(err)
	}
	for index, target := range targets {
		host := nestedSampleValue(input.Targets[target.Name].Values["app"], "app", "backendHost")
		if !strings.HasPrefix(fmt.Sprint(host), fmt.Sprintf("<TARGET_%d_", index+1)) {
			t.Fatal("target placeholders collide")
		}
	}
}

func TestConfigureSampleOmitsRedundantTargetComponents(t *testing.T) {
	contents, err := ConfigureSample("online", []ConfigureTarget{{
		Name:     "node-a",
		Ansible:  map[string]any{"ansible_connection": "local"},
		Solution: true,
		IAM:      "install",
		Protocol: "https",
		Address:  "shared",
	}})
	if err != nil {
		t.Fatal(err)
	}
	var input Installer
	if err := yaml.Unmarshal(contents, &input); err != nil {
		t.Fatal(err)
	}
	if input.Targets["node-a"].Components != nil {
		t.Fatal("single target sample should omit redundant target components")
	}
	if !input.Components["infra"] || !input.Components["iam"] || !input.Components["app"] {
		t.Fatal("global components should retain selected solution scope")
	}
}

func TestConfigureSampleKeepsTargetComponentsWhenNarrowerThanGlobal(t *testing.T) {
	contents, err := ConfigureSample("online", []ConfigureTarget{
		{Name: "node-a", Ansible: map[string]any{"ansible_connection": "local"}, Solution: true, IAM: "install", Protocol: "https", Address: "shared"},
		{Name: "node-b", Ansible: map[string]any{"ansible_connection": "local"}, Solution: false, IAM: "install", Protocol: "https", Address: "shared"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var input Installer
	if err := yaml.Unmarshal(contents, &input); err != nil {
		t.Fatal(err)
	}
	if input.Targets["node-a"].Components != nil {
		t.Fatal("target with global-equivalent components should inherit from root")
	}
	components := input.Targets["node-b"].Components
	if components == nil || !components["infra"] || components["app"] || components["iam"] {
		t.Fatal("narrower target components must remain explicit")
	}
}

func TestTargetComponentsValidateBeforeWriting(t *testing.T) {
	for _, scope := range []string{"{}", "{infra: false}", "{iam: true, alien: true}", "{agent: true}"} {
		content := strings.Replace(installerFixture, "  node-b:\n", "  node-b:\n    components: "+scope+"\n", 1)
		if _, err := LoadInstaller(writeInstaller(t, content), ""); err == nil {
			t.Fatalf("invalid target component scope accepted: %s", scope)
		}
	}
	content := strings.Replace(installerFixture, "  node-b:\n", "  node-b:\n    components: {infra: true}\n", 1)
	resolved, err := Resolve(Options{ConfigFile: writeInstaller(t, content), CacheRoot: filepath.Join(t.TempDir(), "cache")})
	if err != nil {
		t.Fatal(err)
	}
	vars := readGeneratedMap(t, resolved.Root, "inventory/host_vars/node-b.yaml")
	if vars["eva_target_enabled_components"] != "infra" {
		t.Fatal("host-specific component bridge missing")
	}
	if _, err := os.Stat(filepath.Join(resolved.Root, "site-values/site.yaml")); err != nil {
		t.Fatal(err)
	}
	if !resolved.Config.TargetComponents["node-a"]["iam"] {
		t.Fatal("target with omitted scope must inherit global components")
	}
}

func TestTargetComponentBridgeAgainstAnsible(t *testing.T) {
	ansible, err := exec.LookPath("ansible-playbook")
	if err != nil {
		t.Skip("ansible-playbook is not available")
	}
	var input Installer
	if err := yaml.Unmarshal([]byte(installerFixture), &input); err != nil {
		t.Fatal(err)
	}
	input.Components = map[string]bool{"infra": true, "app": true}
	input.Targets = map[string]InstallerTarget{
		"node-a": {Components: map[string]bool{"app": true}, Ansible: map[string]any{"ansible_connection": "local"}},
		"node-b": {Components: map[string]bool{"infra": true}, Ansible: map[string]any{"ansible_connection": "local"}},
	}
	resolved, err := input.Materialize(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	config, playbook := filepath.Join(root, "ansible.cfg"), filepath.Join(root, "scope.yaml")
	if err := os.WriteFile(config, []byte("[defaults]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	contents := `- hosts: all
  gather_facts: false
  tasks:
    - name: Assert effective per-host component list
      ansible.builtin.assert:
        that:
          - "eva_enabled_components == {'node-a': 'app', 'node-b': 'infra'}[inventory_hostname]"
`
	if err := os.WriteFile(playbook, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, onlyApp := range []bool{false, true} {
		selected := resolved
		if onlyApp {
			selected, err = resolved.SelectComponents([]string{"app"})
			if err != nil {
				t.Fatal(err)
			}
		}
		args := []string{"-i", filepath.Join(resolved.Root, "inventory/inventory.ini"), playbook}
		for _, value := range selected.AnsibleExtraVars() {
			args = append(args, "-e", value)
		}
		if onlyApp {
			args = append(args, "--limit", "node-a")
		}
		command := exec.Command(ansible, args...)
		command.Env = append(os.Environ(), "ANSIBLE_CONFIG="+config)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("Ansible component bridge failed: %v\n%s", err, output)
		}
	}
}
