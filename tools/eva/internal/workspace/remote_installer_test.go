package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRemoteInstallerUsesImmutableWorkspaceAndScopesTargetSecrets(t *testing.T) {
	var source Installer
	if err := yaml.Unmarshal([]byte(installerFixture), &source); err != nil {
		t.Fatal(err)
	}
	source.Repository.Mode, source.Repository.Registry, source.Credentials.AWS = "remote", "harbor.example.com:32080", nil
	contents, _ := yaml.Marshal(source)
	input, err := LoadInstaller(writeInstaller(t, string(contents)), "")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := input.Materialize(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := InstallerFromWorkspace(resolved)
	if err != nil || !restored.HasSSHTargets() {
		t.Fatalf("installer reconstruction failed: %v", err)
	}
	// The source config and preparation AWS credentials are not needed again.
	if _, err := os.Stat(filepath.Join(resolved.Root, "credentials/aws_key.ini")); !os.IsNotExist(err) {
		t.Fatal("Remote snapshot retained AWS credentials")
	}
	config, err := restored.LocalTargetConfig("node-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"example-access", "example-secret", "001234", "literal ' quotes # and spaces", "node-b", "keys/id_ed25519"} {
		if strings.Contains(string(config), secret) {
			t.Fatalf("transport/other-target input was transferred: %s", secret)
		}
	}
	if !strings.Contains(string(config), "raw-license-secret") || !strings.Contains(string(config), "iam.example.com") {
		t.Fatal("target installation values were lost")
	}
	var targetInput Installer
	if err := yaml.Unmarshal(config, &targetInput); err != nil {
		t.Fatal(err)
	}
	if targetInput.Targets["node-a"].Ansible["ansible_connection"] != "local" || len(targetInput.Targets) != 1 {
		t.Fatal("transferred config must install on the target itself")
	}
}
