package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

func TestPrepareExtractsAndReusesImmutableRelease(t *testing.T) {
	root := writeRelease(t, map[string]string{
		"eva-tool.tar.gz": "tool",
		"eva-infra.tar.gz": gzipTar(t, map[string]string{
			"ansible.cfg":                         "[defaults]\n",
			"src/playbook-preflight.yaml":         "---\n",
			"src/playbook-vars.yaml":              "---\n",
			"src/infra/playbooks/site_infra.yaml": "---\n",
		}),
		"eva-solution.tar.gz": gzipTar(t, map[string]string{
			"src/solution/playbooks/site_eva_app.yaml": "---\n",
		}),
	})
	resolved, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(resolved, filepath.Join(t.TempDir(), "releases"))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	for _, path := range []string{"ansible.cfg", "src/playbook-preflight.yaml", "src/infra/playbooks/site_infra.yaml", "src/solution/playbooks/site_eva_app.yaml", preparedMarkerName} {
		if _, err := os.Stat(filepath.Join(prepared.Root, path)); err != nil {
			t.Fatalf("prepared Release is missing %s: %v", path, err)
		}
	}
	preparedResolved, err := Resolve(prepared.Root)
	if err != nil || !preparedResolved.Prepared {
		t.Fatalf("Resolve(prepared) = %#v, %v", preparedResolved, err)
	}
	reused, err := Prepare(resolved, filepath.Dir(prepared.Root))
	if err != nil {
		t.Fatal(err)
	}
	if reused.Root != prepared.Root {
		t.Fatalf("reused root = %q, want %q", reused.Root, prepared.Root)
	}
}

func TestPrepareRejectsArchivePathTraversal(t *testing.T) {
	root := writeRelease(t, map[string]string{
		"eva-tool.tar.gz":     "tool",
		"eva-infra.tar.gz":    gzipTar(t, map[string]string{"../escape": "no"}),
		"eva-solution.tar.gz": gzipTar(t, map[string]string{"src/solution/placeholder": "ok"}),
	})
	resolved, err := Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(resolved, t.TempDir()); err == nil {
		t.Fatal("Prepare() accepted path traversal archive")
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

func gzipTar(t *testing.T, entries map[string]string) string {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(writer)
	for name, contents := range entries {
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(contents)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.String()
}
