package apt

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestEnsureHealthyOrUnknownRepositoriesDoNotRepair(t *testing.T) {
	for _, exitCode := range []int{0, 100} {
		service := Service{
			Fetch: func(string) ([]byte, error) { t.Fatal("unexpected key download"); return nil, nil },
			Run: func(command Command) (Result, error) {
				if command.Name != "apt-get" || strings.Join(command.Args, " ") != "update" {
					t.Fatal("healthy/unknown APT failure changed repository files")
				}
				return Result{ExitCode: exitCode, Output: "unrelated repository output"}, nil
			},
		}
		changed, err := service.Ensure(nil)
		if changed || (exitCode == 0) != (err == nil) {
			t.Fatalf("exit=%d changed=%t err=%v", exitCode, changed, err)
		}
	}
}

func TestEnsureRollsBackIfAPTStillFailsAfterAutomaticRepair(t *testing.T) {
	var commands []Command
	updates := 0
	service := Service{
		Fetch: func(string) ([]byte, error) { return []byte("official key fixture"), nil },
		Run: func(command Command) (Result, error) {
			commands = append(commands, command)
			switch command.Name {
			case "gpg":
				return Result{Output: "fpr:::::::::" + jenkinsFingerprint + ":\n"}, nil
			case "apt-get":
				updates++
				return Result{ExitCode: 100, Output: "Err: " + jenkinsRepositoryURL + "\nNO_PUBKEY 7198F4B714ABFC68\n"}, nil
			}
			return Result{}, nil
		},
	}
	var output bytes.Buffer
	changed, err := service.Ensure(&output)
	if changed || err == nil || !strings.Contains(err.Error(), "were restored") || updates != 2 {
		t.Fatalf("failed repair was not rolled back: changed=%t updates=%d err=%v", changed, updates, err)
	}
	if countCommand(commands, "install", jenkinsSourcePath) < 2 || countCommand(commands, "install", jenkinsKeyPath) < 2 {
		t.Fatal("automatic recovery did not restore both original files")
	}
}

func TestEnsureRejectsUnexpectedSigningKeyBeforeChangingRepositoryFiles(t *testing.T) {
	service := Service{
		Fetch: func(string) ([]byte, error) { return []byte("unexpected key fixture"), nil },
		Run: func(command Command) (Result, error) {
			switch command.Name {
			case "apt-get":
				return Result{ExitCode: 100, Output: "Err: " + jenkinsRepositoryURL + "\nNO_PUBKEY 7198F4B714ABFC68\n"}, nil
			case "test":
				return Result{ExitCode: 1}, nil
			case "gpg":
				return Result{Output: "fpr:::::::::UNEXPECTED:\n"}, nil
			default:
				t.Fatalf("repository files changed before fingerprint verification: %s", command.Name)
				return Result{}, nil
			}
		},
	}
	changed, err := service.Ensure(nil)
	if changed || err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("unexpected signing key was accepted: changed=%t err=%v", changed, err)
	}
}

func TestCheckRecognizesJenkins2026MissingKey(t *testing.T) {
	service := Service{Run: func(command Command) (Result, error) {
		if command.Name != "apt-get" || !command.Privileged {
			t.Fatalf("command = %#v", command)
		}
		return Result{ExitCode: 100, Output: "Err: https://pkg.jenkins.io/debian-stable binary/ Release\nThe following signatures couldn't be verified because the public key is not available: NO_PUBKEY 7198F4B714ABFC68\n"}, nil
	}}
	diagnosis, err := service.Check()
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if diagnosis.Healthy || diagnosis.Problem == nil || diagnosis.Problem.KeyID != "7198F4B714ABFC68" {
		t.Fatalf("diagnosis = %#v", diagnosis)
	}
	report := diagnosis.KnownProblemReport()
	for _, text := range []string{jenkinsRepositoryURL, jenkinsKeyPath, jenkinsSourcePath, "Missing signing key 7198F4B714ABFC68"} {
		if !strings.Contains(report, text) {
			t.Fatalf("report = %q, missing %q", report, text)
		}
	}
}

func TestCheckRecognizesJenkins2026MissingKeyBeforeExitCode(t *testing.T) {
	service := Service{Run: func(Command) (Result, error) {
		return Result{
			ExitCode: 0,
			Output:   "Err: https://pkg.jenkins.io/debian-stable binary/ Release\nNO_PUBKEY 7198F4B714ABFC68\n",
		}, nil
	}}

	diagnosis, err := service.Check()
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if diagnosis.Healthy || diagnosis.Problem == nil || diagnosis.Problem.KeyID != "7198F4B714ABFC68" {
		t.Fatalf("diagnosis = %#v", diagnosis)
	}
}

func TestRemediateKnownJenkinsInstallsVerifiedKeyAndCanonicalSource(t *testing.T) {
	var commands []Command
	service := Service{
		Fetch: func(url string) ([]byte, error) {
			if url != jenkinsKeyURL {
				t.Fatalf("fetch URL = %q", url)
			}
			return []byte("official key"), nil
		},
		Run: func(command Command) (Result, error) {
			commands = append(commands, command)
			switch command.Name {
			case "test":
				return Result{ExitCode: 1}, nil
			case "gpg":
				return Result{Output: "fpr:::::::::" + jenkinsFingerprint + ":\n"}, nil
			case "apt-get":
				return Result{}, nil
			default:
				return Result{}, nil
			}
		},
	}
	if err := service.RemediateKnownJenkins(); err != nil {
		t.Fatalf("RemediateKnownJenkins() error = %v", err)
	}
	if !containsCommand(commands, "install", jenkinsKeyPath) || !containsCommand(commands, "install", jenkinsSourcePath) {
		t.Fatalf("commands = %#v", commands)
	}
	if !containsCommand(commands, "apt-get", "update") {
		t.Fatalf("commands = %#v", commands)
	}
	var gpg Command
	for _, command := range commands {
		if command.Name == "gpg" {
			gpg = command
			break
		}
	}
	if gpg.Name == "" {
		t.Fatalf("commands do not run gpg: %#v", commands)
	}
	if !containsArgsInOrder(gpg.Args, "--homedir", "gnupg") ||
		!containsArgsInOrder(gpg.Args, "--no-default-keyring", "--keyring", "/dev/null") {
		t.Fatalf("gpg args do not isolate the keyring: %#v", gpg.Args)
	}
}

func TestRemediateKnownJenkinsRestoresBackupAfterFailedValidation(t *testing.T) {
	var commands []Command
	aptCalls := 0
	service := Service{
		Fetch: func(string) ([]byte, error) { return []byte("official key"), nil },
		Run: func(command Command) (Result, error) {
			commands = append(commands, command)
			switch command.Name {
			case "test":
				return Result{}, nil
			case "gpg":
				return Result{Output: "fpr:::::::::" + jenkinsFingerprint + ":\n"}, nil
			case "apt-get":
				aptCalls++
				return Result{ExitCode: 100, Output: "APT still fails"}, nil
			default:
				return Result{}, nil
			}
		},
	}
	err := service.RemediateKnownJenkins()
	if err == nil || !strings.Contains(err.Error(), "were restored") || aptCalls != 1 {
		t.Fatalf("RemediateKnownJenkins() error = %v, apt calls = %d", err, aptCalls)
	}
	if countCommand(commands, "install", jenkinsSourcePath) < 2 || countCommand(commands, "install", jenkinsKeyPath) < 2 {
		t.Fatalf("commands do not restore both files: %#v", commands)
	}
}

func TestRemediateKnownJenkinsRestoresBackupWhenKeyInstallFails(t *testing.T) {
	var commands []Command
	keyInstallAttempts := 0
	service := Service{
		Fetch: func(string) ([]byte, error) { return []byte("official key"), nil },
		Run: func(command Command) (Result, error) {
			commands = append(commands, command)
			switch command.Name {
			case "test":
				return Result{ExitCode: 1}, nil
			case "gpg":
				return Result{Output: "fpr:::::::::" + jenkinsFingerprint + ":\n"}, nil
			case "install":
				if strings.Contains(strings.Join(command.Args, " "), jenkinsKeyPath) {
					keyInstallAttempts++
					if keyInstallAttempts == 1 {
						return Result{ExitCode: 1, Output: "disk full"}, nil
					}
				}
				return Result{}, nil
			default:
				return Result{}, nil
			}
		},
	}
	err := service.RemediateKnownJenkins()
	if err == nil || !strings.Contains(err.Error(), "were restored") {
		t.Fatalf("RemediateKnownJenkins() error = %v", err)
	}
	if !containsCommand(commands, "rm", jenkinsSourcePath) || !containsCommand(commands, "rm", jenkinsKeyPath) {
		t.Fatalf("commands do not remove incomplete files: %#v", commands)
	}
}

func containsCommand(commands []Command, name, argument string) bool {
	return countCommand(commands, name, argument) > 0
}

func countCommand(commands []Command, name, argument string) int {
	count := 0
	for _, command := range commands {
		if command.Name == name && strings.Contains(strings.Join(command.Args, " "), argument) {
			count++
		}
	}
	return count
}

func containsArgsInOrder(args []string, values ...string) bool {
	position := 0
	for _, argument := range args {
		if strings.Contains(argument, values[position]) {
			position++
			if position == len(values) {
				return true
			}
		}
	}
	return false
}

func ExampleDiagnosis_KnownProblemReport() {
	diagnosis := Diagnosis{Problem: &Problem{Repository: jenkinsRepositoryURL, KeyID: "7198F4B714ABFC68"}}
	fmt.Print(diagnosis.KnownProblemReport())
	// Output:
	// APT repository validation failed.
	//
	// Repository:
	// https://pkg.jenkins.io/debian-stable
	//
	// Problem:
	// Missing signing key 7198F4B714ABFC68
	//
	// Suggested action:
	// Install the official Jenkins 2026 repository key and update
	// the Jenkins LTS repository configuration.
	//
	// Files to modify:
	// /etc/apt/keyrings/jenkins-keyring.asc
	// /etc/apt/sources.list.d/jenkins.list
	//
	// Manual remediation equivalent:
	// sudo install -d -m 0755 /etc/apt/keyrings
	// curl -fsSL https://pkg.jenkins.io/debian-stable/jenkins.io-2026.key | sudo tee /etc/apt/keyrings/jenkins-keyring.asc >/dev/null
	// echo "deb [signed-by=/etc/apt/keyrings/jenkins-keyring.asc] https://pkg.jenkins.io/debian-stable binary/" | sudo tee /etc/apt/sources.list.d/jenkins.list >/dev/null
	// sudo chmod 0644 /etc/apt/keyrings/jenkins-keyring.asc
	// sudo chmod 0644 /etc/apt/sources.list.d/jenkins.list
	// sudo apt-get update
}
