// Package apt validates APT repositories and repairs supported signing-key
// failures during EVA installation and Runtime bootstrap.
package apt

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	jenkinsRepositoryURL = "https://pkg.jenkins.io/debian-stable"
	jenkinsKeyURL        = "https://pkg.jenkins.io/debian-stable/jenkins.io-2026.key"
	jenkinsFingerprint   = "5E386EADB55F01504CAE8BCF7198F4B714ABFC68"
	jenkinsKeyPath       = "/etc/apt/keyrings/jenkins-keyring.asc"
	jenkinsSourcePath    = "/etc/apt/sources.list.d/jenkins.list"
)

type Command struct {
	Name       string
	Args       []string
	Privileged bool
}

type Result struct {
	Output   string
	ExitCode int
}

type Runner func(Command) (Result, error)
type Fetcher func(string) ([]byte, error)

type Problem struct {
	Repository string
	KeyID      string
}

type Diagnosis struct {
	Output  string
	Healthy bool
	Problem *Problem
}

type Service struct {
	Run   Runner
	Fetch Fetcher
}

func NewService() Service {
	return Service{Run: systemRunner, Fetch: fetchKey}
}

// Ensure repairs only a recognized failure and revalidates APT before returning.
// The existing repair restores its original files when validation fails.
func (service Service) Ensure(output io.Writer) (bool, error) {
	if output == nil {
		output = io.Discard
	}
	diagnosis, err := service.Check()
	if err != nil {
		if diagnosis.Output != "" {
			fmt.Fprintln(output, diagnosis.Output)
		}
		return false, err
	}
	if diagnosis.Healthy {
		return false, nil
	}
	var repairErr error
	if diagnosis.Problem.Repository == nvidiaRepositoryURL {
		fmt.Fprintln(output, "[INFO] Repairing the NVIDIA Container Toolkit APT repository signing key")
		repairErr = service.RemediateKnownNVIDIA()
	} else {
		fmt.Fprintln(output, "[INFO] Repairing the Jenkins APT repository signing key")
		repairErr = service.RemediateKnownJenkins()
	}
	if repairErr != nil {
		return false, repairErr
	}
	fmt.Fprintln(output, "[OK] APT repository repair and validation succeeded")
	return true, nil
}

// Check recognizes supported signing-key failures even when apt-get exits zero
// after falling back to stale indexes. It does not change APT configuration.
func (service Service) Check() (Diagnosis, error) {
	result, err := service.run(Command{Name: "apt-get", Args: []string{"update"}, Privileged: true})
	if err != nil {
		return Diagnosis{}, fmt.Errorf("run apt-get update: %w", err)
	}
	diagnosis := Diagnosis{Output: result.Output}
	if isJenkins2026KeyFailure(result.Output) {
		diagnosis.Problem = &Problem{Repository: jenkinsRepositoryURL, KeyID: "7198F4B714ABFC68"}
		return diagnosis, nil
	}
	if strings.Contains(result.Output, nvidiaRepositoryURL) && strings.Contains(strings.ToUpper(result.Output), "NO_PUBKEY DDCAE044F796ECB0") {
		diagnosis.Problem = &Problem{Repository: nvidiaRepositoryURL, KeyID: "DDCAE044F796ECB0"}
		return diagnosis, nil
	}
	if result.ExitCode == 0 && !hasFailedIndexes(result.Output) {
		diagnosis.Healthy = true
		return diagnosis, nil
	}
	return diagnosis, fmt.Errorf("APT repository validation failed; inspect apt-get update output before applying EVA")
}

func (diagnosis Diagnosis) KnownProblemReport() string {
	if diagnosis.Problem == nil {
		return ""
	}
	if diagnosis.Problem.Repository == nvidiaRepositoryURL {
		return fmt.Sprintf("APT repository validation failed.\n\nRepository:\n%s\n\nProblem:\nMissing signing key %s\n\nAutomatic repair installs the fingerprint-verified official NVIDIA key at %s and scopes existing NVIDIA sources to it.\n",
			diagnosis.Problem.Repository, diagnosis.Problem.KeyID, nvidiaKeyPath)
	}
	return fmt.Sprintf(`APT repository validation failed.

Repository:
%s

Problem:
Missing signing key %s

Suggested action:
Install the official Jenkins 2026 repository key and update
the Jenkins LTS repository configuration.

Files to modify:
%s
%s

Manual remediation equivalent:
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL %s | sudo tee %s >/dev/null
echo "deb [signed-by=%s] %s binary/" | sudo tee %s >/dev/null
sudo chmod 0644 %s
sudo chmod 0644 %s
sudo apt-get update
`, diagnosis.Problem.Repository, diagnosis.Problem.KeyID, jenkinsKeyPath, jenkinsSourcePath,
		jenkinsKeyURL, jenkinsKeyPath, jenkinsKeyPath, jenkinsRepositoryURL, jenkinsSourcePath,
		jenkinsKeyPath, jenkinsSourcePath)
}

// RemediateKnownJenkins replaces only the Jenkins keyring and source entry.
// It restores both files when the post-change APT validation does not pass.
func (service Service) RemediateKnownJenkins() error {
	if service.Fetch == nil {
		return errors.New("APT remediation fetcher is not configured")
	}
	temporaryDirectory, err := os.MkdirTemp("", "eva-jenkins-apt-")
	if err != nil {
		return fmt.Errorf("create remediation staging directory: %w", err)
	}
	defer os.RemoveAll(temporaryDirectory)

	sourceBackup, sourceExists, err := service.backup(jenkinsSourcePath, filepath.Join(temporaryDirectory, "jenkins.list.backup"))
	if err != nil {
		return err
	}
	keyBackup, keyExists, err := service.backup(jenkinsKeyPath, filepath.Join(temporaryDirectory, "jenkins-keyring.asc.backup"))
	if err != nil {
		return err
	}

	key, err := service.Fetch(jenkinsKeyURL)
	if err != nil {
		return fmt.Errorf("download official Jenkins 2026 repository key: %w", err)
	}
	keyPath := filepath.Join(temporaryDirectory, "jenkins-keyring.asc")
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		return fmt.Errorf("stage Jenkins repository key: %w", err)
	}
	gpgHome := filepath.Join(temporaryDirectory, "gnupg")
	if err := os.Mkdir(gpgHome, 0o700); err != nil {
		return fmt.Errorf("create temporary GnuPG home: %w", err)
	}
	if err := service.verifyFingerprint(gpgHome, keyPath); err != nil {
		return err
	}
	sourcePath := filepath.Join(temporaryDirectory, "jenkins.list")
	if err := os.WriteFile(sourcePath, []byte("deb [signed-by="+jenkinsKeyPath+"] "+jenkinsRepositoryURL+" binary/\n"), 0o600); err != nil {
		return fmt.Errorf("stage Jenkins repository source: %w", err)
	}

	if err := service.require(Command{Name: "install", Args: []string{"-d", "-m", "0755", "/etc/apt/keyrings"}, Privileged: true}); err != nil {
		return err
	}
	if err := service.require(Command{Name: "install", Args: []string{"-m", "0644", keyPath, jenkinsKeyPath}, Privileged: true}); err != nil {
		restoreErr := service.restore(sourceBackup, sourceExists, keyBackup, keyExists)
		if restoreErr != nil {
			return fmt.Errorf("install Jenkins repository key: %w; restore backup: %v", err, restoreErr)
		}
		return fmt.Errorf("install Jenkins repository key: %w; the previous source and keyring were restored", err)
	}
	if err := service.require(Command{Name: "install", Args: []string{"-m", "0644", sourcePath, jenkinsSourcePath}, Privileged: true}); err != nil {
		restoreErr := service.restore(sourceBackup, sourceExists, keyBackup, keyExists)
		if restoreErr != nil {
			return fmt.Errorf("install Jenkins repository source: %w; restore backup: %v", err, restoreErr)
		}
		return err
	}

	diagnosis, checkErr := service.Check()
	if checkErr == nil && diagnosis.Healthy {
		return nil
	}
	restoreErr := service.restore(sourceBackup, sourceExists, keyBackup, keyExists)
	if restoreErr != nil {
		return fmt.Errorf("Jenkins repository remediation failed (%v); restore backup: %w", remediationFailure(diagnosis, checkErr), restoreErr)
	}
	return fmt.Errorf("Jenkins repository remediation failed (%v); the previous source and keyring were restored", remediationFailure(diagnosis, checkErr))
}

func (service Service) backup(path, backupPath string) (string, bool, error) {
	result, err := service.run(Command{Name: "test", Args: []string{"-f", path}, Privileged: true})
	if err != nil {
		return "", false, fmt.Errorf("inspect %s: %w", path, err)
	}
	if result.ExitCode == 1 {
		return backupPath, false, nil
	}
	if result.ExitCode != 0 {
		return "", false, fmt.Errorf("inspect %s: exit code %d: %s", path, result.ExitCode, strings.TrimSpace(result.Output))
	}
	if err := service.require(Command{Name: "cp", Args: []string{"--preserve=mode", "--", path, backupPath}, Privileged: true}); err != nil {
		return "", false, fmt.Errorf("back up %s: %w", path, err)
	}
	return backupPath, true, nil
}

func (service Service) restore(sourceBackup string, sourceExists bool, keyBackup string, keyExists bool) error {
	var failures []string
	for _, backup := range []struct {
		path   string
		exists bool
		target string
	}{
		{sourceBackup, sourceExists, jenkinsSourcePath},
		{keyBackup, keyExists, jenkinsKeyPath},
	} {
		var err error
		if backup.exists {
			err = service.require(Command{Name: "install", Args: []string{"-m", "0644", backup.path, backup.target}, Privileged: true})
		} else {
			err = service.require(Command{Name: "rm", Args: []string{"-f", "--", backup.target}, Privileged: true})
		}
		if err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

func (service Service) verifyFingerprint(gpgHome, keyPath string) error {
	return service.verifyRepositoryFingerprint(gpgHome, keyPath, jenkinsFingerprint, "Jenkins 2026")
}

func (service Service) verifyRepositoryFingerprint(gpgHome, keyPath, expected, repository string) error {
	result, err := service.run(Command{Name: "gpg", Args: []string{
		"--batch",
		"--no-options",
		"--homedir",
		gpgHome,
		"--no-default-keyring",
		"--keyring",
		"/dev/null",
		"--with-colons",
		"--show-keys",
		keyPath,
	}})
	if err != nil {
		return fmt.Errorf("inspect %s repository key fingerprint: %w", repository, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("inspect %s repository key fingerprint: exit code %d: %s", repository, result.ExitCode, strings.TrimSpace(result.Output))
	}
	verified := false
	primary := true
	for _, line := range strings.Split(result.Output, "\n") {
		fields := strings.Split(line, ":")
		if fields[0] == "pub" {
			primary = true
		} else if fields[0] == "sub" {
			primary = false
		} else if len(fields) > 9 && fields[0] == "fpr" && primary {
			if !strings.EqualFold(fields[9], expected) {
				return fmt.Errorf("official %s key fingerprint mismatch; expected %s", repository, expected)
			}
			verified = true
		}
	}
	if verified {
		return nil
	}
	return fmt.Errorf("official %s key fingerprint mismatch; expected %s", repository, expected)
}

func (service Service) require(command Command) error {
	result, err := service.run(command)
	if err != nil {
		return fmt.Errorf("run %s: %w", command.Name, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("run %s: exit code %d: %s", command.Name, result.ExitCode, strings.TrimSpace(result.Output))
	}
	return nil
}

func (service Service) run(command Command) (Result, error) {
	if service.Run == nil {
		return Result{}, errors.New("APT remediation runner is not configured")
	}
	return service.Run(command)
}

func remediationFailure(diagnosis Diagnosis, err error) string {
	if err != nil {
		return err.Error()
	}
	if diagnosis.Problem != nil {
		return "apt-get update still reports the " + diagnosis.Problem.Repository + " signing-key problem"
	}
	return "apt-get update did not succeed"
}

func hasFailedIndexes(output string) bool {
	for _, line := range strings.Split(strings.ToLower(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "err:") || strings.HasPrefix(line, "e:") || strings.HasPrefix(line, "w: failed to fetch ") ||
			strings.HasPrefix(line, "w: an error occurred during the signature verification") {
			return true
		}
	}
	return false
}

func isJenkins2026KeyFailure(output string) bool {
	return strings.Contains(output, jenkinsRepositoryURL) && strings.Contains(strings.ToUpper(output), "NO_PUBKEY 7198F4B714ABFC68")
}

func systemRunner(command Command) (Result, error) {
	name := command.Name
	arguments := append([]string(nil), command.Args...)
	if command.Privileged && os.Geteuid() != 0 {
		name = "sudo"
		arguments = append([]string{"--", command.Name}, arguments...)
	}
	process := exec.Command(name, arguments...)
	process.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	output, err := process.CombinedOutput()
	result := Result{Output: string(output)}
	if err == nil {
		return result, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.ExitCode = exitError.ExitCode()
		return result, nil
	}
	return Result{}, err
}

func fetchKey(url string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("unexpected HTTP status %s", response.Status)
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if len(contents) == 0 {
		return nil, errors.New("empty response")
	}
	return bytes.Clone(contents), nil
}
