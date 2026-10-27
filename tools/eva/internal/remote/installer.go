package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"eva-deployer/tools/eva/internal/release"
	"eva-deployer/tools/eva/internal/workspace"
	"gopkg.in/yaml.v3"
)

const InstallerExecutionMode = "remote_targets"

type InstallerConnector struct {
	Verifier    TargetVerifier
	HostKeyRoot string
}

func NewInstallerConnector() InstallerConnector {
	return InstallerConnector{Verifier: NewTargetVerifier(NewTargetStore("", "")), HostKeyRoot: "/var/lib/eva/ssh-host-keys"}
}

// Connect uses the installer credentials directly. Only host-key trust is
// persisted; the separate managed Target registry is not involved.
func (connector InstallerConnector) Connect(ctx context.Context, site, name string, target workspace.InstallerTarget, requiredBytes int64) (TargetConnection, error) {
	return connector.connect(ctx, site, name, target, requiredBytes, true)
}

func (connector InstallerConnector) ConnectForCheck(ctx context.Context, site, name string, target workspace.InstallerTarget) (TargetConnection, error) {
	return connector.connect(ctx, site, name, target, 0, false)
}

func (connector InstallerConnector) connect(ctx context.Context, site, name string, target workspace.InstallerTarget, requiredBytes int64, preflight bool) (TargetConnection, error) {
	config, credential, identity, err := installerConnectionSettings(name, target)
	if err != nil {
		return TargetConnection{}, err
	}
	if workspace.ValidateConfigureTargetName(site) != nil {
		return TargetConnection{}, errors.New("installer site identity is invalid")
	}
	key := sha256.Sum256([]byte(config.Host + ":" + strconv.Itoa(config.Port)))
	keyPath := filepath.Join(connector.HostKeyRoot, hex.EncodeToString(key[:])+".yaml")
	contents, err := os.ReadFile(keyPath)
	known := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return TargetConnection{}, errors.New("cannot read installer SSH host key")
	}
	if known {
		if info, err := os.Lstat(keyPath); err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || yaml.Unmarshal(contents, &config.HostKey) != nil {
			return TargetConnection{}, errors.New("installer SSH host key record is invalid")
		}
	} else {
		config.HostKey, err = connector.Verifier.ScanHostKey(ctx, config.Host, config.Port)
		if err != nil {
			return TargetConnection{}, err
		}
	}
	verifier := connector.Verifier
	identityCleanup := func() {}
	if identity != "" {
		info, err := os.Lstat(identity)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
			return TargetConnection{}, errors.New("installer SSH private key must be a private regular file")
		}
		contents, err := os.ReadFile(identity)
		if err != nil {
			return TargetConnection{}, errors.New("cannot read installer SSH private key")
		}
		directory, err := os.MkdirTemp("", "eva-install-key-")
		if err != nil {
			return TargetConnection{}, err
		}
		identityCleanup = func() { _ = os.RemoveAll(directory) }
		verifier.Store.CredentialRoot = directory
		if err := os.Mkdir(filepath.Join(directory, config.Name), 0o700); err != nil {
			identityCleanup()
			return TargetConnection{}, err
		}
		if err := os.WriteFile(filepath.Join(directory, config.Name, credential.IdentityFile), contents, 0o600); err != nil {
			identityCleanup()
			return TargetConnection{}, err
		}
	}
	var connection TargetConnection
	if preflight {
		connection, err = verifier.VerifyConfiguration(ctx, config, credential, requiredBytes)
	} else {
		connection, err = verifier.AuthenticateConfiguration(ctx, config, credential)
	}
	identityCleanup()
	if err != nil {
		return TargetConnection{}, err
	}
	if !known {
		contents, _ := yaml.Marshal(config.HostKey)
		if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
			_ = connection.Cleanup()
			return TargetConnection{}, err
		}
		file, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			current, readErr := os.ReadFile(keyPath)
			if readErr != nil || !bytes.Equal(current, contents) {
				err = errors.New("installer SSH host key changed during connection")
			} else {
				err = nil
			}
		} else if err == nil {
			_, err = file.Write(contents)
			closeErr := file.Close()
			if err == nil {
				err = closeErr
			}
			if err != nil {
				_ = os.Remove(keyPath)
			}
		}
		if err != nil {
			_ = connection.Cleanup()
			return TargetConnection{}, err
		}
	}
	return connection, nil
}

func installerConnectionSettings(name string, target workspace.InstallerTarget) (TargetConfiguration, TargetCredential, string, error) {
	if workspace.ValidateConfigureTargetName(name) != nil {
		return TargetConfiguration{}, TargetCredential{}, "", errors.New("installer target name is invalid")
	}
	if len(name) > 63 {
		digest := sha256.Sum256([]byte(name))
		name = "t-" + hex.EncodeToString(digest[:16])
	}
	value := func(key string) string { result, _ := target.Ansible[key].(string); return result }
	if connection := value("ansible_connection"); connection != "" && connection != "ssh" {
		return TargetConfiguration{}, TargetCredential{}, "", errors.New("Remote controller installation requires SSH targets; use a separate config for current-server installation")
	}
	config := TargetConfiguration{SchemaVersion: "v1", Name: name, Host: value("ansible_host"), User: value("ansible_user"), Port: 22}
	if port, exists := target.Ansible["ansible_port"]; exists {
		number, err := strconv.Atoi(fmt.Sprint(port))
		if err != nil || number < 1 || number > 65535 {
			return TargetConfiguration{}, TargetCredential{}, "", errors.New("installer SSH port is invalid")
		}
		config.Port = number
	}
	credential := TargetCredential{SchemaVersion: "v1", SSHPassword: value("ansible_password"), SudoPassword: value("ansible_become_password")}
	if credential.SSHPassword == "" {
		credential.SSHPassword = value("ansible_ssh_pass")
	}
	if credential.SudoPassword == "" {
		credential.SudoPassword = value("ansible_become_pass")
	}
	if become, exists := target.Ansible["ansible_become"]; exists && fmt.Sprint(become) != "true" {
		return TargetConfiguration{}, TargetCredential{}, "", errors.New("Remote controller installation requires target sudo access")
	}
	identity := value("ansible_ssh_private_key_file")
	if identity == "" {
		identity = value("ansible_private_key_file")
	}
	config.Authentication = TargetAuthentication{Method: "password", CredentialRef: name}
	if credential.SSHPassword == "" && identity != "" {
		config.Authentication.Method, credential.IdentityFile = "public_key", "identity"
	} else if credential.SSHPassword == "" {
		return TargetConfiguration{}, TargetCredential{}, "", errors.New("Remote installer target requires ansible_password or ansible_ssh_private_key_file")
	}
	config.Sudo = TargetAuthentication{Method: "passwordless"}
	if credential.SudoPassword != "" {
		config.Sudo = TargetAuthentication{Method: "password", CredentialRef: name}
	}
	// Validate the literal connection fields before scanning or sending credentials.
	config.HostKey = TargetHostKey{Algorithm: "ssh-ed25519", Fingerprint: "SHA256:pending"}
	if err := ValidateTargetConfiguration(config); err != nil {
		return TargetConfiguration{}, TargetCredential{}, "", err
	}
	return config, credential, identity, nil
}

type TargetAction struct {
	Action       string   `json:"action"`
	Site         string   `json:"site"`
	Name         string   `json:"name"`
	ReleaseRoot  string   `json:"release_root"`
	Config       []byte   `json:"config,omitempty"`
	Args         []string `json:"args,omitempty"`
	Verbose      bool     `json:"verbose,omitempty"`
	SudoPassword string   `json:"sudo_password,omitempty"`
}

type InstallerTargetResult struct {
	Name                   string
	StartedAt, CompletedAt time.Time
	Err                    error
}

type InstallerService struct {
	Publish         Service
	Connect         func(context.Context, string, string, workspace.InstallerTarget, int64) (TargetConnection, error)
	ConnectForCheck func(context.Context, string, string, workspace.InstallerTarget) (TargetConnection, error)
	Action          func(context.Context, TargetConnection, TargetAction, Streams) error
}

func NewInstallerService() InstallerService {
	connector := NewInstallerConnector()
	return InstallerService{Publish: NewService(), Connect: connector.Connect, ConnectForCheck: connector.ConnectForCheck, Action: runTargetAction}
}

func (service InstallerService) Install(ctx context.Context, input workspace.Installer, resolved release.Resolved, args []string, streams Streams) ([]InstallerTargetResult, error) {
	if streams.Stdout == nil {
		streams.Stdout = io.Discard
	}
	if streams.Stderr == nil {
		streams.Stderr = io.Discard
	}
	if workspace.NormalizeRepositoryMode(input.Repository.Mode) != "remote" {
		return nil, errors.New("Remote controller installation requires repository.mode: remote")
	}
	if service.Connect == nil || service.Action == nil || service.Publish.ResolvePayload == nil || service.Publish.ResolveRuntime == nil {
		return nil, errors.New("Remote installer service is not configured")
	}
	names := selectedInstallerTargets(input)
	if len(names) == 0 {
		return nil, errors.New("Remote installer has no selected targets")
	}
	configs := map[string][]byte{}
	for _, name := range names {
		if _, _, _, err := installerConnectionSettings(name, input.Targets[name]); err != nil {
			return nil, fmt.Errorf("target %s: %w", name, err)
		}
		config, err := input.LocalTargetConfig(name)
		if err != nil {
			return nil, err
		}
		configs[name] = config
	}
	payload, err := service.Publish.ResolvePayload(resolved, input.Repository.Registry, input.Repository.Project)
	if err != nil {
		return nil, err
	}
	runtime, err := service.Publish.ResolveRuntime(resolved, input.Repository.Registry, input.Repository.Project)
	if err != nil {
		return nil, err
	}
	bytes, err := TargetTransferSize(resolved.Root, payload.Directory, runtime.Directory)
	if err != nil {
		return nil, err
	}
	connections := map[string]TargetConnection{}
	defer func() {
		for _, connection := range connections {
			_ = connection.Cleanup()
		}
	}()
	for _, name := range names {
		connection, err := service.Connect(ctx, input.Site.ID, name, input.Targets[name], bytes)
		if err != nil {
			return nil, fmt.Errorf("target %s: %w", name, err)
		}
		connections[name] = connection
	}
	var results []InstallerTargetResult
	var failures []error
	for _, name := range names {
		fmt.Fprintf(streams.Stdout, "\n[INFO] Remote installation target=%s\n", name)
		result := InstallerTargetResult{Name: name, StartedAt: time.Now().UTC()}
		publisher := service.Publish
		publisher.ResolvePayload = func(release.Resolved, string, string) (PayloadSource, error) { return payload, nil }
		publisher.ResolveRuntime = func(release.Resolved, string, string) (RuntimeArtifactSource, error) { return runtime, nil }
		publisher.ResolveManagedTarget = func(string, int64) (TargetConnection, error) {
			connection := connections[name]
			connection.cleanup = nil // The install/check service owns the master lifetime.
			return connection, nil
		}
		publishName := name
		if ValidateTargetName(publishName) != nil {
			digest := sha256.Sum256([]byte(name))
			publishName = "t-" + hex.EncodeToString(digest[:16])
		}
		_, err := publisher.PublishWithResult(PublishOptions{Release: resolved, Target: publishName, Registry: input.Repository.Registry, Project: input.Repository.Project, Streams: streams})
		if err == nil {
			err = service.Action(ctx, connections[name], TargetAction{Action: "install", Site: input.Site.ID, Name: name,
				ReleaseRoot: filepath.Join("/var/lib/eva/inbox/releases", resolved.Metadata.Version), Config: configs[name], Args: args}, streams)
		}
		if err != nil {
			result.Err = errors.New("Remote target installation failed; inspect the target output and operation logs")
			failures = append(failures, fmt.Errorf("target %s: %w", name, result.Err))
		}
		result.CompletedAt = time.Now().UTC()
		results = append(results, result)
	}
	return results, errors.Join(failures...)
}

func (service InstallerService) Check(ctx context.Context, input workspace.Installer, args []string, verbose bool, streams Streams) error {
	if service.ConnectForCheck == nil || service.Action == nil {
		return errors.New("Remote installer check service is not configured")
	}
	if streams.Stdout == nil {
		streams.Stdout = io.Discard
	}
	if streams.Stderr == nil {
		streams.Stderr = io.Discard
	}
	names := selectedInstallerTargets(input)
	if len(names) == 0 {
		return errors.New("Remote installer has no selected targets")
	}
	var failures []error
	for _, name := range names {
		fmt.Fprintf(streams.Stdout, "\nEVA installation check target=%s\n", name)
		connection, err := service.ConnectForCheck(ctx, input.Site.ID, name, input.Targets[name])
		if err == nil {
			err = service.Action(ctx, connection, TargetAction{Action: "check", Args: args, Verbose: verbose}, streams)
			_ = connection.Cleanup()
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("target %s: installation check failed", name))
		}
	}
	return errors.Join(failures...)
}

func selectedInstallerTargets(input workspace.Installer) []string {
	var names []string
	for name, target := range input.Targets {
		for component, enabled := range input.Components {
			if enabled && (target.Components == nil || target.Components[component]) {
				names = append(names, name)
				break
			}
		}
	}
	sort.Strings(names)
	return names
}

func runTargetAction(ctx context.Context, connection TargetConnection, action TargetAction, streams Streams) error {
	action.SudoPassword = connection.sudoPassword
	contents, err := json.Marshal(action)
	if err != nil {
		return err
	}
	arguments := []string{}
	for _, option := range connection.SSHOptions {
		arguments = append(arguments, "-o", option)
	}
	arguments = append(arguments, connection.Target, remoteCommand("python3", "-c", targetActionScript))
	command := exec.CommandContext(ctx, "ssh", arguments...)
	command.Stdin, command.Stdout, command.Stderr = bytes.NewReader(contents), streams.Stdout, streams.Stderr
	return command.Run()
}

// Passwords/configuration travel only through SSH stdin. sudo receives a
// separate password-only stdin for each command, including NOPASSWD cases.
const targetActionScript = `import base64, hashlib, json, os, subprocess, sys, tempfile
request = json.load(sys.stdin)
password = request.get("sudo_password", "")
sudo = ["sudo", "-S", "-p", ""] if password else ["sudo", "-n"]
def root(args):
    result = subprocess.run(sudo + args, input=(password + "\n").encode() if password else b"")
    if result.returncode:
        raise SystemExit(result.returncode)
if request["action"] == "check":
    root(["eva", "check"] + request.get("args", []) + (["--verbose"] if request.get("verbose") else []))
else:
    config = base64.b64decode(request["config"], validate=True)
    digest = hashlib.sha256(config).hexdigest()
    directory = "/var/lib/eva/install-configs/" + request["site"] + "/" + request["name"]
    destination = directory + "/" + digest + ".yaml"
    fd, temporary = tempfile.mkstemp(prefix="eva-install-", suffix=".yaml")
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(config)
        root(["install", "-d", "-m", "0700", directory])
        root(["install", "-m", "0600", temporary, destination])
        root(["bash", request["release_root"] + "/eva-tool-installer.sh"])
        root(["eva", "install", "--release", request["release_root"], "--config", destination, "--yes"] + request.get("args", []))
    finally:
        os.unlink(temporary)
`
