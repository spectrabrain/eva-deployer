package runtime

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type pythonVersionProbe func(string) (string, error)

// Keep the system Python unchanged. On older controller hosts a separately
// installed Python can run the pinned Ansible packages in the managed venv.
func selectOnlineRuntimePython() (string, error) {
	return selectOnlineRuntimePythonWithProbe(probePythonVersion)
}

func probePythonVersion(name string) (string, error) {
	output, err := exec.Command(name, "-c", "import sys; print('%d.%d' % sys.version_info[:2])").Output()
	return strings.TrimSpace(string(output)), err
}

func checkPythonVenv(name string) error {
	return exec.Command(name, "-c", "import ensurepip, venv").Run()
}

func selectOnlineRuntimePythonWithProbe(probe pythonVersionProbe) (string, error) {
	if probe == nil {
		return "", fmt.Errorf("Online Runtime Python version probe is not configured")
	}
	var detected []string
	for _, name := range []string{"python3", "python3.12", "python3.13", "python3.14"} {
		version, err := probe(name)
		if err != nil {
			continue
		}
		version = strings.TrimSpace(version)
		parts := strings.Split(version, ".")
		if len(parts) != 2 {
			continue
		}
		major, majorErr := strconv.Atoi(parts[0])
		minor, minorErr := strconv.Atoi(parts[1])
		if majorErr != nil || minorErr != nil || major < 0 || minor < 0 {
			continue
		}
		detected = append(detected, name+"="+version)
		if major == 3 && minor >= 12 {
			return name, nil
		}
	}
	description := strings.Join(detected, ", ")
	if description == "" {
		description = "no usable Python found"
	}
	return "", fmt.Errorf("Online Runtime requires Python 3.12 or newer for ansible==13.6.0 and ansible-core==2.20.5; detected %s. Install python3.12 and python3.12-venv alongside the system Python, then rerun sudo eva runtime bootstrap. Ubuntu 22.04's default Python 3.10 is incompatible; installing python3-venv alone does not upgrade Python", description)
}

// Host preparation runs only for the managed /opt/eva/runtime installation.
// Existing compatible Python installations are reused; Ubuntu 22.04 can get
// Python 3.12 alongside its system Python through the deadsnakes repository.
func prepareOnlineRuntimePython(run prerequisiteRunner, probe pythonVersionProbe, checkVenv func(string) error, readOSRelease func() ([]byte, error)) error {
	python, err := selectOnlineRuntimePythonWithProbe(probe)
	if err != nil {
		fmt.Println("[INFO] Online Runtime is preparing Python 3.12 and venv")
		if err := ensureOnlinePythonRepository(run, readOSRelease, "python3.12"); err != nil {
			return err
		}
		if err := installOnlinePrerequisitePackages(run, "python3.12", "python3.12-venv"); err != nil {
			return err
		}
		python, err = selectOnlineRuntimePythonWithProbe(probe)
		if err != nil {
			return fmt.Errorf("verify automatically installed Runtime Python: %w", err)
		}
	}
	if err := checkVenv(python); err == nil {
		return nil
	}
	version, err := probe(python)
	if err != nil {
		return fmt.Errorf("inspect Runtime Python venv package: %w", err)
	}
	venvPackage := "python" + strings.TrimSpace(version) + "-venv"
	if err := ensureOnlinePythonRepository(run, readOSRelease, venvPackage); err != nil {
		return err
	}
	if err := installOnlinePrerequisitePackages(run, venvPackage); err != nil {
		return err
	}
	if err := checkVenv(python); err != nil {
		return fmt.Errorf("verify Runtime Python venv after installing %s: %w", venvPackage, err)
	}
	return nil
}

func ensureOnlinePythonRepository(run prerequisiteRunner, readOSRelease func() ([]byte, error), pkg string) error {
	available, err := onlinePythonPackageAvailable(run, pkg)
	if err != nil || available {
		return err
	}
	contents, err := readOSRelease()
	if err != nil {
		return fmt.Errorf("identify OS for Runtime Python package installation: %w", err)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(contents), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			fields[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), "\"'")
		}
	}
	if fields["ID"] != "ubuntu" || fields["VERSION_ID"] != "22.04" {
		return fmt.Errorf("Runtime requires Python 3.12 or newer; no APT candidate for %s on %s %s", pkg, fields["ID"], fields["VERSION_ID"])
	}
	fmt.Println("[INFO] Online Runtime is configuring the Python repository for Ubuntu 22.04")
	if err := installOnlinePrerequisitePackages(run, "software-properties-common"); err != nil {
		return err
	}
	if _, err := run("add-apt-repository", "--yes", "--no-update", "ppa:deadsnakes/ppa"); err != nil {
		return fmt.Errorf("configure Runtime Python repository: %w", err)
	}
	if _, err := run("apt-get", "update"); err != nil {
		return fmt.Errorf("update Runtime Python repository index: %w", err)
	}
	available, err = onlinePythonPackageAvailable(run, pkg)
	if err != nil {
		return err
	}
	if !available {
		return fmt.Errorf("Runtime Python repository has no APT candidate for %s", pkg)
	}
	return nil
}

func onlinePythonPackageAvailable(run prerequisiteRunner, pkg string) (bool, error) {
	output, err := run("apt-cache", "policy", pkg)
	if err != nil {
		return false, fmt.Errorf("check Runtime Python package %s: %w", pkg, err)
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "Candidate:" && fields[1] != "(none)" {
			return true, nil
		}
	}
	return false, nil
}
