package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"
)

const dockerDaemonConfigPath = "/etc/docker/daemon.json"

type dockerDaemonConfigSnapshot struct {
	Contents []byte
	Mode     os.FileMode
	Existed  bool
}

func ensureManagedRegistryTransport(
	ctx context.Context,
	registry string,
) error {
	changed, snapshot, err := mergeDockerInsecureRegistry(
		dockerDaemonConfigPath,
		registry,
	)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}

	if err := validateDockerDaemonConfig(ctx, dockerDaemonConfigPath); err != nil {
		return restoreAfterDockerValidationFailure(
			dockerDaemonConfigPath,
			snapshot,
			fmt.Errorf("validate Docker daemon configuration: %w", err),
		)
	}

	if err := restartDocker(ctx); err != nil {
		return restoreAfterRegistryTransportFailure(
			ctx,
			snapshot,
			fmt.Errorf("restart Docker after registry configuration: %w", err),
		)
	}

	if err := waitForDocker(ctx, 30*time.Second); err != nil {
		return restoreAfterRegistryTransportFailure(ctx, snapshot, err)
	}
	return nil
}

func mergeDockerInsecureRegistry(
	path string,
	registry string,
) (bool, dockerDaemonConfigSnapshot, error) {
	snapshot, err := readDockerDaemonConfig(path)
	if err != nil {
		return false, dockerDaemonConfigSnapshot{}, err
	}

	config := map[string]any{}
	if snapshot.Existed {
		if err := json.Unmarshal(snapshot.Contents, &config); err != nil {
			return false, snapshot, errors.New(
				"Docker daemon configuration is invalid JSON",
			)
		}
	}

	registries, err := dockerInsecureRegistries(config)
	if err != nil {
		return false, snapshot, err
	}
	if slices.Contains(registries, registry) {
		return false, snapshot, nil
	}

	registries = append(registries, registry)
	config["insecure-registries"] = registries

	updated, err := json.MarshalIndent(config, "", "    ")
	if err != nil {
		return false, snapshot, errors.New(
			"encode Docker daemon configuration",
		)
	}
	updated = append(updated, '\n')

	mode := os.FileMode(0o644)
	if snapshot.Existed && snapshot.Mode != 0 {
		mode = snapshot.Mode
	}

	if err := writeDockerDaemonConfig(path, updated, mode); err != nil {
		return false, snapshot, err
	}
	return true, snapshot, nil
}

func dockerInsecureRegistries(config map[string]any) ([]string, error) {
	value, exists := config["insecure-registries"]
	if !exists {
		return []string{}, nil
	}

	items, ok := value.([]any)
	if !ok {
		return nil, errors.New(
			"Docker insecure-registries must be an array",
		)
	}

	registries := make([]string, 0, len(items))
	for _, item := range items {
		registry, ok := item.(string)
		if !ok {
			return nil, errors.New(
				"Docker insecure-registries must contain only strings",
			)
		}
		registries = append(registries, registry)
	}
	return registries, nil
}

func readDockerDaemonConfig(
	path string,
) (dockerDaemonConfigSnapshot, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return dockerDaemonConfigSnapshot{}, nil
	}
	if err != nil {
		return dockerDaemonConfigSnapshot{},
			errors.New("inspect Docker daemon configuration")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return dockerDaemonConfigSnapshot{},
			errors.New("Docker daemon configuration must be a regular file")
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		return dockerDaemonConfigSnapshot{},
			errors.New("read Docker daemon configuration")
	}

	return dockerDaemonConfigSnapshot{
		Contents: contents,
		Mode:     info.Mode().Perm(),
		Existed:  true,
	}, nil
}

func writeDockerDaemonConfig(
	path string,
	contents []byte,
	mode os.FileMode,
) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return errors.New("create Docker configuration directory")
	}

	temporary, err := os.CreateTemp(
		filepath.Dir(path),
		".daemon.json-*",
	)
	if err != nil {
		return errors.New(
			"create temporary Docker daemon configuration",
		)
	}

	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return errors.New(
			"write temporary Docker daemon configuration",
		)
	}
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return errors.New(
			"set Docker daemon configuration permissions",
		)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return errors.New("sync Docker daemon configuration")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("close Docker daemon configuration")
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return errors.New("publish Docker daemon configuration")
	}
	return nil
}

func restoreDockerDaemonConfig(
	path string,
	snapshot dockerDaemonConfigSnapshot,
) error {
	if !snapshot.Existed {
		if err := os.Remove(path); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}

	mode := snapshot.Mode
	if mode == 0 {
		mode = 0o644
	}
	return writeDockerDaemonConfig(path, snapshot.Contents, mode)
}

func restoreAfterDockerValidationFailure(
	path string,
	snapshot dockerDaemonConfigSnapshot,
	cause error,
) error {
	if err := restoreDockerDaemonConfig(path, snapshot); err != nil {
		return fmt.Errorf(
			"%w; restore Docker daemon configuration: %v",
			cause,
			err,
		)
	}
	return fmt.Errorf("%w; previous configuration restored", cause)
}

func restoreAfterRegistryTransportFailure(
	ctx context.Context,
	snapshot dockerDaemonConfigSnapshot,
	cause error,
) error {
	if err := restoreDockerDaemonConfig(
		dockerDaemonConfigPath,
		snapshot,
	); err != nil {
		return fmt.Errorf(
			"%w; restore Docker daemon configuration: %v",
			cause,
			err,
		)
	}

	if err := restartDocker(ctx); err != nil {
		return fmt.Errorf(
			"%w; previous configuration restored but Docker restart failed: %v",
			cause,
			err,
		)
	}

	return fmt.Errorf("%w; previous configuration restored", cause)
}

func validateDockerDaemonConfig(
	ctx context.Context,
	path string,
) error {
	command := exec.CommandContext(
		ctx,
		"dockerd",
		"--validate",
		"--config-file",
		path,
	)
	if err := command.Run(); err != nil {
		return errors.New("dockerd rejected the configuration")
	}
	return nil
}

func restartDocker(ctx context.Context) error {
	return exec.CommandContext(
		ctx,
		"systemctl",
		"restart",
		"docker",
	).Run()
}

func waitForDocker(
	ctx context.Context,
	timeout time.Duration,
) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		if exec.CommandContext(ctx, "docker", "info").Run() == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New(
				"Docker did not become ready after restart",
			)
		case <-ticker.C:
		}
	}
}

func waitForManagedHarborAPI(
	ctx context.Context,
	receipt HarborReceipt,
	timeout time.Duration,
) error {
	endpoint := receipt.Protocol + "://" +
		receipt.Registry +
		"/api/v2.0/ping"

	client := &http.Client{
		Timeout: 4 * time.Second,
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		request, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			endpoint,
			nil,
		)
		if err != nil {
			return errors.New(
				"Managed Harbor readiness probe could not be created",
			)
		}

		response, err := client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New(
				"Managed Harbor did not become ready after Docker restart",
			)
		case <-ticker.C:
		}
	}
}

func recoverManagedHarbor(
	ctx context.Context,
	receipt HarborReceipt,
) error {
	if receipt.ManagedBy != "eva" ||
		receipt.InstallRoot != "/opt/eva/harbor" ||
		receipt.Protocol != "http" {
		return errors.New(
			"Managed Harbor recovery target does not match receipt",
		)
	}

	composePath := filepath.Join(
		receipt.InstallRoot,
		"harbor",
		"docker-compose.yml",
	)
	if err := validateManagedHarborComposeFile(composePath); err != nil {
		return err
	}

	command := exec.CommandContext(
		ctx,
		"docker",
		"compose",
		"--file",
		composePath,
		"up",
		"-d",
	)
	if err := command.Run(); err != nil {
		return errors.New("Managed Harbor compose recovery failed")
	}

	if err := waitForManagedHarborAPI(
		ctx,
		receipt,
		3*time.Minute,
	); err != nil {
		return fmt.Errorf(
			"Managed Harbor compose recovery did not become ready: %w",
			err,
		)
	}
	return nil
}

func validateManagedHarborComposeFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New(
			"Managed Harbor compose configuration is unavailable",
		)
	}
	if info.Mode()&os.ModeSymlink != 0 ||
		!info.Mode().IsRegular() {
		return errors.New(
			"Managed Harbor compose configuration must be a regular file",
		)
	}
	return nil
}
