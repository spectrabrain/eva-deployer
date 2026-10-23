package workspace

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

const installerFixture = `schema_version: v1
site:
  id: customer-a
repository:
  mode: online
components:
  infra: true
  iam: true
  app: true
credentials:
  aws:
    aws_access_key_id: example-access
    aws_secret_access_key: example-secret
targets:
  node-a:
    ansible:
      ansible_connection: local
      ansible_user: eva
      ansible_become: true
      ansible_password: "literal ' quotes # and spaces"
      ansible_become_password: "001234"
      ansible_ssh_private_key_file: keys/id_ed25519
      ansible_host: 192.0.2.10
    values:
      iam:
        config:
          host: iam.example.com
      app:
        app:
          license:
            api_key: raw-license-secret
  node-b:
    ansible:
      ansible_connection: ssh
      ansible_host: 192.0.2.11
`

func writeInstaller(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "installer.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInstallerMaterializesExistingWorkspaceContract(t *testing.T) {
	path := writeInstaller(t, installerFixture)
	cache := filepath.Join(t.TempDir(), "workspaces")
	resolved, err := Resolve(Options{ConfigFile: path, CacheRoot: cache})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.SiteID != "customer-a" || resolved.AnsibleMode != "online_repository" || resolved.Config.Repository.Project != "eva" {
		t.Fatalf("unexpected workspace site/mode/project")
	}
	if !strings.HasPrefix(resolved.Root, filepath.Join(cache, "customer-a")+string(filepath.Separator)) {
		t.Fatal("workspace did not use managed cache")
	}
	inventory, err := os.ReadFile(filepath.Join(resolved.Root, "inventory/inventory.ini"))
	if err != nil || string(inventory) != "[eva]\nnode-a\nnode-b\n" {
		t.Fatalf("inventory mismatch: %v", err)
	}
	vars := readGeneratedMap(t, resolved.Root, "inventory/host_vars/node-a.yaml")
	if vars["ansible_become"] != true || vars["ansible_become_password"] != "001234" || vars["ansible_password"] != "literal ' quotes # and spaces" {
		t.Fatal("Ansible variable values/types changed")
	}
	if vars["ansible_ssh_private_key_file"] != filepath.Join(filepath.Dir(path), "keys/id_ed25519") {
		t.Fatal("relative SSH key path did not resolve against config")
	}
	values := readGeneratedMap(t, resolved.Root, "site-values/app.yaml")
	if _, ok := values["192.0.2.10"]; !ok {
		t.Fatal("values must be keyed by ansible_host, not inventory alias")
	}
	if block, ok := values["192.0.2.11"].(map[string]any); !ok || len(block) != 0 {
		t.Fatal("missing empty values block for target without overrides")
	}
	aws, err := os.ReadFile(filepath.Join(resolved.Root, "credentials/aws_key.ini"))
	if err != nil || !strings.Contains(string(aws), "aws_secret_access_key = example-secret\n") || !strings.Contains(string(aws), "region = ap-northeast-2\n") {
		t.Fatalf("AWS credential contract mismatch: %v", err)
	}
	if err := filepath.WalkDir(cache, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		want := os.FileMode(0o600)
		if entry.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Errorf("private mode mismatch for %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func readGeneratedMap(t *testing.T, root, name string) map[string]any {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := yaml.Unmarshal(contents, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestInstallerSnapshotsPersistAndDoNotOverwrite(t *testing.T) {
	path := writeInstaller(t, installerFixture)
	cache := filepath.Join(t.TempDir(), "workspaces")
	options := Options{ConfigFile: path, CacheRoot: cache}
	first, err := Resolve(options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Resolve(options)
	if err != nil || second.Root != first.Root {
		t.Fatalf("identical input did not reuse snapshot: %v", err)
	}
	changed := strings.Replace(installerFixture, "raw-license-secret", "updated-license-secret", 1)
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := Resolve(options)
	if err != nil || third.Root == first.Root {
		t.Fatalf("changed input did not produce a new snapshot: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(Options{Workspace: first.Root}); err != nil {
		t.Fatal("snapshot depends on source config")
	}
	values, err := os.ReadFile(filepath.Join(first.Root, "site-values/app.yaml"))
	if err != nil || !strings.Contains(string(values), "raw-license-secret") || strings.Contains(string(values), "updated-license-secret") {
		t.Fatal("new config mutated old operation input")
	}
}

func TestInstallerConcurrentPublication(t *testing.T) {
	options := Options{ConfigFile: writeInstaller(t, installerFixture), CacheRoot: filepath.Join(t.TempDir(), "cache")}
	var workers sync.WaitGroup
	roots := make(chan string, 8)
	for index := 0; index < cap(roots); index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			resolved, err := Resolve(options)
			if err != nil {
				t.Errorf("concurrent publication failed: %v", err)
				return
			}
			roots <- resolved.Root
		}()
	}
	workers.Wait()
	close(roots)
	first := ""
	for root := range roots {
		if first != "" && first != root {
			t.Fatal("identical concurrent inputs published different snapshots")
		}
		first = root
	}
	entries, err := os.ReadDir(filepath.Join(options.CacheRoot, "customer-a"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("publication left incomplete staging directories: %v", err)
	}
}

func TestInstallerRejectsInvalidInputsBeforeWriting(t *testing.T) {
	for name, content := range map[string]string{
		"schema":      strings.Replace(installerFixture, "schema_version: v1", "schema_version: v2", 1),
		"unknown":     installerFixture + "unknown: value\n",
		"duplicate":   installerFixture + "schema_version: v1\n",
		"second doc":  installerFixture + "---\nextra: true\n",
		"empty doc":   installerFixture + "---\n",
		"traversal":   strings.Replace(installerFixture, "node-a:", "../node-a:", 1),
		"host path":   strings.Replace(installerFixture, "192.0.2.10", "../../escape", 1),
		"same host":   strings.Replace(installerFixture, "192.0.2.11", "192.0.2.10", 1),
		"group alias": strings.Replace(installerFixture, "node-a:", "all:", 1),
		"aws newline": strings.Replace(installerFixture, "example-secret", `"example\nsecret"`, 1),
		"bad values":  strings.Replace(installerFixture, "      iam:", "      infra:", 1),
		"type secret": strings.Replace(installerFixture, "infra: true", "infra: do-not-print-secret", 1),
	} {
		t.Run(name, func(t *testing.T) {
			cache := filepath.Join(t.TempDir(), "workspaces")
			_, err := Resolve(Options{ConfigFile: writeInstaller(t, content), CacheRoot: cache})
			if err == nil {
				t.Fatal("invalid installer input accepted")
			}
			if strings.Contains(err.Error(), "do-not-print-secret") || strings.Contains(err.Error(), "example-secret") {
				t.Fatal("validation error disclosed secret")
			}
			if _, err := os.Stat(cache); !os.IsNotExist(err) {
				t.Fatal("invalid config wrote to disk")
			}
		})
	}
}

func TestInstallerOptionsAndSourceProtection(t *testing.T) {
	path := writeInstaller(t, installerFixture)
	for _, options := range []Options{
		{ConfigFile: path, Workspace: t.TempDir()},
		{ConfigFile: path, SiteID: "other-site"},
	} {
		if _, err := Resolve(options); err == nil {
			t.Fatal("conflicting options accepted")
		}
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInstaller(path, ""); err == nil {
		t.Fatal("publicly readable installer config accepted")
	}
	link := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInstaller(link, ""); err == nil {
		t.Fatal("symlink config accepted")
	}
}

func TestInstallerRejectsTamperedSnapshotAndSymlinkCache(t *testing.T) {
	for _, kind := range []string{"content", "permissions", "symlink", "extra", "missing"} {
		t.Run(kind, func(t *testing.T) {
			options := Options{ConfigFile: writeInstaller(t, installerFixture), CacheRoot: filepath.Join(t.TempDir(), "cache")}
			resolved, err := Resolve(options)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(resolved.Root, "site-values/app.yaml")
			switch kind {
			case "content":
				err = os.WriteFile(path, []byte("tampered"), 0o600)
			case "permissions":
				err = os.Chmod(path, 0o644)
			case "symlink":
				if err = os.Remove(path); err == nil {
					err = os.Symlink(options.ConfigFile, path)
				}
			case "extra":
				err = os.WriteFile(filepath.Join(resolved.Root, "extra"), []byte("x"), 0o600)
			case "missing":
				err = os.Remove(path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Resolve(options); err == nil {
				t.Fatal("tampered snapshot accepted or overwritten")
			}
		})
	}
	parent := t.TempDir()
	link := filepath.Join(parent, "cache")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(Options{ConfigFile: writeInstaller(t, installerFixture), CacheRoot: filepath.Join(link, "nested")}); err == nil {
		t.Fatal("symlinked cache ancestor accepted")
	}
}

func TestInstallerOfflineModesDoNotMaterializeAWSCredentials(t *testing.T) {
	for _, mode := range []string{"remote", "local"} {
		content := strings.Replace(installerFixture, "mode: online", "mode: "+mode+"\n  registry: harbor.example.com:32080", 1)
		if _, err := LoadInstaller(writeInstaller(t, content), ""); err == nil {
			t.Fatal("offline mode accepted AWS credentials")
		}
		start := strings.Index(content, "credentials:\n")
		end := strings.Index(content, "targets:\n")
		content = content[:start] + content[end:]
		resolved, err := Resolve(Options{ConfigFile: writeInstaller(t, content), CacheRoot: filepath.Join(t.TempDir(), "cache")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(resolved.Root, "credentials/aws_key.ini")); !os.IsNotExist(err) {
			t.Fatal("offline workspace contains AWS credentials")
		}
	}
}

func TestInstallerOnlineAliasesUseCanonicalSnapshot(t *testing.T) {
	path := writeInstaller(t, installerFixture)
	cache := filepath.Join(t.TempDir(), "cache")
	canonical, err := Resolve(Options{ConfigFile: path, CacheRoot: cache})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"online", "cloud", "CLOUD", "online_repository", "cloud_repository"} {
		t.Run(mode, func(t *testing.T) {
			content := strings.Replace(installerFixture, "mode: online", "mode: "+mode, 1)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			input, err := LoadInstaller(path, "")
			if err != nil {
				t.Fatal(err)
			}
			if input.Repository.Mode != "online" || input.Credentials.AWS == nil {
				t.Fatal("legacy mode did not retain online AWS input")
			}
			// Materialize also accepts callers constructing Installer directly.
			input.Repository.Mode = mode
			resolved, err := input.Materialize(cache)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Root != canonical.Root || resolved.AnsibleMode != "online_repository" {
				t.Fatal("equivalent modes produced different workspace snapshots")
			}
			site := readGeneratedMap(t, resolved.Root, "site-values/site.yaml")
			if site["repository"].(map[string]any)["mode"] != "online" {
				t.Fatal("new workspace did not serialize canonical online mode")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != content {
				t.Fatal("legacy installer source was rewritten")
			}
		})
	}
}

func TestInstallerInventoryWithAnsible(t *testing.T) {
	ansible, err := exec.LookPath("ansible-inventory")
	if err != nil {
		t.Skip("ansible-inventory is not available")
	}
	resolved, err := Resolve(Options{ConfigFile: writeInstaller(t, installerFixture), CacheRoot: filepath.Join(t.TempDir(), "cache")})
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "ansible.cfg")
	if err := os.WriteFile(config, []byte("[defaults]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(ansible, "-i", filepath.Join(resolved.Root, "inventory/inventory.ini"), "--list")
	command.Env = append(os.Environ(), "ANSIBLE_CONFIG="+config)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("ansible-inventory failed: %v", err)
	}
	var inventory struct {
		Meta struct {
			Hostvars map[string]map[string]any `json:"hostvars"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(output, &inventory); err != nil {
		t.Fatal("invalid Ansible inventory output")
	}
	vars := inventory.Meta.Hostvars["node-a"]
	if vars["ansible_become"] != true || vars["ansible_become_password"] != "001234" || vars["ansible_password"] != "literal ' quotes # and spaces" {
		t.Fatal("Ansible inventory changed variable values/types")
	}
}
