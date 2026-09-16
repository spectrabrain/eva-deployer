package argocd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteAndLoadReceipt(t *testing.T) {
	root := t.TempDir()
	completedAt := time.Date(
		2026,
		9,
		16,
		12,
		48,
		0,
		0,
		time.UTC,
	)

	expected := Receipt{
		SchemaVersion: receiptSchemaVersion,
		SiteID:        testWorkspaceSite,
		ClusterName:   testLegacyCluster,
		ClusterServer: testClusterServer,
		Applications: []string{
			"legacy-a-eva-app",
			"legacy-a-eva-agent",
		},
		CompletedAt: completedAt,
	}

	if err := WriteReceipt(root, expected); err != nil {
		t.Fatalf("WriteReceipt() error = %v", err)
	}

	path := ReceiptPath(root, testWorkspaceSite)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Fatalf(
			"receipt mode = %#o, want 0600",
			info.Mode().Perm(),
		)
	}

	siteInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}

	if siteInfo.Mode().Perm() != 0o700 {
		t.Fatalf(
			"site directory mode = %#o, want 0700",
			siteInfo.Mode().Perm(),
		)
	}

	loaded, err := LoadReceipt(root, testWorkspaceSite)
	if err != nil {
		t.Fatalf("LoadReceipt() error = %v", err)
	}

	if loaded.SiteID != expected.SiteID ||
		loaded.ClusterName != expected.ClusterName ||
		loaded.ClusterServer != expected.ClusterServer ||
		!loaded.CompletedAt.Equal(completedAt) {
		t.Fatalf("loaded receipt = %#v", loaded)
	}

	detection := Detection{
		SiteID: testWorkspaceSite,
		Applications: []string{
			"legacy-a-eva-app",
		},
	}

	if err := ReceiptCovers(loaded, detection); err != nil {
		t.Fatalf("ReceiptCovers() error = %v", err)
	}
}

func TestLoadReceiptRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	siteDirectory := filepath.Join(root, testWorkspaceSite)

	if err := os.MkdirAll(siteDirectory, 0o700); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(t.TempDir(), "receipt.yaml")
	if err := os.WriteFile(
		outside,
		[]byte("schema_version: v1\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(
		outside,
		filepath.Join(siteDirectory, receiptFileName),
	); err != nil {
		t.Fatal(err)
	}

	_, err := LoadReceipt(root, testWorkspaceSite)
	if err == nil ||
		!strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf(
			"LoadReceipt() error = %v, want symlink error",
			err,
		)
	}
}

func TestReceiptCoversRejectsUnknownApplication(
	t *testing.T,
) {
	receipt := Receipt{
		SchemaVersion: receiptSchemaVersion,
		SiteID:        testWorkspaceSite,
		ClusterName:   testLegacyCluster,
		Applications: []string{
			"legacy-a-eva-app",
		},
		CompletedAt: time.Now().UTC(),
	}

	err := ReceiptCovers(
		receipt,
		Detection{
			SiteID: testWorkspaceSite,
			Applications: []string{
				"legacy-a-eva-vision",
			},
		},
	)

	if err == nil ||
		!strings.Contains(err.Error(), "does not cover") {
		t.Fatalf("ReceiptCovers() error = %v", err)
	}
}

func TestLoadReceiptRejectsWrongSite(t *testing.T) {
	root := t.TempDir()

	receipt := Receipt{
		SchemaVersion: receiptSchemaVersion,
		SiteID:        testWorkspaceSite,
		ClusterName:   testLegacyCluster,
		Applications: []string{
			"legacy-a-eva-app",
		},
		CompletedAt: time.Now().UTC(),
	}

	if err := WriteReceipt(root, receipt); err != nil {
		t.Fatal(err)
	}

	source := ReceiptPath(root, testWorkspaceSite)
	destinationDirectory := filepath.Join(root, "customer-b")

	if err := os.MkdirAll(
		destinationDirectory,
		0o700,
	); err != nil {
		t.Fatal(err)
	}

	contents, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(
		filepath.Join(
			destinationDirectory,
			receiptFileName,
		),
		contents,
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	_, err = LoadReceipt(root, "customer-b")
	if err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("LoadReceipt() error = %v", err)
	}
}
