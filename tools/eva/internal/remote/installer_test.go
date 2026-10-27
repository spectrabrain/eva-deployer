package remote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"eva-deployer/tools/eva/internal/release"
	"eva-deployer/tools/eva/internal/workspace"
	"golang.org/x/crypto/ssh"
)

func installerTestInput() workspace.Installer {
	input := workspace.Installer{SchemaVersion: "v1", Targets: map[string]workspace.InstallerTarget{}}
	input.Site.ID, input.Repository.Mode, input.Repository.Registry, input.Repository.Project = "customer-a", "remote", "harbor.example.internal:32080", "eva"
	input.Components = map[string]bool{"infra": true, "n8n": true}
	for _, name := range []string{"node-a", "node-b"} {
		input.Targets[name] = workspace.InstallerTarget{Ansible: map[string]any{"ansible_connection": "ssh", "ansible_host": name + ".example.com", "ansible_user": "eva", "ansible_password": "private-ssh", "ansible_become_password": "private-sudo"}}
	}
	return input
}

func TestInstallerConnectionAcceptsWizardUserNamesAndSSHPorts(t *testing.T) {
	input := installerTestInput()
	target := input.Targets["node-a"]
	target.Ansible["ansible_port"] = 2222
	for _, user := range []string{"eva", "_eva", "eva$"} {
		target.Ansible["ansible_user"] = user
		config, _, _, err := installerConnectionSettings("node-a", target)
		if err != nil || config.User != user || config.Port != 2222 {
			t.Fatalf("valid wizard connection rejected: %v", err)
		}
	}
	target.Ansible["ansible_user"] = "eva;id"
	if _, _, _, err := installerConnectionSettings("node-a", target); err == nil {
		t.Fatal("invalid connection username accepted")
	}
}

func TestInstallerVerifiesAllTargetsBeforePublishingAndRunsScopedInstallation(t *testing.T) {
	input := installerTestInput()
	resolved := writeOriginalRelease(t, false)
	var calls []string
	var output bytes.Buffer
	service := InstallerService{
		Connect: func(_ context.Context, _, name string, _ workspace.InstallerTarget, size int64) (TargetConnection, error) {
			if size <= 0 {
				t.Fatal("storage requirement missing")
			}
			calls = append(calls, "verify:"+name)
			return TargetConnection{Target: "eva@" + name + ".example.com", sudoMethod: "passwordless"}, nil
		},
		Publish: Service{ResolveBackend: func() (string, error) { return "/backend", nil },
			ResolvePayload: func(release.Resolved, string, string) (PayloadSource, error) {
				return PayloadSource{Directory: t.TempDir()}, nil
			},
			ResolveRuntime: func(release.Resolved, string, string) (RuntimeArtifactSource, error) {
				return RuntimeArtifactSource{Directory: t.TempDir()}, nil
			},
			Run: func(_ string, args []string, _ Streams) error { calls = append(calls, "publish:"+args[7]); return nil }},
		Action: func(_ context.Context, _ TargetConnection, action TargetAction, _ Streams) error {
			calls = append(calls, "install:"+action.Name)
			if action.Action != "install" || !strings.Contains(string(action.Config), "ansible_connection: local") || strings.Contains(string(action.Config), "private-") {
				t.Fatal("installation did not scope/remove transport credentials")
			}
			return nil
		},
	}
	results, err := service.Install(context.Background(), input, resolved, nil, Streams{Stdout: &output, Stderr: &output})
	if err != nil || len(results) != 2 {
		t.Fatalf("installation: %v %#v", err, results)
	}
	if len(calls) != 6 || calls[0] != "verify:node-a" || calls[1] != "verify:node-b" || calls[3] != "install:node-a" || calls[5] != "install:node-b" {
		t.Fatalf("incorrect target sequencing: %v", calls)
	}
	service.Connect = func(_ context.Context, _, name string, _ workspace.InstallerTarget, _ int64) (TargetConnection, error) {
		if name == "node-b" {
			return TargetConnection{}, errors.New("target unreachable")
		}
		return TargetConnection{Target: "eva@node-a.example.com", sudoMethod: "passwordless"}, nil
	}
	calls = nil
	if _, err := service.Install(context.Background(), input, resolved, nil, Streams{Stdout: &output}); err == nil || len(calls) != 0 {
		t.Fatal("a target failed preflight but publish/install still ran")
	}
}

func TestInstallerConnectorPinsHostKeyWithoutStoringPasswords(t *testing.T) {
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	public, _ := ssh.NewPublicKey(private.Public())
	root := t.TempDir()
	connector := InstallerConnector{HostKeyRoot: filepath.Join(root, "trust"), Verifier: TargetVerifier{
		RuntimeRoot: filepath.Join(root, "runtime"), StartSSH: func(context.Context, []string, string) error { return nil },
		Run: func(_ context.Context, name string, args []string, _ []byte) ([]byte, error) {
			if name == "ssh-keyscan" {
				return []byte("fixture " + string(ssh.MarshalAuthorizedKey(public))), nil
			}
			if strings.Contains(strings.Join(args, " "), "uname") {
				return []byte("Linux\nx86_64\n"), nil
			}
			if strings.Contains(strings.Join(args, " "), "df -Pk") {
				return []byte("Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/test 10000000 1 9000000 1% /\n"), nil
			}
			return nil, nil
		}}}
	input := installerTestInput()
	connection, err := connector.Connect(context.Background(), input.Site.ID, "node-a", input.Targets["node-a"], 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Cleanup()
	files, _ := filepath.Glob(filepath.Join(connector.HostKeyRoot, "*.yaml"))
	if len(files) != 1 {
		t.Fatal("host trust was not persisted")
	}
	contents, _ := os.ReadFile(files[0])
	if strings.Contains(string(contents), "private-") {
		t.Fatal("transport credential was persisted")
	}
	_, changed, _ := ed25519.GenerateKey(rand.Reader)
	public, _ = ssh.NewPublicKey(changed.Public())
	connector.Verifier.StartSSH = func(context.Context, []string, string) error {
		t.Fatal("credentials sent after host key change")
		return nil
	}
	if _, err := connector.Connect(context.Background(), input.Site.ID, "node-a", input.Targets["node-a"], 0); err == nil || !strings.Contains(err.Error(), "host key changed") {
		t.Fatalf("changed key accepted: %v", err)
	}
}

func TestTargetActionKeepsSecretsOffSSHArgv(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("Python unavailable")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	_ = os.Mkdir(bin, 0o700)
	ssh := "#!/bin/sh\nprintf '%s\\n' \"$*\" >\"$ACTION_ARGS\"\ncat >\"$ACTION_INPUT\"\n"
	_ = os.WriteFile(filepath.Join(bin, "ssh"), []byte(ssh), 0o700)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("ACTION_ARGS", filepath.Join(root, "argv"))
	t.Setenv("ACTION_INPUT", filepath.Join(root, "stdin"))
	action := TargetAction{Action: "install", Site: "customer-a", Name: "node-a", Config: []byte("password: private-license")}
	if err := runTargetAction(context.Background(), TargetConnection{Target: "eva@target", sudoPassword: "private-sudo"}, action, Streams{Stdout: io.Discard, Stderr: io.Discard}); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(root, "argv"))
	if strings.Contains(string(args), "private-") {
		t.Fatal("credential/config was exposed in SSH arguments")
	}
	contents, _ := os.ReadFile(filepath.Join(root, "stdin"))
	if !strings.Contains(string(contents), "private-sudo") {
		t.Fatal("sudo credential was not delivered through private stdin")
	}
}

func TestTargetActionSeparatesSudoInputFromConfigIncludingCachedSudo(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("Python unavailable")
	}
	// Run the actual remote Python script. The command boundary stands in for
	// sudo/Tool, including cached sudo that does not consume the password.
	script, _ := json.Marshal(targetActionScript)
	harness := `import json, os, subprocess, sys
calls = []
temporary = None
class Result:
    returncode = 0
def run(args, input):
    global temporary
    prefix = ["sudo", "-S", "-p", ""] if os.environ["TEST_SUDO"] else ["sudo", "-n"]
    assert args[:len(prefix)] == prefix
    assert input == (os.environ["TEST_SUDO"] + "\n").encode() if os.environ["TEST_SUDO"] else input == b""
    command = args[len(prefix):]
    calls.append(command)
    if command[:3] == ["install", "-m", "0600"]:
        temporary = command[3]
        assert os.stat(temporary).st_mode & 0o777 == 0o600
        with open(temporary, "rb") as stream:
            assert stream.read() == b"license: private-license\n"
        assert command[4].startswith("/var/lib/eva/install-configs/customer-a/node-a/")
    return Result()
subprocess.run = run
exec(` + string(script) + `)
assert temporary and not os.path.exists(temporary)
assert len(calls) == 4
assert calls[2] == ["bash", "/var/lib/eva/inbox/releases/3.3.0/eva-tool-installer.sh"]
assert calls[3][:5] == ["eva", "install", "--release", "/var/lib/eva/inbox/releases/3.3.0", "--config"]
assert "--yes" in calls[3]
`
	for _, password := range []string{"private-sudo", ""} {
		action := TargetAction{Action: "install", Site: "customer-a", Name: "node-a", ReleaseRoot: "/var/lib/eva/inbox/releases/3.3.0", Config: []byte("license: private-license\n"), SudoPassword: password}
		contents, _ := json.Marshal(action)
		command := exec.Command("python3", "-c", harness)
		command.Stdin = bytes.NewReader(contents)
		command.Env = append(os.Environ(), "TEST_SUDO="+password)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("target action failed (password configured=%t): %v %s", password != "", err, output)
		}
	}
}

func TestInstallerCheckUsesSelectedTargetsAndContinuesAfterFailure(t *testing.T) {
	input := installerTestInput()
	input.Targets["unselected"] = workspace.InstallerTarget{Components: map[string]bool{}}
	var checks []string
	service := InstallerService{
		ConnectForCheck: func(_ context.Context, _, name string, _ workspace.InstallerTarget) (TargetConnection, error) {
			checks = append(checks, name)
			if name == "node-a" {
				return TargetConnection{}, errors.New("unreachable")
			}
			return TargetConnection{}, nil
		},
		Action: func(_ context.Context, _ TargetConnection, action TargetAction, _ Streams) error {
			if action.Action != "check" || len(action.Config) > 0 || !action.Verbose {
				t.Fatal("check attempted to install or transfer config")
			}
			checks = append(checks, "check")
			return nil
		},
	}
	if err := service.Check(context.Background(), input, nil, true, Streams{}); err == nil {
		t.Fatal("failed target check was ignored")
	}
	if strings.Join(checks, ",") != "node-a,node-b,check" {
		t.Fatalf("check selection/sequencing: %v", checks)
	}
}
