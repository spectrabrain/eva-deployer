package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const secretValues = "password: PRIVATE-TEST-SECRET\nscript: |\n  unchanged\n"

func testRoot(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("backup inspection runs on Linux Ansible targets")
	}
	return filepath.Join(t.TempDir(), "backups with space")
}

func fixtureMetadata() map[string]any {
	sum := sha256.Sum256([]byte(secretValues))
	return map[string]any{
		"created_at": "20261020T120000.123456Z", "site": "site-1", "namespace": "eva",
		"release": "custom-app", "component": "app", "revision": "7",
		"chart": "eva-app-3.3.0-beta.1+build.7", "app_version": "3.3.0-beta.1+build.7",
		"version": "3.3.0-beta.1+build.7", "version_source": "app_version",
		"release_status": "deployed", "values_kind": "user-supplied",
		"values_file": "app-user-values.yaml", "sha256": hex.EncodeToString(sum[:]), "sensitive": true,
	}
}

func saveFixture(t *testing.T, root string, m map[string]any, legacy bool, suffix string) string {
	t.Helper()
	third, _ := m["version"].(string)
	if legacy {
		third = m["release"].(string)
	}
	dir := filepath.Join(root, m["site"].(string), m["namespace"].(string), third, m["created_at"].(string)+"-"+suffix)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeMetadata(t, dir, m)
	if err := os.WriteFile(filepath.Join(dir, m["values_file"].(string)), []byte(secretValues), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeMetadata(t *testing.T, dir string, m map[string]any) {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func invoke(t *testing.T, root string, args ...string) (string, string, error) {
	t.Helper()
	var out, diagnostic bytes.Buffer
	args = append(args, "--root", root)
	err := Run(args, &out, &diagnostic)
	return out.String(), diagnostic.String(), err
}

func assertSafeFailure(t *testing.T, root string, args ...string) {
	t.Helper()
	out, diagnostic, err := invoke(t, root, args...)
	if err == nil {
		t.Fatal("expected safe refusal")
	}
	if out != "" || strings.Contains(diagnostic+err.Error(), "PRIVATE-TEST-SECRET") {
		t.Fatalf("unsafe failure: output=%q diagnostic=%q error=%v", out, diagnostic, err)
	}
}

func TestDefaultListAndShowNeverReadOrPrintValues(t *testing.T) {
	root := testRoot(t)
	m := fixtureMetadata()
	m["unrecognized_password"] = secretValues
	dir := saveFixture(t, root, m, false, "one")
	// A bad checksum deliberately proves metadata inspection doesn't read Values.
	if err := os.WriteFile(filepath.Join(dir, "app-user-values.yaml"), []byte("different secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{nil, {"list"}, {"show"}, {"show", "--latest"}} {
		out, diagnostic, err := invoke(t, root, command...)
		if err != nil || !strings.Contains(out, dir) || strings.Contains(out+diagnostic, "PRIVATE-TEST-SECRET") || strings.Contains(out, "different secret") || diagnostic != "" {
			t.Fatalf("metadata inspection: output=%q diagnostic=%q error=%v", out, diagnostic, err)
		}
	}
	assertSafeFailure(t, root, "values")
}

func TestValuesExplicitRawOutputWithWarningAndExactSelectors(t *testing.T) {
	root := testRoot(t)
	dir := saveFixture(t, root, fixtureMetadata(), false, "one")
	relative, _ := filepath.Rel(root, dir)
	for _, selector := range [][]string{
		{"--path", dir}, {"--path", relative}, {"--latest", "--component", "app"},
		{"--site", "site-1", "--namespace", "eva", "--release", "custom-app", "--version", "3.3.0-beta.1+build.7"},
	} {
		out, diagnostic, err := invoke(t, root, append([]string{"values"}, selector...)...)
		if err != nil || out != secretValues || !strings.Contains(diagnostic, "WARNING: sensitive") || strings.Contains(diagnostic, "PRIVATE-TEST-SECRET") {
			t.Fatalf("raw output: diagnostic=%q error=%v", diagnostic, err)
		}
	}
}

func TestLegacyAndVersionLayoutsFilteredByMetadata(t *testing.T) {
	root := testRoot(t)
	old := fixtureMetadata()
	old["created_at"] = "20261019T120000.000000Z"
	delete(old, "version")
	delete(old, "version_source")
	legacy := saveFixture(t, root, old, true, "old")
	newDir := saveFixture(t, root, fixtureMetadata(), false, "new")
	out, _, err := invoke(t, root, "list", "--component", "app", "--version", "3.3.0-beta.1+build.7", "--release", "custom-app")
	if err != nil || !strings.Contains(out, legacy) || !strings.Contains(out, newDir) || strings.Index(out, newDir) > strings.Index(out, legacy) {
		t.Fatalf("metadata discovery/order: %q, %v", out, err)
	}
	assertSafeFailure(t, root, "show", "--component", "app")
	out, _, err = invoke(t, root, "show", "--latest", "--component", "app")
	if err != nil || !strings.Contains(out, newDir) || strings.Contains(out, legacy) {
		t.Fatalf("latest: %q, %v", out, err)
	}
	for flag, wrong := range map[string]string{"--site": "another", "--namespace": "another", "--component": "iam", "--version": "9.0.0", "--release": "another"} {
		assertSafeFailure(t, root, "show", "--path", legacy, flag, wrong)
	}
}

func TestLegacyMissingVersionFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name, chart, want, source string
		app                       any
	}{
		{"blank", "eva-app-3.3.0-beta.1+build.7", "3.3.0-beta.1+build.7", "chart", ""},
		{"missing", "eva-app-3.2.1", "3.2.1", "chart", nil},
		{"null", "eva-app-v3.2.1-rc.1", "v3.2.1-rc.1", "chart", nil},
		{"unsafe", "eva-app-3.2.1", "3.2.1", "chart", "../../PRIVATE-TEST-SECRET"},
		{"unknown", "eva-app", "unknown-revision-7", "revision", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := testRoot(t)
			m := fixtureMetadata()
			delete(m, "version")
			delete(m, "version_source")
			m["app_version"], m["chart"] = tc.app, tc.chart
			if tc.name == "missing" {
				delete(m, "app_version")
			}
			saveFixture(t, root, m, true, "one")
			out, _, err := invoke(t, root, "show", "--version", tc.want)
			if err != nil || !strings.Contains(out, "version_source: "+tc.source) || strings.Contains(out, "PRIVATE-TEST-SECRET") {
				t.Fatalf("version fallback: %q, %v", out, err)
			}
		})
	}
}

func TestLatestCannotCrossReleaseContext(t *testing.T) {
	for _, field := range []string{"site", "namespace", "release", "component"} {
		t.Run(field, func(t *testing.T) {
			root := testRoot(t)
			first := fixtureMetadata()
			saveFixture(t, root, first, false, "one")
			second := fixtureMetadata()
			second[field] = "another"
			second["created_at"] = "20261021T120000.000000Z"
			if field == "component" {
				second["values_file"] = "another-user-values.yaml"
			}
			dir := saveFixture(t, root, second, false, "two")
			for _, command := range []string{"show", "values"} {
				assertSafeFailure(t, root, command)
				assertSafeFailure(t, root, command, "--latest")
			}
			out, _, err := invoke(t, root, "show", "--latest", "--"+field, "another")
			if err != nil || !strings.Contains(out, dir) {
				t.Fatalf("refined selection: %q, %v", out, err)
			}
		})
	}
}

func TestVersionMetadataFieldsAreIndependentlyOptional(t *testing.T) {
	for _, missing := range []string{"version", "version_source"} {
		t.Run(missing, func(t *testing.T) {
			root := testRoot(t)
			m := fixtureMetadata()
			dir := saveFixture(t, root, m, false, "one")
			delete(m, missing)
			writeMetadata(t, dir, m)
			if _, _, err := invoke(t, root, "values", "--path", dir); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLatestComponentStillRefusesMultipleReleasesOrSites(t *testing.T) {
	for _, field := range []string{"site", "release"} {
		t.Run(field, func(t *testing.T) {
			root := testRoot(t)
			saveFixture(t, root, fixtureMetadata(), false, "one")
			m := fixtureMetadata()
			m[field] = "another"
			saveFixture(t, root, m, false, "two")
			assertSafeFailure(t, root, "values", "--latest", "--component", "app")
		})
	}
}

func TestListDeterministicTimestampOrderingNotSemverOrdering(t *testing.T) {
	root := testRoot(t)
	old := fixtureMetadata()
	old["version"], old["app_version"] = "9.0.0", "9.0.0"
	old["created_at"] = "20261019T120000.000000Z"
	older := saveFixture(t, root, old, false, "one")
	one := saveFixture(t, root, fixtureMetadata(), false, "aaa")
	two := saveFixture(t, root, fixtureMetadata(), false, "zzz")
	out, _, err := invoke(t, root, "list")
	if err != nil || strings.Index(out, one) >= strings.Index(out, two) || strings.Index(out, two) >= strings.Index(out, older) {
		t.Fatalf("list order: %q, %v", out, err)
	}
	assertSafeFailure(t, root, "show", "--latest") // Tied timestamps need an exact path.
}

func TestMissingRootAndIncompleteRunsDoNotCreateAnything(t *testing.T) {
	root := testRoot(t)
	out, _, err := invoke(t, root, "list")
	if err != nil || !strings.Contains(out, "CREATED") {
		t.Fatalf("missing root list: %q, %v", out, err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("reader created root")
	}
	assertSafeFailure(t, root, "show")
	dir := saveFixture(t, root, fixtureMetadata(), false, "one")
	if err := os.Rename(filepath.Join(dir, "metadata.json"), filepath.Join(dir, "metadata.json.partial")); err != nil {
		t.Fatal(err)
	}
	out, _, err = invoke(t, root, "list")
	if err != nil || strings.Contains(out, "custom-app") {
		t.Fatalf("partial discovery: %q, %v", out, err)
	}
	assertSafeFailure(t, root, "values", "--path", dir)
}

func TestDangerousMetadataRefusedWithoutContents(t *testing.T) {
	for _, change := range []struct {
		field string
		value any
	}{
		{"site", "../../PRIVATE-TEST-SECRET"}, {"site", "wrong-site"}, {"namespace", "wrong-ns"},
		{"release", "bad\nPRIVATE-TEST-SECRET"}, {"component", "../escape"},
		{"values_file", "../../PRIVATE-TEST-SECRET"}, {"values_file", "/tmp/PRIVATE-TEST-SECRET"},
		{"values_file", "other-user-values.yaml"}, {"values_file", "app-user-values.yaml.partial"},
		{"revision", "0"}, {"sha256", "PRIVATE-TEST-SECRET"}, {"version", "../PRIVATE-TEST-SECRET"},
		{"version", "unrelated"}, {"version_source", "PRIVATE-TEST-SECRET"}, {"version_source", "chart"},
		{"created_at", "2026-10-20"}, {"created_at", "20261021T120000.000000Z"},
		{"chart", "PRIVATE-TEST-SECRET\x1b[31m"}, {"sensitive", false}, {"values_kind", "all"},
		{"release_status", "bad\nPRIVATE-TEST-SECRET"}, {"site", []string{"PRIVATE-TEST-SECRET"}},
	} {
		t.Run(change.field, func(t *testing.T) {
			root := testRoot(t)
			m := fixtureMetadata()
			dir := saveFixture(t, root, m, false, "one")
			m[change.field] = change.value
			writeMetadata(t, dir, m)
			assertSafeFailure(t, root, "list")
			assertSafeFailure(t, root, "values", "--path", dir)
		})
	}
}

func TestMetadataReadBoundAndInvalidJSON(t *testing.T) {
	for _, data := range []string{"null", "[]", "{PRIVATE-TEST-SECRET", strings.Repeat("x", maxMetadata+1)} {
		root := testRoot(t)
		dir := saveFixture(t, root, fixtureMetadata(), false, "one")
		if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		assertSafeFailure(t, root, "list")
	}
}

func TestSymlinksAtEveryDirectoryLevelAndRootAncestorRefused(t *testing.T) {
	for _, level := range []string{"root-ancestor", "root", "site", "namespace", "version", "run"} {
		t.Run(level, func(t *testing.T) {
			root := testRoot(t)
			dir := saveFixture(t, root, fixtureMetadata(), false, "one")
			path := map[string]string{"root-ancestor": filepath.Dir(root), "root": root,
				"site": filepath.Join(root, "site-1"), "namespace": filepath.Join(root, "site-1", "eva"),
				"version": filepath.Dir(dir), "run": dir}[level]
			if err := os.Rename(path, path+"-real"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(path+"-real", path); err != nil {
				t.Fatal(err)
			}
			assertSafeFailure(t, root, "list")
			assertSafeFailure(t, root, "values", "--path", dir)
		})
	}
}

func TestSymlinkFilesRefusedEvenForListAndWithinRoot(t *testing.T) {
	for _, name := range []string{"metadata.json", "app-user-values.yaml", "unused.partial"} {
		t.Run(name, func(t *testing.T) {
			root := testRoot(t)
			dir := saveFixture(t, root, fixtureMetadata(), false, "one")
			path := filepath.Join(dir, name)
			if name != "unused.partial" {
				if err := os.Rename(path, path+"-real"); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path+"-real", []byte(secretValues), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(path+"-real", path); err != nil {
				t.Fatal(err)
			}
			assertSafeFailure(t, root, "list")
			assertSafeFailure(t, root, "values", "--path", dir)
		})
	}
}

func TestTraversalAndOutsideRootRefused(t *testing.T) {
	root := testRoot(t)
	dir := saveFixture(t, root, fixtureMetadata(), false, "one")
	for _, path := range []string{"../PRIVATE-TEST-SECRET", root + "/../elsewhere", dir + "/../" + filepath.Base(dir),
		root + "-sibling/site-1/eva/v1/run", "/etc/passwd", root, "site-1/eva/3.3.0-beta.1+build.7", "site-1\\escape"} {
		assertSafeFailure(t, root, "values", "--path", path)
	}
	for _, rootPath := range []string{"/", root + "/../", root + "\nPRIVATE-TEST-SECRET"} {
		assertSafeFailure(t, rootPath, "list")
	}
	for _, flag := range []string{"--site", "--namespace", "--release", "--component", "--version"} {
		assertSafeFailure(t, root, "list", flag, "../PRIVATE-TEST-SECRET")
	}
}

func TestMissingValuesAndNonRegularFilesRefused(t *testing.T) {
	for _, name := range []string{"metadata.json", "app-user-values.yaml"} {
		root := testRoot(t)
		dir := saveFixture(t, root, fixtureMetadata(), false, "one")
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if name == "app-user-values.yaml" {
			assertSafeFailure(t, root, "show", "--path", dir)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		assertSafeFailure(t, root, "values", "--path", dir)
	}
}

func TestValuesLimitAndChecksumFailBeforeAnyOutput(t *testing.T) {
	root := testRoot(t)
	dir := saveFixture(t, root, fixtureMetadata(), false, "one")
	path := filepath.Join(dir, "app-user-values.yaml")
	if err := os.WriteFile(path, []byte("password: PRIVATE-TEST-SECRET-changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertSafeFailure(t, root, "values")
	if err := os.Truncate(path, maxValues+1); err != nil {
		t.Fatal(err)
	}
	assertSafeFailure(t, root, "values")
}

func TestReadOnlyFilesAndPermissionsUnchanged(t *testing.T) {
	root := testRoot(t)
	dir := saveFixture(t, root, fixtureMetadata(), false, "one")
	path := filepath.Join(dir, "metadata.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	for _, command := range []string{"list", "show", "values"} {
		if _, _, err := invoke(t, root, command); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := os.ReadFile(path)
	newInfo, _ := os.Stat(path)
	if !bytes.Equal(before, after) || newInfo.Mode() != info.Mode() || !newInfo.ModTime().Equal(info.ModTime()) {
		t.Fatal("inspection mutated backup metadata")
	}
}

func TestChangedCompletionMarkerAfterSelectionRefused(t *testing.T) {
	root := testRoot(t)
	m := fixtureMetadata()
	dir := saveFixture(t, root, m, false, "one")
	parts, err := selectPath(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := openRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	child, err := descend(handle, parts)
	if err != nil {
		t.Fatal(err)
	}
	s, complete, err := readSnapshot(child, parts)
	child.Close()
	if err != nil || !complete {
		t.Fatalf("read selection: complete=%v, error=%v", complete, err)
	}
	m["sha256"] = strings.Repeat("0", 64)
	writeMetadata(t, dir, m)
	values, err := readValues(handle, s)
	if err == nil || len(values) != 0 || strings.Contains(err.Error(), "PRIVATE-TEST-SECRET") {
		t.Fatalf("changed marker: bytes=%d, error=%v", len(values), err)
	}
}

func TestHelpAndInvalidOptions(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"help"}, {"list", "--help"}, {"values", "--help"}, {"show", "--help"}} {
		var out, diagnostic bytes.Buffer
		if err := Run(args, &out, &diagnostic); err != nil || !strings.Contains(out.String(), DefaultRoot) || diagnostic.Len() != 0 {
			t.Fatalf("help: %v, %v", args, err)
		}
	}
	root := testRoot(t)
	for _, args := range [][]string{
		{"unknown-PRIVATE-TEST-SECRET"}, {"--bad-PRIVATE-TEST-SECRET"}, {"list", "--latest"},
		{"list", "--path", "anything"}, {"show", "--latest", "--path", "anything"},
		{"values", "PRIVATE-TEST-SECRET"}, {"show", "--release"},
	} {
		assertSafeFailure(t, root, args...)
	}
}
