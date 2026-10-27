package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"eva-deployer/tools/eva/internal/apt"
)

// prerequisiteRunner streams diagnostics to the operator and also returns them
// for narrowly classifying APT dependency failures. Other failures are not repaired.
type prerequisiteRunner func(string, ...string) (string, error)

func prepareOnlineRuntimeHost(destination string) error {
	if destination != DefaultRoot {
		return nil
	}
	return installOnlinePrerequisites(runPrerequisiteCommand)
}

func installOnlinePrerequisites(run prerequisiteRunner) error {
	return installOnlinePrerequisitesWithRepository(run, apt.NewService())
}

func installOnlinePrerequisitesWithRepository(run prerequisiteRunner, repository apt.Service) error {
	if run == nil {
		return errors.New("Online Runtime prerequisite runner is not configured")
	}
	var updateFailure error
	repository.Run = func(command apt.Command) (apt.Result, error) {
		output, err := run(command.Name, command.Args...)
		result := apt.Result{Output: output}
		var exit interface{ ExitCode() int }
		if errors.As(err, &exit) {
			if command.Name == "apt-get" && len(command.Args) == 1 && command.Args[0] == "update" && updateFailure == nil {
				updateFailure = err
			}
			result.ExitCode = exit.ExitCode()
			return result, nil
		}
		return result, err
	}
	if _, err := repository.Ensure(os.Stdout); err != nil {
		return fmt.Errorf("update APT package index for Online Runtime bootstrap: %w", errors.Join(err, updateFailure))
	}
	install := []string{"install", "--yes", "--no-remove", "ca-certificates", "python3", "python3-venv"}
	output, installErr := run("apt-get", install...)
	if installErr == nil {
		return nil
	}
	if !isAPTDependencyFailure(output, installErr) {
		return fmt.Errorf("install Online Runtime prerequisites: %w", installErr)
	}

	fmt.Fprintln(os.Stdout, "[INFO] Online Runtime detected broken APT dependencies; checking a no-removal repair plan")
	if err := repairOnlineDependencies(run); err != nil {
		return fmt.Errorf("install Online Runtime prerequisites: %w; automatic dependency recovery failed: %w", installErr, err)
	}
	fmt.Fprintln(os.Stdout, "[INFO] APT dependency recovery succeeded; retrying Online Runtime prerequisites")
	if _, err := run("apt-get", install...); err != nil {
		return fmt.Errorf("retry Online Runtime prerequisites after dependency recovery: %w", err)
	}
	return nil
}

func isAPTDependencyFailure(output string, err error) bool {
	var exit interface{ ExitCode() int }
	if !errors.As(err, &exit) || exit.ExitCode() != 100 {
		return false
	}
	text := strings.ToLower(output)
	// Require APT's explicit fix-broken advice, not exit code 100 alone (also
	// used for network, authentication, lock and unavailable-package failures).
	return strings.Contains(text, "unmet dependencies") &&
		(strings.Contains(text, "--fix-broken install") || strings.Contains(text, "apt-get -f install"))
}

func repairOnlineDependencies(run prerequisiteRunner) error {
	// Explicit false values override permissive site-wide apt.conf settings.
	// --yes alone is not a sufficient safeguard if Allow-Downgrades is configured.
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
	simulate := append([]string{"--simulate"}, policy...)
	simulate = append(simulate, "install")
	plan, err := run("apt-get", simulate...)
	if err != nil {
		return fmt.Errorf("simulate APT dependency repair: %w", err)
	}
	if err := validateAPTRepairPlan(plan); err != nil {
		return fmt.Errorf("refuse unsafe or unrecognized APT dependency repair plan: %w", err)
	}
	fmt.Fprintln(os.Stdout, "[INFO] Repairing APT dependencies without removals, downgrades or held-package changes")
	repair := append([]string{"--yes"}, policy...)
	repair = append(repair, "install")
	if _, err := run("apt-get", repair...); err != nil {
		return fmt.Errorf("repair APT dependencies: %w", err)
	}
	if _, err := run("dpkg", "--force-confold", "--configure", "-a"); err != nil {
		return fmt.Errorf("configure repaired APT packages: %w", err)
	}
	if _, err := run("apt-get", "check"); err != nil {
		return fmt.Errorf("verify repaired APT dependencies: %w", err)
	}
	// dpkg --audit can exit 0 even when it reports partially configured packages.
	audit, err := run("dpkg", "--audit")
	if err != nil {
		return fmt.Errorf("audit repaired APT packages: %w", err)
	}
	if strings.TrimSpace(audit) != "" {
		return errors.New("APT dependency recovery left incomplete packages; Runtime was not published")
	}
	return nil
}

var aptRepairSummary = regexp.MustCompile(`(?m)^([0-9]+) upgraded, ([0-9]+) newly installed, ([0-9]+) to remove and ([0-9]+) not upgraded\.$`)

func validateAPTRepairPlan(output string) error {
	text := strings.ToLower(output)
	for _, marker := range []string{
		"the following packages will be removed:", "the following packages will be downgraded:",
		"the following held packages will be changed:", "essential packages will be removed",
		"warning:",
	} {
		if strings.Contains(text, marker) {
			return fmt.Errorf("APT reported %q", marker)
		}
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "E:") {
			return errors.New("APT reported an error")
		}
		if strings.HasPrefix(line, "Remv ") {
			return errors.New("APT plan contains a package removal")
		}
	}
	summary := aptRepairSummary.FindAllStringSubmatch(output, -1)
	if len(summary) != 1 || summary[0][3] != "0" {
		return errors.New("expected a single APT repair summary with zero removals")
	}
	return nil
}

func runPrerequisiteCommand(name string, args ...string) (string, error) {
	command := exec.Command(name, args...)
	// Stable diagnostics for classification; do not allow locale or debconf to
	// turn an automatic bootstrap repair into an unbounded interactive prompt.
	environment := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "LC_ALL=") && !strings.HasPrefix(entry, "DEBIAN_FRONTEND=") {
			environment = append(environment, entry)
		}
	}
	command.Env = append(environment, "LC_ALL=C", "DEBIAN_FRONTEND=noninteractive")
	var output bytes.Buffer
	// A single shared Writer lets os/exec serialize stdout and stderr copies.
	stream := io.MultiWriter(os.Stdout, &output)
	command.Stdout, command.Stderr = stream, stream
	err := command.Run()
	return output.String(), err
}
