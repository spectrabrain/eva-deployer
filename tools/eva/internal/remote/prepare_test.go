package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPrepareUsesOrderedBackendsAndStopsAfterFailure(t *testing.T) {
	resolved := writeOriginalRelease(t, true)
	if err := os.WriteFile(filepath.Join(resolved.Root, "checksums.sha256"), []byte("release checksums\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var called []string
	service := PrepareService{
		PreparationRoot: t.TempDir(),
		ResolveBackend:  func(relative string) (string, error) { return "/installed/" + relative, nil },
		Run: func(_ context.Context, options ProcessOptions) error {
			called = append(called, filepath.Base(options.Path))
			if options.Env["EVA_CACHE_ROOT"] == "" || options.Env["EVA_AGENT_QDRANT_SNAPSHOT_SOURCE"] != "harbor" || options.Env["EVA_AGENT_QDRANT_VALUES_FILE"] != "values-k3s.harbor.yaml" {
				t.Fatalf("Remote environment defaults = %#v", options.Env)
			}
			return errors.New("intentional backend failure")
		},
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
	if loadErr != nil || manifest.Status != ManifestFailed || manifest.Steps[1].Status != StepFailed {
		t.Fatalf("failed manifest = %#v, error = %v", manifest, loadErr)
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
