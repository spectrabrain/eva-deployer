package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOnlineRuntimePythonSelection(t *testing.T) {
	for _, test := range []struct {
		name     string
		versions map[string]string
		want     string
	}{
		{"Ubuntu 24.04", map[string]string{"python3": "3.12"}, "python3"},
		{"newer default", map[string]string{"python3": "3.14"}, "python3"},
		{"Ubuntu 22.04 with side-by-side Python", map[string]string{"python3": "3.10", "python3.12": "3.12"}, "python3.12"},
		{"side-by-side 3.13", map[string]string{"python3": "3.10", "python3.13": "3.13"}, "python3.13"},
		{"reject old default", map[string]string{"python3": "3.10"}, ""},
		{"reject Python 3.11", map[string]string{"python3": "3.11"}, ""},
		{"missing Python", nil, ""},
		{"invalid probe result", map[string]string{"python3": "3.12.invalid"}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := selectOnlineRuntimePythonWithProbe(func(name string) (string, error) {
				if version, ok := test.versions[name]; ok {
					return version, nil
				}
				return "", errors.New("not installed")
			})
			if got != test.want {
				t.Fatalf("selected %q, want %q (error: %v)", got, test.want, err)
			}
			if test.want != "" && err != nil {
				t.Fatal(err)
			}
			if test.want == "" {
				if err == nil || !strings.Contains(err.Error(), "requires Python 3.12") || !strings.Contains(err.Error(), "python3.12-venv") {
					t.Fatalf("expected actionable Python prerequisite error, got %v", err)
				}
				if version, ok := test.versions["python3"]; ok && version == "3.10" && !strings.Contains(err.Error(), "python3=3.10") {
					t.Fatalf("missing detected Python version: %v", err)
				}
			}
		})
	}
}

func TestOnlineRuntimeProbesSideBySidePythonOnPATH(t *testing.T) {
	bin := t.TempDir()
	for name, version := range map[string]string{"python3": "3.10", "python3.12": "3.12"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nprintf '%s\\n' '"+version+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	python, err := selectOnlineRuntimePython()
	if err != nil || python != "python3.12" {
		t.Fatalf("side-by-side Python was not selected: %q, %v", python, err)
	}
}

func TestBootstrapOnlineCreatesVenvWithSelectedPython(t *testing.T) {
	spec, payloads := testOnlineBootstrapSpec(t)
	dependencies := testOnlineBootstrapDependencies(t, payloads)
	dependencies.selectPython = func() (string, error) { return "python3.12", nil }
	run := dependencies.run
	created := false
	dependencies.run = func(name string, args ...string) error {
		if len(args) >= 2 && args[0] == "-m" && args[1] == "venv" {
			if name != "python3.12" {
				t.Fatalf("created Runtime with incompatible default Python: %s", name)
			}
			created = true
			return run("python3", args...)
		}
		return run(name, args...)
	}
	if _, err := bootstrapOnline(filepath.Join(t.TempDir(), "runtime"), spec, dependencies); err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("selected Python was not used for Runtime venv")
	}
}

func TestBootstrapOnlineOldPythonFailsBeforePipAndPreservesRuntime(t *testing.T) {
	destination := createRuntime(t)
	spec, payloads := testOnlineBootstrapSpec(t)
	dependencies := testOnlineBootstrapDependencies(t, payloads)
	dependencies.selectPython = func() (string, error) {
		return selectOnlineRuntimePythonWithProbe(func(name string) (string, error) {
			if name == "python3" {
				return "3.10", nil
			}
			return "", errors.New("not installed")
		})
	}
	dependencies.run = func(name string, args ...string) error {
		t.Fatalf("incompatible Python started venv or pip: %s %v", name, args)
		return nil
	}
	dependencies.download = func(string, string) error {
		t.Fatal("incompatible Python started Runtime downloads")
		return nil
	}
	if _, err := bootstrapOnline(destination, spec, dependencies); err == nil || !strings.Contains(err.Error(), "python3=3.10") {
		t.Fatalf("expected detected Python 3.10 error: %v", err)
	}
	resolved, err := Resolve(destination)
	if err != nil || resolved.Descriptor.Version != "3.2.0" {
		t.Fatalf("Python prerequisite failure changed existing Runtime: %#v, %v", resolved.Descriptor, err)
	}
}

func TestPrepareOnlineRuntimePythonReusesCompatibleEnvironment(t *testing.T) {
	err := prepareOnlineRuntimePython(
		func(name string, args ...string) (string, error) {
			t.Fatalf("compatible Python changed APT: %s %v", name, args)
			return "", nil
		},
		func(string) (string, error) { return "3.12", nil },
		func(string) error { return nil },
		func() ([]byte, error) { t.Fatal("compatible Python inspected OS repositories"); return nil, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestPrepareOnlineRuntimePythonInstallsOnUbuntu2204(t *testing.T) {
	for _, existingRepository := range []bool{false, true} {
		t.Run(map[bool]string{false: "add repository", true: "reuse repository"}[existingRepository], func(t *testing.T) {
			steps := []prerequisiteStep{{command: []string{"apt-cache", "policy", "python3.12"}, output: "Candidate: (none)\n"}}
			if existingRepository {
				steps[0].output = "Candidate: 3.12.16-1+jammy1\n"
			} else {
				steps = append(steps,
					prerequisiteStep{command: []string{"apt-get", "install", "--yes", "--no-remove", "software-properties-common"}},
					prerequisiteStep{command: []string{"add-apt-repository", "--yes", "--no-update", "ppa:deadsnakes/ppa"}},
					prerequisiteStep{command: []string{"apt-get", "update"}},
					prerequisiteStep{command: []string{"apt-cache", "policy", "python3.12"}, output: "Candidate: 3.12.16-1+jammy1\n"},
				)
			}
			steps = append(steps, prerequisiteStep{command: []string{"apt-get", "install", "--yes", "--no-remove", "python3.12", "python3.12-venv"}})
			run, verify := prerequisiteScript(t, steps)
			installed := false
			err := prepareOnlineRuntimePython(
				func(name string, args ...string) (string, error) {
					output, err := run(name, args...)
					if name == "apt-get" && len(args) > 3 && args[3] == "python3.12" && err == nil {
						installed = true
					}
					return output, err
				},
				func(name string) (string, error) {
					if name == "python3" {
						return "3.10", nil
					}
					if name == "python3.12" && installed {
						return "3.12", nil
					}
					return "", errors.New("not installed")
				},
				func(name string) error {
					if name != "python3.12" || !installed {
						t.Fatalf("prepared incompatible Python: %s", name)
					}
					return nil
				},
				func() ([]byte, error) {
					if existingRepository {
						t.Fatal("available Python package added a new repository")
					}
					return []byte("ID=ubuntu\nVERSION_ID=\"22.04\"\n"), nil
				},
			)
			verify()
			if err != nil || !installed {
				t.Fatalf("Python was not automatically installed: %v", err)
			}
		})
	}
}

func TestPrepareOnlineRuntimePythonInstallsMissingVenv(t *testing.T) {
	run, verify := prerequisiteScript(t, []prerequisiteStep{
		{command: []string{"apt-cache", "policy", "python3.12-venv"}, output: "Candidate: 3.12.16-1+jammy1\n"},
		{command: []string{"apt-get", "install", "--yes", "--no-remove", "python3.12-venv"}},
	})
	checks := 0
	err := prepareOnlineRuntimePython(run,
		func(name string) (string, error) {
			if name == "python3" {
				return "3.10", nil
			}
			return "3.12", nil
		},
		func(string) error {
			checks++
			if checks == 1 {
				return errors.New("ensurepip unavailable")
			}
			return nil
		},
		func() ([]byte, error) { t.Fatal("available venv package added a repository"); return nil, nil },
	)
	verify()
	if err != nil || checks != 2 {
		t.Fatalf("venv was not automatically installed and verified: %v", err)
	}
}

func TestPythonRepositoryDoesNotAddPPAOnOtherOS(t *testing.T) {
	for _, osRelease := range []string{"ID=debian\nVERSION_ID=12\n", "ID=ubuntu\nVERSION_ID=\"24.04\"\n"} {
		run, verify := prerequisiteScript(t, []prerequisiteStep{{command: []string{"apt-cache", "policy", "python3.12"}}})
		err := ensureOnlinePythonRepository(run, func() ([]byte, error) { return []byte(osRelease), nil }, "python3.12")
		verify()
		if err == nil || !strings.Contains(err.Error(), "no APT candidate") {
			t.Fatalf("unexpected repository setup result: %v", err)
		}
	}
}

func TestPythonRepositoryFailuresStopAutomaticPreparation(t *testing.T) {
	steps := []prerequisiteStep{
		{command: []string{"apt-cache", "policy", "python3.12"}},
		{command: []string{"apt-get", "install", "--yes", "--no-remove", "software-properties-common"}},
		{command: []string{"add-apt-repository", "--yes", "--no-update", "ppa:deadsnakes/ppa"}},
		{command: []string{"apt-get", "update"}},
		{command: []string{"apt-cache", "policy", "python3.12"}, output: "Candidate: 3.12.16\n"},
	}
	for stage := range steps {
		failure := errors.New("repository preparation unavailable")
		partial := append([]prerequisiteStep(nil), steps[:stage+1]...)
		partial[stage].err = failure
		run, verify := prerequisiteScript(t, partial)
		err := ensureOnlinePythonRepository(run, func() ([]byte, error) { return []byte("ID=ubuntu\nVERSION_ID=22.04\n"), nil }, "python3.12")
		verify()
		if !errors.Is(err, failure) {
			t.Fatalf("stage %d failure was not preserved: %v", stage, err)
		}
	}
}
