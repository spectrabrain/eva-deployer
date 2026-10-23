package remote

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerCredentialPresentUsesDockerConfigOverride(t *testing.T) {
	configRoot := t.TempDir()
	t.Setenv("DOCKER_CONFIG", configRoot)
	for _, key := range []string{"10.159.57.172:32080", "https://10.159.57.172:32080"} {
		contents := []byte(`{"auths":{"` + key + `":{}}}`)
		if err := os.WriteFile(filepath.Join(configRoot, "config.json"), contents, 0o600); err != nil {
			t.Fatal(err)
		}
		if !dockerCredentialPresent("10.159.57.172:32080") {
			t.Fatalf("credential key %q in DOCKER_CONFIG was not found", key)
		}
	}
}

func TestPreflightChecksAllCategoriesWithoutSensitiveReport(t *testing.T) {
	p := readyPreflight(t)
	report, err := p.Check(context.Background(), PreflightOptions{ReleaseRoot: t.TempDir(), PreparationRoot: t.TempDir(), Registry: "harbor.example.internal:32080", Project: "eva"})
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if report.SchemaVersion != preflightSchemaVersion || len(report.Categories) != 6 || report.Registry != "harbor.example.internal:32080" {
		t.Fatalf("report = %#v", report)
	}
	serialized := report.Registry + report.Project + strings.Join(report.Categories, ",")
	for _, secret := range []string{"token", "password", "AKIA", "authorization"} {
		if strings.Contains(strings.ToLower(serialized), strings.ToLower(secret)) {
			t.Fatalf("report contains sensitive value %q", secret)
		}
	}
}

func TestPreflightBoundsToolProbe(t *testing.T) {
	p := readyPreflight(t)
	p.Timeout = 10 * time.Millisecond
	p.Version = func(ctx context.Context, _ string, _ ...string) (string, error) { <-ctx.Done(); return "", ctx.Err() }
	_, err := p.Check(context.Background(), PreflightOptions{ReleaseRoot: t.TempDir(), PreparationRoot: t.TempDir(), Registry: "harbor.example.internal:32080", Project: "eva"})
	if err == nil || !strings.Contains(err.Error(), "host tools") {
		t.Fatalf("Check() error = %v", err)
	}
}

func TestPreflightFailsClosedBeforeAnyExternalProbe(t *testing.T) {
	p := readyPreflight(t)
	p.LookPath = func(name string) (string, error) {
		if name == "aws" {
			return "", errors.New("missing")
		}
		return "/bin/tool", nil
	}
	called := false
	p.Dial = func(context.Context, string, string) (net.Conn, error) {
		called = true
		return nil, errors.New("unexpected")
	}
	_, err := p.Check(context.Background(), PreflightOptions{ReleaseRoot: t.TempDir(), PreparationRoot: t.TempDir(), Registry: "harbor.example.internal:32080", Project: "eva"})
	if err == nil || !strings.Contains(err.Error(), "host tools") || called {
		t.Fatalf("Check() error=%v external=%v", err, called)
	}
}
