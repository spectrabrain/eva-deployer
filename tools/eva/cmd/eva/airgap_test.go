package main

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"eva-deployer/tools/eva/internal/airgap"
	"eva-deployer/tools/eva/internal/release"
	remotecommand "eva-deployer/tools/eva/internal/remote"
)

func TestRunAirgapDefaultOutputFromCurrentRelease(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%t", existing), func(t *testing.T) {
			root := writeRemotePublishRelease(t)
			resolved, err := release.Resolve(root)
			if err != nil {
				t.Fatal(err)
			}
			previousReceipt, previousCredential := defaultCurrentReleaseReceiptPath, defaultRemoteAWSCredentialPath
			previousPrepare, previousValidate := prepareAirgapBundle, validateRemoteAWSCredential
			t.Cleanup(func() {
				defaultCurrentReleaseReceiptPath, defaultRemoteAWSCredentialPath = previousReceipt, previousCredential
				prepareAirgapBundle, validateRemoteAWSCredential = previousPrepare, previousValidate
			})
			defaultCurrentReleaseReceiptPath = filepath.Join(t.TempDir(), "current.yaml")
			if err := release.WriteCurrentReceipt(defaultCurrentReleaseReceiptPath, resolved, "eva-tool-installer", time.Now()); err != nil {
				t.Fatal(err)
			}
			defaultRemoteAWSCredentialPath = filepath.Join(t.TempDir(), "aws_key.ini")
			credential := remotecommand.AWSCredential{AccessKeyID: "fixture-access", SecretAccessKey: "fixture-private", Region: "ap-northeast-2"}
			if err := remotecommand.WriteAWSCredential(defaultRemoteAWSCredentialPath, credential); err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SUDO_USER", "")
			filename := fmt.Sprintf("eva-airgap-bundle_3.2.0_%s_%s.tar.gz", resolved.Metadata.Platform.OS, resolved.Metadata.Platform.Arch)
			output := filepath.Join(home, filename)
			if existing {
				if err := os.WriteFile(output, []byte("keep-existing-bundle"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			validated, prepared := false, false
			validateRemoteAWSCredential = func(context.Context, remotecommand.AWSCredential) error {
				validated = true
				return nil
			}
			prepareAirgapBundle = func(_ context.Context, options airgap.Options) (string, error) {
				prepared = true
				if options.Release.Root != root || options.Output != output || options.AWSCredential != credential || options.VLLMProfile != "L40sx1" || options.DriverPackage != "nvidia-driver-580" {
					t.Fatal("preparation did not receive Current Release, default output, credential, profile and default driver")
				}
				return options.Output, os.WriteFile(options.Output, []byte("prepared-bundle"), 0o600)
			}
			var runErr error
			stdout := captureRemoteTargetStdout(t, func() error {
				runErr = run([]string{"airgap", "prepare", "--vllm-profile", "L40sx1", "--yes"})
				return nil
			})
			contents, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			if existing {
				if runErr == nil || !strings.Contains(runErr.Error(), output) || !strings.Contains(runErr.Error(), "--output") || validated || prepared {
					t.Fatalf("existing default output: error=%v validated=%t prepared=%t", runErr, validated, prepared)
				}
				if string(contents) != "keep-existing-bundle" || strings.Contains(stdout, "[OK]") {
					t.Fatal("existing output changed or completion was reported")
				}
				return
			}
			if runErr != nil || !validated || !prepared || string(contents) != "prepared-bundle" {
				t.Fatalf("default preparation: error=%v validated=%t prepared=%t", runErr, validated, prepared)
			}
			for _, want := range []string{"[INFO] vllm_profile=L40sx1 driver_package=nvidia-driver-580", "[INFO] bundle_output=" + output, "[OK] Bundle directory: " + home, "[OK] Bundle filename: " + filename} {
				if !strings.Contains(stdout, want) {
					t.Fatalf("missing output %q in %q", want, stdout)
				}
			}
			info, err := os.Stat(output)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("Bundle private permissions changed: info=%v error=%v", info, err)
			}
		})
	}
}

func TestAirgapInvokingUserHome(t *testing.T) {
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	owner, err := airgapInvokingUser(0, account.Username)
	if err != nil {
		t.Fatal(err)
	}
	if owner.home != account.HomeDir || strconv.Itoa(owner.uid) != account.Uid || strconv.Itoa(owner.gid) != account.Gid {
		t.Fatalf("sudo must use account home and ownership, got %+v", owner)
	}
	owner, err = airgapInvokingUser(1000, "unknown-sudo-user")
	if err != nil || owner.home != home || owner.uid != os.Getuid() || owner.gid != os.Getgid() {
		t.Fatalf("non-root home and ownership: owner=%+v error=%v", owner, err)
	}
	if _, err := airgapInvokingUser(0, "eva-airgap-nonexistent-"+filepath.Base(home)); err == nil {
		t.Fatal("unknown sudo account must not silently fall back to root's home")
	}
	t.Setenv("HOME", "relative-home")
	if _, err := airgapInvokingUser(1000, ""); err == nil {
		t.Fatal("relative home accepted")
	}
}

func TestSetAirgapOutputOwnerRequiresRegularFile(t *testing.T) {
	directory := t.TempDir()
	output := filepath.Join(directory, "bundle.tar.gz")
	if err := os.WriteFile(output, []byte("bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner := airgapUser{home: directory, uid: os.Getuid(), gid: os.Getgid()}
	if err := setAirgapOutputOwner(output, owner); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("ownership update changed private permissions: info=%v error=%v", info, err)
	}
	link := filepath.Join(directory, "link.tar.gz")
	if err := os.Symlink(output, link); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{directory, link, filepath.Join(directory, "missing.tar.gz")} {
		if err := setAirgapOutputOwner(invalid, owner); err == nil {
			t.Fatalf("accepted non-regular output %s", invalid)
		}
	}
}
