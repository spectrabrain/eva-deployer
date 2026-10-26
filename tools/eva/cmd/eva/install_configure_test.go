package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"eva-deployer/tools/eva/internal/plan"
	"eva-deployer/tools/eva/internal/release"
	"eva-deployer/tools/eva/internal/workspace"
	"gopkg.in/yaml.v3"
)

func configureTest(t *testing.T, answers string, passwords ...string) (workspace.Installer, string, string) {
	t.Helper()
	home := t.TempDir()
	var messages bytes.Buffer
	read := func() (string, error) {
		if len(passwords) == 0 {
			t.Fatal("unexpected password prompt")
		}
		value := passwords[0]
		passwords = passwords[1:]
		return value, nil
	}
	if err := runInstallConfigure(nil, strings.NewReader(answers), &messages, home, "eva", read); err != nil {
		t.Fatal(err)
	}
	if len(passwords) != 0 {
		t.Fatal("password prompt skipped")
	}
	path := filepath.Join(home, "eva-install.yaml")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("wizard did not create a private config in the current account's home")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var installer workspace.Installer
	if err := yaml.Unmarshal(contents, &installer); err != nil {
		t.Fatal(err)
	}
	return installer, messages.String(), path
}

func effectiveTargetComponents(input workspace.Installer, name string) map[string]bool {
	target := input.Targets[name]
	if target.Components != nil {
		return target.Components
	}
	return input.Components
}

func TestInstallConfigureLocalRepositoryModes(t *testing.T) {
	for choice, mode := range map[string]string{"1": "online", "2": "remote", "3": "local"} {
		for _, scope := range []string{"1", "2"} {
			t.Run(mode+"/scope-"+scope, func(t *testing.T) {
				answers := choice + "\n"
				if mode == "online" {
					answers += "1\n"
				}
				answers += scope + "\n"
				if scope == "2" {
					answers += "2\n1\n1\n1\n" // HTTPS/shared/co-install/Agent+Vision
				}
				input, messages, path := configureTest(t, answers)
				if input.Repository.Mode != mode || (input.Credentials.AWS != nil) != (mode == "online") {
					t.Fatal("repository inputs do not follow the environment selection")
				}
				target := input.Targets["server-a"]
				components := effectiveTargetComponents(input, "server-a")
				if len(input.Targets) != 1 || target.Ansible["ansible_connection"] != "local" || target.Ansible["ansible_user"] != "eva" {
					t.Fatal("local install must target the current account and server only")
				}
				if components["app"] != (scope == "2") || (len(target.Values) != 0) != (scope == "2") {
					t.Fatal("infra-only sample retained solution components/values")
				}
				if scope == "2" {
					want := "offline"
					if mode == "online" {
						want = "online"
					}
					license := target.Values["app"]["app"].(map[string]any)["license"].(map[string]any)
					if license["activation_mode"] != want {
						t.Fatal("wizard license default/selection mismatch")
					}
				}
				if !strings.Contains(messages, path) || !strings.Contains(messages, "workspace validate") || !strings.Contains(messages, "<UPPER_CASE>") {
					t.Fatal("wizard lost completion instructions")
				}
				if scope == "1" && (strings.Contains(messages, "Solution 접속 프로토콜") || strings.Contains(messages, "라이선스 서버 인터넷 연결")) {
					t.Fatal("infra-only selection must skip all solution questions")
				}
				if strings.Contains(messages, "라이선스 서버 인터넷 연결") {
					t.Fatal("license activation must not add a connectivity question")
				}
				if strings.Contains(messages, "sudo ssh") {
					t.Fatal("local connections must not request SSH host key registration")
				}
			})
		}
	}
}

func TestInstallConfigureOnlineHTTP(t *testing.T) {
	// Online/current server/Solution/HTTP/IP/IAM+App/no Agent or Vision.
	input, messages, _ := configureTest(t, "1\n1\n2\n1\n3\n1\n4\n")
	target := input.Targets["server-a"]
	components := effectiveTargetComponents(input, "server-a")
	if input.Repository.Mode != "online" || target.Ansible["ansible_connection"] != "local" ||
		!components["infra"] || !components["iam"] || !components["app"] ||
		components["agent"] || components["vision"] {
		t.Fatal("single-server Online HTTP component selection changed")
	}
	for _, component := range []string{"iam", "app"} {
		ingress := target.Values[component]["ingress"].(map[string]any)
		tls := ingress["tls"].(map[string]any)
		if tls["enabled"] != false || tls["create"] != false {
			t.Fatalf("%s HTTP must disable ingress TLS and its Secret Job", component)
		}
		if _, exists := tls["hostPath"]; exists {
			t.Fatalf("%s HTTP must not request a host certificate", component)
		}
	}
	app := target.Values["app"]["app"].(map[string]any)
	if app["backendSecure"] != false || app["backendPublicPort"] != 80 {
		t.Fatal("HTTP frontend protocol/port mismatch")
	}
	if !strings.Contains(messages, "3.3.0-beta.9") || !strings.Contains(messages, "3.3.0-beta.1") || !strings.Contains(messages, "E2E") {
		t.Fatal("completion must state published baseline and outstanding live verification")
	}
}

func TestInstallConfigureMixedBastionTargets(t *testing.T) {
	answers := strings.Join([]string{
		"1", "2", // Online + Bastion
		"infra-a", "192.0.2.10", "eva", "1", "1", // Infra-only, add another
		"iam-app", "192.0.2.11", "eva", "2", // don't reuse Infra-only
		"2", "2", "3", "1", "1", "1", // Solution/HTTPS/IP/co-install/Agent+Vision/add
		"external-app", "192.0.2.12", "eva", "2", // different solution
		"2", "1", "2", "2", "4", "2", // Solution/HTTP/separate/external/no extras/done
	}, "\n") + "\n"
	secrets := []string{"001 # ssh ' first", "private-second", "private-third"}
	input, messages, path := configureTest(t, answers, secrets...)
	for _, secret := range secrets {
		if strings.Contains(messages, secret) {
			t.Fatal("password leaked to wizard output")
		}
	}
	if len(input.Targets) != 3 || len(input.Targets["infra-a"].Values) != 0 || effectiveTargetComponents(input, "external-app")["iam"] {
		t.Fatal("mixed scope/SSO selections were not preserved")
	}
	for index, name := range []string{"infra-a", "iam-app", "external-app"} {
		vars := input.Targets[name].Ansible
		if vars["ansible_connection"] != "ssh" || vars["ansible_password"] != secrets[index] || vars["ansible_become_password"] != secrets[index] || vars["ansible_ssh_private_key_file"] != nil {
			t.Fatal("Bastion connection password contract changed")
		}
	}
	for _, required := range []string{"설정된 SSH 계정·비밀번호", "최초 host key를 자동 등록", "이미 등록된 키가 변경되면 중단"} {
		if !strings.Contains(messages, required) {
			t.Fatalf("Bastion automatic SSH connection guidance is missing %q", required)
		}
	}
	for _, unwanted := range []string{"sudo ssh", "StrictHostKeyChecking=ask", "StrictHostKeyChecking=no", "ANSIBLE_HOST_KEY_CHECKING=False", "ssh-keyscan"} {
		if strings.Contains(messages, unwanted) {
			t.Fatalf("Bastion must not require manual SSH preparation or disable key checking: %s", unwanted)
		}
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Fill all required markers, including independently named per-target inputs.
	var document yaml.Node
	if err := yaml.Unmarshal(contents, &document); err != nil {
		t.Fatal(err)
	}
	var fill func(*yaml.Node)
	fill = func(node *yaml.Node) {
		if node.Kind == yaml.ScalarNode && strings.Contains(node.Value, "<TARGET_") {
			node.Value = "filled-target-input"
		}
		for _, child := range node.Content {
			fill(child)
		}
	}
	fill(&document)
	contents, err = yaml.Marshal(&document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fillInstallerSample(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := workspace.Resolve(workspace.Options{ConfigFile: path, CacheRoot: filepath.Join(t.TempDir(), "cache")})
	if err != nil {
		t.Fatal(err)
	}
	draft := plan.Build(resolved, release.Resolved{}, time.Now())
	for _, step := range draft.Steps {
		want := []string{"external-app", "iam-app", "infra-a"}
		switch step.Component {
		case "config", "app":
			want = []string{"external-app", "iam-app"}
		case "iam", "agent", "vision":
			want = []string{"iam-app"}
		}
		if !reflect.DeepEqual(step.Targets, want) {
			t.Fatalf("%s targets = %v, want %v", step.Component, step.Targets, want)
		}
	}
	public, err := plan.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if strings.Contains(string(public), secret) {
			t.Fatal("Bastion password leaked into Plan")
		}
	}
}

func TestInstallConfigureReusesOptionsNotConnections(t *testing.T) {
	answers := "1\n2\nfirst\n192.0.2.10\neva\n2\n2\n1\n1\n4\n1\nsecond\n192.0.2.11\n\n1\n2\n"
	input, messages, _ := configureTest(t, answers, "first-private", "second-private")
	first, second := input.Targets["first"], input.Targets["second"]
	if !reflect.DeepEqual(first.Components, second.Components) || second.Ansible["ansible_host"] != "192.0.2.11" || second.Ansible["ansible_password"] != "second-private" {
		t.Fatal("reusing choices must not reuse target identity or credentials")
	}
	if strings.Count(messages, "Solution 접속 프로토콜") != 1 {
		t.Fatal("reuse should not repeat all profile questions")
	}
}

func TestInstallConfigureRejectsInvalidAndCancelledInputWithoutSaving(t *testing.T) {
	for name, answers := range map[string]string{
		"EOF":           "",
		"after choices": "1\n2\nnode-a\n192.0.2.10\neva\n",
		"control":       "\x1b\n",
		"long line":     strings.Repeat("x", 4097) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			var output bytes.Buffer
			err := runInstallConfigure(nil, strings.NewReader(answers), &output, home, "eva", func() (string, error) { return "", errors.New("do-not-print-password") })
			if err == nil || strings.Contains(err.Error(), "do-not-print-password") {
				t.Fatal("cancelled input must fail without secret-bearing errors")
			}
			if _, err := os.Lstat(filepath.Join(home, "eva-install.yaml")); !os.IsNotExist(err) {
				t.Fatal("cancelled wizard created a file")
			}
		})
	}
}

func TestInstallConfigureRetriesInvalidChoicesAndTargetIdentity(t *testing.T) {
	answers := "9\n1\n2\nall\nnode-a\nlocalhost\n127.0.0.1\n192.0.2.10\n-invalid-user\neva\n1\n1\nnode-a\nnode-b\n192.0.2.10\n192.0.2.11\n\n1\n2\n"
	input, _, _ := configureTest(t, answers, "first-secret", "second-secret")
	if len(input.Targets) != 2 || input.Targets["node-b"].Ansible["ansible_host"] != "192.0.2.11" {
		t.Fatal("wizard did not reprompt for invalid/duplicate identity")
	}
}

func TestInstallConfigureProtectsExistingFilesAndSymlinks(t *testing.T) {
	for _, kind := range []string{"existing", "symlink"} {
		for _, location := range []string{"default", "absolute-output", "relative-output"} {
			t.Run(kind+"/"+location, func(t *testing.T) {
				home := t.TempDir()
				path := filepath.Join(home, "eva-install.yaml")
				var args []string
				if location != "default" {
					path = filepath.Join(home, "custom install's.yaml")
					outputPath := path
					if location == "relative-output" {
						cwd, err := os.Getwd()
						if err != nil {
							t.Fatal(err)
						}
						outputPath, err = filepath.Rel(cwd, path)
						if err != nil {
							t.Fatal(err)
						}
					}
					args = []string{"--output", outputPath}
				}
				original := []byte("private-original")
				if kind == "existing" {
					if err := os.WriteFile(path, original, 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(filepath.Join(home, "missing"), path); err != nil {
					t.Fatal(err)
				}
				var output bytes.Buffer
				err := runInstallConfigure(args, strings.NewReader(""), &output, home, "eva", nil)
				if err == nil || output.Len() != 0 {
					t.Fatal("must reject existing outputs before asking questions")
				}
				if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "--output") {
					t.Fatalf("existing file error must show the absolute path and alternative output option: %v", err)
				}
				if strings.Contains(err.Error(), string(original)) {
					t.Fatal("existing file contents leaked to the error")
				}
				if kind == "existing" {
					contents, err := os.ReadFile(path)
					if err != nil || !bytes.Equal(contents, original) {
						t.Fatal("existing file changed")
					}
				}
			})
		}
	}
}

func TestInstallConfigureDispatchHelpAndNonTTY(t *testing.T) {
	if err := run([]string{"install", "configure", "--help"}); err != nil {
		t.Fatal(err)
	}
	previous := os.Stdin
	file, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	os.Stdin = file
	t.Cleanup(func() { os.Stdin = previous })
	if err := run([]string{"install", "configure"}); err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Fatal("configure must not read passwords from redirected input or treat configure as a Release")
	}
}

func TestInstallConfigureOutputOverride(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "custom install's.yaml")
	var output bytes.Buffer
	if err := runInstallConfigure([]string{"--output", path}, strings.NewReader("1\n1\n1\n"), &output, home, "eva", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(home, "eva-install.yaml")); !os.IsNotExist(err) {
		t.Fatal("--output also created the default path")
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("override path was not created privately")
	}
	if !strings.Contains(output.String(), shellQuoteConfigurePath(path)) {
		t.Fatal("completion command must shell-quote spaces and apostrophes")
	}
}
