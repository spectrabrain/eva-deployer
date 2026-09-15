package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveValidatesManagedTools(t *testing.T) {
	root := createRuntime(t)
	resolved, err := Resolve(root)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Descriptor.Version != "3.2.0" {
		t.Fatalf("runtime version = %q", resolved.Descriptor.Version)
	}
	path, err := resolved.ToolPath("ansible-playbook")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "venv", "bin", "ansible-playbook"); path != want {
		t.Fatalf("ToolPath() = %q, want %q", path, want)
	}
	if _, err := resolved.ToolPath("sh"); err == nil {
		t.Fatal("ToolPath(sh) unexpectedly succeeded")
	}
}

func TestResolveRejectsToolEscapingRuntimeRoot(t *testing.T) {
	root := createRuntime(t)
	descriptor := filepath.Join(root, descriptorName)
	contents, err := os.ReadFile(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	contents = []byte(strings.Replace(string(contents), "bin/helm", "../helm", 1))
	if err := os.WriteFile(descriptor, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(root); err == nil || !strings.Contains(err.Error(), "escapes the runtime root") {
		t.Fatalf("Resolve() error = %v, want root escape error", err)
	}
}

func TestResolveRejectsSymlinkOutsideRuntimeRoot(t *testing.T) {
	root := createRuntime(t)
	outside := filepath.Join(t.TempDir(), "helm")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "bin", "helm")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "bin", "helm")); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(root); err == nil || !strings.Contains(err.Error(), "resolves outside the runtime root") {
		t.Fatalf("Resolve() error = %v, want external symlink error", err)
	}
}

func TestInstallPublishesValidatedRuntime(t *testing.T) {
	source := createRuntime(t)
	destination := filepath.Join(t.TempDir(), "runtime")
	installed, err := Install(source, destination)
	if err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if installed.Root != destination {
		t.Fatalf("installed root = %q, want %q", installed.Root, destination)
	}
	contents, err := os.ReadFile(filepath.Join(destination, "runtime.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "version: 3.2.0") {
		t.Fatalf("installed descriptor = %q", contents)
	}
}

func TestInstallLeavesExistingRuntimeWhenSourceIsInvalid(t *testing.T) {
	destination := createRuntime(t)
	source := t.TempDir()
	if _, err := Install(source, destination); err == nil {
		t.Fatal("Install() unexpectedly succeeded")
	}
	if _, err := Resolve(destination); err != nil {
		t.Fatalf("existing runtime was not preserved: %v", err)
	}
}

func TestInstallRejectsIdenticalSourceAndDestination(t *testing.T) {
	root := createRuntime(t)
	if _, err := Install(root, root); err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("Install() error = %v, want source/destination error", err)
	}
}

func TestInstallRelocatesPythonVirtualEnvConsoleScripts(t *testing.T) {
	source := createRuntime(t)
	ansiblePath := filepath.Join(source, "venv", "bin", "ansible-playbook")
	if err := os.WriteFile(ansiblePath, []byte("#!"+filepath.Join(source, "venv", "bin", "python")+"\nprint('ansible')\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "runtime")
	if _, err := Install(source, destination); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(destination, "venv", "bin", "ansible-playbook"))
	if err != nil {
		t.Fatal(err)
	}
	want := "#!" + filepath.Join(destination, "venv", "bin", "python") + "\n"
	if !strings.HasPrefix(string(contents), want) {
		t.Fatalf("relocated script = %q, want prefix %q", contents, want)
	}
}

func TestBootstrapOfflineInstallsRuntimeSubtree(t *testing.T) {
	payload := writeOfflinePayload(t, createRuntime(t))
	destination := filepath.Join(t.TempDir(), "runtime")
	installed, err := BootstrapOffline(payload, destination)
	if err != nil {
		t.Fatalf("BootstrapOffline() error = %v", err)
	}
	if installed.Root != destination || installed.Descriptor.Version != "3.2.0" {
		t.Fatalf("installed runtime = %#v", installed)
	}
}

func TestBootstrapOfflineRejectsPathTraversal(t *testing.T) {
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(gzipWriter)
	if err := archive.WriteHeader(&tar.Header{Name: "../runtime/runtime.yaml", Mode: 0o600, Size: 3, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte("bad")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(t.TempDir(), "eva-offline.tar.gz")
	if err := os.WriteFile(payload, buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := BootstrapOffline(payload, filepath.Join(t.TempDir(), "runtime")); err == nil || !strings.Contains(err.Error(), "escapes archive root") {
		t.Fatalf("BootstrapOffline() error = %v, want path traversal error", err)
	}
}

func TestBootstrapOnlinePublishesValidatedRuntime(t *testing.T) {
	spec, payloads := testOnlineBootstrapSpec(t)
	destination := filepath.Join(t.TempDir(), "runtime")
	installed, err := bootstrapOnline(destination, spec, testOnlineBootstrapDependencies(t, payloads))
	if err != nil {
		t.Fatalf("bootstrapOnline() error = %v", err)
	}
	if installed.Root != destination || installed.Descriptor.Version != "cloud-test" {
		t.Fatalf("installed runtime = %#v", installed)
	}
	for _, name := range []string{"ansible-playbook", "helm", "kubectl", "kustomize", "oras"} {
		if _, err := installed.ToolPath(name); err != nil {
			t.Fatalf("ToolPath(%q) error = %v", name, err)
		}
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o775 || info.Mode()&os.ModeSetgid == 0 {
		t.Fatalf("runtime directory mode = %#o, want setgid 2775", info.Mode())
	}
}

func TestBootstrapOnlineInstallsPinnedAnsibleCollections(t *testing.T) {
	spec, payloads := testOnlineBootstrapSpec(t)
	spec.AnsibleCollections = []string{"ansible.posix:==2.2.2"}
	dependencies := testOnlineBootstrapDependencies(t, payloads)
	runWithEnv := dependencies.runWithEnv
	var galaxyArgs []string
	var galaxyEnvironment []string
	dependencies.runWithEnv = func(environment []string, name string, args ...string) error {
		if strings.HasSuffix(name, filepath.Join("venv", "bin", "ansible-galaxy")) {
			galaxyArgs = append([]string(nil), args...)
			galaxyEnvironment = append([]string(nil), environment...)
		}
		return runWithEnv(environment, name, args...)
	}

	if _, err := bootstrapOnline(filepath.Join(t.TempDir(), "runtime"), spec, dependencies); err != nil {
		t.Fatalf("bootstrapOnline() error = %v", err)
	}
	if len(galaxyArgs) != 6 ||
		galaxyArgs[0] != "collection" || galaxyArgs[1] != "install" ||
		galaxyArgs[2] != "--no-deps" || galaxyArgs[3] != "--collections-path" ||
		!strings.HasSuffix(galaxyArgs[4], "collections") || galaxyArgs[5] != "ansible.posix:==2.2.2" {
		t.Fatalf("ansible-galaxy arguments = %#v", galaxyArgs)
	}
	collectionPath := galaxyArgs[4]
	if !containsEnvironment(galaxyEnvironment, "ANSIBLE_COLLECTIONS_PATH="+collectionPath) {
		t.Fatalf("ansible-galaxy environment = %#v", galaxyEnvironment)
	}
}

func TestCollectionInstallEnvironmentUsesOnlyStagingCollectionPath(t *testing.T) {
	environment := collectionInstallEnvironment(
		[]string{"KEEP=value", "ANSIBLE_COLLECTIONS_PATH=/operator/collections"},
		"/opt/eva/.eva-runtime-cloud-123/collections",
	)
	if !containsEnvironment(environment, "KEEP=value") ||
		!containsEnvironment(environment, "ANSIBLE_COLLECTIONS_PATH=/opt/eva/.eva-runtime-cloud-123/collections") ||
		containsEnvironment(environment, "ANSIBLE_COLLECTIONS_PATH=/operator/collections") {
		t.Fatalf("collection install environment = %#v", environment)
	}
}

func TestBootstrapOnlineLeavesExistingRuntimeWhenDownloadFails(t *testing.T) {
	destination := createRuntime(t)
	spec, payloads := testOnlineBootstrapSpec(t)
	dependencies := testOnlineBootstrapDependencies(t, payloads)
	dependencies.download = func(string, string) error {
		return fmt.Errorf("download unavailable")
	}
	if _, err := bootstrapOnline(destination, spec, dependencies); err == nil || !strings.Contains(err.Error(), "download Runtime") {
		t.Fatalf("bootstrapOnline() error = %v, want download error", err)
	}
	resolved, err := Resolve(destination)
	if err != nil {
		t.Fatalf("existing runtime was not preserved: %v", err)
	}
	if resolved.Descriptor.Version != "3.2.0" {
		t.Fatalf("existing runtime version = %q", resolved.Descriptor.Version)
	}
}

func TestBootstrapOnlineRejectsUnsupportedPlatform(t *testing.T) {
	spec, payloads := testOnlineBootstrapSpec(t)
	dependencies := testOnlineBootstrapDependencies(t, payloads)
	dependencies.goarch = "arm64"
	if _, err := bootstrapOnline(filepath.Join(t.TempDir(), "runtime"), spec, dependencies); err == nil || !strings.Contains(err.Error(), "linux/amd64") {
		t.Fatalf("bootstrapOnline() error = %v, want platform error", err)
	}
}

func testOnlineBootstrapSpec(t *testing.T) (onlineBootstrapSpec, map[string][]byte) {
	t.Helper()
	payloads := map[string][]byte{
		"kubectl":   []byte("#!/bin/sh\necho kubectl\n"),
		"helm":      testArchive(t, "linux-amd64/helm", []byte("#!/bin/sh\necho helm\n")),
		"kustomize": testArchive(t, "kustomize", []byte("#!/bin/sh\necho kustomize\n")),
		"oras":      testArchive(t, "oras", []byte("#!/bin/sh\necho oras\n")),
	}
	artifacts := make([]onlineArtifact, 0, len(payloads))
	for name, payload := range payloads {
		artifact := onlineArtifact{
			Name:       name,
			URL:        "https://runtime.example.invalid/" + name,
			SHA256:     testSHA256(payload),
			TargetPath: "bin/" + name,
		}
		switch name {
		case "helm":
			artifact.ArchivePath = "linux-amd64/helm"
		case "kustomize", "oras":
			artifact.ArchivePath = name
		}
		artifacts = append(artifacts, artifact)
	}
	return onlineBootstrapSpec{
		Version:             "cloud-test",
		AnsibleRequirements: []string{"ansible==test"},
		Artifacts:           artifacts,
	}, payloads
}

func testOnlineBootstrapDependencies(t *testing.T, payloads map[string][]byte) onlineBootstrapDependencies {
	t.Helper()
	run := func(name string, args ...string) error {
		if name != "python3" {
			return nil
		}
		venv := args[len(args)-1]
		for _, path := range []string{"bin/python", "bin/ansible-playbook"} {
			fullPath := filepath.Join(venv, path)
			if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
				return err
			}
			contents := []byte("#!/bin/sh\n")
			if path == "bin/ansible-playbook" {
				contents = []byte("#!" + filepath.Join(venv, "bin", "python") + "\nprint('ansible')\n")
			}
			if err := os.WriteFile(fullPath, contents, 0o755); err != nil {
				return err
			}
		}
		return nil
	}
	return onlineBootstrapDependencies{
		goos: "linux", goarch: "amd64", run: run,
		runWithEnv: func(_ []string, name string, args ...string) error {
			return run(name, args...)
		},
		download: func(source, destination string) error {
			name := filepath.Base(source)
			contents, ok := payloads[name]
			if !ok {
				return fmt.Errorf("unexpected URL %s", source)
			}
			return os.WriteFile(destination, contents, 0o600)
		},
	}
}

func containsEnvironment(environment []string, expected string) bool {
	for _, entry := range environment {
		if entry == expected {
			return true
		}
	}
	return false
}

func testArchive(t *testing.T, name string, contents []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(gzipWriter)
	if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(contents)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func testSHA256(contents []byte) string {
	hash := sha256.Sum256(contents)
	return hex.EncodeToString(hash[:])
}

func createRuntime(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	paths := map[string]string{
		"ansible-playbook": "venv/bin/ansible-playbook",
		"helm":             "bin/helm",
		"kubectl":          "bin/kubectl",
		"kustomize":        "bin/kustomize",
		"oras":             "bin/oras",
	}
	for _, path := range paths {
		fullPath := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	contents := "schema_version: v1\nversion: 3.2.0\ntools:\n"
	for name, path := range paths {
		contents += "  " + name + ": " + path + "\n"
	}
	if err := os.WriteFile(filepath.Join(root, descriptorName), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeOfflinePayload(t *testing.T, runtimeRoot string) string {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(gzipWriter)
	if err := filepath.WalkDir(runtimeRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(runtimeRoot, path)
		if err != nil {
			return err
		}
		if relative == "." || entry.IsDir() {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header := &tar.Header{Name: "runtime/" + filepath.ToSlash(relative), Mode: int64(info.Mode().Perm()), Size: int64(len(contents)), Typeflag: tar.TypeReg}
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		_, err = archive.Write(contents)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(t.TempDir(), "eva-offline.tar.gz")
	if err := os.WriteFile(payload, buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return payload
}
