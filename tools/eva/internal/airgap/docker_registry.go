package airgap

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"eva-deployer/tools/eva/internal/remote"
	dockerreference "github.com/distribution/reference"
)

type registryDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform,omitempty"`
}

// Docker can silently export only the manifest when cached children are
// incomplete. Read the same immutable target from the registry using the
// managed ORAS and existing private Docker auth, without changing daemon data.
func saveRegistryImages(ctx context.Context, run remote.ProcessRunner, process remote.ProcessOptions, archive string, images []string, targets map[string]string, private io.Writer) error {
	oras := process.Env["EVA_ORAS_BINARY"]
	if !filepath.IsAbs(oras) {
		return errors.New("managed ORAS is required to recover incomplete Docker content; no Bundle was published")
	}
	work, err := os.MkdirTemp(filepath.Dir(archive), ".registry-export-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	var parts []string
	for i, image := range images {
		named, err := dockerreference.ParseNormalizedNamed(image)
		if err != nil {
			return err
		}
		if _, err := registryBlobName(targets[image]); err != nil {
			return errors.New("registry recovery requires a verified sha256 target; no Bundle was published")
		}
		pinned := dockerreference.TrimNamed(named).String() + "@" + targets[image]
		layout := filepath.Join(work, fmt.Sprintf("layout-%d", i))
		process.Path = oras
		process.Args = []string{"cp", "--no-tty", "--platform", "linux/amd64", "--to-oci-layout"}
		if config := process.Env["DOCKER_CONFIG"]; config != "" {
			process.Args = append(process.Args, "--from-registry-config", filepath.Join(config, "config.json"))
		}
		process.Args = append(process.Args, pinned, layout+":source")
		process.Streams = remote.Streams{Stdout: private, Stderr: private}
		fmt.Fprintf(private, "[INFO] recovering image content from registry: %s\n", pinned)
		if err := run(ctx, process); err != nil {
			fmt.Fprintf(private, "[ERROR] registry image recovery: %v\n", err)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("registry image recovery failed; no Bundle was published")
		}
		part := filepath.Join(work, fmt.Sprintf("%d.tar", i))
		if err := archiveRegistryLayout(ctx, layout, image, part); err != nil {
			return fmt.Errorf("verify recovered registry image: %w", err)
		}
		if err := validateDockerArchive(part, []string{image}); err != nil {
			return fmt.Errorf("registry image content is incomplete: %w", err)
		}
		parts = append(parts, part)
	}
	if len(parts) == 1 {
		return os.Rename(parts[0], archive)
	}
	return mergeDockerExports(ctx, parts, archive)
}

func registryBlobName(digest string) (string, error) {
	if len(digest) != 71 || digest[:7] != "sha256:" {
		return "", errors.New("unsupported registry content digest")
	}
	value, err := hex.DecodeString(digest[7:])
	if err != nil || len(value) != sha256.Size || hex.EncodeToString(value) != digest[7:] {
		return "", errors.New("invalid registry content digest")
	}
	return "blobs/sha256/" + digest[7:], nil
}

func readRegistryJSON(path string, target any) error {
	file, err := openArchiveSource(path, nil)
	if err != nil {
		return err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, archiveJSONLimit+1))
	if err != nil {
		return err
	}
	if len(contents) > archiveJSONLimit {
		return errors.New("registry metadata exceeds 16 MiB")
	}
	return json.Unmarshal(contents, target)
}

// Emit the usual Docker/OCI hybrid archive from verified registry blobs. Tags
// are explicit, fully qualified in OCI metadata, and familiar in manifest.json.
func archiveRegistryLayout(ctx context.Context, layout, image, destination string) (resultErr error) {
	var index struct {
		SchemaVersion int                  `json:"schemaVersion"`
		Manifests     []registryDescriptor `json:"manifests"`
	}
	if err := readRegistryJSON(filepath.Join(layout, "index.json"), &index); err != nil {
		return err
	}
	if index.SchemaVersion != 2 || len(index.Manifests) != 1 {
		return errors.New("registry copy must select one linux/amd64 manifest")
	}
	root := index.Manifests[0]
	if root.MediaType != "application/vnd.oci.image.manifest.v1+json" && root.MediaType != "application/vnd.docker.distribution.manifest.v2+json" {
		return errors.New("registry copy did not select an image manifest")
	}
	manifestName, err := registryBlobName(root.Digest)
	if err != nil {
		return err
	}
	var manifest struct {
		SchemaVersion int                  `json:"schemaVersion"`
		Config        registryDescriptor   `json:"config"`
		Layers        []registryDescriptor `json:"layers"`
	}
	if err := readRegistryJSON(filepath.Join(layout, manifestName), &manifest); err != nil {
		return err
	}
	if manifest.SchemaVersion != 2 {
		return errors.New("invalid registry image manifest")
	}
	configName, err := registryBlobName(manifest.Config.Digest)
	if err != nil {
		return err
	}
	var config struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	}
	if err := readRegistryJSON(filepath.Join(layout, configName), &config); err != nil {
		return err
	}
	if config.OS != "linux" || config.Architecture != "amd64" {
		return errors.New("registry image config must target linux/amd64")
	}
	canonical, err := dockerImageReferenceKey(image)
	if err != nil {
		return err
	}
	named, err := dockerreference.ParseNormalizedNamed(canonical)
	if err != nil {
		return err
	}
	tagged, ok := named.(dockerreference.Tagged)
	if !ok {
		return errors.New("registry recovery requires an image tag")
	}
	root.Annotations = map[string]string{"io.containerd.image.name": canonical, "org.opencontainers.image.ref.name": tagged.Tag()}
	root.Platform = &struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	}{OS: "linux", Architecture: "amd64"}
	index.Manifests[0] = root
	entry := dockerSaveManifestEntry{Config: configName, RepoTags: []string{image}, Layers: []string{}}
	blobs := make(map[string]registryDescriptor)
	for _, desc := range append([]registryDescriptor{root, manifest.Config}, manifest.Layers...) {
		name, err := registryBlobName(desc.Digest)
		if err != nil {
			return err
		}
		if old, exists := blobs[name]; exists && old.Size != desc.Size {
			return errors.New("conflicting registry blob sizes")
		}
		blobs[name] = desc
	}
	for _, desc := range manifest.Layers {
		name, _ := registryBlobName(desc.Digest)
		entry.Layers = append(entry.Layers, name)
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err := output.Close(); resultErr == nil {
			resultErr = err
		}
		if resultErr != nil {
			os.Remove(destination)
		}
	}()
	tw := tar.NewWriter(output)
	names := make([]string, 0, len(blobs))
	for name := range blobs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := func() error {
			file, err := openArchiveSource(filepath.Join(layout, name), nil)
			if err != nil {
				return err
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil || info.Size() != blobs[name].Size {
				return fmt.Errorf("registry blob size mismatch: %s", name)
			}
			if err := tw.WriteHeader(cleanArchiveHeader(name, 0o644, info.Size(), false)); err != nil {
				return err
			}
			hash := sha256.New()
			if _, err := io.Copy(io.MultiWriter(tw, hash), file); err != nil {
				return err
			}
			if "sha256:"+hex.EncodeToString(hash.Sum(nil)) != blobs[name].Digest {
				return fmt.Errorf("registry blob digest mismatch: %s", name)
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}
	indexData := struct {
		SchemaVersion int                  `json:"schemaVersion"`
		MediaType     string               `json:"mediaType"`
		Manifests     []registryDescriptor `json:"manifests"`
	}{2, "application/vnd.oci.image.index.v1+json", index.Manifests}
	for _, item := range []struct {
		name  string
		value any
	}{{"oci-layout", map[string]string{"imageLayoutVersion": "1.0.0"}}, {"index.json", indexData}, {"manifest.json", []dockerSaveManifestEntry{entry}}} {
		body, err := json.Marshal(item.value)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(cleanArchiveHeader(item.name, 0o644, int64(len(body)), false)); err != nil {
			return err
		}
		if _, err := tw.Write(body); err != nil {
			return err
		}
	}
	return tw.Close()
}
