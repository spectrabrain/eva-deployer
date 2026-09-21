package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestManifestRoundTripAtomicWriteAndMode(t *testing.T) {
	clock := fixedClock()
	store := NewManifestStore(t.TempDir(), clock)
	manifest := testManifest(t, clock, []string{"validate-release"})
	if err := store.Save(manifest); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	path, err := store.ManifestPath(manifest.Release.ReleaseVersion)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("manifest mode = %o, want 640", got)
	}
	loaded, err := store.Load(manifest.Release.ReleaseVersion)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Release.ReleaseVersion != manifest.Release.ReleaseVersion ||
		loaded.Release.ReleaseYAMLSHA256 != manifest.Release.ReleaseYAMLSHA256 ||
		loaded.Release.ChecksumsSHA256 != manifest.Release.ChecksumsSHA256 ||
		loaded.Repository != manifest.Repository || loaded.Status != ManifestPending {
		t.Fatalf("loaded manifest = %#v", loaded)
	}
}

func TestManifestLoadRejectsUnknownAndMultipleDocuments(t *testing.T) {
	clock := fixedClock()
	store := NewManifestStore(t.TempDir(), clock)
	manifest := testManifest(t, clock, []string{"validate-release"})
	contents, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.ManifestPath(manifest.Release.ReleaseVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"unknown: value\n", "---\nschema_version: v1\n"} {
		if err := os.WriteFile(path, append(contents, []byte(suffix)...), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(manifest.Release.ReleaseVersion); err == nil {
			t.Fatalf("Load() accepted invalid manifest suffix %q", suffix)
		}
	}
}

func TestManifestValidationRejectsUnsafeIdentityStateAndEvidence(t *testing.T) {
	clock := fixedClock()
	for _, testCase := range []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"unsupported schema", func(manifest *Manifest) { manifest.SchemaVersion = "v2" }},
		{"invalid digest", func(manifest *Manifest) { manifest.Release.ReleaseYAMLSHA256 = "bad" }},
		{"invalid status", func(manifest *Manifest) { manifest.Status = "complete" }},
		{"duplicate steps", func(manifest *Manifest) { manifest.Steps = append(manifest.Steps, manifest.Steps[0]) }},
		{"absolute evidence", func(manifest *Manifest) { manifest.Steps[0].Evidence = []string{"/tmp/evidence"} }},
		{"secret evidence", func(manifest *Manifest) { manifest.Steps[0].Evidence = []string{"cache/.aws/credentials"} }},
		{"running step missing start", func(manifest *Manifest) { manifest.Steps[0].Status = StepRunning }},
		{"succeeded missing completion", func(manifest *Manifest) {
			manifest.Steps[0].Status = StepSucceeded
			manifest.Steps[0].StartedAt = clock()
			manifest.Steps[0].Evidence = []string{"cache/release.txt"}
		}},
		{"unsafe persisted error", func(manifest *Manifest) {
			manifest.Status = ManifestFailed
			manifest.CompletedAt = clock()
			manifest.Steps[0].Status = StepFailed
			manifest.Steps[0].StartedAt = clock()
			manifest.Steps[0].CompletedAt = clock()
			manifest.Steps[0].Error = "token=actual-secret"
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			manifest := testManifest(t, clock, []string{"validate-release"})
			testCase.mutate(&manifest)
			if err := ValidateManifest(manifest); err == nil {
				t.Fatal("ValidateManifest() accepted invalid manifest")
			}
		})
	}
}

func TestRegistryAndProjectValidation(t *testing.T) {
	for _, value := range []string{"", "https://harbor.example", "harbor.example/", "localhost", "localhost:443", "127.0.0.1:32080", "-registry", "harbor.example/path", "[::1]:443"} {
		if _, err := ValidateRegistry(value); err == nil {
			t.Fatalf("ValidateRegistry(%q) succeeded", value)
		}
	}
	for _, value := range []string{"10.159.56.124:32080", "harbor.main.internal", "harbor.main.internal:443"} {
		if got, err := ValidateRegistry(value); err != nil || got != value {
			t.Fatalf("ValidateRegistry(%q) = %q, %v", value, got, err)
		}
	}
	if got, err := ValidateProject(""); err != nil || got != "eva" {
		t.Fatalf("ValidateProject(default) = %q, %v", got, err)
	}
	for _, value := range []string{"team/project", "https:project", "with space", "harbor-token", "secret"} {
		if _, err := ValidateProject(value); err == nil {
			t.Fatalf("ValidateProject(%q) succeeded", value)
		}
	}
}

func TestManifestIdentityMismatchIsRejected(t *testing.T) {
	clock := fixedClock()
	store := NewManifestStore(t.TempDir(), clock)
	manifest := testManifest(t, clock, []string{"validate-release"})
	if err := store.Save(manifest); err != nil {
		t.Fatal(err)
	}
	different := manifest.Release
	different.RepositoryRegistry = "harbor.other.internal"
	if _, _, err := store.LoadOrCreate(different, []string{"validate-release"}); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("LoadOrCreate(identity mismatch) error = %v", err)
	}
}

func TestBuildPreparationIdentityUsesReleaseAndChecksumDigests(t *testing.T) {
	resolved := writeOriginalRelease(t, true)
	checksums := "test checksums manifest\n"
	if err := os.WriteFile(filepath.Join(resolved.Root, "checksums.sha256"), []byte(checksums), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := BuildPreparationIdentity(resolved, "harbor.main.internal:443", "")
	if err != nil {
		t.Fatalf("BuildPreparationIdentity() error = %v", err)
	}
	if identity.ReleaseVersion != resolved.Metadata.Version || identity.RepositoryProject != "eva" || identity.RepositoryRegistry != "harbor.main.internal:443" || len(identity.ReleaseYAMLSHA256) != 64 || len(identity.ChecksumsSHA256) != 64 {
		t.Fatalf("identity = %#v", identity)
	}
}

func TestManifestSaveFailureDoesNotReplaceExistingManifest(t *testing.T) {
	clock := fixedClock()
	store := NewManifestStore(t.TempDir(), clock)
	manifest := testManifest(t, clock, []string{"validate-release"})
	if err := store.Save(manifest); err != nil {
		t.Fatal(err)
	}
	store.beforeRename = func() error { return os.ErrPermission }
	manifest.UpdatedAt = manifest.UpdatedAt.Add(time.Minute)
	if err := store.Save(manifest); err == nil {
		t.Fatal("Save() succeeded despite injected pre-rename failure")
	}
	loaded, err := store.Load(manifest.Release.ReleaseVersion)
	if err != nil || loaded.UpdatedAt != fixedClock()() {
		t.Fatalf("existing manifest changed after failed save: %#v, %v", loaded, err)
	}
}

func testManifest(t *testing.T, clock Clock, names []string) Manifest {
	t.Helper()
	manifest, err := NewManifest(PreparationIdentity{
		ReleaseVersion:     "3.2.0",
		ReleaseYAMLSHA256:  strings.Repeat("a", 64),
		ChecksumsSHA256:    strings.Repeat("b", 64),
		RepositoryRegistry: "harbor.main.internal:443",
		RepositoryProject:  "eva",
	}, names, clock)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func fixedClock() Clock {
	return func() time.Time { return time.Date(2026, 9, 21, 1, 2, 3, 0, time.UTC) }
}
