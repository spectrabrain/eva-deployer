package remote

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testManagedRegistry = "10.159.57.172:32080"

func TestMergeDockerInsecureRegistryPreservesSettings(
	t *testing.T,
) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	original := []byte(`{
  "default-runtime": "nvidia",
  "runtimes": {
    "nvidia": {
      "args": [],
      "path": "nvidia-container-runtime"
    }
  }
}`)

	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}

	changed, snapshot, err := mergeDockerInsecureRegistry(
		path,
		testManagedRegistry,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("registry configuration was not changed")
	}
	if !snapshot.Existed ||
		string(snapshot.Contents) != string(original) ||
		snapshot.Mode != 0o640 {
		t.Fatal("previous configuration was not preserved")
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var config map[string]any
	if err := json.Unmarshal(contents, &config); err != nil {
		t.Fatal(err)
	}
	if config["default-runtime"] != "nvidia" {
		t.Fatal("default runtime was changed")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf(
			"updated Docker daemon config mode=%#o expected=%#o",
			info.Mode().Perm(),
			os.FileMode(0o640),
		)
	}

	registries, ok := config["insecure-registries"].([]any)
	if !ok ||
		len(registries) != 1 ||
		registries[0] != testManagedRegistry {
		t.Fatalf(
			"unexpected insecure registries: %#v",
			registries,
		)
	}
}

func TestMergeDockerInsecureRegistryIsIdempotent(
	t *testing.T,
) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	contents := []byte(
		`{"insecure-registries":["10.159.57.172:32080"]}`,
	)

	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}

	changed, _, err := mergeDockerInsecureRegistry(
		path,
		testManagedRegistry,
	)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("existing registry configuration was rewritten")
	}
}

func TestMergeDockerInsecureRegistryCreatesConfiguration(
	t *testing.T,
) {
	path := filepath.Join(
		t.TempDir(),
		"docker",
		"daemon.json",
	)

	changed, snapshot, err := mergeDockerInsecureRegistry(
		path,
		testManagedRegistry,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || snapshot.Existed {
		t.Fatal("new configuration result is invalid")
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestMergeDockerInsecureRegistryRejectsInvalidInput(
	t *testing.T,
) {
	cases := map[string]string{
		"invalid JSON":          `{`,
		"registry not an array": `{"insecure-registries":"bad"}`,
		"registry not strings":  `{"insecure-registries":[1]}`,
	}

	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(
				t.TempDir(),
				"daemon.json",
			)
			if err := os.WriteFile(
				path,
				[]byte(contents),
				0o644,
			); err != nil {
				t.Fatal(err)
			}

			if _, _, err := mergeDockerInsecureRegistry(
				path,
				testManagedRegistry,
			); err == nil {
				t.Fatal("invalid configuration was accepted")
			}

			actual, err := os.ReadFile(path)
			if err != nil || string(actual) != contents {
				t.Fatalf(
					"invalid configuration changed: %q, %v",
					actual,
					err,
				)
			}
		})
	}
}

func TestRestoreDockerDaemonConfigRestoresFile(
	t *testing.T,
) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	original := []byte(`{"default-runtime":"nvidia"}`)

	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}

	_, snapshot, err := mergeDockerInsecureRegistry(
		path,
		testManagedRegistry,
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := restoreDockerDaemonConfig(
		path,
		snapshot,
	); err != nil {
		t.Fatal(err)
	}

	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != string(original) {
		t.Fatalf(
			"restored configuration = %q, %v",
			actual,
			err,
		)
	}
}

func TestRestoreAfterDockerValidationFailure(
	t *testing.T,
) {
	path := filepath.Join(t.TempDir(), "daemon.json")
	original := []byte(`{"default-runtime":"nvidia"}`)

	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}

	_, snapshot, err := mergeDockerInsecureRegistry(
		path,
		testManagedRegistry,
	)
	if err != nil {
		t.Fatal(err)
	}

	cause := errors.New("dockerd rejected the configuration")
	err = restoreAfterDockerValidationFailure(
		path,
		snapshot,
		cause,
	)
	if err == nil {
		t.Fatal("validation rollback unexpectedly succeeded")
	}
	if !strings.Contains(
		err.Error(),
		"previous configuration restored",
	) {
		t.Fatalf("validation rollback error=%v", err)
	}

	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != string(original) {
		t.Fatalf(
			"restored configuration=%q expected=%q",
			actual,
			original,
		)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf(
			"restored mode=%#o expected=%#o",
			info.Mode().Perm(),
			os.FileMode(0o640),
		)
	}
}

func TestValidateManagedHarborComposeFile(
	t *testing.T,
) {
	t.Run("regular file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "docker-compose.yml")
		if err := os.WriteFile(
			path,
			[]byte("services: {}\n"),
			0o640,
		); err != nil {
			t.Fatal(err)
		}

		if err := validateManagedHarborComposeFile(path); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "docker-compose.yml")
		if err := validateManagedHarborComposeFile(path); err == nil {
			t.Fatal("missing compose file was accepted")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target.yml")
		path := filepath.Join(root, "docker-compose.yml")

		if err := os.WriteFile(
			target,
			[]byte("services: {}\n"),
			0o640,
		); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}

		if err := validateManagedHarborComposeFile(path); err == nil {
			t.Fatal("symlink compose file was accepted")
		}
	})
}
