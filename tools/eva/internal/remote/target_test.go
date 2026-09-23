package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestTargetStoreWritesAndLoadsSeparatedCredential(t *testing.T) {
	store := NewTargetStore(filepath.Join(t.TempDir(), "targets"), filepath.Join(t.TempDir(), "credentials"))
	config := testTargetConfiguration()
	credential := TargetCredential{SchemaVersion: "v1", SSHPassword: "ssh-secret", SudoPassword: "sudo-secret"}
	if err := store.Write(config, credential); err != nil {
		t.Fatal(err)
	}
	loaded, loadedCredential, err := store.Load(config.Name)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Host != config.Host || loadedCredential.SSHPassword != credential.SSHPassword || loadedCredential.SudoPassword != credential.SudoPassword {
		t.Fatalf("loaded target differs: %#v %#v", loaded, loadedCredential)
	}
	configPath, _ := store.ConfigPath(config.Name)
	credentialPath, _ := store.CredentialPath(config.Name)
	contents, err := os.ReadFile(configPath)
	if err != nil || string(contents) == "" || string(contents) == "ssh-secret" {
		t.Fatalf("configuration leaked credential: %q %v", contents, err)
	}
	for path, want := range map[string]os.FileMode{filepath.Dir(configPath): 0o750, configPath: 0o640, filepath.Dir(credentialPath): 0o700, credentialPath: 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("mode %s = %o, want %o", path, info.Mode().Perm(), want)
		}
	}
}

func TestTargetVerifierUsesPinnedHostKeyAndConnectionContext(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := ssh.NewPublicKey(privateKey.Public())
	if err != nil {
		t.Fatal(err)
	}
	config := testTargetConfiguration()
	config.HostKey = TargetHostKey{Algorithm: publicKey.Type(), Fingerprint: ssh.FingerprintSHA256(publicKey)}
	var startedPassword string
	verifier := TargetVerifier{
		Store:       NewTargetStore(filepath.Join(t.TempDir(), "targets"), filepath.Join(t.TempDir(), "credentials")),
		RuntimeRoot: t.TempDir(),
		StartSSH: func(_ context.Context, _ []string, password string) error {
			startedPassword = password
			return nil
		},
		Run: func(_ context.Context, path string, arguments []string, _ []byte) ([]byte, error) {
			if path == "ssh-keyscan" {
				return []byte("target " + string(ssh.MarshalAuthorizedKey(publicKey))), nil
			}
			joined := strings.Join(arguments, " ")
			switch {
			case strings.Contains(joined, "uname"):
				return []byte("Linux\nx86_64\n"), nil
			case strings.Contains(joined, "df -Pk"):
				return []byte("Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/test 10000000 1 9000000 1% /\n"), nil
			default:
				return nil, nil
			}
		},
	}
	connection, err := verifier.VerifyConfiguration(context.Background(), config, TargetCredential{SchemaVersion: "v1", SSHPassword: "ssh-secret", SudoPassword: "sudo-secret"}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Cleanup()
	if startedPassword != "ssh-secret" {
		t.Fatal("SSH password was not supplied to the narrow authentication path")
	}
	if connection.Target != "eva@10.159.56.197" ||
		connection.SudoMode() != "password" ||
		!strings.Contains(
			strings.Join(connection.SSHOptions, "\n"),
			"StrictHostKeyChecking=yes",
		) ||
		strings.Contains(
			strings.Join(connection.SSHOptions, "\n"),
			"ssh-secret",
		) {
		t.Fatalf(
			"unsafe or incomplete connection context: %#v",
			connection,
		)
	}
}

func TestWriteAskpassRuntimeUsesRestrictedSecretFile(
	t *testing.T,
) {
	directory := t.TempDir()

	helper, secret, err := writeAskpassRuntime(
		directory,
		"ssh-secret",
	)
	if err != nil {
		t.Fatal(err)
	}

	for path, expectedMode := range map[string]os.FileMode{
		helper: 0o700,
		secret: 0o600,
	} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("%s is not a regular file", path)
		}
		if actual := info.Mode().Perm(); actual != expectedMode {
			t.Fatalf(
				"%s mode=%o, want=%o",
				path,
				actual,
				expectedMode,
			)
		}
	}

	secretContents, err := os.ReadFile(secret)
	if err != nil {
		t.Fatal(err)
	}
	if string(secretContents) != "ssh-secret\n" {
		t.Fatal("askpass secret content differs")
	}

	helperContents, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(
		string(helperContents),
		"ssh-secret",
	) {
		t.Fatal("askpass helper contains the SSH password")
	}
	if !strings.Contains(
		string(helperContents),
		"EVA_SSH_ASKPASS_SECRET",
	) {
		t.Fatal("askpass helper does not use the secret path")
	}
}

func TestWriteAskpassRuntimeRejectsEmptyPassword(
	t *testing.T,
) {
	if _, _, err := writeAskpassRuntime(
		t.TempDir(),
		"",
	); err == nil {
		t.Fatal("empty SSH password was accepted")
	}
}

func TestTargetVerifierRejectsChangedHostKeyBeforeAuthentication(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := ssh.NewPublicKey(privateKey.Public())
	if err != nil {
		t.Fatal(err)
	}
	config := testTargetConfiguration()
	called := false
	verifier := TargetVerifier{RuntimeRoot: t.TempDir(), StartSSH: func(context.Context, []string, string) error { called = true; return nil }, Run: func(_ context.Context, path string, _ []string, _ []byte) ([]byte, error) {
		if path == "ssh-keyscan" {
			return []byte("target " + string(ssh.MarshalAuthorizedKey(publicKey))), nil
		}
		return nil, nil
	}}
	if _, err := verifier.VerifyConfiguration(context.Background(), config, TargetCredential{SchemaVersion: "v1", SSHPassword: "ssh-secret", SudoPassword: "sudo-secret"}, 0); err == nil || !strings.Contains(err.Error(), "host key changed") {
		t.Fatalf("error = %v", err)
	}
	if called {
		t.Fatal("SSH authentication was attempted after host key mismatch")
	}
}

func TestTargetStoreRejectsUnknownFieldsAndSymlinks(t *testing.T) {
	store := NewTargetStore(filepath.Join(t.TempDir(), "targets"), filepath.Join(t.TempDir(), "credentials"))
	config := testTargetConfiguration()
	if err := store.Write(config, TargetCredential{SchemaVersion: "v1", SSHPassword: "secret", SudoPassword: "secret"}); err != nil {
		t.Fatal(err)
	}
	configPath, _ := store.ConfigPath(config.Name)
	if err := os.WriteFile(configPath, []byte("schema_version: v1\nname: site-dev-196\nunknown: value\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load(config.Name); err == nil {
		t.Fatal("Load accepted unknown configuration field")
	}
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/tmp/not-a-target", configPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load(config.Name); err == nil {
		t.Fatal("Load accepted config symlink")
	}
}

func TestTargetValidationRejectsUnsafeNamesAndEnums(t *testing.T) {
	for _, name := range []string{"", "../target", "/target", "target name", "target/child"} {
		if err := ValidateTargetName(name); err == nil {
			t.Fatalf("accepted unsafe name %q", name)
		}
	}
	config := testTargetConfiguration()
	config.Authentication.Method = "token"
	if err := ValidateTargetConfiguration(config); err == nil {
		t.Fatal("accepted unknown authentication method")
	}
	config = testTargetConfiguration()
	config.Sudo.Method = "token"
	if err := ValidateTargetConfiguration(config); err == nil {
		t.Fatal("accepted unknown sudo method")
	}
}

func TestTargetStoreLoadsPublicKeyCredentialOnlyWithManagedIdentity(t *testing.T) {
	store := NewTargetStore(filepath.Join(t.TempDir(), "targets"), filepath.Join(t.TempDir(), "credentials"))
	source := filepath.Join(t.TempDir(), "identity")
	if err := os.WriteFile(source, []byte("private-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := testTargetConfiguration()
	config.Authentication.Method = "public_key"
	credential := TargetCredential{SchemaVersion: "v1", IdentityFile: "id_ed25519", SudoPassword: "sudo-secret"}
	if err := store.WriteIdentity(config.Name, source); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(config, credential); err != nil {
		t.Fatal(err)
	}
	if _, loaded, err := store.Load(config.Name); err != nil || loaded.IdentityFile != "id_ed25519" {
		t.Fatalf("Load() credential=%#v err=%v", loaded, err)
	}
	identityPath := filepath.Join(store.CredentialRoot, config.Name, "id_ed25519")
	if err := os.Remove(identityPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load(config.Name); err == nil {
		t.Fatal("Load accepted missing public identity")
	}
}

func testTargetConfiguration() TargetConfiguration {
	return TargetConfiguration{SchemaVersion: "v1", Name: "site-dev-196", Host: "10.159.56.197", Port: 22, User: "eva", Authentication: TargetAuthentication{Method: "password", CredentialRef: "site-dev-196"}, Sudo: TargetAuthentication{Method: "password", CredentialRef: "site-dev-196"}, HostKey: TargetHostKey{Algorithm: "ssh-ed25519", Fingerprint: "SHA256:abcdefghijklmnopqrstuvwxyz0123456789"}}
}
