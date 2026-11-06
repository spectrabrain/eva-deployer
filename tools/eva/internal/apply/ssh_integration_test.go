package apply

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"eva-deployer/tools/eva/internal/operation"
	"eva-deployer/tools/eva/internal/plan"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Exercise the real Ansible/OpenSSH password path without contacting any remote
// target or reading/writing the operator's SSH configuration and trusted keys.
func TestTargetSSHAutomaticallyRegistersFirstKeyAndRejectsChanges(t *testing.T) {
	ansible, err := exec.LookPath("ansible-playbook")
	if err != nil {
		t.Skip("ansible-playbook is not available")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH is not available")
	}
	server := newPasswordSSHServer(t)
	workspace := createWorkspace(t)
	releaseRoot := createReleaseSource(t, "")
	runtimeRoot := createRuntime(t)
	config, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "ansible.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	writeSSHTestFile(t, filepath.Join(releaseRoot, "ansible.cfg"), config, 0o600)
	writeSSHTestFile(t, filepath.Join(runtimeRoot, "venv/bin/ansible-playbook"), []byte("#!/bin/sh\nexec '"+strings.ReplaceAll(ansible, "'", "'\\''")+"' \"$@\"\n"), 0o700)
	playbook := []byte("- hosts: eva\n  gather_facts: false\n  tasks:\n    - name: Check target password SSH\n      ansible.builtin.raw: eva-ssh-probe\n      changed_when: false\n")
	writeSSHTestFile(t, filepath.Join(releaseRoot, expectedPlaybooks["precondition"]), playbook, 0o600)
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	_, port, err := net.SplitHostPort(server.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	writeSSHTestFile(t, filepath.Join(workspace, "inventory/inventory.ini"), []byte("[eva]\nnode-a\n"), 0o600)
	if err := os.MkdirAll(filepath.Join(workspace, "inventory/host_vars"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeSSHTestFile(t, filepath.Join(workspace, "inventory/host_vars/node-a.yaml"), []byte(fmt.Sprintf(`ansible_connection: ssh
ansible_host: 127.0.0.1
ansible_port: %s
ansible_user: eva-ssh-test
ansible_password: test-password-not-for-output
ansible_ssh_common_args: '-F /dev/null -o UserKnownHostsFile=%s -o GlobalKnownHostsFile=/dev/null -o ControlPath=none -o UpdateHostKeys=no -o PubkeyAuthentication=no'
`, port, knownHosts)), 0o600)
	// Isolate inherited configuration; use the actual Release SSH policy.
	t.Setenv("ANSIBLE_CONFIG", filepath.Join(releaseRoot, "ansible.cfg"))
	for _, name := range []string{"ANSIBLE_SSH_ARGS", "ANSIBLE_SSH_COMMON_ARGS", "ANSIBLE_SSH_EXTRA_ARGS"} {
		t.Setenv(name, "")
		// An empty environment value still overrides the Release's ssh_args.
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ANSIBLE_HOST_KEY_CHECKING", "True")
	t.Setenv("ANSIBLE_SSH_HOST_KEY_CHECKING", "True")
	t.Setenv("ANSIBLE_NOCOLOR", "1")

	var output bytes.Buffer
	stateRoot := t.TempDir()
	options := Options{StateRoot: stateRoot, LogRoot: t.TempDir(), RuntimeRoot: runtimeRoot, Stdout: &output, Stderr: &output}
	steps := []plan.Step{{Component: "precondition", Playbook: expectedPlaybooks["precondition"], Targets: []string{"node-a"}}}
	record := createOperation(t, stateRoot, workspace, releaseRoot, steps)
	document, err := operation.LoadPlan(stateRoot, record)
	if err != nil {
		t.Fatal(err)
	}
	preflight := func(wantSuccess bool) {
		t.Helper()
		output.Reset()
		_, err := RunPrecondition(options, document)
		if (err == nil) != wantSuccess {
			t.Fatalf("preflight success=%t: %v\n%s", wantSuccess, err, &output)
		}
		if strings.Contains(output.String(), "test-password-not-for-output") {
			t.Fatal("SSH password leaked to command output")
		}
	}
	assertPinned := func(want []byte) []byte {
		t.Helper()
		contents, err := os.ReadFile(knownHosts)
		if err != nil {
			t.Fatal(err)
		}
		callback, err := knownhosts.New(knownHosts)
		if err != nil {
			t.Fatal(err)
		}
		if err := callback(server.listener.Addr().String(), server.listener.Addr(), server.original.PublicKey()); err != nil {
			t.Fatalf("original host key was not registered: %v\n%s", err, contents)
		}
		if want != nil && !bytes.Equal(contents, want) {
			t.Fatalf("known_hosts was unexpectedly changed:\nbefore=%s\nafter=%s", want, contents)
		}
		return contents
	}

	preflight(true) // Unknown key, password auth, and nonstandard port.
	pinned := assertPinned(nil)
	if server.authentications.Load() == 0 || !strings.Contains(output.String(), "ok=1") {
		t.Fatalf("preflight did not authenticate and run the target task:\n%s", &output)
	}
	preflight(true) // A subsequent connection must not duplicate the key.
	assertPinned(pinned)

	// Installation must also work without a previous preflight or manual SSH.
	if err := os.Remove(knownHosts); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	completed, err := Execute(options, record)
	if err != nil || completed.Status != operation.Succeeded {
		t.Fatalf("install with unknown key: %v\n%s", err, &output)
	}
	pinned = assertPinned(nil)

	server.key.Store(newSSHTestSigner(t))
	authentications := server.authentications.Load()
	preflight(false)
	// Some Ansible versions replace OpenSSH's changed-key diagnostic with a
	// generic host-key-checking error. Assert the trust/authentication behavior.
	assertPinned(pinned)
	record = createOperation(t, stateRoot, workspace, releaseRoot, steps)
	output.Reset()
	failed, err := Execute(options, record)
	if err == nil || failed.Status != operation.Failed {
		t.Fatalf("installation accepted a changed host key: %v\n%s", err, &output)
	}
	retried, err := operation.Retry(stateRoot, failed, fixedClock()())
	if err != nil {
		t.Fatal(err)
	}
	output.Reset()
	failed, err = Execute(options, retried)
	if err == nil || failed.Status != operation.Failed {
		t.Fatalf("retry accepted a changed host key: %v\n%s", err, &output)
	}
	if server.authentications.Load() != authentications {
		t.Fatal("credentials were sent after the host key changed")
	}
	assertPinned(pinned)

	// Restoring the trusted key allows the saved operation to be retried.
	server.key.Store(server.original)
	retried, err = operation.Retry(stateRoot, failed, fixedClock()())
	if err != nil {
		t.Fatal(err)
	}
	output.Reset()
	completed, err = Execute(options, retried)
	if err != nil || completed.Status != operation.Succeeded {
		t.Fatalf("retry with original key: %v\n%s", err, &output)
	}
	assertPinned(pinned)
}

func writeSSHTestFile(t *testing.T, path string, contents []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, contents, mode); err != nil {
		t.Fatal(err)
	}
}

func newSSHTestSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

type passwordSSHServer struct {
	listener        net.Listener
	original        ssh.Signer
	key             atomic.Value
	authentications atomic.Int32
}

func newPasswordSSHServer(t *testing.T) *passwordSSHServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &passwordSSHServer{listener: listener, original: newSSHTestSigner(t)}
	server.key.Store(server.original)
	var mutex sync.Mutex
	connections := make(map[net.Conn]bool)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			mutex.Lock()
			connections[connection] = true
			mutex.Unlock()
			go func() {
				defer func() {
					connection.Close()
					mutex.Lock()
					delete(connections, connection)
					mutex.Unlock()
				}()
				config := &ssh.ServerConfig{PasswordCallback: func(metadata ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
					server.authentications.Add(1)
					if metadata.User() != "eva-ssh-test" || string(password) != "test-password-not-for-output" {
						return nil, fmt.Errorf("invalid test credentials")
					}
					return nil, nil
				}}
				config.AddHostKey(server.key.Load().(ssh.Signer))
				_, channels, requests, err := ssh.NewServerConn(connection, config)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					if incoming.ChannelType() != "session" {
						incoming.Reject(ssh.UnknownChannelType, "only sessions supported")
						continue
					}
					channel, requests, err := incoming.Accept()
					if err != nil {
						continue
					}
					for request := range requests {
						request.Reply(true, nil)
						if request.Type == "exec" {
							io.WriteString(channel, "eva-ssh-probe-ok\n")
							channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
							channel.Close()
							break
						}
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-done
		mutex.Lock()
		defer mutex.Unlock()
		for connection := range connections {
			connection.Close()
		}
	})
	return server
}
