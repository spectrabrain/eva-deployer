package airgap

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"eva-deployer/tools/eva/internal/remote"
)

const singleDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
const multiOCIIndex = "application/vnd.oci.image.index.v1+json"
const platformTargetError = "no suitable export target found: image was found but does not provide the specified platform (linux/amd64)"

func dockerMergeTestEntries(t *testing.T, tags []string, oci bool) []archiveTestEntry {
	t.Helper()
	entries := archiveTestDockerEntries(t)
	if oci {
		entries[0].header.Name = "blobs/sha256/" + archiveTestDigest(entries[0].body)
		for _, i := range []int{2, 3} {
			entries[i].header.Name = "blobs/sha256/" + archiveTestDigest(entries[i].body)
		}
	}
	entries = archiveTestChangeManifest(t, entries, func(manifest []dockerSaveManifestEntry) {
		manifest[0].Config = entries[0].header.Name
		manifest[0].Layers = []string{entries[2].header.Name, entries[3].header.Name}
		manifest[0].RepoTags = tags
	})
	if oci {
		var rootDescriptors []map[string]any
		config := map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": "sha256:" + archiveTestDigest(entries[0].body), "size": len(entries[0].body)}
		var layers []map[string]any
		for _, i := range []int{2, 3} {
			layers = append(layers, map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": "sha256:" + archiveTestDigest(entries[i].body), "size": len(entries[i].body)})
		}
		body, err := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json", "config": config, "layers": layers})
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, archiveTestRegular("blobs/sha256/"+archiveTestDigest(body), body))
		for _, tag := range tags {
			canonical, err := dockerImageReferenceKey(tag)
			if err != nil {
				t.Fatal(err)
			}
			rootDescriptors = append(rootDescriptors, map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": "sha256:" + archiveTestDigest(body), "size": len(body), "platform": map[string]string{"os": "linux", "architecture": "amd64"}, "annotations": map[string]string{"io.containerd.image.name": canonical, "org.opencontainers.image.ref.name": canonical[strings.LastIndex(canonical, ":")+1:]}})
		}
		index, err := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": rootDescriptors})
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, archiveTestRegular("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)), archiveTestRegular("index.json", index))
	}
	return entries
}

func TestSaveDockerImagesSingleManifestCompatibility(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%t", mixed), func(t *testing.T) {
			images := []string{"example.test/dispatcher:3.3.0"}
			if mixed {
				images = append(images, "example.test/multi:1")
			}
			archive := filepath.Join(t.TempDir(), "images.tar")
			var log bytes.Buffer
			calls, saves := 0, 0
			run := func(_ context.Context, process remote.ProcessOptions) error {
				calls++
				if reflect.DeepEqual(process.Args[:2], []string{"image", "inspect"}) {
					if !reflect.DeepEqual(process.Args[4:], images) {
						t.Fatalf("inspection included unrelated images: %q", process.Args)
					}
					fmt.Fprintf(process.Streams.Stdout, `{"os":"linux","architecture":"amd64","descriptor":{"mediaType":%q}}`+"\n", singleDockerManifest)
					if mixed {
						fmt.Fprintf(process.Streams.Stdout, `{"os":"linux","architecture":"amd64","descriptor":{"mediaType":%q}}`+"\n", multiOCIIndex)
					}
					return nil
				}
				saves++
				if saves == 1 {
					archiveTestWriteFile(t, archive, []byte("partial export"))
					fmt.Fprintln(process.Streams.Stderr, platformTargetError)
					return errors.New("private Docker failure")
				}
				flagged := len(process.Args) >= 6 && process.Args[3] == "--platform"
				selected := process.Args[3:]
				if flagged {
					if process.Args[4] != "linux/amd64" {
						t.Fatal("index selected a different platform")
					}
					selected = process.Args[5:]
				}
				if flagged && (!mixed || !reflect.DeepEqual(selected, images[1:])) || !flagged && !reflect.DeepEqual(selected, images[:1]) {
					t.Fatalf("incorrect Docker export group: %q", process.Args)
				}
				return os.WriteFile(process.Args[2], archiveTestTarBytes(t, dockerMergeTestEntries(t, selected, mixed)), 0o600)
			}
			if err := saveDockerImages(context.Background(), run, remote.ProcessOptions{Streams: remote.Streams{Stderr: &log}}, archive, images); err != nil {
				t.Fatal(err)
			}
			if err := validateDockerArchive(archive, images); err != nil {
				t.Fatalf("compatibility archive is not importable: %v", err)
			}
			wantCalls := 3
			if mixed {
				wantCalls++
			}
			if calls != wantCalls {
				t.Fatalf("unexpected commands: %d", calls)
			}
			if paths, _ := filepath.Glob(filepath.Join(filepath.Dir(archive), ".docker-export-*")); len(paths) != 0 {
				t.Fatal("temporary export groups remain")
			}
		})
	}
}

func TestDockerManifestCompatibilityRejectsUnverifiedTargets(t *testing.T) {
	for _, metadata := range []string{
		`{"os":"linux","architecture":"arm64","descriptor":{"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}}`,
		`{"os":"linux","architecture":"amd64","descriptor":{"mediaType":"unknown"}}`,
		`{"os":"linux","architecture":"amd64","descriptor":{"mediaType":"application/vnd.oci.image.index.v1+json"}}`,
		`{"os":"linux","architecture":"amd64","descriptor":null}`,
		`invalid-json`,
	} {
		t.Run(metadata, func(t *testing.T) {
			saves := 0
			run := func(_ context.Context, process remote.ProcessOptions) error {
				if process.Args[0] == "image" {
					fmt.Fprintln(process.Streams.Stdout, metadata)
					return nil
				}
				saves++
				fmt.Fprintln(process.Streams.Stderr, platformTargetError)
				return errors.New("export failed")
			}
			err := saveDockerImages(context.Background(), run, remote.ProcessOptions{}, filepath.Join(t.TempDir(), "images.tar"), []string{"example.test/image:1"})
			if err == nil || saves != 1 {
				t.Fatalf("unverified target used unrestricted save: saves=%d error=%v", saves, err)
			}
		})
	}
}

func TestMergeDockerExportsPreservesTagsAndLayouts(t *testing.T) {
	for _, oci := range []bool{false, true} {
		t.Run(fmt.Sprintf("OCI=%t", oci), func(t *testing.T) {
			images := []string{"example.test/first:1", "example.test/second:2"}
			var sources []string
			for _, image := range images {
				sources = append(sources, archiveTestWriteTar(t, dockerMergeTestEntries(t, []string{image}, oci), false))
			}
			out := filepath.Join(t.TempDir(), "merged.tar")
			if err := mergeDockerExports(context.Background(), sources, out); err != nil {
				t.Fatal(err)
			}
			if err := validateDockerArchive(out, images); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(out)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			tr := tar.NewReader(file)
			seen := make(map[string]bool)
			for {
				header, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil || seen[header.Name] {
					t.Fatalf("invalid or duplicate merged member: %v", err)
				}
				seen[header.Name] = true
				if header.Name == "index.json" {
					var index struct {
						Manifests []struct{ Annotations map[string]string }
					}
					if err := json.NewDecoder(tr).Decode(&index); err != nil || len(index.Manifests) != 2 {
						t.Fatal("OCI index lost an export group")
					}
					for i, image := range images {
						canonical, _ := dockerImageReferenceKey(image)
						if index.Manifests[i].Annotations["io.containerd.image.name"] != canonical {
							t.Fatal("OCI import tags changed")
						}
					}
				}
			}
			if oci != seen["index.json"] || oci != seen["oci-layout"] {
				t.Fatal("archive format changed")
			}
		})
	}
}

func TestMergeDockerExportsRejectsConflictAndCancellation(t *testing.T) {
	for _, kind := range []string{"conflicting member", "mixed layouts", "missing manifest", "duplicate member", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			first := dockerMergeTestEntries(t, []string{"first:1"}, false)
			second := dockerMergeTestEntries(t, []string{"second:1"}, false)
			ctx := context.Background()
			switch kind {
			case "conflicting member":
				first = append(first, archiveTestRegular("shared", []byte("original")))
				second = append(second, archiveTestRegular("shared", []byte("different")))
			case "mixed layouts":
				second = dockerMergeTestEntries(t, []string{"second:1"}, true)
			case "missing manifest":
				second = second[:1]
			case "duplicate member":
				second = append(second, second[0])
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			sources := []string{archiveTestWriteTar(t, first, false), archiveTestWriteTar(t, second, false)}
			out := filepath.Join(t.TempDir(), "merged.tar")
			if err := mergeDockerExports(ctx, sources, out); err == nil {
				t.Fatal("invalid export groups were merged")
			}
			if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed merge retained partial output")
			}
		})
	}
}

func TestDockerGroupExportFailureStopsWithoutPartialArchive(t *testing.T) {
	for _, failure := range []string{"inspection", "index export", "single export"} {
		t.Run(failure, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "images.tar")
			saves := 0
			var log bytes.Buffer
			run := func(_ context.Context, process remote.ProcessOptions) error {
				if process.Args[0] == "image" {
					if failure == "inspection" {
						return errors.New("private-credential inspection failure")
					}
					for _, mediaType := range []string{singleDockerManifest, multiOCIIndex} {
						fmt.Fprintf(process.Streams.Stdout, `{"os":"linux","architecture":"amd64","descriptor":{"mediaType":%q}}`+"\n", mediaType)
					}
					return nil
				}
				saves++
				if saves == 1 {
					fmt.Fprintln(process.Streams.Stderr, platformTargetError)
					return errors.New("initial platform rejection")
				}
				if failure == "index export" && saves == 2 || failure == "single export" && saves == 3 {
					archiveTestWriteFile(t, process.Args[2], []byte("partial group"))
					return errors.New("private-credential export failure")
				}
				return os.WriteFile(process.Args[2], archiveTestTarBytes(t, dockerMergeTestEntries(t, []string{"index:1"}, true)), 0o600)
			}
			err := saveDockerImages(context.Background(), run, remote.ProcessOptions{Streams: remote.Streams{Stderr: &log}}, archive, []string{"single:1", "index:1"})
			if err == nil || strings.Contains(err.Error(), "private-credential") || !strings.Contains(log.String(), "private-credential") {
				t.Fatalf("export failed without private evidence or exposed diagnostics: %v", err)
			}
			if _, err := os.Stat(archive); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed grouped export retained an archive")
			}
			if paths, _ := filepath.Glob(filepath.Join(filepath.Dir(archive), ".docker-export-*")); len(paths) != 0 {
				t.Fatal("failed grouped export retained temporary parts")
			}
		})
	}
}

// Explicit opt-in: load tiny synthetic archives under unique test tags, then
// remove only those tags. Existing images, containers and daemon settings are
// preserved. This checks the importer in addition to archive validation.
func TestMergeDockerExportsRealLoad(t *testing.T) {
	if os.Getenv("EVA_TEST_DOCKER_LOAD_MERGE") != "1" {
		t.Skip("set EVA_TEST_DOCKER_LOAD_MERGE=1 to test Docker import with temporary image tags")
	}
	for _, oci := range []bool{false, true} {
		t.Run(fmt.Sprintf("OCI=%t", oci), func(t *testing.T) {
			prefix := fmt.Sprintf("eva-airgap-merge-check-%d", time.Now().UnixNano())
			images := []string{prefix + ":first", prefix + ":second"}
			var sources []string
			for _, image := range images {
				sources = append(sources, archiveTestWriteTar(t, dockerMergeTestEntries(t, []string{image}, oci), false))
			}
			archive := filepath.Join(t.TempDir(), "merged.tar")
			if err := mergeDockerExports(context.Background(), sources, archive); err != nil {
				t.Fatal(err)
			}
			if err := validateDockerArchive(archive, images); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				var loaded []string
				for _, image := range images {
					if exec.Command("docker", "image", "inspect", image).Run() == nil {
						loaded = append(loaded, image)
					}
				}
				if len(loaded) == 0 {
					return
				}
				command := exec.Command("docker", append([]string{"image", "rm"}, loaded...)...)
				if output, err := command.CombinedOutput(); err != nil {
					t.Errorf("remove temporary test tags: %v: %s", err, output)
				}
			})
			if output, err := exec.Command("docker", "load", "--input", archive).CombinedOutput(); err != nil {
				t.Fatalf("load merged export: %v: %s", err, output)
			}
			for _, image := range images {
				output, err := exec.Command("docker", "image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", image).CombinedOutput()
				if err != nil || strings.TrimSpace(string(output)) != "linux/amd64" {
					t.Fatalf("import lost a tag or platform: %s: %v: %s", image, err, output)
				}
			}
		})
	}
}
