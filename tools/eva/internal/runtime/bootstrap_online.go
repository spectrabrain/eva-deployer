package runtime

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const cloudRuntimeVersion = "1.0.1"

type onlineArtifact struct {
	Name        string
	URL         string
	SHA256      string
	ArchivePath string
	TargetPath  string
}

type onlineBootstrapSpec struct {
	Version             string
	AnsibleRequirements []string
	AnsibleCollections  []string
	Artifacts           []onlineArtifact
}

type onlineBootstrapDependencies struct {
	download   func(string, string) error
	run        func(string, ...string) error
	runWithEnv func([]string, string, ...string) error
	goos       string
	goarch     string
}

var defaultOnlineBootstrapSpec = onlineBootstrapSpec{
	Version: cloudRuntimeVersion,
	AnsibleRequirements: []string{
		"ansible==13.6.0",
		"ansible-core==2.20.5",
	},
	AnsibleCollections: []string{
		"ansible.posix:==2.2.2",
	},
	Artifacts: []onlineArtifact{
		{
			Name:       "kubectl",
			URL:        "https://dl.k8s.io/release/v1.36.2/bin/linux/amd64/kubectl",
			SHA256:     "1e9045ec32bea85da43de85f0065358529ea7c7a152eca78154fba5b58c27d82",
			TargetPath: "bin/kubectl",
		},
		{
			Name:        "helm",
			URL:         "https://get.helm.sh/helm-v4.0.1-linux-amd64.tar.gz",
			SHA256:      "e0365548f01ed52a58a1181ad310b604a3244f59257425bb1739499372bdff60",
			ArchivePath: "linux-amd64/helm",
			TargetPath:  "bin/helm",
		},
		{
			Name:        "kustomize",
			URL:         "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2Fv5.4.3/kustomize_v5.4.3_linux_amd64.tar.gz",
			SHA256:      "3669470b454d865c8184d6bce78df05e977c9aea31c30df3c669317d43bcc7a7",
			ArchivePath: "kustomize",
			TargetPath:  "bin/kustomize",
		},
		{
			Name:        "oras",
			URL:         "https://github.com/oras-project/oras/releases/download/v1.3.3/oras_1.3.3_linux_amd64.tar.gz",
			SHA256:      "9ce999f8d2de03fc03968b29d743077a58783e545e5eaa53917ca177352d0e59",
			ArchivePath: "oras",
			TargetPath:  "bin/oras",
		},
	},
}

// BootstrapOnline builds the managed Cloud Runtime from pinned upstream tools.
// The payload is staged and validated before Install atomically publishes it.
func BootstrapOnline(root string) (Resolved, error) {
	return bootstrapOnline(root, defaultOnlineBootstrapSpec, onlineBootstrapDependencies{
		download:   downloadFile,
		run:        runCommand,
		runWithEnv: runCommandWithEnv,
		goos:       goruntime.GOOS,
		goarch:     goruntime.GOARCH,
	})
}

func bootstrapOnline(root string, spec onlineBootstrapSpec, dependencies onlineBootstrapDependencies) (Resolved, error) {
	if dependencies.goos != "linux" || dependencies.goarch != "amd64" {
		return Resolved{}, fmt.Errorf("Cloud Runtime bootstrap supports linux/amd64 only, got %s/%s", dependencies.goos, dependencies.goarch)
	}
	if spec.Version == "" || len(spec.AnsibleRequirements) == 0 || len(spec.Artifacts) == 0 {
		return Resolved{}, fmt.Errorf("Cloud Runtime bootstrap specification is incomplete")
	}
	if dependencies.download == nil || dependencies.run == nil || dependencies.runWithEnv == nil {
		return Resolved{}, fmt.Errorf("Cloud Runtime bootstrap dependencies are incomplete")
	}
	if root == "" {
		root = DefaultRoot
	}
	destination, err := filepath.Abs(root)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve runtime destination: %w", err)
	}
	if destination == DefaultRoot && os.Geteuid() != 0 {
		return Resolved{}, fmt.Errorf("Cloud Runtime bootstrap into %s requires root; run sudo eva runtime bootstrap", DefaultRoot)
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return Resolved{}, fmt.Errorf("create runtime parent: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".eva-runtime-cloud-")
	if err != nil {
		return Resolved{}, fmt.Errorf("create Cloud Runtime staging directory: %w", err)
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, runtimeDirectoryMode); err != nil {
		return Resolved{}, fmt.Errorf("set Cloud Runtime staging directory mode: %w", err)
	}

	if err := dependencies.run("python3", "-m", "venv", filepath.Join(staging, "venv")); err != nil {
		return Resolved{}, fmt.Errorf("create Runtime Python environment (install python3-venv if unavailable): %w", err)
	}
	python := filepath.Join(staging, "venv", "bin", "python")
	if err := dependencies.run(python, append([]string{"-m", "pip", "install", "--disable-pip-version-check", "--no-input"}, spec.AnsibleRequirements...)...); err != nil {
		return Resolved{}, fmt.Errorf("install Runtime Ansible dependencies: %w", err)
	}
	if err := installAnsibleCollections(staging, spec.AnsibleCollections, dependencies.runWithEnv); err != nil {
		return Resolved{}, err
	}

	downloadDirectory := filepath.Join(staging, ".downloads")
	if err := os.MkdirAll(downloadDirectory, 0o700); err != nil {
		return Resolved{}, fmt.Errorf("create Runtime download directory: %w", err)
	}
	for _, artifact := range spec.Artifacts {
		if err := installOnlineArtifact(staging, downloadDirectory, artifact, dependencies.download); err != nil {
			return Resolved{}, err
		}
	}
	if err := os.RemoveAll(downloadDirectory); err != nil {
		return Resolved{}, fmt.Errorf("remove Runtime download directory: %w", err)
	}
	if err := writeCloudDescriptor(staging, spec.Version); err != nil {
		return Resolved{}, err
	}
	if _, err := Resolve(staging); err != nil {
		return Resolved{}, fmt.Errorf("validate staged Cloud Runtime: %w", err)
	}
	return Install(staging, destination)
}

func installAnsibleCollections(root string, collections []string, run func([]string, string, ...string) error) error {
	if len(collections) == 0 {
		return nil
	}
	galaxy := filepath.Join(root, "venv", "bin", "ansible-galaxy")
	collectionRoot := filepath.Join(root, "collections")
	arguments := []string{"collection", "install", "--no-deps", "--collections-path", collectionRoot}
	arguments = append(arguments, collections...)
	if err := run(collectionInstallEnvironment(os.Environ(), collectionRoot), galaxy, arguments...); err != nil {
		return fmt.Errorf("install pinned Runtime Ansible collections: %w", err)
	}
	return nil
}

func collectionInstallEnvironment(base []string, collectionRoot string) []string {
	environment := make([]string, 0, len(base)+1)
	for _, entry := range base {
		if !strings.HasPrefix(entry, "ANSIBLE_COLLECTIONS_PATH=") {
			environment = append(environment, entry)
		}
	}
	return append(environment, "ANSIBLE_COLLECTIONS_PATH="+collectionRoot)
}

func installOnlineArtifact(root, downloadDirectory string, artifact onlineArtifact, download func(string, string) error) error {
	if artifact.Name == "" || artifact.URL == "" || artifact.SHA256 == "" || artifact.TargetPath == "" {
		return fmt.Errorf("Cloud Runtime artifact definition is incomplete")
	}
	if _, err := hex.DecodeString(artifact.SHA256); err != nil || len(artifact.SHA256) != sha256.Size*2 {
		return fmt.Errorf("Cloud Runtime artifact %s has an invalid SHA-256", artifact.Name)
	}
	downloadPath := filepath.Join(downloadDirectory, artifact.Name)
	if err := download(artifact.URL, downloadPath); err != nil {
		return fmt.Errorf("download Runtime %s: %w", artifact.Name, err)
	}
	if err := verifyFileSHA256(downloadPath, artifact.SHA256); err != nil {
		return fmt.Errorf("verify Runtime %s: %w", artifact.Name, err)
	}
	target := filepath.Join(root, filepath.FromSlash(artifact.TargetPath))
	if !isWithin(root, target) {
		return fmt.Errorf("Cloud Runtime artifact %s target escapes runtime root", artifact.Name)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create Runtime %s directory: %w", artifact.Name, err)
	}
	if artifact.ArchivePath == "" {
		if err := copyFile(downloadPath, target, 0o755); err != nil {
			return fmt.Errorf("install Runtime %s: %w", artifact.Name, err)
		}
		return nil
	}
	if err := extractArchiveExecutable(downloadPath, artifact.ArchivePath, target); err != nil {
		return fmt.Errorf("install Runtime %s: %w", artifact.Name, err)
	}
	return nil
}

func writeCloudDescriptor(root, version string) error {
	contents, err := yaml.Marshal(Descriptor{
		SchemaVersion: schemaVersion,
		Version:       version,
		Tools: map[string]string{
			"ansible-playbook": "venv/bin/ansible-playbook",
			"helm":             "bin/helm",
			"kubectl":          "bin/kubectl",
			"kustomize":        "bin/kustomize",
			"oras":             "bin/oras",
		},
	})
	if err != nil {
		return fmt.Errorf("encode Runtime descriptor: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, descriptorName), contents, 0o644); err != nil {
		return fmt.Errorf("write Runtime descriptor: %w", err)
	}
	return nil
}

func downloadFile(source, destination string) error {
	response, err := (&http.Client{Timeout: 10 * time.Minute}).Get(source)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("unexpected HTTP status %s", response.Status)
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, response.Body); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}

func runCommand(name string, args ...string) error {
	return runCommandWithEnv(os.Environ(), name, args...)
}

func runCommandWithEnv(environment []string, name string, args ...string) error {
	command := exec.Command(name, args...)
	command.Env = environment
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

func verifyFileSHA256(path, expected string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expected) {
		return fmt.Errorf("SHA-256 mismatch")
	}
	return nil
}

func extractArchiveExecutable(archivePath, archiveEntry, destination string) error {
	if archiveEntry == "" || strings.HasPrefix(archiveEntry, "/") || pathpkg.Clean(archiveEntry) != archiveEntry || strings.HasPrefix(archiveEntry, "../") {
		return fmt.Errorf("invalid archive entry %q", archiveEntry)
	}
	input, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer input.Close()
	gzipReader, err := gzip.NewReader(input)
	if err != nil {
		return err
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if header.Name != archiveEntry {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return fmt.Errorf("archive entry is not a regular file")
		}
		if err := copyArchiveFile(tarReader, destination, 0o755); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("archive entry is missing: %s", archiveEntry)
}
