package remote

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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

func TestLocalHarborRecoversOnlySyslogStartFailures(t *testing.T) {
	for _, kind := range []string{
		"logger-running", "logger-stopped", "logger-unready", "logger-start-fails", "dependent-start-fails",
		"remote", "https", "wrong-port", "loopback-binding", "unrelated-image", "intentional-stop",
		"missing-logger", "different-project", "different-files", "missing-compose", "ambiguous-proxy",
		"no-compose", "docker-unavailable", "inspect-error", "unexpected-id",
	} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			compose := filepath.Join(root, "docker-compose.yml")
			if err := os.WriteFile(compose, []byte("services: {}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			makeContainer := func(id, service, image, port string) harborRecoveryContainer {
				c := harborRecoveryContainer{ID: id, Image: "goharbor/" + image + ":v2.15.2", Labels: map[string]string{
					composeLabel + "project": "harbor", composeLabel + "service": service,
					composeLabel + "project.working_dir": root, composeLabel + "project.config_files": compose,
				}}
				c.State.Status = "exited"
				c.State.Error = "failed to create task for container: failed to initialize logging driver: dial tcp 127.0.0.1:1514: connect: connection refused"
				c.Log.Type = "syslog"
				c.Log.Config = map[string]string{"syslog-address": "tcp://localhost:1514"}
				if port != "" {
					c.Ports = map[string][]struct{ HostIP, HostPort string }{"8080/tcp": {{HostIP: "0.0.0.0", HostPort: port}}}
				}
				return c
			}
			proxy := makeContainer("proxy-id", "proxy", "nginx-photon", "32080")
			core := makeContainer("core-id", "core", "harbor-core", "")
			logger := makeContainer("log-id", "log", "harbor-log", "1514")
			logger.Log.Type, logger.State.Error, logger.State.Status, logger.State.Running = "json-file", "", "running", true
			// Neither an intentionally stopped Harbor service nor a similarly
			// labelled unrelated workload may be started as a side effect.
			spare := makeContainer("stopped-id", "portal", "harbor-portal", "")
			spare.State.Error = ""
			other := makeContainer("other-id", "other", "other", "")
			other.Image = "example.test/not-harbor:latest"
			protocol := "http"
			switch kind {
			case "logger-stopped", "logger-start-fails":
				logger.State.Status, logger.State.Running = "exited", false
			case "https":
				protocol = "https"
			case "wrong-port":
				proxy.Ports = nil
			case "loopback-binding":
				proxy.Ports["8080/tcp"][0].HostIP = "127.0.0.1"
			case "unrelated-image":
				proxy.Image = "example.test/nginx-photon:v2.15.2"
			case "intentional-stop":
				proxy.State.Error, core.State.Error = "", ""
			case "different-project":
				logger.Labels[composeLabel+"project"] = "other"
			case "different-files":
				logger.Labels[composeLabel+"project.config_files"] = "/different/compose.yml"
			case "missing-compose":
				if err := os.Remove(compose); err != nil {
					t.Fatal(err)
				}
			}
			containers := []harborRecoveryContainer{proxy, core, spare, other}
			if kind != "missing-logger" {
				containers = append(containers, logger)
			}
			if kind == "ambiguous-proxy" {
				duplicate := proxy
				duplicate.ID = "duplicate-id"
				containers = append(containers, duplicate)
			}
			var events []string
			r := harborRecovery{
				localIPs: func(context.Context, string) ([]net.IP, error) {
					if kind == "remote" {
						return nil, nil
					}
					return []net.IP{net.ParseIP("10.83.33.15")}, nil
				},
				waitLogger: func(context.Context, string) error {
					events = append(events, "logger-ready")
					if kind == "logger-unready" {
						return context.DeadlineExceeded
					}
					return nil
				},
				run: func(_ context.Context, args ...string) ([]byte, error) {
					switch args[0] {
					case "ps":
						if kind == "no-compose" {
							return nil, nil
						}
						if kind == "docker-unavailable" {
							return nil, errors.New("Docker unavailable")
						}
						var ids []string
						for _, c := range containers {
							ids = append(ids, c.ID)
						}
						return []byte(strings.Join(ids, "\n")), nil
					case "inspect":
						if kind == "inspect-error" {
							return nil, errors.New("inspect failed")
						}
						if args[1] != "--format" || args[2] != harborRecoveryInspect {
							t.Fatal("inspection requested credential-bearing configuration")
						}
						// inspect emits one JSON object per container ID.
						var out []string
						for _, c := range containers {
							if kind == "unexpected-id" {
								c.ID = "unrequested-id"
							}
							data, _ := json.Marshal(c)
							out = append(out, string(data))
						}
						return []byte(strings.Join(out, "\n")), nil
					case "start":
						events = append(events, strings.Join(args, " "))
						if kind == "logger-start-fails" || kind == "dependent-start-fails" {
							return nil, errors.New("private daemon detail")
						}
						return nil, nil
					default:
						t.Fatalf("unsafe Docker command: %v", args)
						return nil, nil
					}
				},
			}
			recovered, err := r.recover(context.Background(), "10.83.33.15:32080", protocol)
			success := kind == "logger-running" || kind == "logger-stopped"
			wantError := map[string]bool{"logger-unready": true, "logger-start-fails": true, "dependent-start-fails": true,
				"inspect-error": true, "unexpected-id": true,
				"missing-logger": true, "different-project": true, "different-files": true, "missing-compose": true, "ambiguous-proxy": true, "intentional-stop": true}[kind]
			if recovered != success || (err != nil) != wantError {
				t.Fatalf("recovered=%v err=%v events=%v", recovered, err, events)
			}
			if err != nil && strings.Contains(err.Error(), "private daemon detail") {
				t.Fatal("daemon diagnostic leaked")
			}
			var want []string
			switch kind {
			case "logger-running", "dependent-start-fails":
				want = []string{"logger-ready", "start proxy-id core-id"}
			case "logger-stopped":
				want = []string{"start log-id", "logger-ready", "start proxy-id core-id"}
			case "logger-start-fails":
				want = []string{"start log-id"}
			case "logger-unready":
				want = []string{"logger-ready"}
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("recovery ordering=%v want=%v", events, want)
			}
		})
	}
}

func TestWaitHarborLoggerUsesTCPAndCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := waitHarborLogger(context.Background(), listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := waitHarborLogger(ctx, "127.0.0.1:0"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancelled logger probe did not terminate promptly")
	}
}

func TestHarborReadinessRequiresPingAndHealthyComponents(t *testing.T) {
	for _, healthy := range []bool{false, true} {
		t.Run(map[bool]string{false: "ping-only", true: "healthy"}[healthy], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2.0/ping":
					w.WriteHeader(http.StatusOK)
				case "/api/v2.0/health":
					status := "unhealthy"
					if healthy {
						status = "healthy"
					}
					_, _ = w.Write([]byte(`{"status":"` + status + `","components":[{"name":"core","status":"healthy"},{"name":"registry","status":"` + status + `"}]}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			receipt := HarborReceipt{Protocol: "http", Registry: strings.TrimPrefix(server.URL, "http://")}
			err := waitForManagedHarborAPI(context.Background(), receipt, 50*time.Millisecond)
			if (err == nil) != healthy {
				t.Fatalf("readiness healthy=%v error=%v", healthy, err)
			}
		})
	}
}
