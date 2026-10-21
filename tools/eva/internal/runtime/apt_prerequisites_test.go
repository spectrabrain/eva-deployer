package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type prerequisiteExitError int

func (err prerequisiteExitError) Error() string { return fmt.Sprintf("exit status %d", err) }
func (err prerequisiteExitError) ExitCode() int { return int(err) }

const brokenAPTDependencies = `You might want to run 'apt --fix-broken install' to correct these.
The following packages have unmet dependencies:
 libc6-dev : Depends: libc6 (= 2.39-0ubuntu8.7) but 2.39-0ubuntu8.9 is to be installed
E: Unmet dependencies. Try 'apt --fix-broken install' with no packages (or specify a solution).
`

type prerequisiteStep struct {
	command []string
	output  string
	err     error
}

// These expectations deliberately do not use production command builders:
// dropping a safety option must cause a regression failure.
func prerequisiteRepairCommands() [][]string {
	policy := []string{
		"--fix-broken", "--no-remove",
		"-o", "APT::Get::allow-downgrades=false",
		"-o", "APT::Get::allow-change-held-packages=false",
		"-o", "APT::Get::allow-remove-essential=false",
		"-o", "APT::Get::AllowUnauthenticated=false",
		"-o", "APT::Get::AutomaticRemove=false",
		"-o", "APT::Get::force-yes=false",
		"-o", "Dpkg::Options::=--force-confold",
	}
	preview := append([]string{"apt-get", "--simulate"}, policy...)
	repair := append([]string{"apt-get", "--yes"}, policy...)
	return [][]string{
		{"apt-get", "update"},
		{"apt-get", "install", "--yes", "--no-remove", "ca-certificates", "python3", "python3-venv"},
		append(preview, "install"),
		append(repair, "install"),
		{"dpkg", "--force-confold", "--configure", "-a"},
		{"apt-get", "check"},
		{"dpkg", "--audit"},
		{"apt-get", "install", "--yes", "--no-remove", "ca-certificates", "python3", "python3-venv"},
	}
}

func runPrerequisiteSteps(t *testing.T, steps []prerequisiteStep) error {
	t.Helper()
	run, verify := prerequisiteScript(t, steps)
	err := installOnlinePrerequisites(run)
	verify()
	return err
}

func prerequisiteScript(t *testing.T, steps []prerequisiteStep) (prerequisiteRunner, func()) {
	t.Helper()
	index := 0
	run := func(name string, args ...string) (string, error) {
		t.Helper()
		if index >= len(steps) {
			t.Fatalf("unexpected prerequisite command: %s %v", name, args)
		}
		step := steps[index]
		index++
		command := append([]string{name}, args...)
		if !reflect.DeepEqual(command, step.command) {
			t.Fatalf("command %d = %q, want %q", index, command, step.command)
		}
		return step.output, step.err
	}
	return run, func() {
		t.Helper()
		if index != len(steps) {
			t.Fatalf("executed %d commands, want %d", index, len(steps))
		}
	}
}

func aptRecoveryFixture(t *testing.T) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("testdata", "apt-repair-nine-upgrades.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func TestInstallOnlinePrerequisitesHealthy(t *testing.T) {
	commands := prerequisiteRepairCommands()
	if err := runPrerequisiteSteps(t, []prerequisiteStep{{command: commands[0]}, {command: commands[1]}}); err != nil {
		t.Fatal(err)
	}
}

func TestInstallOnlinePrerequisitesAutomaticallyRepairsNineUpgrades(t *testing.T) {
	commands := prerequisiteRepairCommands()
	steps := make([]prerequisiteStep, len(commands))
	for index, command := range commands {
		steps[index].command = command
	}
	steps[1].output, steps[1].err = brokenAPTDependencies, prerequisiteExitError(100)
	steps[2].output = aptRecoveryFixture(t)
	if err := runPrerequisiteSteps(t, steps); err != nil {
		t.Fatalf("expected automatic recovery and prerequisite retry: %v", err)
	}
}

func TestInstallOnlinePrerequisitesDoesNotRepairUnrelatedFailures(t *testing.T) {
	commands := prerequisiteRepairCommands()
	for _, test := range []struct {
		name, output string
		stage        int
		err          error
	}{
		{"index signature", "NO_PUBKEY", 0, prerequisiteExitError(100)},
		{"download", "E: Failed to fetch https://example.invalid/package.deb", 1, prerequisiteExitError(100)},
		{"lock", "E: Could not get lock /var/lib/dpkg/lock-frontend", 1, prerequisiteExitError(100)},
		{"missing package", "E: Unable to locate package python3-venv", 1, prerequisiteExitError(100)},
		{"held requested package", "E: Unable to correct problems, you have held broken packages.", 1, prerequisiteExitError(100)},
		{"dpkg interrupted", "E: dpkg was interrupted", 1, prerequisiteExitError(100)},
		{"other exit", brokenAPTDependencies, 1, prerequisiteExitError(1)},
		{"cannot launch", brokenAPTDependencies, 1, errors.New("command unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			steps := []prerequisiteStep{{command: commands[0]}}
			if test.stage == 1 {
				steps = append(steps, prerequisiteStep{command: commands[1]})
			}
			steps[test.stage].output, steps[test.stage].err = test.output, test.err
			err := runPrerequisiteSteps(t, steps)
			if !errors.Is(err, test.err) {
				t.Fatalf("original failure must be preserved without repair: %v", err)
			}
		})
	}
}

func TestInstallOnlinePrerequisitesStopsAtFailedRecoveryStage(t *testing.T) {
	commands := prerequisiteRepairCommands()
	for stage := 2; stage < len(commands); stage++ {
		t.Run(fmt.Sprint(stage), func(t *testing.T) {
			steps := make([]prerequisiteStep, stage+1)
			for index := range steps {
				steps[index].command = commands[index]
			}
			original := prerequisiteExitError(100)
			steps[1].output, steps[1].err = brokenAPTDependencies, original
			steps[2].output = aptRecoveryFixture(t)
			failure := errors.New("recovery stage failed")
			steps[stage].err = failure
			err := runPrerequisiteSteps(t, steps)
			if !errors.Is(err, failure) {
				t.Fatalf("failure was not propagated: %v", err)
			}
			if stage < len(commands)-1 && !errors.Is(err, original) {
				t.Fatalf("original dependency failure was lost: %v", err)
			}
		})
	}
}

func TestInstallOnlinePrerequisitesDoesNotLoopWhenRetryFails(t *testing.T) {
	commands := prerequisiteRepairCommands()
	steps := make([]prerequisiteStep, len(commands))
	for index, command := range commands {
		steps[index].command = command
	}
	steps[1].output, steps[1].err = brokenAPTDependencies, prerequisiteExitError(100)
	steps[2].output = aptRecoveryFixture(t)
	steps[7].output, steps[7].err = brokenAPTDependencies, prerequisiteExitError(100)
	if err := runPrerequisiteSteps(t, steps); err == nil || !strings.Contains(err.Error(), "retry Online Runtime prerequisites") {
		t.Fatalf("retry must stop rather than start another repair: %v", err)
	}
}

func TestInstallOnlinePrerequisitesRejectsIncompleteDpkgAudit(t *testing.T) {
	commands := prerequisiteRepairCommands()
	steps := make([]prerequisiteStep, 7)
	for index := range steps {
		steps[index].command = commands[index]
	}
	steps[1].output, steps[1].err = brokenAPTDependencies, prerequisiteExitError(100)
	steps[2].output = aptRecoveryFixture(t)
	steps[6].output = "The following packages have been unpacked but not yet configured: libc6-dev"
	if err := runPrerequisiteSteps(t, steps); err == nil || !strings.Contains(err.Error(), "incomplete packages") {
		t.Fatalf("audit with exit 0 and output must fail: %v", err)
	}
}

func TestInstallOnlinePrerequisitesRejectsUnsafeRepairPlans(t *testing.T) {
	fixture := aptRecoveryFixture(t)
	for name, plan := range map[string]string{
		"no summary":     "Inst libc6-dev [old] (new Ubuntu)\n",
		"duplicate":      fixture + fixture,
		"removals":       strings.Replace(fixture, "0 to remove", "1 to remove", 1),
		"removal step":   fixture + "Remv ubuntu-minimal [1.0]\n",
		"removal header": fixture + "The following packages will be REMOVED:\n ubuntu-minimal\n",
		"downgrade":      fixture + "The following packages will be DOWNGRADED:\n libc6\n",
		"hold change":    fixture + "The following held packages will be changed:\n libc6\n",
		"unsafe warning": fixture + "WARNING: The following packages cannot be authenticated!\n",
		"APT error":      fixture + "E: Unable to correct dependencies\n",
	} {
		t.Run(name, func(t *testing.T) {
			commands := prerequisiteRepairCommands()
			err := runPrerequisiteSteps(t, []prerequisiteStep{
				{command: commands[0]},
				{command: commands[1], output: brokenAPTDependencies, err: prerequisiteExitError(100)},
				{command: commands[2], output: plan},
			})
			if err == nil || !strings.Contains(err.Error(), "refuse unsafe or unrecognized") {
				t.Fatalf("plan must be refused before any repair command: %v", err)
			}
		})
	}
}

func TestPrerequisiteRunnerCapturesDiagnosticsWithStableEnvironment(t *testing.T) {
	t.Setenv("LC_ALL", "ko_KR.UTF-8")
	t.Setenv("DEBIAN_FRONTEND", "dialog")
	t.Setenv("EVA_PREREQUISITE_KEEP", "preserved")
	output, err := runPrerequisiteCommand("/bin/sh", "-c", `printf '%s\n' "$LC_ALL" "$DEBIAN_FRONTEND" "$EVA_PREREQUISITE_KEEP"; printf 'diagnostic\n' >&2; exit 100`)
	if !isAPTDependencyFailure(brokenAPTDependencies, err) {
		t.Fatalf("runner must preserve the actual exit status: %v", err)
	}
	for _, value := range []string{"C\n", "noninteractive\n", "preserved\n", "diagnostic\n"} {
		if !strings.Contains(output, value) {
			t.Errorf("runner did not capture %q", value)
		}
	}
}

func TestInstallOnlinePrerequisitesRejectsMissingRunner(t *testing.T) {
	if err := installOnlinePrerequisites(nil); err == nil {
		t.Fatal("missing runner accepted")
	}
}

func TestBootstrapOnlineContinuesAfterAutomaticDependencyRecovery(t *testing.T) {
	commands := prerequisiteRepairCommands()
	steps := make([]prerequisiteStep, len(commands))
	for index, command := range commands {
		steps[index].command = command
	}
	steps[1].output, steps[1].err = brokenAPTDependencies, prerequisiteExitError(100)
	steps[2].output = aptRecoveryFixture(t)
	runner, verify := prerequisiteScript(t, steps)
	destination := createRuntime(t)
	spec, payloads := testOnlineBootstrapSpec(t)
	dependencies := testOnlineBootstrapDependencies(t, payloads)
	prepared := false
	dependencies.prepareHost = func(root string) error {
		if root != destination {
			t.Fatalf("host preparation destination = %q, want %q", root, destination)
		}
		if err := installOnlinePrerequisites(runner); err != nil {
			return err
		}
		prepared = true
		return nil
	}
	run := dependencies.run
	dependencies.run = func(name string, args ...string) error {
		if !prepared {
			t.Fatal("Runtime construction started before prerequisite recovery completed")
		}
		return run(name, args...)
	}
	installed, err := bootstrapOnline(destination, spec, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	verify()
	if !prepared || installed.Descriptor.Version != "online-test" {
		t.Fatal("recovered bootstrap did not publish the validated Runtime")
	}
}

func TestBootstrapOnlinePreservesRuntimeWhenDependencyRecoveryFails(t *testing.T) {
	commands := prerequisiteRepairCommands()
	failure := errors.New("repair unavailable")
	runner, verify := prerequisiteScript(t, []prerequisiteStep{
		{command: commands[0]},
		{command: commands[1], output: brokenAPTDependencies, err: prerequisiteExitError(100)},
		{command: commands[2], err: failure},
	})
	destination := createRuntime(t)
	spec, payloads := testOnlineBootstrapSpec(t)
	dependencies := testOnlineBootstrapDependencies(t, payloads)
	dependencies.prepareHost = func(string) error { return installOnlinePrerequisites(runner) }
	dependencies.run = func(name string, args ...string) error {
		t.Fatalf("failed recovery started Runtime construction: %s %v", name, args)
		return nil
	}
	dependencies.download = func(string, string) error {
		t.Fatal("failed recovery started Runtime downloads")
		return nil
	}
	if _, err := bootstrapOnline(destination, spec, dependencies); !errors.Is(err, failure) {
		t.Fatalf("bootstrap lost dependency recovery failure: %v", err)
	}
	verify()
	resolved, err := Resolve(destination)
	if err != nil || resolved.Descriptor.Version != "3.2.0" {
		t.Fatalf("failed dependency recovery changed existing Runtime: %#v, %v", resolved.Descriptor, err)
	}
}

func TestBootstrapOnlineRequiresHostPreparationDependency(t *testing.T) {
	spec, payloads := testOnlineBootstrapSpec(t)
	dependencies := testOnlineBootstrapDependencies(t, payloads)
	dependencies.prepareHost = nil
	if _, err := bootstrapOnline(filepath.Join(t.TempDir(), "runtime"), spec, dependencies); err == nil || !strings.Contains(err.Error(), "dependencies are incomplete") {
		t.Fatalf("missing host preparation dependency accepted: %v", err)
	}
}

func TestPrepareOnlineRuntimeHostSkipsCustomDestination(t *testing.T) {
	if err := prepareOnlineRuntimeHost(filepath.Join(t.TempDir(), "runtime")); err != nil {
		t.Fatal(err)
	}
}
