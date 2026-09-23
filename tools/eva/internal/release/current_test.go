package release

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSelectPrefersExplicitThenCurrentThenWorkingDirectory(t *testing.T) {
	currentRoot := currentTestRelease(t)
	explicitRoot := currentTestRelease(t)
	receiptPath := filepath.Join(t.TempDir(), "releases", "current.yaml")
	current, err := Resolve(currentRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCurrentReceipt(receiptPath, current, "eva-tool-installer", time.Now()); err != nil {
		t.Fatal(err)
	}
	selected, err := Select(SelectionOptions{Explicit: explicitRoot, ReceiptPath: receiptPath})
	if err != nil {
		t.Fatal(err)
	}
	if selected.Source != SelectionExplicit || selected.Resolved.Root != explicitRoot {
		t.Fatalf("explicit selection = %#v", selected)
	}
	selected, err = Select(SelectionOptions{ReceiptPath: receiptPath})
	if err != nil {
		t.Fatal(err)
	}
	if selected.Source != SelectionCurrent || selected.Resolved.Root != currentRoot {
		t.Fatalf("current selection = %#v", selected)
	}
	loaded, err := LoadCurrentReceipt(receiptPath)
	if err != nil || loaded.ReleaseRoot != currentRoot {
		t.Fatalf("explicit override altered receipt: %#v, %v", loaded, err)
	}
}

func TestSelectUsesWorkingDirectoryOnlyWithoutReceipt(t *testing.T) {
	root := currentTestRelease(t)
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	selected, err := Select(SelectionOptions{ReceiptPath: filepath.Join(t.TempDir(), "missing.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if selected.Source != SelectionWorkingDirectory || selected.Resolved.Root != root {
		t.Fatalf("working-directory selection = %#v", selected)
	}
}

func TestSelectRejectsInvalidCurrentReceiptWithoutFallback(t *testing.T) {
	root := currentTestRelease(t)
	receiptPath := filepath.Join(t.TempDir(), "current.yaml")
	resolved, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCurrentReceipt(receiptPath, resolved, "eva-tool-installer", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "checksums.sha256"), []byte("changed\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Select(SelectionOptions{ReceiptPath: receiptPath}); err == nil {
		t.Fatal("Select accepted receipt whose identity no longer matches")
	}
}

func TestSelectResolvesSemanticVersionOnlyInsideRemoteInbox(t *testing.T) {
	inbox := t.TempDir()
	releaseRoot := filepath.Join(inbox, "3.2.0")
	if err := os.Mkdir(releaseRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"eva-tool.tar.gz", "eva-infra.tar.gz", "eva-solution.tar.gz"} {
		if err := os.WriteFile(filepath.Join(releaseRoot, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	metadata := "version: 3.2.0\nplatform:\n  os: " + runtime.GOOS + "\n  arch: " + runtime.GOARCH + "\nartifacts:\n"
	checksums := ""
	for _, name := range []string{"eva-tool.tar.gz", "eva-infra.tar.gz", "eva-solution.tar.gz"} {
		contents, _ := os.ReadFile(filepath.Join(releaseRoot, name))
		metadata += "  - name: " + strings.TrimSuffix(name, ".tar.gz") + "\n    file: " + name + "\n    sha256: " + checksum(string(contents)) + "\n"
		checksums += checksum(string(contents)) + "  " + name + "\n"
	}
	if err := os.WriteFile(filepath.Join(releaseRoot, "release.yaml"), []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(releaseRoot, "checksums.sha256"), []byte(checksums), 0o640); err != nil {
		t.Fatal(err)
	}
	selected, err := Select(SelectionOptions{Explicit: "3.2.0", InboxRoot: inbox, ReceiptPath: filepath.Join(t.TempDir(), "missing.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	if selected.Source != SelectionExplicit || selected.Resolved.Root != releaseRoot {
		t.Fatalf("inbox version selection = %#v", selected)
	}
}

func TestWriteCurrentReceiptUsesSafeModes(t *testing.T) {
	root := currentTestRelease(t)
	resolved, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "releases", "current.yaml")
	if err := WriteCurrentReceipt(path, resolved, "eva-tool-installer", time.Now()); err != nil {
		t.Fatal(err)
	}
	for candidate, want := range map[string]os.FileMode{filepath.Dir(path): 0o750, path: 0o640} {
		info, err := os.Stat(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("mode of %s = %o, want %o", candidate, got, want)
		}
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) == "" || string(contents) == "AWS credential" {
		t.Fatalf("unexpected receipt contents: %q", contents)
	}
}

func TestWriteCurrentReceiptRoundTripsYAMLSpecialReleaseRoot(t *testing.T) {
	for _, suffix := range []string{"release #1", "release key: value"} {
		t.Run(suffix, func(t *testing.T) {
			original := currentTestRelease(t)
			root := filepath.Join(t.TempDir(), suffix)
			if err := os.Rename(original, root); err != nil {
				t.Fatal(err)
			}
			resolved, err := Resolve(root)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "releases", "current.yaml")
			if err := WriteCurrentReceipt(path, resolved, "eva-tool-installer", time.Now()); err != nil {
				t.Fatal(err)
			}
			receipt, loaded, err := LoadCurrentRelease(path)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.ReleaseRoot != root || loaded.Root != root {
				t.Fatalf("receipt root=%q loaded root=%q, want %q", receipt.ReleaseRoot, loaded.Root, root)
			}
		})
	}
}

func TestWriteCurrentReceiptRejectsReceiptSymlink(t *testing.T) {
	root := currentTestRelease(t)
	resolved, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	target := filepath.Join(directory, "target.yaml")
	path := filepath.Join(directory, "current.yaml")
	if err := os.WriteFile(target, []byte("not a receipt\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := WriteCurrentReceipt(path, resolved, "eva-tool-installer", time.Now()); err == nil {
		t.Fatal("WriteCurrentReceipt accepted a receipt symlink")
	}
}

func currentTestRelease(t *testing.T) string {
	t.Helper()
	root := writeRelease(t, map[string]string{
		"eva-tool.tar.gz":     "tool",
		"eva-infra.tar.gz":    "infra",
		"eva-solution.tar.gz": "solution",
	})
	checksums := ""
	for _, name := range []string{"eva-tool.tar.gz", "eva-infra.tar.gz", "eva-solution.tar.gz"} {
		contents, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		checksums += checksum(string(contents)) + "  " + name + "\n"
	}
	if err := os.WriteFile(filepath.Join(root, "checksums.sha256"), []byte(checksums), 0o640); err != nil {
		t.Fatal(err)
	}
	return root
}
