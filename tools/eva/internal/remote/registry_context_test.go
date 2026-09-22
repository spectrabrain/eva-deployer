package remote

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveRegistryContextUsesReceiptAndIsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbor.yaml")
	if err := WriteHarborReceipt(path, testReceipt()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	context, err := ResolveRegistryContext("", "", path)
	if err != nil {
		t.Fatal(err)
	}
	if context.Registry != testReceipt().Registry || context.Project != "eva" || context.Source != "receipt" {
		t.Fatalf("context = %#v", context)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("resolver changed receipt")
	}
}

func TestResolveRegistryContextExplicitAndConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbor.yaml")
	if err := WriteHarborReceipt(path, testReceipt()); err != nil {
		t.Fatal(err)
	}
	context, err := ResolveRegistryContext(testReceipt().Registry, "", path)
	if err != nil || context.Source != "explicit" {
		t.Fatalf("same explicit context = %#v, %v", context, err)
	}
	if _, err := ResolveRegistryContext("other.example.internal:32080", "", path); err == nil {
		t.Fatal("conflicting registry accepted")
	}
}

func TestResolveRegistryContextFailsClosedForMissingOrInvalidReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbor.yaml")
	if _, err := ResolveRegistryContext("", "", path); err == nil {
		t.Fatal("missing receipt accepted without explicit registry")
	}
	if _, err := ResolveRegistryContext("harbor.example.internal:32080", "", path); err != nil {
		t.Fatalf("explicit registry without receipt failed: %v", err)
	}
	if err := os.WriteFile(path, []byte("registry: harbor.example.internal:32080\npassword: secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveRegistryContext("", "", path); err == nil {
		t.Fatal("invalid receipt accepted")
	}
}
