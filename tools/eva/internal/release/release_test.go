package release

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveValidDirectory(t *testing.T) {
	root := writeRelease(t, map[string]string{
		"eva-tool.tar.gz":     "tool",
		"eva-infra.tar.gz":    "infra",
		"eva-solution.tar.gz": "solution",
	})

	resolved, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Metadata.Version != "3.2.0" || resolved.Root != root {
		t.Fatalf("unexpected release: %#v", resolved)
	}
}

func TestResolveRejectsChecksumMismatch(t *testing.T) {
	root := writeRelease(t, map[string]string{
		"eva-tool.tar.gz":     "tool",
		"eva-infra.tar.gz":    "infra",
		"eva-solution.tar.gz": "solution",
	})
	if err := os.WriteFile(filepath.Join(root, "eva-tool.tar.gz"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(root); err == nil {
		t.Fatal("Resolve() accepted an artifact checksum mismatch")
	}
}

func TestResolveRejectsTagUntilS3ResolverExists(t *testing.T) {
	if _, err := Resolve("3.2.0"); err == nil {
		t.Fatal("Resolve() accepted a tag without an S3 resolver")
	}
}

func writeRelease(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	metadata := fmt.Sprintf(`version: 3.2.0
platform:
  os: %s
  arch: %s
artifacts:
  - name: eva-tool
    file: eva-tool.tar.gz
    sha256: %s
  - name: eva-infra
    file: eva-infra.tar.gz
    sha256: %s
  - name: eva-solution
    file: eva-solution.tar.gz
    sha256: %s
`, runtime.GOOS, runtime.GOARCH,
		checksum(files["eva-tool.tar.gz"]),
		checksum(files["eva-infra.tar.gz"]),
		checksum(files["eva-solution.tar.gz"]),
	)
	if err := os.WriteFile(filepath.Join(root, "release.yaml"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func checksum(value string) string {
	hash := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", hash)
}
