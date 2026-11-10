package airgap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	var diagnostic bytes.Buffer
	// Keep Docker diagnostics private, just like download backend output.
	process.Streams = remote.Streams{Stdout: io.Discard, Stderr: &diagnostic}
	if err := run(ctx, process); err == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	message := strings.ToLower(diagnostic.String())
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
	process.Streams = remote.Streams{Stdout: io.Discard, Stderr: io.Discard}
	if err := run(ctx, process); err != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("Docker image export failed; no Bundle was published")
	}
	return nil
}
