package remote

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const publishBackendRelativePath = "scripts/remote/publish_release_to_target.sh"
const libexecRootOverrideEnv = "EVA_REMOTE_LIBEXEC_ROOT"

// ResolvePublishBackend locates the installed, versioned Remote publish
// backend. It intentionally has no repository or working-directory fallback.
func ResolvePublishBackend() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate EVA executable: %w", err)
	}
	return resolvePublishBackend(executable, os.Getenv(libexecRootOverrideEnv))
}

func resolvePublishBackend(executable, override string) (string, error) {
	backendRoot, err := resolveBackendRoot(executable, override)
	if err != nil {
		return "", err
	}
	backend := filepath.Join(backendRoot, filepath.FromSlash(publishBackendRelativePath))
	info, err := os.Lstat(backend)
	if err != nil {
		return "", fmt.Errorf("Remote publish backend is missing: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("Remote publish backend must be a regular non-symlink file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("Remote publish backend is not executable")
	}

	resolvedBackend, err := filepath.EvalSymlinks(backend)
	if err != nil {
		return "", fmt.Errorf("resolve Remote publish backend: %w", err)
	}
	if !isWithin(backendRoot, resolvedBackend) {
		return "", errors.New("Remote publish backend escapes the backend root")
	}
	return resolvedBackend, nil
}

func resolveBackendRoot(executable, override string) (string, error) {
	var root string
	if override != "" {
		absolute, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("resolve Remote backend override: %w", err)
		}
		root = absolute
	} else {
		realExecutable, err := filepath.EvalSymlinks(executable)
		if err != nil {
			return "", fmt.Errorf("resolve EVA executable path: %w", err)
		}
		info, err := os.Stat(realExecutable)
		if err != nil {
			return "", fmt.Errorf("read EVA executable: %w", err)
		}
		if !info.Mode().IsRegular() {
			return "", errors.New("EVA executable is not a regular file")
		}
		root = filepath.Join(filepath.Dir(filepath.Dir(realExecutable)), "libexec", "remote-root")
	}

	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("Remote backend root is unavailable: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("Remote backend root is not a directory")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve Remote backend root: %w", err)
	}
	return resolvedRoot, nil
}

func isWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
