package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

func TestWriteAskpassSecretUsesRestrictedFileOnly(t *testing.T) {
	directory := t.TempDir()
	secret, err := writeAskpassSecret(directory, "ssh-secret")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(secret)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("secret file = %v, %v", info, err)
	}
	contents, err := os.ReadFile(secret)
	if err != nil || string(contents) != "ssh-secret\n" {
		t.Fatalf("secret contents = %q, %v", contents, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("runtime directory must hold only the secret: %v, %v", entries, err)
	}
	if _, err := writeAskpassSecret(t.TempDir(), ""); err == nil {
		t.Fatal("empty SSH password was accepted")
	}
}

func TestServeAskpassRequiresModeAndPrivateSecret(t *testing.T) {
	directory := t.TempDir()
	secret, err := writeAskpassSecret(directory, "ssh-secret")
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	t.Setenv(askpassModeEnv, "")
	t.Setenv(askpassSecretEnv, secret)
	if handled, _ := ServeAskpassIfRequested(&output); handled || output.Len() != 0 {
		t.Fatal("askpass ran without askpass mode")
	}
	t.Setenv(askpassModeEnv, "1")
	if handled, err := ServeAskpassIfRequested(&output); !handled || err != nil || output.String() != "ssh-secret\n" {
		t.Fatalf("askpass handled=%v err=%v output=%q", handled, err, output.String())
	}
	if err := os.Chmod(secret, 0o644); err != nil {
		t.Fatal(err)
	}
	if handled, err := ServeAskpassIfRequested(&strings.Builder{}); !handled || err == nil {
		t.Fatal("askpass served a group/world-readable secret")
	}
	link := filepath.Join(directory, "link")
	if err := os.Chmod(secret, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv(askpassSecretEnv, link)
	if handled, err := ServeAskpassIfRequested(&strings.Builder{}); !handled || err == nil {
		t.Fatal("askpass followed a secret symlink")
	}
}

// Regression: /run is commonly mounted noexec. The askpass helper must not be
// executed from the runtime directory, which may only store the secret.
func TestRunAskpassSSHSucceedsOnNoexecRuntimeRoot(t *testing.T) {
	runtimeRoot := t.TempDir()
	if err := syscall.Mount("tmpfs", runtimeRoot, "tmpfs", syscall.MS_NOEXEC|syscall.MS_NOSUID|syscall.MS_NODEV, "mode=0700"); err != nil {
		t.Skipf("mounting a noexec tmpfs requires CAP_SYS_ADMIN (run under sudo or unshare -rm): %v", err)
	}
	t.Cleanup(func() { _ = syscall.Unmount(runtimeRoot, 0) })
	probe := filepath.Join(runtimeRoot, "probe")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(probe).Run(); err == nil {
		t.Fatal("test runtime root is not noexec")
	}
	// No location guard here: the result must come from the real noexec execve.
	runAskpassSSHWithFakeClient(t, runtimeRoot, false)
}

func TestRunAskpassSSHKeepsHelperOutsideRuntimeRoot(t *testing.T) {
	runAskpassSSHWithFakeClient(t, t.TempDir(), true)
}

func runAskpassSSHWithFakeClient(t *testing.T, runtimeRoot string, requireHelperOutsideRuntime bool) {
	t.Helper()
	previousRoot, previousExecutable := askpassRuntimeRoot, askpassExecutable
	t.Cleanup(func() { askpassRuntimeRoot, askpassExecutable = previousRoot, previousExecutable })
	askpassRuntimeRoot = runtimeRoot

	// The test binary stands in for the installed eva binary: TestMain serves
	// askpass mode exactly like cmd/eva main does.
	bin := t.TempDir()
	report := filepath.Join(bin, "report")
	locationGuard := ""
	if requireHelperOutsideRuntime {
		locationGuard = "case \"$SSH_ASKPASS\" in \"$ASKPASS_RUNTIME_ROOT\"/*) echo helper-in-runtime-root >\"$ASKPASS_REPORT\"; exit 1 ;; esac\n"
	}
	fakeSSH := "#!/bin/sh\n" + locationGuard +
		"[ -n \"$(ls \"$ASKPASS_RUNTIME_ROOT\")\" ] || exit 1\n" +
		"password=$(\"$SSH_ASKPASS\" 'eva@target password: ') || { echo askpass-exec-failed >\"$ASKPASS_REPORT\"; exit 1; }\n" +
		"[ \"$password\" = ssh-secret ] || { echo wrong-password >\"$ASKPASS_REPORT\"; exit 1; }\n" +
		"echo ok >\"$ASKPASS_REPORT\"\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(fakeSSH), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ASKPASS_RUNTIME_ROOT", runtimeRoot)
	t.Setenv("ASKPASS_REPORT", report)

	err := runAskpassSSH(context.Background(), []string{"eva@target", "true"}, "ssh-secret")
	result, _ := os.ReadFile(report)
	if err != nil || strings.TrimSpace(string(result)) != "ok" {
		t.Fatalf("runAskpassSSH err=%v report=%q", err, strings.TrimSpace(string(result)))
	}
	entries, err := os.ReadDir(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "askpass-") {
			t.Fatalf("askpass runtime directory was not removed: %s", entry.Name())
		}
	}
}

func TestMain(m *testing.M) {
	if handled, err := ServeAskpassIfRequested(os.Stdout); handled {
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
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

func TestTargetStoreListsTargetsWithAddressesAndReportsDamage(t *testing.T) {
	store := NewTargetStore(filepath.Join(t.TempDir(), "targets"), filepath.Join(t.TempDir(), "credentials"))
	if listings, err := store.List(); err != nil || len(listings) != 0 {
		t.Fatalf("missing registry listings = %#v, %v", listings, err)
	}
	for _, target := range []struct{ name, host string }{{"site-mg-c", "10.159.57.20"}, {"site-dev-196", "10.159.56.197"}} {
		config := testTargetConfiguration()
		config.Name, config.Host = target.name, target.host
		config.Authentication.CredentialRef, config.Sudo.CredentialRef = target.name, target.name
		if err := store.Write(config, TargetCredential{SchemaVersion: "v1", SSHPassword: "ssh-secret", SudoPassword: "sudo-secret"}); err != nil {
			t.Fatal(err)
		}
	}
	credentialPath, _ := store.CredentialPath("site-mg-c")
	if err := os.Remove(credentialPath); err != nil {
		t.Fatal(err)
	}
	listings, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listings) != 2 || listings[0].Name != "site-dev-196" || listings[1].Name != "site-mg-c" {
		t.Fatalf("listings = %#v", listings)
	}
	if listings[0].Err != nil || listings[0].Config.Host != "10.159.56.197" {
		t.Fatalf("healthy listing = %#v", listings[0])
	}
	if listings[1].Err == nil || listings[1].Config.Host != "10.159.57.20" {
		t.Fatalf("credential-damaged listing must keep its address and report an error: %#v", listings[1])
	}
}

// Regression: ssh joins remote arguments with spaces, so an unquoted empty
// sudo prompt vanished and `sudo -S -p "" true` reached the Target as a usage
// error regardless of the password. The fake transport reproduces that join.
func TestTargetSudoCommandsSurviveSSHArgumentJoin(t *testing.T) {
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	fakeSudo := "#!/bin/sh\n" +
		"if [ \"$1\" = -n ]; then shift; printf 'n:%s\\n' \"$*\" >>\"$SUDO_CALLS\"; exec true; fi\n" +
		"[ \"$1\" = -S ] && [ \"$2\" = -p ] && [ \"$3\" = '' ] && [ $# -ge 4 ] || { echo usage >&2; exit 1; }\n" +
		"shift 3; read -r password; [ \"$password\" = \"$SUDO_EXPECTED\" ] || exit 1\n" +
		"printf 'S:%s\\n' \"$*\" >>\"$SUDO_CALLS\"\n"
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte(fakeSudo), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUDO_CALLS", calls)
	t.Setenv("SUDO_EXPECTED", "it's a \"sudo\" secret")
	target := "eva@target.example.internal"
	run := func(ctx context.Context, path string, args []string, stdin []byte) ([]byte, error) {
		index := -1
		for position, arg := range args {
			if arg == target {
				index = position
			}
		}
		if path != "ssh" || index < 0 {
			t.Fatalf("unexpected command %s %q", path, args)
		}
		// Exactly what ssh sends: the remaining words joined by spaces.
		command := exec.CommandContext(ctx, "sh", "-c", strings.Join(args[index+1:], " "))
		command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		command.Stdin = strings.NewReader(string(stdin))
		return command.CombinedOutput()
	}
	verifier := TargetVerifier{Run: run}
	for _, method := range []string{"password", "passwordless"} {
		t.Run(method, func(t *testing.T) {
			_ = os.Remove(calls)
			config := testTargetConfiguration()
			if method == "passwordless" {
				config.Sudo = TargetAuthentication{Method: "passwordless"}
			}
			credential := TargetCredential{SchemaVersion: "v1", SSHPassword: "ssh-secret", SudoPassword: "it's a \"sudo\" secret"}
			if err := verifier.verifySudo(context.Background(), config, credential, "known_hosts", "control", target); err != nil {
				t.Fatalf("verifySudo: %v", err)
			}
			if err := verifier.probeInbox(context.Background(), config, credential, "known_hosts", "control", target); err != nil {
				t.Fatalf("probeInbox: %v", err)
			}
			recorded, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(recorded)), "\n")
			prefix := "S:"
			if method == "passwordless" {
				prefix = "n:"
			}
			if len(lines) != 5 || lines[0] != prefix+"true" || lines[1] != prefix+"mkdir -p /var/lib/eva/inbox/releases" {
				t.Fatalf("remote sudo calls = %q", lines)
			}
		})
	}
	if err := verifier.verifySudo(context.Background(), testTargetConfiguration(), TargetCredential{SchemaVersion: "v1", SSHPassword: "ssh-secret", SudoPassword: "wrong"}, "known_hosts", "control", target); err == nil {
		t.Fatal("wrong sudo password was accepted")
	}
}

func TestTargetStoreRemoveDeletesOnlyNamedTarget(t *testing.T) {
	store := NewTargetStore(filepath.Join(t.TempDir(), "targets"), filepath.Join(t.TempDir(), "credentials"))
	for _, name := range []string{"site-mg-c", "site-mg-x"} {
		config := testTargetConfiguration()
		config.Name, config.Authentication.CredentialRef, config.Sudo.CredentialRef = name, name, name
		if err := store.Write(config, TargetCredential{SchemaVersion: "v1", SSHPassword: "ssh-secret", SudoPassword: "sudo-secret"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Remove("site-mg-x"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(store.RegistryRoot, "site-mg-x"), filepath.Join(store.CredentialRoot, "site-mg-x")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived removal: %v", path, err)
		}
	}
	if _, _, err := store.Load("site-mg-c"); err != nil {
		t.Fatalf("other Target was affected: %v", err)
	}
	if err := store.Remove("site-mg-x"); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("second removal error = %v", err)
	}
	for _, name := range []string{"", ".", "..", "../targets", "a/b"} {
		if err := store.Remove(name); err == nil {
			t.Fatalf("unsafe name %q was accepted", name)
		}
	}
	if _, _, err := store.Load("site-mg-c"); err != nil {
		t.Fatalf("unsafe names affected another Target: %v", err)
	}
}

func TestTargetStoreRemoveHandlesPartialAndLinkedEntries(t *testing.T) {
	store := NewTargetStore(filepath.Join(t.TempDir(), "targets"), filepath.Join(t.TempDir(), "credentials"))
	config := testTargetConfiguration()
	if err := store.Write(config, TargetCredential{SchemaVersion: "v1", SSHPassword: "ssh-secret", SudoPassword: "sudo-secret"}); err != nil {
		t.Fatal(err)
	}
	// A credential without configuration is invisible to list but must still go.
	if err := os.RemoveAll(filepath.Join(store.RegistryRoot, config.Name)); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(config.Name); err != nil {
		t.Fatalf("orphaned credential was not removed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(store.CredentialRoot, config.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphaned credential survived: %v", err)
	}
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(store.RegistryRoot, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove("linked"); err == nil {
		t.Fatal("linked Target directory was accepted")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("symlink target contents were touched: %v", err)
	}
}
