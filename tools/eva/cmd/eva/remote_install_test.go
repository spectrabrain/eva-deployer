package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"eva-deployer/tools/eva/internal/operation"
	"eva-deployer/tools/eva/internal/release"
	"eva-deployer/tools/eva/internal/remote"
	"golang.org/x/crypto/ssh"
)

func remoteControllerConfig(t *testing.T) string {
	t.Helper()
	previousCache, previousReceipt := defaultWorkspaceCacheRoot, defaultRemoteBootstrapReceiptPath
	defaultWorkspaceCacheRoot = filepath.Join(t.TempDir(), "workspaces")
	defaultRemoteBootstrapReceiptPath = filepath.Join(t.TempDir(), "missing-receipt")
	t.Cleanup(func() { defaultWorkspaceCacheRoot, defaultRemoteBootstrapReceiptPath = previousCache, previousReceipt })
	contents := `schema_version: v1
site:
  id: remote-a
repository:
  mode: remote
  registry: harbor.example.com:32080
  project: eva
components:
  infra: true
  n8n: true
targets:
  node-a:
    ansible:
      ansible_connection: ssh
      ansible_host: 192.0.2.10
      ansible_user: eva
      ansible_password: private-ssh-a
      ansible_become_password: private-sudo-a
      repository_password: private-harbor-a
  node-b:
    ansible:
      ansible_connection: ssh
      ansible_host: 192.0.2.11
      ansible_user: eva
      ansible_password: private-ssh-b
      ansible_become_password: private-sudo-b
      repository_password: private-harbor-b
`
	path := filepath.Join(t.TempDir(), "install.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mockRemoteInstaller(t *testing.T, action func(remote.TargetAction) error) {
	t.Helper()
	previous := newRemoteInstallerService
	t.Cleanup(func() { newRemoteInstallerService = previous })
	payload, runtime := t.TempDir(), t.TempDir()
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	public, _ := ssh.NewPublicKey(private.Public())
	connector := remote.InstallerConnector{HostKeyRoot: filepath.Join(t.TempDir(), "trust"), Verifier: remote.TargetVerifier{
		RuntimeRoot: filepath.Join(t.TempDir(), "connections"),
		StartSSH:    func(context.Context, []string, string) error { return nil },
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
		},
	}}
	newRemoteInstallerService = func() remote.InstallerService {
		return remote.InstallerService{
			Connect:         connector.Connect,
			ConnectForCheck: connector.ConnectForCheck,
			Publish: remote.Service{
				ResolveBackend: func() (string, error) { return "/unused-backend", nil },
				ResolvePayload: func(release.Resolved, string, string) (remote.PayloadSource, error) {
					return remote.PayloadSource{Directory: payload}, nil
				},
				ResolveRuntime: func(release.Resolved, string, string) (remote.RuntimeArtifactSource, error) {
					return remote.RuntimeArtifactSource{Directory: runtime}, nil
				},
				Run: func(_ string, _ []string, _ remote.Streams) error { return nil },
			},
			Action: func(_ context.Context, _ remote.TargetConnection, request remote.TargetAction, _ remote.Streams) error {
				return action(request)
			},
		}
	}
}

func TestRemoteControllerInstallRetryAndCheckUseTargetSnapshot(t *testing.T) {
	config := remoteControllerConfig(t)
	releaseRoot, state, logs := writeRemotePublishRelease(t), t.TempDir(), t.TempDir()
	failed := true
	var installed []remote.TargetAction
	checks := 0
	mockRemoteInstaller(t, func(request remote.TargetAction) error {
		if request.Action == "check" {
			checks++
			if !request.Verbose {
				t.Fatal("verbose check option was lost")
			}
			if strings.Join(request.Args, " ") != "--runtime-root /unused-main-runtime" {
				t.Fatal("check did not use the target installation Runtime root")
			}
			return nil
		}
		installed = append(installed, request)
		other := "private-harbor-b"
		if request.Name == "node-b" {
			other = "private-harbor-a"
		}
		if strings.Contains(string(request.Config), "private-ssh") || strings.Contains(string(request.Config), "private-sudo") || strings.Contains(string(request.Config), other) {
			t.Fatal("target config exposed transport or another target's credentials")
		}
		if !strings.Contains(string(request.Config), "ansible_connection: local") || !strings.Contains(string(request.Config), "private-harbor-") || strings.Contains(string(request.Config), "infra: true") {
			t.Fatal("target installation values or selected component scope were lost")
		}
		if failed && request.Name == "node-b" {
			return errors.New("simulated target failure")
		}
		return nil
	})
	var installErr error
	output := captureRemoteTargetStdout(t, func() error {
		installErr = run([]string{"install", "--release", releaseRoot, "--config", config, "--component", "n8n", "--state-root", state, "--log-root", logs, "--runtime-root", "/unused-main-runtime", "--yes"})
		return nil
	})
	if installErr == nil || len(installed) != 2 || !strings.Contains(output, "status: failed") {
		t.Fatalf("Remote installation was not aggregated: %v %s", installErr, output)
	}
	record, err := operation.Latest(state)
	if err != nil || record.Status != operation.Failed {
		t.Fatalf("failed operation missing: %v", err)
	}
	document, err := operation.LoadPlan(state, record)
	if err != nil || document.ExecutionMode != remote.InstallerExecutionMode || len(document.TargetInstallArgs) == 0 {
		t.Fatalf("Remote execution plan missing: %v", err)
	}
	for _, path := range []string{record.PlanPath, record.ResultPath, filepath.Join(record.LogDirectory, "ansible.log")} {
		contents, err := os.ReadFile(path)
		if err != nil || strings.Contains(string(contents), "private-") {
			t.Fatalf("operation file missing or leaked credentials: %s %v", path, err)
		}
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	failed = false
	captureRemoteTargetStdout(t, func() error { return run([]string{"retry", "--state-root", state, "--log-root", logs, "--yes"}) })
	retry, err := operation.Latest(state)
	if err != nil || retry.Status != operation.Succeeded || retry.SourceOperationID != record.ID || len(installed) != 4 {
		t.Fatalf("Remote retry lost source operation: %v %#v", err, retry)
	}
	for i := 0; i < 2; i++ {
		if string(installed[i].Config) != string(installed[i+2].Config) {
			t.Fatal("retry changed target input")
		}
	}
	output = captureRemoteTargetStdout(t, func() error {
		return run([]string{"check", "--state-root", state, "--runtime-root", "/unused-main-runtime", "--verbose"})
	})
	if checks != 2 || !strings.Contains(output, "target=node-a") || !strings.Contains(output, "target=node-b") {
		t.Fatalf("Main check did not route to targets: %s", output)
	}
}

func TestRemoteControllerSavedPlanAppliesWithoutSourceConfig(t *testing.T) {
	config := remoteControllerConfig(t)
	state, logs := t.TempDir(), t.TempDir()
	installs := 0
	mockRemoteInstaller(t, func(request remote.TargetAction) error { installs++; return nil })
	captureRemoteTargetStdout(t, func() error {
		return run([]string{"plan", "--release", writeRemotePublishRelease(t), "--config", config, "--save", "--state-root", state})
	})
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	captureRemoteTargetStdout(t, func() error { return run([]string{"apply", "--state-root", state, "--log-root", logs, "--yes"}) })
	if installs != 2 {
		t.Fatalf("saved Remote plan did not execute targets: %d", installs)
	}
}

func TestRemoteControllerRejectsOverridesBeforeSavingOrInstalling(t *testing.T) {
	config := remoteControllerConfig(t)
	contents, _ := os.ReadFile(config)
	contents = []byte(strings.Replace(string(contents), "  n8n: true", "  n8n: true\n  app: true", 1))
	if err := os.WriteFile(config, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "operations")
	releaseRoot := writeRemotePublishRelease(t)
	for _, command := range []string{"plan", "install"} {
		args := []string{command, "--release", releaseRoot, "--config", config, "--set", "app:replicaCount=2", "--state-root", state}
		if command == "plan" {
			args = append(args, "--save")
		} else {
			args = append(args, "--yes")
		}
		err := run(args)
		if err == nil || !strings.Contains(err.Error(), "targets.<name>.values") {
			t.Fatalf("Remote override was not clearly rejected: %v", err)
		}
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("unsupported override created an operation")
	}
}
