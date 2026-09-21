package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testReceipt() HarborReceipt {
	return HarborReceipt{SchemaVersion: "v1", ManagedBy: "eva", Registry: "harbor.example.internal:32080", Project: "eva", HarborVersion: "2.15.2", InstallRoot: "/opt/eva/harbor", DataRoot: "/var/lib/eva/harbor", Protocol: "http"}
}
func TestBootstrapWritesAndReusesManagedReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbor.yaml")
	calls := 0
	service := BootstrapService{EnsureRuntime: func(context.Context) error { calls++; return nil }, EnsureDocker: func(context.Context) error { calls++; return nil }, EnsureHarbor: func(context.Context, string, string, bool) (HarborReceipt, error) { calls++; return testReceipt(), nil }, CheckHarbor: func(context.Context, HarborReceipt) error { calls++; return nil }}
	if _, err := service.Bootstrap(context.Background(), BootstrapOptions{Registry: "harbor.example.internal:32080", Yes: true, ReceiptPath: path}); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatalf("bootstrap calls=%d", calls)
	}
	calls = 0
	if _, err := service.Bootstrap(context.Background(), BootstrapOptions{Registry: "harbor.example.internal:32080", Yes: true, ReceiptPath: path}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
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
