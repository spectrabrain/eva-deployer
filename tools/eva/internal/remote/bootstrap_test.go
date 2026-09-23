package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testReceipt() HarborReceipt {
	return HarborReceipt{SchemaVersion: "v1", ManagedBy: "eva", Registry: "harbor.example.internal:32080", Project: "eva", HarborVersion: "2.15.2", InstallRoot: "/opt/eva/harbor", DataRoot: "/var/lib/eva/harbor", Protocol: "http"}
}

func withTestCredentialHooks(service BootstrapService) BootstrapService {
	service.Login = func(context.Context, string, string, string) error { return nil }
	service.Credential = func(string) bool { return true }
	service.Password = func(HarborReceipt) (string, error) { return "test-password", nil }
	return service
}

func TestBootstrapWritesAndReusesManagedReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbor.yaml")
	calls := 0
	service := withTestCredentialHooks(BootstrapService{EnsureRuntime: func(context.Context) error { calls++; return nil }, EnsureDocker: func(context.Context) error { calls++; return nil }, EnsureHarbor: func(context.Context, string, string, bool) (HarborReceipt, error) { calls++; return testReceipt(), nil }, CheckHarbor: func(context.Context, HarborReceipt) error { calls++; return nil }})
	service.Login = func(context.Context, string, string, string) error { calls++; return nil }
	if _, err := service.Bootstrap(context.Background(), BootstrapOptions{Registry: "harbor.example.internal:32080", Yes: true, ReceiptPath: path}); err != nil {
		t.Fatal(err)
	}
	if calls != 5 {
		t.Fatalf("bootstrap calls=%d", calls)
	}
	calls = 0
	if _, err := service.Bootstrap(context.Background(), BootstrapOptions{Registry: "harbor.example.internal:32080", Yes: true, ReceiptPath: path}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("reuse calls=%d", calls)
	}
}
func TestBootstrapFailsWithoutConfirmationOrOnConflict(t *testing.T) {
	service := NewBootstrapService()
	if _, err := service.Bootstrap(context.Background(), BootstrapOptions{Registry: "harbor.example.internal:32080"}); err == nil {
		t.Fatal("bootstrap without --yes succeeded")
	}
	path := filepath.Join(t.TempDir(), "harbor.yaml")
	if err := WriteHarborReceipt(path, testReceipt()); err != nil {
		t.Fatal(err)
	}
	service.CheckHarbor = func(context.Context, HarborReceipt) error { return errors.New("unreachable") }
	if _, err := service.Bootstrap(context.Background(), BootstrapOptions{Registry: "other.example.internal:32080", Yes: true, ReceiptPath: path}); err == nil {
		t.Fatal("conflicting receipt accepted")
	}
}

func TestBootstrapRevalidatesReceiptWithoutRegistryAndReplacesOnlyAfterChecks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbor.yaml")
	if err := WriteHarborReceipt(path, testReceipt()); err != nil {
		t.Fatal(err)
	}
	checks := 0
	service := withTestCredentialHooks(BootstrapService{
		EnsureRuntime: func(context.Context) error { return nil },
		EnsureDocker:  func(context.Context) error { return nil },
		EnsureHarbor: func(_ context.Context, registry, project string, _ bool) (HarborReceipt, error) {
			receipt := testReceipt()
			receipt.Registry, receipt.Project = registry, project
			return receipt, nil
		},
		CheckHarbor: func(context.Context, HarborReceipt) error { checks++; return nil },
	})
	if receipt, err := service.Bootstrap(context.Background(), BootstrapOptions{Yes: true, ReceiptPath: path}); err != nil || receipt.Registry != testReceipt().Registry || checks != 1 {
		t.Fatalf("receipt revalidation = %#v, %v, checks=%d", receipt, err, checks)
	}
	if _, err := service.Bootstrap(context.Background(), BootstrapOptions{Registry: "other.example.internal:32080", ReceiptPath: path, Yes: true}); err == nil {
		t.Fatal("registry conflict accepted without replacement")
	}
	if _, err := service.Bootstrap(context.Background(), BootstrapOptions{Registry: "other.example.internal:32080", ReceiptPath: path}); err == nil {
		t.Fatal("replacement accepted without confirmation")
	}
	if receipt, err := service.Bootstrap(context.Background(), BootstrapOptions{Registry: "other.example.internal:32080", ReceiptPath: path, Yes: true, ReplaceRegistry: true}); err != nil || receipt.Registry != "other.example.internal:32080" {
		t.Fatalf("replacement = %#v, %v", receipt, err)
	}
	stored, err := LoadHarborReceipt(path)
	if err != nil || stored.Registry != "other.example.internal:32080" {
		t.Fatalf("stored replacement = %#v, %v", stored, err)
	}
}

func TestBootstrapReplacementFailurePreservesExistingReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbor.yaml")
	if err := WriteHarborReceipt(path, testReceipt()); err != nil {
		t.Fatal(err)
	}
	service := withTestCredentialHooks(BootstrapService{
		EnsureRuntime: func(context.Context) error { return nil },
		EnsureDocker:  func(context.Context) error { return nil },
		EnsureHarbor: func(context.Context, string, string, bool) (HarborReceipt, error) {
			return HarborReceipt{}, errors.New("new registry is unavailable")
		},
		CheckHarbor: func(context.Context, HarborReceipt) error { return nil },
	})
	if _, err := service.Bootstrap(context.Background(), BootstrapOptions{Registry: "other.example.internal:32080", Yes: true, ReplaceRegistry: true, ReceiptPath: path}); err == nil {
		t.Fatal("failed replacement succeeded")
	}
	stored, err := LoadHarborReceipt(path)
	if err != nil || stored.Registry != testReceipt().Registry {
		t.Fatalf("replacement failure changed receipt: %#v, %v", stored, err)
	}
}

func TestBootstrapWritesReceiptOnlyAfterManagedCredentialIsReady(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbor.yaml")
	loginCalled := false
	service := withTestCredentialHooks(BootstrapService{
		EnsureRuntime: func(context.Context) error { return nil },
		EnsureDocker:  func(context.Context) error { return nil },
		EnsureHarbor:  func(context.Context, string, string, bool) (HarborReceipt, error) { return testReceipt(), nil },
		CheckHarbor:   func(context.Context, HarborReceipt) error { return nil },
	})
	service.Password = func(HarborReceipt) (string, error) { return "credential-not-logged", nil }
	service.Login = func(_ context.Context, registry, username, password string) error {
		loginCalled = registry == testReceipt().Registry && username == "admin" && password == "credential-not-logged"
		return errors.New("login rejected")
	}
	if _, err := service.Bootstrap(context.Background(), BootstrapOptions{Registry: testReceipt().Registry, Yes: true, ReceiptPath: path}); err == nil || !strings.Contains(err.Error(), "prepare Managed Harbor credential") {
		t.Fatalf("bootstrap error = %v", err)
	}
	if !loginCalled {
		t.Fatal("managed Harbor login did not receive expected registry and username")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("receipt written after login failure: %v", err)
	}
}

func TestBootstrapExternalHarborNeverUsesManagedCredential(t *testing.T) {
	receipt := testReceipt()
	receipt.ManagedBy = "external"
	path := filepath.Join(t.TempDir(), "harbor.yaml")
	if err := WriteHarborReceipt(path, receipt); err != nil {
		t.Fatal(err)
	}
	loginCalled, passwordCalled := false, false
	service := BootstrapService{
		CheckHarbor: func(context.Context, HarborReceipt) error { return nil },
		Credential:  func(string) bool { return true },
		Login:       func(context.Context, string, string, string) error { loginCalled = true; return nil },
		Password:    func(HarborReceipt) (string, error) { passwordCalled = true; return "secret", nil },
	}
	if _, err := service.Bootstrap(context.Background(), BootstrapOptions{Yes: true, ReceiptPath: path}); err != nil {
		t.Fatal(err)
	}
	if loginCalled || passwordCalled {
		t.Fatal("external Harbor used a managed credential source")
	}
}

func TestManagedHarborPasswordUsesMatchingExistingConfiguration(t *testing.T) {
	receipt := testReceipt()
	path := filepath.Join(t.TempDir(), "harbor.yml")
	if err := os.WriteFile(path, []byte("hostname: harbor.example.internal\nhttp:\n  port: 32080\nharbor_admin_password: preserved-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	password, err := managedHarborPasswordAt(receipt, path)
	if err != nil || password != "preserved-password" {
		t.Fatalf("password=%q err=%v", password, err)
	}
	if err := os.WriteFile(path, []byte("hostname: other.example.internal\nhttp:\n  port: 32080\nharbor_admin_password: preserved-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := managedHarborPasswordAt(receipt, path); err == nil {
		t.Fatal("password from unrelated Harbor config was accepted")
	}
}

func TestDockerLoginPassesPasswordOnlyOnStandardInput(t *testing.T) {
	directory := t.TempDir()
	arguments := filepath.Join(directory, "arguments")
	password := filepath.Join(directory, "password")
	script := filepath.Join(directory, "docker")
	contents := "#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" >\"$LOGIN_ARGUMENTS\"\ncat >\"$LOGIN_PASSWORD\"\n"
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("LOGIN_ARGUMENTS", arguments)
	t.Setenv("LOGIN_PASSWORD", password)
	if err := dockerLogin(context.Background(), "10.159.57.172:32080", "admin", "not-an-argument"); err != nil {
		t.Fatal(err)
	}
	gotArguments, err := os.ReadFile(arguments)
	if err != nil || strings.Contains(string(gotArguments), "not-an-argument") || !strings.Contains(string(gotArguments), "--password-stdin") {
		t.Fatalf("docker login arguments are unsafe: %q, %v", gotArguments, err)
	}
	gotPassword, err := os.ReadFile(password)
	if err != nil || string(gotPassword) != "not-an-argument" {
		t.Fatalf("docker login stdin = %q, %v", gotPassword, err)
	}
}
func TestHarborReceiptRejectsUnknownAndSensitiveFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbor.yaml")
	if err := WriteHarborReceipt(path, testReceipt()); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHarborReceipt(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("schema_version: v1\nmanaged_by: eva\nregistry: harbor.example.internal:32080\nproject: eva\nharbor_version: 2.15.2\ninstall_root: /opt/eva/harbor\ndata_root: /var/lib/eva/harbor\nprotocol: http\npassword: no\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHarborReceipt(path); err == nil {
		t.Fatal("sensitive receipt accepted")
	}
}

func TestSanitizeDockerDiagnosticRedactsAndBounds(
	t *testing.T,
) {
	password := "credential-not-logged"
	input := "login rejected " + password + " " +
		strings.Repeat("x", 700)

	actual := sanitizeDockerDiagnostic(input, password)

	if strings.Contains(actual, password) {
		t.Fatal("Docker diagnostic exposed password")
	}
	if !strings.Contains(actual, "[REDACTED]") {
		t.Fatal("Docker diagnostic did not redact password")
	}
	if len(actual) > 515 {
		t.Fatalf(
			"Docker diagnostic is not bounded: %d",
			len(actual),
		)
	}
}

func TestBootstrapPreparesManagedRegistryTransport(
	t *testing.T,
) {
	path := filepath.Join(t.TempDir(), "harbor.yaml")
	calls := []string{}

	service := withTestCredentialHooks(BootstrapService{
		EnsureRuntime: func(context.Context) error {
			calls = append(calls, "runtime")
			return nil
		},
		EnsureDocker: func(context.Context) error {
			calls = append(calls, "docker")
			return nil
		},
		EnsureRegistryTransport: func(
			_ context.Context,
			registry string,
		) error {
			calls = append(calls, "transport:"+registry)
			return nil
		},
		EnsureHarbor: func(
			context.Context,
			string,
			string,
			bool,
		) (HarborReceipt, error) {
			calls = append(calls, "harbor")
			return testReceipt(), nil
		},
		CheckHarbor: func(
			context.Context,
			HarborReceipt,
		) error {
			calls = append(calls, "check")
			return nil
		},
	})

	service.Login = func(
		context.Context,
		string,
		string,
		string,
	) error {
		calls = append(calls, "login")
		return nil
	}

	_, err := service.Bootstrap(
		context.Background(),
		BootstrapOptions{
			Registry:    testReceipt().Registry,
			Yes:         true,
			ReceiptPath: path,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	expected := []string{
		"runtime",
		"docker",
		"transport:" + testReceipt().Registry,
		"harbor",
		"login",
		"check",
	}
	if strings.Join(calls, ",") != strings.Join(expected, ",") {
		t.Fatalf("bootstrap calls=%v expected=%v", calls, expected)
	}
}

func TestBootstrapDoesNotConfigureExternalRegistryTransport(
	t *testing.T,
) {
	path := filepath.Join(t.TempDir(), "harbor.yaml")
	transportCalled := false

	receipt := testReceipt()
	receipt.ManagedBy = "external"
	receipt.HarborVersion = "external"

	service := BootstrapService{
		EnsureRuntime: func(context.Context) error {
			return nil
		},
		EnsureDocker: func(context.Context) error {
			return nil
		},
		EnsureRegistryTransport: func(
			context.Context,
			string,
		) error {
			transportCalled = true
			return nil
		},
		EnsureHarbor: func(
			context.Context,
			string,
			string,
			bool,
		) (HarborReceipt, error) {
			return receipt, nil
		},
		CheckHarbor: func(
			context.Context,
			HarborReceipt,
		) error {
			return nil
		},
		Credential: func(string) bool {
			return true
		},
	}

	_, err := service.Bootstrap(
		context.Background(),
		BootstrapOptions{
			Registry:       receipt.Registry,
			ExternalHarbor: true,
			Yes:            true,
			ReceiptPath:    path,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if transportCalled {
		t.Fatal("External Harbor changed Docker registry transport")
	}
}

func TestBootstrapRecoversManagedHarborAfterTransportRestart(
	t *testing.T,
) {
	receipt := testReceipt()

	path := filepath.Join(t.TempDir(), "harbor.yaml")
	if err := WriteHarborReceipt(path, receipt); err != nil {
		t.Fatal(err)
	}

	transportCalled := false
	probeCalled := false
	recoveryCalled := false
	loginCalled := false

	service := withTestCredentialHooks(BootstrapService{
		EnsureRegistryTransport: func(
			context.Context,
			string,
		) error {
			transportCalled = true
			return nil
		},
		ProbeManagedHarbor: func(
			_ context.Context,
			actual HarborReceipt,
			timeout time.Duration,
		) error {
			probeCalled = actual == receipt &&
				timeout == 10*time.Second
			return context.DeadlineExceeded
		},
		RecoverManagedHarbor: func(
			_ context.Context,
			actual HarborReceipt,
		) error {
			recoveryCalled = actual == receipt
			return nil
		},
		CheckHarbor: func(
			context.Context,
			HarborReceipt,
		) error {
			return nil
		},
	})

	service.Login = func(
		context.Context,
		string,
		string,
		string,
	) error {
		loginCalled = true
		return nil
	}

	if _, err := service.Bootstrap(
		context.Background(),
		BootstrapOptions{
			Yes:         true,
			ReceiptPath: path,
		},
	); err != nil {
		t.Fatal(err)
	}

	if !transportCalled {
		t.Fatal("registry transport was not checked")
	}
	if !probeCalled {
		t.Fatal("Managed Harbor readiness was not checked")
	}
	if !recoveryCalled {
		t.Fatal("Managed Harbor recovery was not called")
	}
	if !loginCalled {
		t.Fatal("Docker login was not called after recovery")
	}
}
