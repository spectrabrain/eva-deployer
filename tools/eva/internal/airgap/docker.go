package airgap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"eva-deployer/tools/eva/internal/remote"
)

// Containerd-backed Docker can retain a multi-platform index even after pulling
// only linux/amd64. Select that platform during export as well, so manifest.json
// cannot select a different variant whose config or layers were not downloaded.
func saveDockerImages(ctx context.Context, run remote.ProcessRunner, process remote.ProcessOptions, archive string, images []string) error {
	base := []string{"save", "--output", archive}
	process.Path = "docker"
	process.Args = append([]string{"save", "--output", archive, "--platform", "linux/amd64"}, images...)
	private := process.Streams.Stderr
	if private == nil {
		private = io.Discard
	}
	var diagnostic bytes.Buffer
	// Retain the original error in the caller's private log as well as the
	// compatibility buffer. Neither upstream output nor runner errors are public.
	fmt.Fprintln(private, "[INFO] docker save --platform linux/amd64")
	process.Streams = remote.Streams{Stdout: io.Discard, Stderr: io.MultiWriter(&diagnostic, private)}
	if err := run(ctx, process); err == nil {
		return nil
	} else {
		fmt.Fprintf(private, "\n[ERROR] platform-specific Docker export: %v\n", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	message := strings.ToLower(diagnostic.String())
	if strings.Contains(message, "no suitable export target found") && strings.Contains(message, "does not provide the specified platform") {
		return saveDockerManifestGroups(ctx, run, process, archive, images, private)
	}
	unsupported := strings.Contains(message, "unknown flag: --platform") ||
		strings.Contains(message, "--platform requires api version") ||
		strings.Contains(message, `"--platform" requires api version`)
	if !unsupported {
		return errors.New("Docker image export failed; no Bundle was published")
	}
	// Docker before API 1.48 has no platform-selective save. Its legacy export is
	// still accepted only after validateDockerArchive verifies every image's
	// architecture, config, layer digests and expected tags. Never fall back for
	// missing content or other export failures, and never alter the daemon.
	if err := os.Remove(archive); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove unsupported Docker export: %w", err)
	}
	process.Args = append(append([]string(nil), base...), images...)
	fmt.Fprintln(private, "[INFO] Docker API lacks platform-selective save; trying legacy export")
	process.Streams = remote.Streams{Stdout: io.Discard, Stderr: private}
	if err := run(ctx, process); err != nil {
		fmt.Fprintf(private, "\n[ERROR] legacy Docker export: %v\n", err)
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("Docker image export failed; no Bundle was published")
	}
	return nil
}

// Docker's containerd store can reject --platform for a single manifest even
// when its config is linux/amd64. Inspect only the requested references before
// using an unrestricted export, and keep platform selection for index images.
func saveDockerManifestGroups(ctx context.Context, run remote.ProcessRunner, process remote.ProcessOptions, archive string, images []string, private io.Writer) error {
	var inspection bytes.Buffer
	process.Args = append([]string{"image", "inspect", "--format", `{"os":{{json .Os}},"architecture":{{json .Architecture}},"descriptor":{{json .Descriptor}}}`}, images...)
	process.Streams = remote.Streams{Stdout: &inspection, Stderr: private}
	if err := run(ctx, process); err != nil {
		fmt.Fprintf(private, "[ERROR] inspect Docker export targets: %v\n", err)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Docker export target inspection failed; no Bundle was published")
	}
	decoder := json.NewDecoder(&inspection)
	var singles, indexes []string
	targets := make(map[string]string)
	for _, image := range images {
		var metadata struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			Descriptor   struct {
				MediaType string `json:"mediaType"`
				Digest    string `json:"digest"`
			} `json:"descriptor"`
		}
		if err := decoder.Decode(&metadata); err != nil || metadata.OS != "linux" || metadata.Architecture != "amd64" {
			return fmt.Errorf("Docker export target %s is not verified linux/amd64; no Bundle was published", image)
		}
		targets[image] = metadata.Descriptor.Digest
		switch metadata.Descriptor.MediaType {
		case "application/vnd.docker.distribution.manifest.v2+json", "application/vnd.oci.image.manifest.v1+json":
			singles = append(singles, image)
		case "application/vnd.docker.distribution.manifest.list.v2+json", "application/vnd.oci.image.index.v1+json":
			indexes = append(indexes, image)
		default:
			return fmt.Errorf("Docker export target %s has an unsupported descriptor; no Bundle was published", image)
		}
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF || len(singles) == 0 {
		return errors.New("Docker export targets do not establish single-manifest compatibility; no Bundle was published")
	}
	if err := os.Remove(archive); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove partial Docker export: %w", err)
	}
	parts, err := os.MkdirTemp(filepath.Dir(archive), ".docker-export-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(parts)
	var archives []string
	for _, group := range []struct {
		images   []string
		platform bool
	}{{indexes, true}, {singles, false}} {
		if len(group.images) == 0 {
			continue
		}
		part := filepath.Join(parts, fmt.Sprintf("%d.tar", len(archives)))
		process.Args = []string{"save", "--output", part}
		if group.platform {
			process.Args = append(process.Args, "--platform", "linux/amd64")
		}
		process.Args = append(process.Args, group.images...)
		process.Streams = remote.Streams{Stdout: io.Discard, Stderr: private}
		fmt.Fprintf(private, "[INFO] exporting %d verified images; platform selection=%t\n", len(group.images), group.platform)
		if err := run(ctx, process); err != nil {
			fmt.Fprintf(private, "[ERROR] Docker group export: %v\n", err)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("Docker image group export failed; no Bundle was published")
		}
		if err := validateDockerArchive(part, group.images); err != nil {
			fmt.Fprintf(private, "[ERROR] Docker export returned incomplete content: %v\n", err)
			if process.Env["EVA_MANAGED_IMAGE_EXPORT"] != "true" {
				return errors.New("Docker exported incomplete image content; no Bundle was published")
			}
			if err := os.Remove(part); err != nil {
				return err
			}
			if err := saveRegistryImages(ctx, run, process, part, group.images, targets, private); err != nil {
				return err
			}
		}
		archives = append(archives, part)
	}
	if len(archives) == 1 {
		return os.Rename(archives[0], archive)
	}
	return mergeDockerExports(ctx, archives, archive)
}
