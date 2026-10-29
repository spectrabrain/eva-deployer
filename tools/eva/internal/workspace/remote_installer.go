package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// InstallerFromWorkspace reconstructs the immutable installer input from its
// private materialized files. It also lets apply/retry/check use the original
// target selection without consulting a subsequently edited config file.
func InstallerFromWorkspace(resolved Resolved) (Installer, error) {
	input := Installer{SchemaVersion: "v1", Config: resolved.Config, Targets: map[string]InstallerTarget{}}
	paths, err := filepath.Glob(filepath.Join(resolved.Root, "inventory", "host_vars", "*.yaml"))
	if err != nil {
		return Installer{}, err
	}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".yaml")
		contents, err := os.ReadFile(path)
		var variables map[string]any
		if err != nil || yaml.Unmarshal(contents, &variables) != nil {
			return Installer{}, errors.New("cannot read private installer target settings")
		}
		delete(variables, "eva_target_enabled_components")
		target := InstallerTarget{Ansible: variables, Components: resolved.Config.TargetComponents[name], Values: map[string]map[string]any{}}
		identity, _ := variables["ansible_host"].(string)
		if identity == "" {
			identity = name
		}
		for _, component := range []string{"iam", "app", "agent", "vision"} {
			if !input.Components[component] || target.Components != nil && !target.Components[component] {
				continue
			}
			contents, err := os.ReadFile(filepath.Join(resolved.Root, "site-values", component+".yaml"))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			var blocks map[string]map[string]any
			if err != nil || yaml.Unmarshal(contents, &blocks) != nil {
				return Installer{}, errors.New("cannot read private installer component values")
			}
			if values := blocks[identity]; len(values) > 0 {
				target.Values[component] = values
			}
		}
		input.Targets[name] = target
	}
	return input, nil
}

func (input Installer) HasSSHTargets() bool {
	for _, target := range input.Targets {
		if connection, _ := target.Ansible["ansible_connection"].(string); connection != "local" {
			return true
		}
	}
	return false
}

// LocalTargetConfig sends only the selected target's installation values.
// Transport credentials and all AWS credentials remain on the controller.
func (input Installer) LocalTargetConfig(name string) ([]byte, error) {
	target, exists := input.Targets[name]
	if !exists {
		return nil, errors.New("installer target is unavailable")
	}
	components := map[string]bool{}
	for component, enabled := range input.Components {
		components[component] = enabled && (target.Components == nil || target.Components[component])
	}
	local := Installer{SchemaVersion: "v1", Config: input.Config, Targets: map[string]InstallerTarget{}}
	local.Components, local.TargetComponents = components, nil
	localTarget := InstallerTarget{Ansible: map[string]any{}, Values: map[string]map[string]any{}}
	for key, value := range target.Ansible {
		if strings.HasPrefix(key, "ansible_ssh_") || key == "ansible_password" || key == "ansible_private_key_file" || key == "ansible_become_password" || key == "ansible_become_pass" {
			continue
		}
		localTarget.Ansible[key] = value
	}
	localTarget.Ansible["ansible_connection"] = "local"
	localTarget.Ansible["ansible_become"] = true
	for component, values := range target.Values {
		if components[component] {
			localTarget.Values[component] = values
		}
	}
	local.Targets[name] = localTarget
	if err := local.validate(""); err != nil {
		return nil, err
	}
	return yaml.Marshal(local)
}
