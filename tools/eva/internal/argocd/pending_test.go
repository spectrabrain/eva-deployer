package argocd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteLoadAndRemovePending(t *testing.T) {
	root := t.TempDir()
	createdAt := time.Date(
		2026,
		9,
		16,
		13,
		7,
		49,
		0,
		time.UTC,
	)

	expected := testPendingHandoff(createdAt)

	if err := WritePending(root, expected); err != nil {
		t.Fatalf("WritePending() error = %v", err)
	}

	path := PendingPath(root, testWorkspaceSite)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Fatalf(
			"pending mode = %#o, want 0600",
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

	loaded, err := LoadPending(
		root,
		testWorkspaceSite,
	)
	if err != nil {
		t.Fatalf("LoadPending() error = %v", err)
	}

	if loaded.SiteID != expected.SiteID ||
		loaded.ClusterName != expected.ClusterName ||
		loaded.ClusterServer != expected.ClusterServer ||
		loaded.RegistrationCommit !=
			expected.RegistrationCommit ||
		!loaded.CreatedAt.Equal(createdAt) {
		t.Fatalf("loaded pending = %#v", loaded)
	}

	receipt := pendingToReceipt(
		loaded,
		createdAt.Add(time.Minute),
	)

	if !hasCompleteGitReceiptMetadata(receipt) {
		t.Fatalf(
			"pending receipt conversion = %#v",
			receipt,
		)
	}

	if err := RemovePending(
		root,
		testWorkspaceSite,
	); err != nil {
		t.Fatalf("RemovePending() error = %v", err)
	}

	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf(
			"pending path still exists: %v",
			err,
		)
	}

	if err := RemovePending(
		root,
		testWorkspaceSite,
	); err != nil {
		t.Fatalf(
			"idempotent RemovePending() error = %v",
			err,
		)
	}
}

func TestLoadPendingRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	siteDirectory := filepath.Join(
		root,
		testWorkspaceSite,
	)

	if err := os.MkdirAll(
		siteDirectory,
		0o700,
	); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(
		t.TempDir(),
		"pending.yaml",
	)

	if err := os.WriteFile(
		outside,
		[]byte("schema_version: v1\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(
		outside,
		PendingPath(root, testWorkspaceSite),
	); err != nil {
		t.Fatal(err)
	}

	_, err := LoadPending(
		root,
		testWorkspaceSite,
	)

	if err == nil ||
		!strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf(
			"LoadPending() error = %v, want symlink error",
			err,
		)
	}

	if err := RemovePending(
		root,
		testWorkspaceSite,
	); err == nil ||
		!strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf(
			"RemovePending() error = %v, want symlink error",
			err,
		)
	}
}

func TestPendingRequiresCompleteGitMetadata(
	t *testing.T,
) {
	pending := testPendingHandoff(time.Now().UTC())

	pending.RegistrationCommit = ""

	if err := validatePending(pending); err == nil {
		t.Fatal(
			"pending with incomplete Git metadata succeeded",
		)
	}

	pending = testPendingHandoff(time.Now().UTC())
	pending.Applications = append(
		pending.Applications,
		pending.Applications[0],
	)

	if err := validatePending(pending); err == nil {
		t.Fatal(
			"pending with duplicate Application succeeded",
		)
	}
}

func TestPendingDoesNotCreateFinalReceipt(
	t *testing.T,
) {
	root := t.TempDir()

	if err := WritePending(
		root,
		testPendingHandoff(time.Now().UTC()),
	); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadReceipt(
		root,
		testWorkspaceSite,
	); err == nil {
		t.Fatal(
			"pending handoff unexpectedly created a final receipt",
		)
	}
}

func testPendingHandoff(
	createdAt time.Time,
) PendingHandoff {
	return PendingHandoff{
		SchemaVersion: pendingSchemaVersion,
		SiteID:        testWorkspaceSite,
		ClusterName:   testLegacyCluster,
		ClusterServer: testClusterServer,
		Applications: []string{
			"legacy-a-eva-agent",
			"legacy-a-eva-app",
		},
		RegistrationApplication: registrationApplicationName,
		RegistrationRepository:  "http://mod.lge.com/hub/prism/eva-argo-shee.git",
		RegistrationBranch:      "main",
		RegistrationManifest:    "registration/clusters/legacy-a.yaml",
		RegistrationCommit:      testGitCommit,
		CreatedAt:               createdAt,
	}
}
