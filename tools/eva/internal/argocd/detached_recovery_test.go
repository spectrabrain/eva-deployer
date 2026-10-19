package argocd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"eva-deployer/tools/eva/internal/plan"
)

type detachedPrompt struct {
	fakePrompt
	receiptRoot  string
	receipts     int
	gitApprovals int
	gitError     error
	receiptError error
}

func (prompt *detachedPrompt) ConfirmGitRemoval(GitRemovalPlan) (bool, error) {
	prompt.gitApprovals++
	return false, errors.New("already detached recovery must not request a Git mutation")
}

func (prompt *detachedPrompt) GitCredentials(plan GitRemovalPlan) (GitCredentials, error) {
	if prompt.gitError != nil {
		return GitCredentials{}, prompt.gitError
	}
	return prompt.fakePrompt.GitCredentials(plan)
}

func (prompt *detachedPrompt) RecordReceipt(receipt Receipt) error {
	prompt.receipts++
	if prompt.receiptError != nil {
		return prompt.receiptError
	}
	return WriteReceipt(prompt.receiptRoot, receipt)
}

func detachedTestPlan() GitRemovalPlan {
	return GitRemovalPlan{
		RegistrationApplication: registrationApplicationName,
		Repository:              "http://mod.lge.com/hub/prism/eva-argo-shee.git", Branch: "main",
		Manifest: "registration/clusters/legacy-a.yaml", ClusterName: testLegacyCluster,
		ExpectedHead: testGitCommit,
	}
}

func detachedTestSession() *fakeSession {
	gitPlan := detachedTestPlan()
	return &fakeSession{
		outputs: map[string]string{
			applicationListCommand():         applicationsJSON(),
			clusterSecretListCommand():       clusterSecretsJSON(),
			registrationApplicationCommand(): registrationApplicationJSON(testClusterSecret, testGitCommit, false),
		},
		outputSequences: map[string][]string{
			gitAbsenceCommand(gitPlan): {"Cloning into 'work'...\nEVA_GIT_ABSENT_COMMIT=" + testGitCommit + "\n"},
		},
		failures: map[string]error{},
	}
}

func assertDetachedReadOnly(t *testing.T, session *fakeSession) {
	t.Helper()
	assertNoMutation(t, session.commands)
	for _, command := range session.commands {
		for _, mutation := range []string{"git rm ", "git commit ", "git push ", "kubectl patch ", "kubectl delete "} {
			if strings.Contains(command, mutation) {
				t.Fatalf("already detached recovery attempted %q", mutation)
			}
		}
	}
}

func TestRunPreflightRecoversAlreadyDetachedClusterAndUnblocksInstall(t *testing.T) {
	releaseRoot := t.TempDir()
	detected := []string{"legacy-a-eva-agent", "legacy-a-eva-agent-init", "legacy-a-eva-app"}
	writeReport(t, releaseRoot, testWorkspaceSite, "host-a", "ok", detected)
	session := detachedTestSession()
	// Other clusters must not be removed or prevent the exact-name check.
	session.outputs[applicationListCommand()] = applicationsJSON(applicationFixture{Name: "legacy-b-eva-app", Server: "https://10.0.0.20:6443"})
	session.outputs[clusterSecretListCommand()] = clusterSecretsJSON(clusterSecretFixture{SecretName: "legacy-b", Name: "legacy-b", Server: "https://10.0.0.20:6443"})
	prompt := &detachedPrompt{fakePrompt: approvedPrompt(), receiptRoot: t.TempDir()}
	err := RunPreflight(testWorkspaceSite, releaseRoot, prompt, func(Credentials) (Session, error) { return session, nil })
	if err != nil {
		t.Fatal(err)
	}
	if prompt.receipts != 1 || prompt.gitApprovals != 0 || !session.closed {
		t.Fatalf("receipts=%d Git approvals=%d closed=%v", prompt.receipts, prompt.gitApprovals, session.closed)
	}
	receipt, err := LoadReceipt(prompt.receiptRoot, testWorkspaceSite)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ClusterName != testLegacyCluster || receipt.ClusterServer != "" || receipt.RegistrationCommit != testGitCommit || !hasCompleteGitReceiptMetadata(receipt) {
		t.Fatalf("receipt = %#v", receipt)
	}
	if err := ReceiptCovers(receipt, Detection{SiteID: testWorkspaceSite, Applications: detected}); err != nil {
		t.Fatal(err)
	}
	if err := RequireNoTrackingWithReceiptRoot(plan.Document{SiteID: testWorkspaceSite, Steps: []plan.Step{{Component: "app"}}}, releaseRoot, prompt.receiptRoot); err != nil {
		t.Fatalf("verified disconnection did not unblock install: %v", err)
	}
	sleeps := 0
	for _, command := range session.commands {
		if command == "sleep 5" {
			sleeps++
		}
	}
	if sleeps != 12 || session.outputIndexes[gitAbsenceCommand(detachedTestPlan())] != 1 {
		t.Fatalf("absence window sleeps=%d, Git reads=%d", sleeps, session.outputIndexes[gitAbsenceCommand(detachedTestPlan())])
	}
	assertDetachedReadOnly(t, session)
}

func TestAlreadyDetachedRecoveryFailsClosedWithoutReceipt(t *testing.T) {
	const later = "cccccccccccccccccccccccccccccccccccccccc"
	appJSON := applicationsJSON(applicationFixture{Name: "legacy-a-eva-app", Server: testClusterServer})
	secretJSON := clusterSecretsJSON(clusterSecretFixture{SecretName: testClusterSecret, Name: testLegacyCluster, Server: testClusterServer})
	regJSON := registrationApplicationJSON(testClusterSecret, testGitCommit, false)
	tests := []struct {
		name  string
		setup func(*fakeSession, *detachedPrompt)
		want  string
	}{
		{"live_registration", func(s *fakeSession, _ *detachedPrompt) { s.outputs[clusterSecretListCommand()] = secretJSON }, "registration still exists"},
		{"live_registration_alias", func(s *fakeSession, _ *detachedPrompt) {
			s.outputs[clusterSecretListCommand()] = clusterSecretsJSON(clusterSecretFixture{SecretName: "another-secret", Name: testLegacyCluster, Server: testClusterServer})
		}, "registration still exists"},
		{"desired_registration", func(s *fakeSession, _ *detachedPrompt) {
			s.outputs[registrationApplicationCommand()] = registrationApplicationJSON(testClusterSecret, testGitCommit, true)
		}, "still desires"},
		{"unsynced_revision", func(s *fakeSession, _ *detachedPrompt) {
			s.outputs[registrationApplicationCommand()] = registrationApplicationJSON(testClusterSecret, later, false)
		}, "current Git HEAD"},
		{"invalid_registration", func(s *fakeSession, _ *detachedPrompt) { s.outputs[registrationApplicationCommand()] = `{}` }, "exactly one spec.source"},
		{"registration_query_failure", func(s *fakeSession, _ *detachedPrompt) {
			s.failures[registrationApplicationCommand()] = errors.New("denied")
		}, "read registration Application"},
		{"git_credentials_failure", func(_ *fakeSession, p *detachedPrompt) { p.gitError = errors.New("cancelled") }, "read Git credentials"},
		{"invalid_git_credentials", func(_ *fakeSession, p *detachedPrompt) { p.fakePrompt.gitCredential = GitCredentials{Username: "user"} }, "access token is required"},
		{"manifest_present_or_clone_failure", func(s *fakeSession, _ *detachedPrompt) {
			s.failures[gitAbsenceCommand(detachedTestPlan())] = errors.New("exit status 1")
		}, "manifest is absent from Git"},
		{"missing_git_marker", func(s *fakeSession, _ *detachedPrompt) {
			s.outputSequences[gitAbsenceCommand(detachedTestPlan())] = []string{""}
		}, "exactly one commit marker"},
		{"duplicate_git_marker", func(s *fakeSession, _ *detachedPrompt) {
			s.outputSequences[gitAbsenceCommand(detachedTestPlan())] = []string{strings.Repeat("EVA_GIT_ABSENT_COMMIT="+testGitCommit+"\n", 2)}
		}, "exactly one commit marker"},
		{"wrong_git_marker", func(s *fakeSession, _ *detachedPrompt) {
			s.outputSequences[gitAbsenceCommand(detachedTestPlan())] = []string{"EVA_GIT_ABSENT_COMMIT=" + later}
		}, "different commit"},
		{"git_head_changes", func(s *fakeSession, _ *detachedPrompt) {
			s.outputSequences[defaultBranchCommand(detachedTestPlan().Repository)] = []string{"ref: refs/heads/main\tHEAD\n" + testGitCommit + "\tHEAD\n", "ref: refs/heads/main\tHEAD\n" + later + "\tHEAD\n"}
		}, "HEAD changed"},
		{"git_head_query_fails", func(s *fakeSession, _ *detachedPrompt) {
			s.failures[defaultBranchCommand(detachedTestPlan().Repository)] = errors.New("denied")
		}, "resolve registration repository"},
		{"git_head_malformed", func(s *fakeSession, _ *detachedPrompt) {
			s.outputSequences[defaultBranchCommand(detachedTestPlan().Repository)] = []string{"invalid"}
		}, "default branch and HEAD"},
		{"related_live_application", func(s *fakeSession, _ *detachedPrompt) {
			s.outputs[applicationListCommand()] = applicationsJSON(applicationFixture{Name: "legacy-a-eva-vision", Server: testClusterServer})
		}, "still exists"},
		{"partially_removed_applications", func(s *fakeSession, _ *detachedPrompt) { s.outputs[applicationListCommand()] = appJSON }, "absent from the management server"},
		{"application_reappears", func(s *fakeSession, _ *detachedPrompt) {
			s.outputSequences[applicationListCommand()] = []string{applicationsJSON(), appJSON}
		}, "recreated or still exists"},
		{"registration_reappears", func(s *fakeSession, _ *detachedPrompt) {
			s.outputSequences[clusterSecretListCommand()] = []string{clusterSecretsJSON(), secretJSON}
		}, "Secret was recreated"},
		{"desired_registration_reappears", func(s *fakeSession, _ *detachedPrompt) {
			s.outputSequences[registrationApplicationCommand()] = []string{regJSON, registrationApplicationJSON(testClusterSecret, testGitCommit, true)}
		}, "again desires"},
		{"revision_changes", func(s *fakeSession, _ *detachedPrompt) {
			s.outputSequences[registrationApplicationCommand()] = []string{regJSON, registrationApplicationJSON(testClusterSecret, later, false)}
		}, "not the handoff commit"},
		{"source_changes", func(s *fakeSession, _ *detachedPrompt) {
			s.outputSequences[registrationApplicationCommand()] = []string{regJSON, strings.Replace(regJSON, `"path":"registration"`, `"path":"other"`, 1)}
		}, "source changed"},
		{"late_application_reappears", func(s *fakeSession, _ *detachedPrompt) {
			seq := make([]string, 14)
			for i := range seq {
				seq[i] = applicationsJSON()
			}
			seq[13] = appJSON
			s.outputSequences[applicationListCommand()] = seq
		}, "recreated or still exists"},
		{"observation_query_failure", func(s *fakeSession, _ *detachedPrompt) { s.failures["sleep 5"] = errors.New("disconnected") }, "disconnected"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			releaseRoot := t.TempDir()
			writeReport(t, releaseRoot, testWorkspaceSite, "host-a", "ok", []string{"legacy-a-eva-agent", "legacy-a-eva-app"})
			session := detachedTestSession()
			prompt := &detachedPrompt{fakePrompt: approvedPrompt(), receiptRoot: t.TempDir()}
			test.setup(session, prompt)
			err := RunPreflight(testWorkspaceSite, releaseRoot, prompt, func(Credentials) (Session, error) { return session, nil })
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
			if prompt.receipts != 0 || prompt.gitApprovals != 0 {
				t.Fatalf("receipts=%d Git mutation approvals=%d", prompt.receipts, prompt.gitApprovals)
			}
			assertDetachedReadOnly(t, session)
		})
	}
}

func TestAlreadyDetachedRecoveryReceiptFailureIsNotSuccess(t *testing.T) {
	root := t.TempDir()
	writeReport(t, root, testWorkspaceSite, "host-a", "ok", []string{"legacy-a-eva-app"})
	prompt := &detachedPrompt{fakePrompt: approvedPrompt(), receiptError: errors.New("disk full")}
	session := detachedTestSession()
	err := RunPreflight(testWorkspaceSite, root, prompt, func(Credentials) (Session, error) { return session, nil })
	if err == nil || !strings.Contains(err.Error(), "disk full") || prompt.receipts != 1 {
		t.Fatalf("error=%v receipts=%d", err, prompt.receipts)
	}
	assertDetachedReadOnly(t, session)
}

func TestDetachedClusterNameRequiresOneExactCluster(t *testing.T) {
	name, err := detachedClusterName([]string{"lge-shee-magok-d-eva-agent", "lge-shee-magok-d-eva-agent-init", "lge-shee-magok-d-eva-app"})
	if err != nil || name != "lge-shee-magok-d" {
		t.Fatalf("name=%q error=%v", name, err)
	}
	for _, detected := range [][]string{nil, {"legacy-a-eva-app", "legacy-b-eva-agent"}, {"unsafe/cluster-eva-app"}, {"legacy-a-eva-app;true"}} {
		if _, err := detachedClusterName(detected); err == nil {
			t.Fatalf("accepted ambiguous/unsafe names %v", detected)
		}
	}
}

func TestGitAbsenceCommandChecksRealGitTreeReadOnly(t *testing.T) {
	for _, scenario := range []string{"absent", "present", "symlink_parent", "absent_parents"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			repository := filepath.Join(root, "origin")
			if err := os.Mkdir(repository, 0o700); err != nil {
				t.Fatal(err)
			}
			git := func(args ...string) string {
				t.Helper()
				command := exec.Command("git", args...)
				command.Dir = repository
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, output)
				}
				return strings.TrimSpace(string(output))
			}
			git("init", "--initial-branch=main")
			if scenario != "absent_parents" {
				if err := os.MkdirAll(filepath.Join(repository, "registration", "clusters"), 0o700); err != nil {
					t.Fatal(err)
				}
				name := "other.yaml"
				if scenario == "present" {
					name = "legacy-a.yaml"
				}
				if err := os.WriteFile(filepath.Join(repository, "registration", "clusters", name), []byte("private Secret content must not be printed"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "symlink_parent" {
				if err := os.Rename(filepath.Join(repository, "registration"), filepath.Join(repository, "other-registration")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("other-registration", filepath.Join(repository, "registration")); err != nil {
					t.Fatal(err)
				}
			}
			git("add", ".")
			git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "fixture")
			head := git("rev-parse", "HEAD")
			gitPlan := GitRemovalPlan{Repository: repository, Branch: "main", Manifest: "registration/clusters/legacy-a.yaml", ExpectedHead: head}
			command := exec.Command("bash", "-c", gitAbsenceCommand(gitPlan))
			command.Env = append(os.Environ(), "HOME="+root)
			command.Stdin = strings.NewReader("test-user\ntest-secret\n")
			output, err := command.CombinedOutput()
			wantSuccess := scenario == "absent" || scenario == "absent_parents"
			if (err == nil) != wantSuccess {
				t.Fatalf("scenario %s: error=%v output=%s", scenario, err, output)
			}
			if wantSuccess {
				if err := requireGitAbsenceMarker(string(output), head); err != nil {
					t.Fatal(err)
				}
			}
			if strings.Contains(string(output), "private Secret") || strings.Contains(string(output), "test-secret") {
				t.Fatal("Git verification printed credentials or manifest content")
			}
			if git("rev-parse", "HEAD") != head || git("status", "--porcelain") != "" {
				t.Fatal("Git verification changed the source repository")
			}
			entries, err := os.ReadDir(filepath.Join(root, "tmp"))
			if err != nil || len(entries) != 0 {
				t.Fatal(fmt.Sprintf("temporary credentials/checkout were not cleaned: entries=%v err=%v", entries, err))
			}
		})
	}
}
