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
