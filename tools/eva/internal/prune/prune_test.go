package prune

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"eva-deployer/tools/eva/internal/operation"

	"gopkg.in/yaml.v3"
)

type fixture struct {
	root      string
	inbox     string
	install   string
	payload   string
	receipt   string
	operation string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	f := fixture{
		root:      root,
		inbox:     filepath.Join(root, "inbox"),
		install:   filepath.Join(root, "releases"),
		payload:   filepath.Join(root, "artifacts-remote"),
		receipt:   filepath.Join(root, "current.yaml"),
		operation: filepath.Join(root, "operations"),
	}
	return f
}

func (f fixture) options(keep int) Options {
	return Options{
		Locations: []Location{
			{Label: "delivered release", Root: f.inbox},
			{Label: "prepared release", Root: f.install},
			{Label: "target payload", Root: f.payload},
		},
		CurrentReceiptPath: f.receipt,
		OperationRoot:      f.operation,
		Keep:               keep,
	}
}

func mkdirFile(t *testing.T, directory string, size int) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "data"), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeCurrent(t *testing.T, f fixture, version, root string) {
	t.Helper()
	contents := "schema_version: v1\nrelease_root: " + root + "\nrelease_version: " + version + "\nrelease_identity: x\nselected_by: test\nselected_at: now\n"
	if err := os.WriteFile(f.receipt, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeOperation(t *testing.T, f fixture, id, status, version string, updated time.Time) {
	t.Helper()
	directory := filepath.Join(f.operation, id)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	record := operation.Record{ID: id, Status: status, CreatedAt: updated, UpdatedAt: updated, SiteID: "site", ReleaseVersion: version, PlanPath: filepath.Join(directory, "plan.yaml")}
	contents, err := yaml.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "operation.yaml"), contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func versionsOf(entries []Entry) []string {
	out := []string{}
	for _, entry := range entries {
		out = append(out, filepath.Base(filepath.Dir(entry.Path))+"/"+entry.Version)
	}
	return out
}

func TestBuildProtectsCurrentOperationsAndNewest(t *testing.T) {
	f := newFixture(t)
	for _, version := range []string{"3.2.0", "3.3.0", "3.4.0-alpha.01", "3.4.0-alpha.02"} {
		mkdirFile(t, filepath.Join(f.inbox, version), 10)
		mkdirFile(t, filepath.Join(f.install, version), 5)
	}
	mkdirFile(t, filepath.Join(f.payload, "3.2.0", "0123", "cache"), 7)
	mkdirFile(t, filepath.Join(f.inbox, ".eva-preflight-abc"), 1) // in-flight probe: never touched
	if err := os.Symlink(filepath.Join(f.inbox, "3.2.0"), filepath.Join(f.inbox, "9.9.9")); err != nil {
		t.Fatal(err)
	}
	// Current points at the newest delivery; the cluster still runs 3.3.0
	// (last success) after a failed upgrade attempt on alpha.02.
	writeCurrent(t, f, "3.4.0-alpha.02", filepath.Join(f.inbox, "3.4.0-alpha.02"))
	base := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	writeOperation(t, f, "20261120T000000Z-site-00000001", operation.Succeeded, "3.2.0", base)
	writeOperation(t, f, "20261121T000000Z-site-00000002", operation.Succeeded, "3.3.0", base.Add(24*time.Hour))
	writeOperation(t, f, "20261125T000000Z-site-00000003", operation.Failed, "3.4.0-alpha.02", base.Add(5*24*time.Hour))

	plan, err := Build(f.options(1))
	if err != nil {
		t.Fatal(err)
	}
	wantDelete := []string{"inbox/3.4.0-alpha.01", "releases/3.4.0-alpha.01", "artifacts-remote/3.2.0", "inbox/3.2.0", "releases/3.2.0"}
	if got := versionsOf(plan.Delete); !slices.Equal(got, wantDelete) {
		t.Fatalf("delete = %v, want %v", got, wantDelete)
	}
	wantKeep := []string{"inbox/3.4.0-alpha.02", "releases/3.4.0-alpha.02", "inbox/3.3.0", "releases/3.3.0"}
	if got := versionsOf(plan.Keep); !slices.Equal(got, wantKeep) {
		t.Fatalf("keep = %v, want %v", got, wantKeep)
	}
	if reasons := strings.Join(plan.Keep[2].Reasons, ","); !strings.Contains(reasons, "last succeeded operation") {
		t.Fatalf("3.3.0 reasons = %s", reasons)
	}
	if plan.DeleteBytes() != 10+5+7+10+5 {
		t.Fatalf("bytes = %d", plan.DeleteBytes())
	}

	if err := Execute(plan, nil); err != nil {
		t.Fatal(err)
	}
	for _, entry := range plan.Delete {
		if _, err := os.Lstat(entry.Path); !os.IsNotExist(err) {
			t.Fatalf("%s still exists: %v", entry.Path, err)
		}
	}
	for _, kept := range append(plan.Keep, Entry{Path: filepath.Join(f.inbox, ".eva-preflight-abc")}) {
		if _, err := os.Stat(kept.Path); err != nil {
			t.Fatalf("%s removed: %v", kept.Path, err)
		}
	}
	// The symlink is skipped, and its target was a real deleted directory.
	if _, err := os.Lstat(filepath.Join(f.inbox, "9.9.9")); err != nil {
		t.Fatalf("symlink removed: %v", err)
	}
}

func TestBuildKeepsNewestVersionsAndPlannedOperations(t *testing.T) {
	f := newFixture(t)
	for _, version := range []string{"3.1.0", "3.2.0", "3.3.0", "3.4.0"} {
		mkdirFile(t, filepath.Join(f.install, version), 1)
	}
	writeOperation(t, f, "20261120T000000Z-site-00000001", operation.Planned, "3.1.0", time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC))
	writeOperation(t, f, "20261121T000000Z-site-00000002", operation.Failed, "3.2.0", time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC))

	plan, err := Build(f.options(2))
	if err != nil {
		t.Fatal(err)
	}
	// 3.4.0/3.3.0 newest, 3.1.0 planned, 3.2.0 latest operation: nothing to delete.
	if len(plan.Delete) != 0 {
		t.Fatalf("delete = %v", versionsOf(plan.Delete))
	}
}

func TestBuildRefusesWhenUnsure(t *testing.T) {
	f := newFixture(t)
	mkdirFile(t, filepath.Join(f.install, "3.1.0"), 1)

	if _, err := Build(f.options(0)); err == nil {
		t.Fatal("keep 0 accepted")
	}

	writeOperation(t, f, "20261120T000000Z-site-00000001", operation.Running, "3.4.0", time.Now())
	if _, err := Build(f.options(1)); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("running operation not refused: %v", err)
	}
	if err := os.RemoveAll(f.operation); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(f.receipt, []byte("release_root: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(f.options(1)); err == nil {
		t.Fatal("incomplete current receipt accepted")
	}
}

func TestBuildProtectsDirectoryHoldingCurrentRootUnderOtherName(t *testing.T) {
	f := newFixture(t)
	mkdirFile(t, filepath.Join(f.inbox, "v3.2.0"), 1)
	mkdirFile(t, filepath.Join(f.inbox, "3.3.0"), 1)
	mkdirFile(t, filepath.Join(f.inbox, "3.4.0"), 1)
	// Receipt version written without the "v" the directory uses.
	writeCurrent(t, f, "3.2.0", filepath.Join(f.inbox, "v3.2.0"))

	plan, err := Build(f.options(1))
	if err != nil {
		t.Fatal(err)
	}
	if got := versionsOf(plan.Delete); !slices.Equal(got, []string{"inbox/3.3.0"}) {
		t.Fatalf("delete = %v", got)
	}
}

func TestCompareVersions(t *testing.T) {
	ordered := []string{"3.4.0", "3.4.0-rc1", "3.4.0-alpha.10", "3.4.0-alpha.02", "3.4.0-alpha.1", "v3.3.0", "3.2.10", "3.2.9", "3.2"}
	for index := 0; index+1 < len(ordered); index++ {
		if compareVersions(ordered[index], ordered[index+1]) <= 0 {
			t.Fatalf("%s should sort after %s", ordered[index], ordered[index+1])
		}
	}
}

func TestExecuteRefusesUnexpectedPaths(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(f.root, "outside")
	mkdirFile(t, outside, 1)
	plan := Plan{Delete: []Entry{{Location: Location{Root: f.inbox}, Version: "3.1.0", Path: outside}}}
	if err := Execute(plan, nil); err == nil {
		t.Fatal("path outside location accepted")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside removed: %v", err)
	}
}
