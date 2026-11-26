package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"eva-deployer/tools/eva/internal/prune"
)

func TestReleasePruneListsThenDeletesWithYes(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox")
	for _, version := range []string{"3.2.0", "3.3.0", "3.4.0"} {
		if err := os.MkdirAll(filepath.Join(inbox, version), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	receipt := filepath.Join(root, "current.yaml")
	if err := os.WriteFile(receipt, []byte("release_root: "+filepath.Join(inbox, "3.3.0")+"\nrelease_version: 3.3.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previousOptions, previousRoot := pruneOptions, pruneRequiresRoot
	t.Cleanup(func() { pruneOptions, pruneRequiresRoot = previousOptions, previousRoot })
	pruneOptions = func() prune.Options {
		return prune.Options{
			Locations:          []prune.Location{{Label: "delivered release", Root: inbox}},
			CurrentReceiptPath: receipt,
			OperationRoot:      filepath.Join(root, "operations"),
		}
	}
	pruneRequiresRoot = func() bool { return false }

	var output bytes.Buffer
	if err := runReleasePrune([]string{"--keep", "1"}, &output); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, want := range []string{"[INFO] current release: 3.3.0", "[KEEP]   3.4.0", "[KEEP]   3.3.0", "[DELETE] 3.2.0", "dry run: nothing deleted"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
	if _, err := os.Stat(filepath.Join(inbox, "3.2.0")); err != nil {
		t.Fatalf("dry run removed 3.2.0: %v", err)
	}

	output.Reset()
	if err := runReleasePrune([]string{"--keep", "1", "--yes"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "[DELETED] "+filepath.Join(inbox, "3.2.0")) {
		t.Fatalf("output:\n%s", output.String())
	}
	if _, err := os.Stat(filepath.Join(inbox, "3.2.0")); !os.IsNotExist(err) {
		t.Fatalf("3.2.0 not removed: %v", err)
	}

	pruneRequiresRoot = func() bool { return true }
	if err := runReleasePrune(nil, &output); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("non-root accepted: %v", err)
	}
}
