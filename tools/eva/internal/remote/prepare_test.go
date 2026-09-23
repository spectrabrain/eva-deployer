package remote

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPrepareUsesOrderedBackendsAndStopsAfterFailure(t *testing.T) {
	resolved := writeOriginalRelease(t, true)
	if err := os.WriteFile(filepath.Join(resolved.Root, "checksums.sha256"), []byte("release checksums\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var called []string
	service := PrepareService{
		PreparationRoot: t.TempDir(),
		CacheRoot:       t.TempDir(),
		ResolveBackend:  func(relative string) (string, error) { return "/installed/" + relative, nil },
		Run: func(_ context.Context, options ProcessOptions) error {
			called = append(called, filepath.Base(options.Path))
			if options.Env["EVA_CACHE_ROOT"] == "" || options.Env["EVA_AGENT_QDRANT_SNAPSHOT_SOURCE"] != "harbor" || options.Env["EVA_AGENT_QDRANT_VALUES_FILE"] != "values-k3s.harbor.yaml" {
				t.Fatalf("Remote environment defaults = %#v", options.Env)
			}
			return errors.New("intentional backend failure")
		},
		Preflight:   readyPreflight(t),
		RuntimeRoot: writeRuntimeFixture(t),
	}
	_, err := service.Prepare(context.Background(), PrepareOptions{Release: resolved, Registry: "harbor.example.internal:32080"})
	if err == nil {
		t.Fatal("Prepare() succeeded after backend failure")
	}
	if want := []string{"download_offline_assets.sh"}; !reflect.DeepEqual(called, want) {
		t.Fatalf("backends invoked = %v, want %v", called, want)
	}
	store := NewManifestStore(service.PreparationRoot, nil)
	manifest, loadErr := store.Load(resolved.Metadata.Version)
	if loadErr != nil || manifest.Status != ManifestFailed || manifest.Steps[3].Status != StepFailed {
		t.Fatalf("failed manifest = %#v, error = %v", manifest, loadErr)
	}
}

func TestPrepareServiceUsesSeparateManagedCacheRoot(t *testing.T) {
	service := NewPrepareService()
	if service.PreparationRoot != DefaultPreparationRoot ||
		service.CacheRoot != DefaultRemoteCacheRoot ||
		service.HarborConfigPath != defaultManagedHarborConfig {
		t.Fatalf(
			"defaults preparation=%q cache=%q harbor=%q",
			service.PreparationRoot,
			service.CacheRoot,
			service.HarborConfigPath,
		)
	}
	preparation := t.TempDir()
	for _, cache := range []string{preparation, filepath.Join(preparation, "cache")} {
		if err := ensureRemoteCacheRoot(preparation, cache); err == nil {
			t.Fatalf("overlapping cache accepted: %s", cache)
		}
	}
	cache := t.TempDir()
	if err := ensureRemoteCacheRoot(preparation, cache); err != nil {
		t.Fatal(err)
	}
	if err := makePreparationLayout(preparation); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(preparation, "cache")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("release preparation cache directory exists: %v", err)
	}
}

func TestPrepareRecordsPreflightFailureBeforeBackends(t *testing.T) {
	resolved := writeOriginalRelease(t, true)
	if err := os.WriteFile(filepath.Join(resolved.Root, "checksums.sha256"), []byte("release checksums\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	preflight := readyPreflight(t)
	preflight.LookPath = func(name string) (string, error) { return "", errors.New("missing " + name) }
	service := PrepareService{PreparationRoot: t.TempDir(), CacheRoot: t.TempDir(), ResolveBackend: func(relative string) (string, error) { return "/installed/" + relative, nil }, Run: func(context.Context, ProcessOptions) error { called = true; return nil }, Preflight: preflight, RuntimeRoot: writeRuntimeFixture(t)}
	_, err := service.Prepare(context.Background(), PrepareOptions{Release: resolved, Registry: "harbor.example.internal:32080"})
	if err == nil || !strings.Contains(err.Error(), "Main preparation preflight failed") || called {
		t.Fatalf("Prepare() error=%v backend=%v", err, called)
	}
	manifest, loadErr := NewManifestStore(service.PreparationRoot, nil).Load(resolved.Metadata.Version)
	if loadErr != nil || manifest.Status != ManifestFailed || manifest.Steps[1].Status != StepFailed || strings.Contains(strings.ToLower(manifest.Steps[1].Error), "missing") {
		t.Fatalf("preflight manifest=%#v error=%v", manifest, loadErr)
	}
}

func readyPreflight(t *testing.T) Preflight {
	t.Helper()
	return Preflight{
		LookPath: func(string) (string, error) { return "/bin/tool", nil },
		Run:      func(context.Context, string, ...string) error { return nil },
		Version:  func(context.Context, string, ...string) (string, error) { return "test 1.0", nil },
		Dial: func(context.Context, string, string) (net.Conn, error) {
			left, right := net.Pipe()
			right.Close()
			return left, nil
		},
		HTTP: func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusUnauthorized, Body: http.NoBody}, nil
		},
		Credential:  func(string) bool { return true },
		AWSValidate: func(context.Context, AWSCredential) error { return nil },
		Docker:      func(context.Context) (string, string, error) { return "amd64", t.TempDir(), nil },
		PlaneReady:  func(string, string) error { return nil },
		DockerRoot:  t.TempDir(), Timeout: time.Second, ExternalSources: nil,
	}
}

func TestMergedEnvironmentOverridesConflictingValues(t *testing.T) {
	got := mergedEnvironment([]string{"KEEP=value", "EVA_AGENT_QDRANT_SNAPSHOT_SOURCE=local_pv"}, map[string]string{"EVA_AGENT_QDRANT_SNAPSHOT_SOURCE": "harbor"})
	values := map[string]string{}
	for _, entry := range got {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	if values["EVA_AGENT_QDRANT_SNAPSHOT_SOURCE"] != "harbor" || values["KEEP"] != "value" {
		t.Fatalf("merged environment = %#v", values)
	}
}
