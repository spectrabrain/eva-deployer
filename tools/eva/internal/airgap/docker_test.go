package airgap

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"eva-deployer/tools/eva/internal/remote"
)

func TestInfraDownloadDefersManagedExport(t *testing.T) {
	backend, err := filepath.Abs("../../../../scripts/download/download_infra_images.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, managed := range []bool{false, true} {
		t.Run(fmt.Sprintf("managed=%t", managed), func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			if err := os.Mkdir(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			// All Docker calls go to this fake, including command -v. The
			// downloader must never perform its own save in managed mode.
			fake := `#!/bin/sh
case "$1" in
pull) exit 0 ;;
save) printf '%s\n' "$@" > "$EVA_TEST_DOCKER_CALLS" ;;
*) exit 97 ;;
esac
`
			if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fake), 0o755); err != nil {
				t.Fatal(err)
			}
			calls := filepath.Join(root, "save-calls.txt")
			cache := filepath.Join(root, "cache with spaces")
			command := exec.Command("bash", backend)
			command.Dir = root
			command.Env = []string{
				"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
				"EVA_CACHE_ROOT=" + cache, "EVA_TEST_DOCKER_CALLS=" + calls,
				"EVA_MANAGED_IMAGE_EXPORT=" + fmt.Sprint(managed),
			}
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("download backend: %v: %s", err, output)
			}
			all, err := os.ReadFile(filepath.Join(cache, "images/infra-images-all.txt"))
			if err != nil {
				t.Fatal(err)
			}
			pulled, err := os.ReadFile(filepath.Join(cache, "images/infra-images-pulled.txt"))
			if err != nil || !bytes.Equal(all, pulled) || !strings.Contains(string(pulled), "rancher/mirrored-pause:3.6\n") {
				t.Fatalf("managed preparation must still download all Infra images: %v", err)
			}
			if managed {
				if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("managed download performed an unvalidated Docker save")
				}
			} else {
				got, err := os.ReadFile(calls)
				want := "save\n-o\n" + filepath.Join(cache, "images/k3s-images.tar") + "\nrancher/mirrored-pause:3.6\n"
				if err != nil || string(got) != want {
					t.Fatalf("standalone download no longer exports its pause image: %q, %v", got, err)
				}
			}
		})
	}
}

func TestSaveDockerImagesArchiveFormats(t *testing.T) {
	for _, layout := range []string{"classic", "OCI", "compressed OCI"} {
		t.Run(layout, func(t *testing.T) {
			entries := archiveTestDockerEntries(t)
			if layout != "classic" {
				config := "blobs/sha256/" + archiveTestDigest(entries[0].body)
				entries[0].header.Name = config
				for _, index := range []int{2, 3} {
					body := entries[index].body
					if layout == "compressed OCI" {
						var compressed bytes.Buffer
						writer := gzip.NewWriter(&compressed)
						if _, err := writer.Write(body); err != nil {
							t.Fatal(err)
						}
						if err := writer.Close(); err != nil {
							t.Fatal(err)
						}
						body = compressed.Bytes()
					}
					entries[index] = archiveTestRegular("blobs/sha256/"+archiveTestDigest(body), body)
				}
				entries = archiveTestChangeManifest(t, entries, func(manifest []dockerSaveManifestEntry) {
					manifest[0].Config = config
					manifest[0].Layers = []string{entries[2].header.Name, entries[3].header.Name}
				})
			}
			// Different tags can share image configs and layers in either store.
			entries = archiveTestChangeManifest(t, entries, func(manifest []dockerSaveManifestEntry) {
				manifest[0].RepoTags = []string{"first:1", "second:1"}
			})
			archive := filepath.Join(t.TempDir(), "images.tar")
			run := func(_ context.Context, process remote.ProcessOptions) error {
				return os.WriteFile(process.Args[2], archiveTestTarBytes(t, entries), 0o600)
			}
			images := []string{"first:1", "second:1"}
			if err := saveDockerImages(context.Background(), run, remote.ProcessOptions{}, archive, images); err != nil {
				t.Fatal(err)
			}
			if err := validateDockerArchive(archive, images); err != nil {
				t.Fatalf("%s export failed validation: %v", layout, err)
			}
		})
	}
}

// Opt-in export of an already cached image. Never pulls, loads, deletes images
// or changes the daemon; it exercises the real CLI, server and archive format.
func TestSaveDockerImagesRealDaemon(t *testing.T) {
	images := strings.Fields(os.Getenv("EVA_TEST_DOCKER_IMAGES"))
	if len(images) == 0 && os.Getenv("EVA_TEST_DOCKER_IMAGE") != "" {
		images = []string{os.Getenv("EVA_TEST_DOCKER_IMAGE")}
	}
	if len(images) == 0 {
		t.Skip("set EVA_TEST_DOCKER_IMAGES to already cached linux/amd64 image references")
	}
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
	}
	if os.Getenv("EVA_TEST_DOCKER_REGISTRY_FALLBACK") == "1" {
		env["EVA_MANAGED_IMAGE_EXPORT"] = "true"
		env["EVA_ORAS_BINARY"] = os.Getenv("EVA_TEST_ORAS_BINARY")
	}
	archive := filepath.Join(t.TempDir(), "images.tar")
	var log bytes.Buffer
	if err := saveDockerImages(context.Background(), runProcess, remote.ProcessOptions{Env: env, Streams: remote.Streams{Stderr: &log}}, archive, images); err != nil {
		t.Log(log.String())
		t.Fatal(err)
	}
	t.Log(log.String())
	if err := validateDockerArchive(archive, images); err != nil {
		t.Fatal(err)
	}
}

func TestSaveDockerImagesSelectsCompletePlatform(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "images.tar")
	images := []string{"example.test/eva:1"}
	missingConfig := "blobs/sha256/" + strings.Repeat("a", 64)
	var calls [][]string
	run := func(_ context.Context, process remote.ProcessOptions) error {
		calls = append(calls, append([]string(nil), process.Args...))
		entries := archiveTestDockerEntries(t)
		if len(process.Args) < 5 || !reflect.DeepEqual(process.Args[3:5], []string{"--platform", "linux/amd64"}) {
			// Reproduce a containerd index selecting an unfetched config while
			// save still succeeds. The complete amd64 config/layers are cached.
			entries = archiveTestChangeManifest(t, entries, func(manifest []dockerSaveManifestEntry) {
				manifest[0].Config = missingConfig
			})
		}
		return os.WriteFile(archive, archiveTestTarBytes(t, entries), 0o600)
	}
	// Exercise the original command and confirm the reported archive failure.
	if err := run(context.Background(), remote.ProcessOptions{Args: []string{"save", "--output", archive, images[0]}}); err != nil {
		t.Fatal(err)
	}
	if err := validateDockerArchive(archive, images); err == nil || !strings.Contains(err.Error(), missingConfig) {
		t.Fatalf("unrestricted export did not reproduce missing config: %v", err)
	}
	calls = nil
	if err := saveDockerImages(context.Background(), run, remote.ProcessOptions{}, archive, images); err != nil {
		t.Fatal(err)
	}
	if err := validateDockerArchive(archive, images); err != nil {
		t.Fatalf("platform-specific export is not transportable: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("unexpected export retries: %q", calls)
	}
}

func TestSaveDockerImagesLegacyCompatibility(t *testing.T) {
	for _, diagnostic := range []string{
		"unknown flag: --platform",
		"--platform requires API version 1.48, but the Docker daemon API version is 1.47",
		`"--platform" requires API version 1.48, but the Docker daemon API version is 1.47`,
	} {
		t.Run(diagnostic, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "images.tar")
			images := []string{"example.test/eva:1", "example.test/eva:latest"}
			work := t.TempDir()
			env := map[string]string{"DOCKER_CONFIG": filepath.Join(work, "docker-auth")}
			calls := 0
			run := func(_ context.Context, process remote.ProcessOptions) error {
				calls++
				if process.Path != "docker" || process.Dir != work || !reflect.DeepEqual(process.Env, env) || process.Streams.Stdout != io.Discard {
					t.Fatalf("unexpected process boundary: %#v", process)
				}
				if calls == 1 {
					if !reflect.DeepEqual(process.Args[3:5], []string{"--platform", "linux/amd64"}) {
						t.Fatalf("platform was not requested: %q", process.Args)
					}
					archiveTestWriteFile(t, archive, []byte("partial export"))
					fmt.Fprintln(process.Streams.Stderr, diagnostic)
					return errors.New("private Docker failure")
				}
				if calls != 2 || !reflect.DeepEqual(process.Args, append([]string{"save", "--output", archive}, images...)) {
					t.Fatalf("unexpected legacy export: %q", process.Args)
				}
				if _, err := os.Lstat(archive); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("partial platform export was not removed before legacy save")
				}
				if process.Streams.Stderr != io.Discard {
					t.Fatal("legacy Docker diagnostics must remain private")
				}
				return os.WriteFile(archive, archiveTestTarBytes(t, archiveTestDockerEntries(t)), 0o600)
			}
			if err := saveDockerImages(context.Background(), run, remote.ProcessOptions{Dir: work, Env: env}, archive, images); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("legacy export calls = %d", calls)
			}
			if err := validateDockerArchive(archive, images); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSaveDockerImagesDoesNotRetryContentFailures(t *testing.T) {
	for _, diagnostic := range []string{
		"content digest sha256:missing: not found",
		"image does not provide the specified platform (linux/amd64)",
		"unknown flag: --different-flag",
		"permission denied",
	} {
		t.Run(diagnostic, func(t *testing.T) {
			calls := 0
			run := func(_ context.Context, process remote.ProcessOptions) error {
				calls++
				fmt.Fprintln(process.Streams.Stderr, diagnostic+" private-test-credential")
				return errors.New("private-test-credential")
			}
			err := saveDockerImages(context.Background(), run, remote.ProcessOptions{}, filepath.Join(t.TempDir(), "images.tar"), []string{"example.test/eva:1"})
			if err == nil || calls != 1 || strings.Contains(err.Error(), "private-test-credential") {
				t.Fatalf("failed export retried or diagnostic exposed: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestSaveDockerImagesRetainsPrivateDiagnostics(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			var log bytes.Buffer
			calls := 0
			run := func(_ context.Context, process remote.ProcessOptions) error {
				calls++
				if process.Streams.Stdout != io.Discard {
					t.Fatal("Docker archive output must remain disconnected from diagnostics")
				}
				if legacy && calls == 1 {
					fmt.Fprintln(process.Streams.Stderr, "unknown flag: --platform")
				} else {
					fmt.Fprintln(process.Streams.Stderr, "content digest sha256:missing: not found private-test-credential")
				}
				return errors.New("runner-private-credential")
			}
			err := saveDockerImages(context.Background(), run, remote.ProcessOptions{Streams: remote.Streams{Stderr: &log}}, filepath.Join(t.TempDir(), "images.tar"), []string{"example.test/eva:1"})
			if err == nil || strings.Contains(err.Error(), "credential") {
				t.Fatalf("failed export succeeded or exposed diagnostics: %v", err)
			}
			if !strings.Contains(log.String(), "sha256:missing") || !strings.Contains(log.String(), "private-test-credential") || !strings.Contains(log.String(), "runner-private-credential") {
				t.Fatal("original Docker or runner error was discarded")
			}
			if legacy && (calls != 2 || !strings.Contains(log.String(), "unknown flag: --platform") || !strings.Contains(log.String(), "legacy export")) || !legacy && calls != 1 {
				t.Fatalf("unexpected retry or missing retry diagnostics: calls=%d log=%s", calls, &log)
			}
		})
	}
}

func TestSaveDockerImagesCancellationAndLegacyFailure(t *testing.T) {
	for _, cancelExport := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelExport), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			run := func(_ context.Context, process remote.ProcessOptions) error {
				calls++
				fmt.Fprintln(process.Streams.Stderr, "unknown flag: --platform")
				if cancelExport {
					cancel()
				}
				return errors.New("private-test-credential")
			}
			err := saveDockerImages(ctx, run, remote.ProcessOptions{}, filepath.Join(t.TempDir(), "images.tar"), []string{"example.test/eva:1"})
			if err == nil || strings.Contains(err.Error(), "private-test-credential") {
				t.Fatalf("failed export succeeded or exposed diagnostics: %v", err)
			}
			if cancelExport {
				if calls != 1 || !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation retried: calls=%d err=%v", calls, err)
				}
			} else if calls != 2 {
				t.Fatalf("legacy failure retry count = %d", calls)
			}
		})
	}
}
