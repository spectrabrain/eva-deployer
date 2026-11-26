package operation

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunLockLiveness(t *testing.T) {
	root := t.TempDir()
	record := Record{ID: "20261125T120633Z-site-0fa472cb", Status: Running}
	if err := os.MkdirAll(filepath.Join(root, record.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := RunLiveness(root, record); err != nil || got != LivenessUnknown {
		t.Fatalf("no lock file: %q %v", got, err)
	}
	lock, err := AcquireRunLock(root, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := RunLiveness(root, record); err != nil || got != LivenessActive {
		t.Fatalf("held lock: %q %v", got, err)
	}
	if _, err := AcquireRunLock(root, record.ID); err == nil {
		t.Fatal("second holder accepted")
	}
	lock.Release()
	if got, err := RunLiveness(root, record); err != nil || got != LivenessInterrupted {
		t.Fatalf("released lock: %q %v", got, err)
	}
}
