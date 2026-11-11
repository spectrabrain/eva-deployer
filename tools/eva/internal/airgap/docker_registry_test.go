package airgap

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"eva-deployer/tools/eva/internal/remote"
)

func registryTestLayout(t *testing.T, image string) (string, registryDescriptor, []archiveTestEntry) {
	t.Helper()
	layout := t.TempDir()
	entries := dockerMergeTestEntries(t, []string{image}, true)
	for _, entry := range entries {
		if entry.header.Typeflag == tar.TypeRegA || entry.header.Typeflag == tar.TypeReg {
			archiveTestWriteFile(t, filepath.Join(layout, entry.header.Name), entry.body)
		}
	}
	var index struct{ Manifests []registryDescriptor }
	if err := readRegistryJSON(filepath.Join(layout, "index.json"), &index); err != nil {
		t.Fatal(err)
	}
	return layout, index.Manifests[0], entries
}

func TestRecoverDockerArchiveFromExactRegistryTarget(t *testing.T) {
	image := "example.test/recovery:1"
	_, root, entries := registryTestLayout(t, image)
	archive := filepath.Join(t.TempDir(), "images.tar")
	var log bytes.Buffer
	saves, copies := 0, 0
	run := func(_ context.Context, process remote.ProcessOptions) error {
		if process.Path == "/trusted/oras" {
			copies++
			if !strings.Contains(strings.Join(process.Args, " "), "--from-registry-config /private/docker/config.json") || process.Args[len(process.Args)-2] != "example.test/recovery@"+root.Digest {
				t.Fatalf("registry recovery did not use pinned source/private auth: %q", process.Args)
			}
			to := strings.TrimSuffix(process.Args[len(process.Args)-1], ":source")
			for _, entry := range entries {
				if entry.header.Typeflag == tar.TypeReg {
					archiveTestWriteFile(t, filepath.Join(to, entry.header.Name), entry.body)
				}
			}
			return nil
		}
		if process.Args[0] == "image" {
			fmt.Fprintf(process.Streams.Stdout, `{"os":"linux","architecture":"amd64","descriptor":{"mediaType":%q,"digest":%q}}`, root.MediaType, root.Digest)
			return nil
		}
		saves++
		if saves == 1 {
			fmt.Fprintln(process.Streams.Stderr, platformTargetError)
			return errors.New("platform-specific export failed")
		}
		// The daemon reports success but exports no config or layers.
		var incomplete []archiveTestEntry
		for _, entry := range entries {
			if !strings.HasPrefix(entry.header.Name, "blobs/") || strings.HasSuffix(entry.header.Name, root.Digest[7:]) {
				incomplete = append(incomplete, entry)
			}
		}
		return os.WriteFile(process.Args[2], archiveTestTarBytes(t, incomplete), 0o600)
	}
	env := map[string]string{"EVA_MANAGED_IMAGE_EXPORT": "true", "EVA_ORAS_BINARY": "/trusted/oras", "DOCKER_CONFIG": "/private/docker"}
	if err := saveDockerImages(context.Background(), run, remote.ProcessOptions{Env: env, Streams: remote.Streams{Stderr: &log}}, archive, []string{image}); err != nil {
		t.Fatalf("registry recovery: %v\n%s", err, &log)
	}
	if err := validateDockerArchive(archive, []string{image}); err != nil {
		t.Fatal(err)
	}
	if saves != 2 || copies != 1 {
		t.Fatalf("unexpected recovery operations: saves=%d copies=%d", saves, copies)
	}
}

func TestRecoverDockerGroupDoesNotPullHealthyImages(t *testing.T) {
	for _, brokenIndex := range []bool{false, true} {
		t.Run(fmt.Sprintf("broken index=%t", brokenIndex), func(t *testing.T) {
			images := []string{"example.test/dispatcher:1", "docker.n8n.io/n8nio/n8n:2.32.7", "example.test/index:1"}
			_, root, recovered := registryTestLayout(t, images[2])
			archive := filepath.Join(t.TempDir(), "images.tar")
			var log bytes.Buffer
			var copies []string
			saves := 0
			run := func(_ context.Context, process remote.ProcessOptions) error {
				if process.Path == "/trusted/oras" {
					pinned := process.Args[len(process.Args)-2]
					copies = append(copies, pinned)
					if pinned != "example.test/index@"+root.Digest {
						return errors.New("public registry unauthenticated pull limit: healthy image must not be fetched")
					}
					layout := strings.TrimSuffix(process.Args[len(process.Args)-1], ":source")
					for _, entry := range recovered {
						if entry.header.Typeflag == tar.TypeReg {
							archiveTestWriteFile(t, filepath.Join(layout, entry.header.Name), entry.body)
						}
					}
					return nil
				}
				if process.Args[0] == "image" {
					for i := range images {
						mediaType := multiOCIIndex
						if i == 0 {
							mediaType = singleDockerManifest
						}
						fmt.Fprintf(process.Streams.Stdout, `{"os":"linux","architecture":"amd64","descriptor":{"mediaType":%q,"digest":%q}}`+"\n", mediaType, root.Digest)
					}
					return nil
				}
				saves++
				if saves == 1 {
					fmt.Fprintln(process.Streams.Stderr, platformTargetError)
					return errors.New("platform-specific export failed")
				}
				selected := process.Args[3:]
				platform := len(selected) > 1 && selected[0] == "--platform"
				if platform {
					if selected[1] != "linux/amd64" {
						t.Fatal("wrong platform selected")
					}
					selected = selected[2:]
				}
				if platform == (len(selected) == 1 && selected[0] == images[0]) {
					t.Fatalf("single/index platform selection lost: %q", process.Args)
				}
				entries := dockerMergeTestEntries(t, selected, true)
				if len(selected) > 1 || brokenIndex && selected[0] == images[2] {
					// The combined export fails validation, but n8n alone is
					// complete. Only a genuinely broken individual image may
					// reach the registry.
					entries = entries[1:]
				}
				return os.WriteFile(process.Args[2], archiveTestTarBytes(t, entries), 0o600)
			}
			env := map[string]string{"EVA_MANAGED_IMAGE_EXPORT": "true", "EVA_ORAS_BINARY": "/trusted/oras", "DOCKER_CONFIG": "/private/auth"}
			if err := saveDockerImages(context.Background(), run, remote.ProcessOptions{Env: env, Streams: remote.Streams{Stderr: &log}}, archive, images); err != nil {
				t.Fatalf("selective recovery failed: %v\n%s", err, &log)
			}
			if err := validateDockerArchive(archive, images); err != nil {
				t.Fatal(err)
			}
			wantCopies := 0
			if brokenIndex {
				wantCopies = 1
			}
			if len(copies) != wantCopies || saves != 5 {
				t.Fatalf("unexpected exports=%d registry copies=%q", saves, copies)
			}
		})
	}
}

func TestRecoverDockerGroupFailureCleansParts(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled=%t", cancelled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var log bytes.Buffer
			run := func(_ context.Context, process remote.ProcessOptions) error {
				if cancelled {
					cancel()
				}
				fmt.Fprintln(process.Streams.Stderr, "private-credential")
				return errors.New("private-credential")
			}
			root := t.TempDir()
			archive := filepath.Join(root, "images.tar")
			image := "example.test/broken:1"
			env := map[string]string{"EVA_ORAS_BINARY": "/trusted/oras"}
			err := recoverDockerImageGroup(ctx, run, remote.ProcessOptions{Env: env}, archive, []string{image, "example.test/unused:1"}, true, map[string]string{image: "sha256:" + strings.Repeat("a", 64)}, &log)
			if err == nil || strings.Contains(err.Error(), "private-credential") || cancelled && !errors.Is(err, context.Canceled) {
				t.Fatalf("failed/cancelled recovery exposed diagnostics or succeeded: %v", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed recovery retained staging: %v %v", entries, err)
			}
		})
	}
}

func TestArchiveRegistryLayoutRejectsIncompleteAndChangedContent(t *testing.T) {
	for _, failure := range []string{"missing config", "changed layer", "wrong architecture", "invalid digest", "wrong size", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			layout, root, _ := registryTestLayout(t, "example.test/image:1")
			name, _ := registryBlobName(root.Digest)
			var manifest struct {
				Config registryDescriptor
				Layers []registryDescriptor
			}
			if err := readRegistryJSON(filepath.Join(layout, name), &manifest); err != nil {
				t.Fatal(err)
			}
			configName, _ := registryBlobName(manifest.Config.Digest)
			layerName, _ := registryBlobName(manifest.Layers[0].Digest)
			ctx := context.Background()
			switch failure {
			case "missing config":
				if err := os.Remove(filepath.Join(layout, configName)); err != nil {
					t.Fatal(err)
				}
			case "changed layer":
				body, _ := os.ReadFile(filepath.Join(layout, layerName))
				body[100] ^= 1
				archiveTestWriteFile(t, filepath.Join(layout, layerName), body)
			case "wrong architecture":
				archiveTestWriteFile(t, filepath.Join(layout, configName), []byte(`{"os":"linux","architecture":"arm64"}`))
			case "invalid digest":
				root.Digest = "sha256:../../private"
				fallthrough
			case "wrong size":
				root.Size++
				body, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []registryDescriptor{root}})
				archiveTestWriteFile(t, filepath.Join(layout, "index.json"), body)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			out := filepath.Join(t.TempDir(), "images.tar")
			if err := archiveRegistryLayout(ctx, layout, "example.test/image:1", out); err == nil {
				t.Fatal("unverified registry content was archived")
			}
			if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed registry copy retained an archive")
			}
		})
	}
}

func TestRegistryRecoveryRealORAS(t *testing.T) {
	oras, err := exec.LookPath("oras")
	if err != nil {
		t.Skip("ORAS is unavailable")
	}
	_, root, entries := registryTestLayout(t, "example.test/source:1")
	blobs := map[string][]byte{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.header.Name, "blobs/sha256/") {
			blobs["sha256:"+filepath.Base(entry.header.Name)] = entry.body
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		digest := filepath.Base(r.URL.Path)
		body, exists := blobs[digest]
		if !exists {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if strings.Contains(r.URL.Path, "/manifests/") {
			w.Header().Set("Content-Type", root.MediaType)
		}
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	}))
	defer server.Close()
	image := strings.TrimPrefix(server.URL, "http://") + "/recovery/image:1"
	auth := t.TempDir()
	archiveTestWriteFile(t, filepath.Join(auth, "config.json"), []byte(`{"auths":{}}`))
	var log bytes.Buffer
	run := func(ctx context.Context, process remote.ProcessOptions) error {
		args := process.Args
		process.Args = append(append([]string(nil), args[:len(args)-2]...), "--from-plain-http", args[len(args)-2], args[len(args)-1])
		return runProcess(ctx, process)
	}
	out := filepath.Join(t.TempDir(), "images.tar")
	env := map[string]string{"EVA_ORAS_BINARY": oras, "DOCKER_CONFIG": auth, "HOME": t.TempDir()}
	if err := saveRegistryImages(context.Background(), run, remote.ProcessOptions{Env: env}, out, []string{image}, map[string]string{image: root.Digest}, &log); err != nil {
		t.Fatalf("real ORAS registry recovery failed: %v\n%s", err, &log)
	}
	if err := validateDockerArchive(out, []string{image}); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryCopyFailureDoesNotExposeCredentials(t *testing.T) {
	run := func(_ context.Context, process remote.ProcessOptions) error {
		fmt.Fprintln(process.Streams.Stderr, "authentication failed: private-credential")
		return errors.New("private-credential")
	}
	var log bytes.Buffer
	out := filepath.Join(t.TempDir(), "images.tar")
	env := map[string]string{"EVA_ORAS_BINARY": "/trusted/oras"}
	err := saveRegistryImages(context.Background(), run, remote.ProcessOptions{Env: env}, out, []string{"example.test/image:1"}, map[string]string{"example.test/image:1": "sha256:" + strings.Repeat("a", 64)}, &log)
	if err == nil || strings.Contains(err.Error(), "private-credential") || !strings.Contains(log.String(), "private-credential") {
		t.Fatalf("registry failure succeeded, lost diagnostics or leaked credentials: %v", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed registry copy retained an archive")
	}
	if paths, _ := filepath.Glob(filepath.Join(filepath.Dir(out), ".registry-export-*")); len(paths) != 0 {
		t.Fatal("failed registry copy retained staging")
	}
}
