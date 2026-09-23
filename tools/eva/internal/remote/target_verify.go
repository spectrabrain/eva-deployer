package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// TargetCommand deliberately accepts argv rather than a shell fragment.  It
// makes Target verification testable without contacting an actual Target.
type TargetCommand func(context.Context, string, []string, []byte) ([]byte, error)

type TargetConnection struct {
	Target       string
	SSHOptions   []string
	sudoMethod   string
	sudoPassword string
	cleanup      func() error
}

func (connection TargetConnection) Cleanup() error {
	if connection.cleanup == nil {
		return nil
	}
	return connection.cleanup()
}

func (connection TargetConnection) SudoMode() string {
	return connection.sudoMethod
}

func (connection TargetConnection) TransportStreams(
	streams Streams,
) (Streams, func(), error) {
	switch connection.sudoMethod {
	case "password":
	case "passwordless":
		return streams, func() {}, nil
	default:
		return Streams{}, func() {},
			errors.New("Target sudo mode is unavailable")
	}

	if connection.sudoPassword == "" {
		return Streams{}, func() {},
			errors.New("Target sudo credential is unavailable")
	}
	if len(streams.ExtraFiles) != 0 {
		return Streams{}, func() {},
			errors.New(
				"Remote publish secret file descriptor is already in use",
			)
	}

	reader, writer, err := os.Pipe()
	if err != nil {
		return Streams{}, func() {}, err
	}

	if _, err := io.WriteString(
		writer,
		connection.sudoPassword+"\n",
	); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return Streams{}, func() {}, err
	}

	if err := writer.Close(); err != nil {
		_ = reader.Close()
		return Streams{}, func() {}, err
	}

	transportStreams := streams
	transportStreams.ExtraFiles = []*os.File{reader}

	cleanup := func() {
		_ = reader.Close()
	}

	return transportStreams, cleanup, nil
}

type TargetVerifier struct {
	Store       TargetStore
	RuntimeRoot string
	Run         TargetCommand
	StartSSH    func(context.Context, []string, string) error
}

func NewTargetVerifier(store TargetStore) TargetVerifier {
	return TargetVerifier{Store: store, RuntimeRoot: "/run/eva/remote-publish", Run: runTargetCommand, StartSSH: runAskpassSSH}
}

// Verify is read-only with respect to the managed registry.  Its short-lived
// runtime files and remote inbox probe are removed before it returns.
func (verifier TargetVerifier) Verify(ctx context.Context, name string, requiredBytes int64) error {
	config, credential, err := verifier.Store.Load(name)
	if err != nil {
		return err
	}
	connection, err := verifier.VerifyConfiguration(ctx, config, credential, requiredBytes)
	if err != nil {
		return err
	}
	return connection.Cleanup()
}

// VerifyConfiguration is also used by target add before either file becomes
// visible in the managed registry.
func (verifier TargetVerifier) VerifyConfiguration(ctx context.Context, config TargetConfiguration, credential TargetCredential, requiredBytes int64) (TargetConnection, error) {
	if err := ValidateTargetConfiguration(config); err != nil {
		return TargetConnection{}, err
	}
	if err := ValidateTargetCredential(config, credential); err != nil {
		return TargetConnection{}, err
	}
	if verifier.Run == nil {
		return TargetConnection{}, errors.New("Remote Target verifier is not configured")
	}
	key, knownHostLine, err := verifier.scanHostKeyLine(ctx, config)
	if err != nil {
		return TargetConnection{}, err
	}
	if key.Algorithm != config.HostKey.Algorithm || key.Fingerprint != config.HostKey.Fingerprint {
		return TargetConnection{}, fmt.Errorf("Target SSH host key changed: %s", config.Name)
	}
	runtimeDirectory, knownHosts, controlPath, err := verifier.newRuntime(config.Name, knownHostLine)
	if err != nil {
		return TargetConnection{}, err
	}
	cleanup := func() error {
		// A failed or already-closed control socket is harmless.  Runtime files
		// are invocation-local and must never be reused.
		_, _ = verifier.Run(context.Background(), "ssh", verifier.sshArgs(config, knownHosts, controlPath, "-O", "exit", config.User+"@"+config.Host), nil)
		return os.RemoveAll(runtimeDirectory)
	}

	options := verifier.sshOptions(config, knownHosts, controlPath)
	if err := verifier.startControlMaster(ctx, config, credential, options); err != nil {
		_ = cleanup()
		return TargetConnection{}, errors.New("Target SSH authentication failed")
	}
	// The transport must reuse this master and must not fall back to an
	// interactive password prompt if the socket is unexpectedly unavailable.
	transportOptions := append(append([]string(nil), options...), "BatchMode=yes")
	connection := TargetConnection{
		Target:       config.User + "@" + config.Host,
		SSHOptions:   transportOptions,
		sudoMethod:   config.Sudo.Method,
		sudoPassword: credential.SudoPassword,
		cleanup:      cleanup,
	}
	if _, err := verifier.Run(ctx, "ssh", verifier.sshArgs(config, knownHosts, controlPath, connection.Target, "true"), nil); err != nil {
		_ = connection.Cleanup()
		return TargetConnection{}, errors.New("Target SSH authentication failed")
	}
	if err := verifier.verifySudo(ctx, config, credential, knownHosts, controlPath, connection.Target); err != nil {
		_ = connection.Cleanup()
		return TargetConnection{}, err
	}
	if err := verifier.verifyPlatform(ctx, config, knownHosts, controlPath, connection.Target); err != nil {
		_ = connection.Cleanup()
		return TargetConnection{}, err
	}
	if err := verifier.verifyStorage(ctx, config, knownHosts, controlPath, connection.Target, requiredBytes); err != nil {
		_ = connection.Cleanup()
		return TargetConnection{}, err
	}
	if err := verifier.probeInbox(ctx, config, credential, knownHosts, controlPath, connection.Target); err != nil {
		_ = connection.Cleanup()
		return TargetConnection{}, err
	}
	return connection, nil
}

func (verifier TargetVerifier) ScanHostKey(ctx context.Context, host string, port int) (TargetHostKey, error) {
	if host == "" || port < 1 || port > 65535 {
		return TargetHostKey{}, errors.New("Remote Target host key request is invalid")
	}
	output, err := verifier.Run(ctx, "ssh-keyscan", []string{"-p", strconv.Itoa(port), host}, nil)
	if err != nil {
		return TargetHostKey{}, errors.New("Target SSH host key scan failed")
	}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		key, _, _, _, parseErr := ssh.ParseAuthorizedKey([]byte(strings.Join(fields[1:], " ")))
		if parseErr == nil {
			return TargetHostKey{Algorithm: key.Type(), Fingerprint: ssh.FingerprintSHA256(key)}, nil
		}
	}
	return TargetHostKey{}, errors.New("Target SSH host key scan returned no usable key")
}

func (verifier TargetVerifier) scanHostKey(ctx context.Context, config TargetConfiguration) (TargetHostKey, error) {
	return verifier.ScanHostKey(ctx, config.Host, config.Port)
}

func (verifier TargetVerifier) scanHostKeyLine(ctx context.Context, config TargetConfiguration) (TargetHostKey, string, error) {
	output, err := verifier.Run(ctx, "ssh-keyscan", []string{"-p", strconv.Itoa(config.Port), config.Host}, nil)
	if err != nil {
		return TargetHostKey{}, "", errors.New("Target SSH host key scan failed")
	}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		key, _, _, _, parseErr := ssh.ParseAuthorizedKey([]byte(strings.Join(fields[1:], " ")))
		if parseErr == nil && key.Type() == config.HostKey.Algorithm {
			return TargetHostKey{Algorithm: key.Type(), Fingerprint: ssh.FingerprintSHA256(key)}, config.Name + " " + string(ssh.MarshalAuthorizedKey(key)), nil
		}
	}
	return TargetHostKey{}, "", errors.New("Target SSH host key scan returned no usable key")
}

func (verifier TargetVerifier) newRuntime(name, knownHostLine string) (string, string, string, error) {
	root := verifier.RuntimeRoot
	if root == "" {
		root = "/run/eva/remote-publish"
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", "", "", err
	}
	directory, err := os.MkdirTemp(root, "p-")
	if err != nil {
		return "", "", "", err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		_ = os.RemoveAll(directory)
		return "", "", "", err
	}
	knownHosts := filepath.Join(directory, "known_hosts")
	// HostKeyAlias is the safe managed name, so no target-controlled hostname
	// is put into the known_hosts syntax.
	if err := os.WriteFile(knownHosts, []byte(knownHostLine), 0o600); err != nil {
		_ = os.RemoveAll(directory)
		return "", "", "", err
	}
	return directory, knownHosts, filepath.Join(directory, "control"), nil
}

func (verifier TargetVerifier) sshOptions(config TargetConfiguration, knownHosts, controlPath string) []string {
	return []string{
		"Port=" + strconv.Itoa(config.Port),
		"StrictHostKeyChecking=yes",
		"UserKnownHostsFile=" + knownHosts,
		"HostKeyAlias=" + config.Name,
		"ControlPath=" + controlPath,
		"ControlMaster=auto",
		"ControlPersist=300",
	}
}

func (verifier TargetVerifier) sshArgs(config TargetConfiguration, knownHosts, controlPath string, tail ...string) []string {
	args := make([]string, 0, len(tail)+14)
	for _, option := range verifier.sshOptions(config, knownHosts, controlPath) {
		args = append(args, "-o", option)
	}
	return append(args, tail...)
}

func (verifier TargetVerifier) startControlMaster(ctx context.Context, config TargetConfiguration, credential TargetCredential, options []string) error {
	args := make([]string, 0, len(options)*2+5)
	for _, option := range options {
		args = append(args, "-o", option)
	}
	args = append(args, "-o", "ControlMaster=yes", "-MNf")
	if config.Authentication.Method == "public_key" {
		identity, err := verifier.identityPath(config, credential)
		if err != nil {
			return err
		}
		args = append(args, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes", "-i", identity)
	}
	args = append(args, config.User+"@"+config.Host)
	if config.Authentication.Method == "password" && verifier.StartSSH != nil {
		return verifier.StartSSH(ctx, args, credential.SSHPassword)
	}
	_, err := verifier.Run(ctx, "ssh", args, nil)
	return err
}

func (verifier TargetVerifier) identityPath(config TargetConfiguration, credential TargetCredential) (string, error) {
	path := filepath.Join(verifier.Store.CredentialRoot, config.Name, credential.IdentityFile)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !isRootOwnedWhenPrivileged(info) {
		return "", errors.New("Remote Target identity credential is unavailable")
	}
	return path, nil
}

func (verifier TargetVerifier) verifySudo(ctx context.Context, config TargetConfiguration, credential TargetCredential, knownHosts, controlPath, target string) error {
	stdin := []byte(nil)
	args := verifier.sshArgs(config, knownHosts, controlPath, target)
	if config.Sudo.Method == "passwordless" {
		args = append(args, "sudo", "-n", "true")
	} else {
		stdin = []byte(credential.SudoPassword + "\n")
		args = append(args, "sudo", "-S", "-p", "", "true")
	}
	if _, err := verifier.Run(ctx, "ssh", args, stdin); err != nil {
		return errors.New("Target sudo authentication failed")
	}
	return nil
}

func (verifier TargetVerifier) verifyPlatform(ctx context.Context, config TargetConfiguration, knownHosts, controlPath, target string) error {
	output, err := verifier.Run(ctx, "ssh", verifier.sshArgs(config, knownHosts, controlPath, target, "uname", "-s", ";", "uname", "-m"), nil)
	if err != nil || strings.TrimSpace(string(output)) != "linux\namd64" && strings.TrimSpace(string(output)) != "Linux\nx86_64" {
		return errors.New("Target platform must be linux/amd64")
	}
	return nil
}

func (verifier TargetVerifier) verifyStorage(ctx context.Context, config TargetConfiguration, knownHosts, controlPath, target string, requiredBytes int64) error {
	// /var/lib/eva is created by the inbox probe on a first Target install, so
	// measure its guaranteed parent filesystem instead of requiring it early.
	output, err := verifier.Run(ctx, "ssh", verifier.sshArgs(config, knownHosts, controlPath, target, "df", "-Pk", "/var/lib"), nil)
	if err != nil {
		return errors.New("Target storage check failed")
	}
	fields := strings.Fields(string(output))
	if len(fields) < 4 {
		return errors.New("Target storage check returned invalid output")
	}
	availableKB, err := strconv.ParseInt(fields[len(fields)-3], 10, 64)
	if err != nil {
		return errors.New("Target storage check returned invalid output")
	}
	if requiredBytes < 1 {
		requiredBytes = 1 << 30
	}
	// Transfer staging and payload materialization can coexist.  Two copies
	// plus 1 GiB reserve is deliberately conservative and release-size based.
	required := requiredBytes*2 + (1 << 30)
	if availableKB*1024 < required {
		return errors.New("Target storage is insufficient")
	}
	return nil
}

func (verifier TargetVerifier) probeInbox(ctx context.Context, config TargetConfiguration, credential TargetCredential, knownHosts, controlPath, target string) error {
	bytes := make([]byte, 6)
	if _, err := rand.Read(bytes); err != nil {
		return err
	}
	probe := "/var/lib/eva/inbox/releases/.eva-preflight-" + hex.EncodeToString(bytes)
	probeFinal := probe + "-published"
	runSudo := func(command ...string) error {
		args := verifier.sshArgs(config, knownHosts, controlPath, target)
		stdin := []byte(nil)
		if config.Sudo.Method == "password" {
			stdin = []byte(credential.SudoPassword + "\n")
			args = append(args, "sudo", "-S", "-p", "")
		} else {
			args = append(args, "sudo", "-n")
		}
		args = append(args, command...)
		_, err := verifier.Run(ctx, "ssh", args, stdin)
		return err
	}
	if err := runSudo("mkdir", "-p", "/var/lib/eva/inbox/releases"); err != nil {
		return errors.New("Target inbox parent probe failed")
	}
	if err := runSudo("mkdir", probe); err != nil {
		return errors.New("Target inbox parent probe failed")
	}
	if err := runSudo("mv", probe, probeFinal); err != nil {
		_ = runSudo("rmdir", probe)
		return errors.New("Target inbox atomic rename probe failed")
	}
	if err := runSudo("rmdir", probeFinal); err != nil {
		return errors.New("Target inbox parent probe cleanup failed")
	}
	return nil
}

func runTargetCommand(ctx context.Context, path string, args []string, stdin []byte) ([]byte, error) {
	command := exec.CommandContext(ctx, path, args...)
	command.Stdin = bytes.NewReader(stdin)
	return command.CombinedOutput()
}

func writeAskpassRuntime(
	directory string,
	password string,
) (string, string, error) {
	if password == "" {
		return "", "", errors.New(
			"SSH authentication failed",
		)
	}

	secret := filepath.Join(directory, "secret")
	helper := filepath.Join(directory, "askpass")

	if err := os.WriteFile(
		secret,
		[]byte(password+"\n"),
		0o600,
	); err != nil {
		return "", "", err
	}

	helperContents := []byte(
		"#!/bin/sh\n" +
			"exec cat -- \"$EVA_SSH_ASKPASS_SECRET\"\n",
	)

	if err := os.WriteFile(
		helper,
		helperContents,
		0o700,
	); err != nil {
		return "", "", err
	}

	return helper, secret, nil
}

func runAskpassSSH(
	ctx context.Context,
	args []string,
	password string,
) error {
	directory, err := os.MkdirTemp(
		"/run/eva",
		"askpass-",
	)
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)

	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}

	helper, secret, err := writeAskpassRuntime(
		directory,
		password,
	)
	if err != nil {
		return err
	}

	command := exec.CommandContext(
		ctx,
		"ssh",
		args...,
	)

	command.Env = append(
		os.Environ(),
		"SSH_ASKPASS="+helper,
		"SSH_ASKPASS_REQUIRE=force",
		"DISPLAY=eva",
		"EVA_SSH_ASKPASS_SECRET="+secret,
	)

	if output, err := command.CombinedOutput(); err != nil {
		_ = output
		return errors.New("SSH authentication failed")
	}

	return nil
}
