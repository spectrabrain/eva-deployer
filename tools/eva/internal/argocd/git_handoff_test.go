package argocd

import (
	"strings"
	"testing"
)

func TestParseTrackingIdentityRequiresExactRegistrationSecret(t *testing.T) {
	identity, err := parseTrackingIdentity("registration:/Secret:argocd/lge-shee-magok-d")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Application != "registration" || identity.Kind != "Secret" || identity.Namespace != "argocd" || identity.Name != "lge-shee-magok-d" {
		t.Fatalf("tracking identity = %#v", identity)
	}
	for _, value := range []string{"registration:/ConfigMap:argocd/name", "registration:/Secret:argocd", "registration:/Secret:argocd/name/other", "registration:/Secret:argocd:name"} {
		if _, err := parseTrackingIdentity(value); err == nil {
			t.Fatalf("parseTrackingIdentity(%q) unexpectedly succeeded", value)
		}
	}
}

func TestRegistrationSourceAndManifestAreStrict(t *testing.T) {
	application, err := parseRegistrationApplication(`{"spec":{"source":{"repoURL":"http://mod.lge.com/hub/prism/eva-argo-shee.git","targetRevision":"HEAD","path":"registration"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	source, err := validatedRegistrationSource(application)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := targetRegistrationManifest(source.Path, "lge-shee-magok-d")
	if err != nil || manifest != "registration/clusters/lge-shee-magok-d.yaml" {
		t.Fatalf("manifest=%q err=%v", manifest, err)
	}
	if _, err := targetRegistrationManifest("../registration", "lge-shee-magok-d"); err == nil {
		t.Fatal("unsafe source path succeeded")
	}
	if _, err := parseRegistrationApplication(`{"spec":{"sources":[{}]}}`); err == nil {
		t.Fatal("multi-source registration Application succeeded")
	}
}

func TestParseDefaultBranchRequiresSymrefAndHead(t *testing.T) {
	branch, head, err := parseDefaultBranch("ref: refs/heads/main\tHEAD\n0123456789012345678901234567890123456789\tHEAD\n")
	if err != nil || branch != "main" || head != "0123456789012345678901234567890123456789" {
		t.Fatalf("branch=%q head=%q err=%v", branch, head, err)
	}
	if _, _, err := parseDefaultBranch("0123\tHEAD\n"); err == nil {
		t.Fatal("invalid default branch output succeeded")
	}
}

func TestGitRemovalCommandIsNarrowAndDoesNotPrintSecret(t *testing.T) {
	command := gitRemovalCommand(GitRemovalPlan{
		Repository: "http://mod.lge.com/hub/prism/eva-argo-shee.git", Branch: "main",
		Manifest: "registration/clusters/lge-shee-magok-d.yaml", ClusterName: "lge-shee-magok-d",
		ClusterServer: "https://10.0.0.10:6443", ExpectedHead: testGitCommit,
	}, "https://10.0.0.10:6443")
	for _, required := range []string{
		"git clone --depth 1 --branch",
		"git rm -- 'registration/clusters/lge-shee-magok-d.yaml'",
		"git diff --cached --name-status",
		"git push origin HEAD:'refs/heads/main'",
		"printf 'EVA_GIT_COMMIT=%s\\n' \"$commit\"",
		"trap 'rm -rf",
		"{.stringData.name}",
		"{.stringData.server}",
		"{.data.name}",
		"{.data.server}",
		"expected_string_data=",
		"expected_data=",
		"EVA_GIT_STAGE_FAILED=validate-manifest-identity",
		"umask 077",
		"git-username",
		"git-secret",
		"git-askpass",
		"GIT_ASKPASS=",
		"GIT_TERMINAL_PROMPT=0",
		"EVA_GIT_USERNAME_FILE=",
		"EVA_GIT_SECRET_FILE=",
		"IFS= read -r git_username",
		"IFS= read -r git_secret",
		"chmod 600",
		"chmod 700",
		"unset git_username git_secret",
	} {
		if !strings.Contains(command, required) {
			t.Fatalf("command missing %q: %s", required, command)
		}
	}
	if strings.Contains(command, "kubectl get secret") || strings.Contains(command, ".data.token") {
		t.Fatalf("Git command exposes Secret data: %s", command)
	}
}

func TestParseGitRemovalCommitAcceptsMixedGitOutput(t *testing.T) {
	output := "Cloning into 'work'...\n[main abcdef123456] commit\nTo origin\nEVA_GIT_COMMIT=" + testGitCommit + "\n"
	commit, err := parseGitRemovalCommit(output)
	if err != nil || commit != testGitCommit {
		t.Fatalf("parseGitRemovalCommit() commit=%q err=%v", commit, err)
	}
}

func TestParseGitRemovalCommitRejectsMissingDuplicateAndInvalidMarkers(t *testing.T) {
	for _, output := range []string{
		"push completed\n",
		"EVA_GIT_COMMIT=" + testGitCommit + "\nEVA_GIT_COMMIT=" + testGitCommit + "\n",
		"EVA_GIT_COMMIT=not-a-sha\n",
	} {
		if _, err := parseGitRemovalCommit(output); err == nil {
			t.Fatalf("parseGitRemovalCommit(%q) unexpectedly succeeded", output)
		}
	}
}
