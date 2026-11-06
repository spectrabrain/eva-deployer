package backup

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestFIFOFilesRefusedWithoutBlocking(t *testing.T) {
	for _, name := range []string{"metadata.json", "app-user-values.yaml"} {
		t.Run(name, func(t *testing.T) {
			root := testRoot(t)
			dir := saveFixture(t, root, fixtureMetadata(), false, "one")
			path := filepath.Join(dir, name)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			assertSafeFailure(t, root, "list")
			assertSafeFailure(t, root, "values", "--path", dir)
		})
	}
}

func TestDirectoryReplacedBySymlinkAfterRootOpenRefused(t *testing.T) {
	root := testRoot(t)
	dir := saveFixture(t, root, fixtureMetadata(), false, "one")
	parts, err := selectPath(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := openRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	site := filepath.Join(root, "site-1")
	if err := os.Rename(site, site+"-real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(site+"-real", site); err != nil {
		t.Fatal(err)
	}
	if child, err := descend(handle, parts); err == nil {
		child.Close()
		t.Fatal("followed a replacement symlink")
	}
}
