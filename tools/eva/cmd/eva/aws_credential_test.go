package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"eva-deployer/tools/eva/internal/airgap"
	"eva-deployer/tools/eva/internal/remote"
)

func TestPreparationAWSCredentialPrompt(t *testing.T) {
	for _, region := range []string{"", "us-east-1"} {
		t.Run("region="+region, func(t *testing.T) {
			var output bytes.Buffer
			calls := 0
			credential, err := promptPreparationAWSCredential(strings.NewReader(" fixture-access \n"+region+"\n"), &output, func() (string, error) {
				calls++
				return " fixture-private-secret ", nil
			})
			wantRegion := region
			if wantRegion == "" {
				wantRegion = remote.DefaultAWSRegion
			}
			if err != nil || calls != 1 || credential.AccessKeyID != "fixture-access" || credential.SecretAccessKey != "fixture-private-secret" || credential.Region != wantRegion {
				t.Fatalf("credential input failed: err=%v secret reads=%d", err, calls)
			}
			for _, label := range []string{"AWS Access Key ID:", "AWS Secret Access Key:", "AWS Region [ap-northeast-2]:"} {
				if !strings.Contains(output.String(), label) {
					t.Fatalf("missing prompt %q", label)
				}
			}
			if strings.Contains(output.String(), credential.SecretAccessKey) {
				t.Fatal("secret was echoed")
			}
		})
	}
}

func TestPreparationAWSCredentialPromptRejectsIncompleteInput(t *testing.T) {
	for _, tc := range []struct {
		name, input, secret string
		secretErr           error
		wantSecretReads     int
	}{
		{"missing access", "\n", "private-fixture", nil, 0},
		{"access EOF", "", "private-fixture", nil, 0},
		{"empty secret", "access\n\n", "  ", nil, 1},
		{"secret read failure", "access\n\n", "private-fixture", errors.New("private-fixture"), 1},
		{"region EOF", "access\n", "private-fixture", nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			calls := 0
			_, err := promptPreparationAWSCredential(strings.NewReader(tc.input), &output, func() (string, error) {
				calls++
				return tc.secret, tc.secretErr
			})
			if err == nil || calls != tc.wantSecretReads {
				t.Fatalf("incomplete credential accepted: err=%v reads=%d", err, calls)
			}
			if strings.Contains(output.String(), "private-fixture") || strings.Contains(err.Error(), "private-fixture") {
				t.Fatal("private input or secret-reader error was exposed")
			}
		})
	}
}

func TestRunAirgapRequestsAndValidatesAWSCredentialBeforePreparation(t *testing.T) {
	for _, state := range []string{"missing", "malformed", "expired", "usable", "invalid replacement"} {
		t.Run(state, func(t *testing.T) {
			previousCredential, previousStat := defaultRemoteAWSCredentialPath, stdinStat
			previousPrompt, previousValidate, previousPrepare := requestPreparationAWSCredential, validateRemoteAWSCredential, prepareAirgapBundle
			t.Cleanup(func() {
				defaultRemoteAWSCredentialPath, stdinStat = previousCredential, previousStat
				requestPreparationAWSCredential, validateRemoteAWSCredential, prepareAirgapBundle = previousPrompt, previousValidate, previousPrepare
			})
			root := writeRemotePublishRelease(t)
			defaultRemoteAWSCredentialPath = filepath.Join(t.TempDir(), "credentials/aws_key.ini")
			stdinStat = func() (os.FileInfo, error) { return fakeCharDeviceInfo{}, nil }
			old := remote.AWSCredential{AccessKeyID: "old-access", SecretAccessKey: "old-private", Region: remote.DefaultAWSRegion}
			fresh := remote.AWSCredential{AccessKeyID: "new-access", SecretAccessKey: "new-private", Region: remote.DefaultAWSRegion}
			if state != "missing" {
				if err := remote.WriteAWSCredential(defaultRemoteAWSCredentialPath, old); err != nil {
					t.Fatal(err)
				}
				if state == "malformed" {
					if err := os.WriteFile(defaultRemoteAWSCredentialPath, []byte("aws_secret_access_key = old-private\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			prompted, prepared, validations := false, false, 0
			requestPreparationAWSCredential = func() (remote.AWSCredential, error) { prompted = true; return fresh, nil }
			validateRemoteAWSCredential = func(_ context.Context, credential remote.AWSCredential) error {
				validations++
				if credential == old && state != "usable" || credential == fresh && state == "invalid replacement" {
					return errors.New("private validation diagnostic")
				}
				return nil
			}
			want := fresh
			if state == "usable" {
				want = old
			}
			prepareAirgapBundle = func(_ context.Context, options airgap.Options) (string, error) {
				prepared = true
				stored, err := remote.LoadAWSCredential(defaultRemoteAWSCredentialPath)
				if err != nil || stored != want || options.AWSCredential != want || validations == 0 {
					t.Fatal("preparation started before the credential was validated and saved")
				}
				info, err := os.Stat(defaultRemoteAWSCredentialPath)
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatal("credential was not saved privately")
				}
				return options.Output, nil
			}
			log, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			previousStderr := os.Stderr
			os.Stderr = log
			t.Cleanup(func() { os.Stderr = previousStderr; log.Close() })
			var runErr error
			stdout := captureRemoteTargetStdout(t, func() error {
				runErr = run([]string{"airgap", "prepare", "--release", root, "--output", filepath.Join(t.TempDir(), "bundle.tar.gz"), "--vllm-profile", "L40sx1", "--yes"})
				return nil
			})
			os.Stderr = previousStderr
			stderr, err := os.ReadFile(log.Name())
			if err != nil {
				t.Fatal(err)
			}
			if prompted != (state != "usable") {
				t.Fatalf("credential state %s prompted=%t", state, prompted)
			}
			if state == "invalid replacement" {
				stored, err := remote.LoadAWSCredential(defaultRemoteAWSCredentialPath)
				if runErr == nil || prepared || err != nil || stored != old {
					t.Fatal("invalid replacement started preparation or overwrote the stored credential")
				}
			} else if runErr != nil || !prepared {
				t.Fatalf("credential setup did not start preparation: %v", runErr)
			}
			for _, private := range []string{"old-private", "new-private", "private validation diagnostic"} {
				if strings.Contains(stdout+string(stderr), private) || runErr != nil && strings.Contains(runErr.Error(), private) {
					t.Fatal("AWS secret or validation diagnostic was exposed")
				}
			}
		})
	}
}
