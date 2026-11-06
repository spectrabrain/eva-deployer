package argocd

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"
)

// detachedClusterName derives only the historical name, not an API server or
// proof of disconnection. Both Git and management state must still be verified.
func detachedClusterName(detected []string) (string, error) {
	clusterName := ""
	for _, application := range detected {
		if !validLegacyApplication(application) {
			return "", fmt.Errorf("unsafe detected Argo CD Application name %q", application)
		}
		name := application[:strings.LastIndex(application, "-eva-")]
		if !safeClusterName(name) || clusterName != "" && clusterName != name {
			return "", errors.New("absent Applications do not identify exactly one legacy cluster")
		}
		clusterName = name
	}
	if clusterName == "" {
		return "", errors.New("no legacy cluster can be inferred from absent Applications")
	}
	return clusterName, nil
}

func gitAbsenceCommand(plan GitRemovalPlan) string {
	return gitCheckoutCommand(plan) + gitAbsenceCheckCommand(plan)
}

func gitReadAbsenceCommand(plan GitRemovalPlan) string {
	// Keep credential helpers available, but disable terminal/GUI prompting.
	// No credentials are read from stdin unless an actual auth failure occurs.
	return "set -euo pipefail; umask 077; base=\"$HOME/tmp\"; mkdir -p \"$base\"; root=$(mktemp -d \"$base/eva-argocd-handoff.XXXXXX\"); trap 'rm -rf \"$root\"' EXIT; " +
		"work=\"$root/repository\"; " + gitReadEnvironment +
		"timeout 60s git clone --depth 1 --branch " + shellQuote(plan.Branch) + " " + shellQuote(plan.Repository) + " \"$work\"; cd \"$work\"; " +
		"[ \"$(timeout 30s git rev-parse HEAD)\" = " + shellQuote(plan.ExpectedHead) + " ]; " + gitAbsenceCheckCommand(plan)
}

const gitReadEnvironment = "export LC_ALL=C GIT_TERMINAL_PROMPT=0 GIT_ASKPASS=/bin/false SSH_ASKPASS=/bin/false GCM_INTERACTIVE=never; "

func gitAbsenceCheckCommand(plan GitRemovalPlan) string {
	// Inspect the Git tree rather than the filesystem: symlinked parents or a
	// sparse checkout must not make a tracked registration appear absent.
	command := ""
	for parent := path.Dir(plan.Manifest); parent != "."; parent = path.Dir(parent) {
		command += "entry=$(timeout 30s git ls-tree HEAD -- " + shellQuote(parent) + "); " +
			"[[ -z \"$entry\" || \"$entry\" == '040000 tree '* ]]; "
	}
	return command +
		"entries=$(timeout 30s git ls-tree HEAD -- " + shellQuote(plan.Manifest) + "); " +
		"[ -z \"$entries\" ]; " +
		"printf 'EVA_GIT_ABSENT_COMMIT=%s\\n' " + shellQuote(plan.ExpectedHead)
}

func requireGitAbsenceMarker(output, head string) error {
	markers := 0
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "EVA_GIT_ABSENT_COMMIT=") {
			if line != "EVA_GIT_ABSENT_COMMIT="+head {
				return errors.New("Git absence verification returned a different commit")
			}
			markers++
		}
	}
	if markers != 1 {
		return errors.New("Git absence verification did not return exactly one commit marker")
	}
	return nil
}

// readOnlyGitAccess tries the management account's existing read access first.
// Only explicit HTTP authentication failures may request credentials, once.
// Transport, certificate, repository-state and manifest failures must not
// turn into misleading password prompts or bypass the absence check.
type readOnlyGitAccess struct {
	session     Session
	prompt      Prompter
	plan        GitRemovalPlan
	credentials GitCredentials
}

func (access *readOnlyGitAccess) run(command, authenticatedCommand string) (string, error) {
	if access.credentials.Username != "" {
		return access.session.RunWithInput(authenticatedCommand, gitCredentialInput(access.credentials))
	}
	output, err := access.session.Run(command)
	if err == nil || !isGitReadAuthenticationFailure(access.plan.Repository, output) {
		return output, err
	}
	reportProgress(access.prompt, "[INFO] Git read access requires authentication to verify the removed registration manifest; no commit or push will be performed.")
	credentials, credentialErr := access.prompt.GitCredentials(access.plan)
	if credentialErr != nil {
		return "", fmt.Errorf("read Git credentials for absence verification: %w", credentialErr)
	}
	if err := validateGitCredentials(credentials); err != nil {
		return "", err
	}
	access.credentials = credentials
	return access.session.RunWithInput(authenticatedCommand, gitCredentialInput(credentials))
}

func isGitReadAuthenticationFailure(repository, output string) bool {
	repositoryURL, err := url.Parse(repository)
	if err != nil || (repositoryURL.Scheme != "http" && repositoryURL.Scheme != "https") {
		return false
	}
	text := strings.ToLower(output)
	for _, marker := range []string{
		"could not read username for", "could not read password for",
		"authentication failed for", "http basic: access denied",
		"the requested url returned error: 401",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func recoverDetachedHandoff(session Session, prompt Prompter, detection Detection, applications []applicationRecord, registrations []clusterRegistration) (Receipt, error) {
	clusterName, err := detachedClusterName(detection.Applications)
	if err != nil {
		return Receipt{}, err
	}
	if err := requireApplicationsAbsent(clusterName, applications); err != nil {
		return Receipt{}, err
	}
	for _, registration := range registrations {
		if registration.Name == clusterName || registration.SecretName == clusterName {
			return Receipt{}, errors.New("Applications are absent but the target cluster registration still exists; disconnection is incomplete")
		}
	}

	reportProgress(prompt, "[INFO] Detected Applications are already absent; verifying cluster registration and Git desired state without deleting resources...")
	output, err := session.Run(registrationApplicationCommand())
	if err != nil {
		return Receipt{}, fmt.Errorf("read registration Application: %w", err)
	}
	registration, err := parseRegistrationApplication(output)
	if err != nil {
		return Receipt{}, fmt.Errorf("validate registration Application: %w", err)
	}
	if registrationContainsSecret(registration, clusterName) {
		return Receipt{}, errors.New("registration Application still desires the target cluster Secret")
	}
	source, err := validatedRegistrationSource(registration)
	if err != nil {
		return Receipt{}, err
	}
	access := readOnlyGitAccess{session: session, prompt: prompt, plan: GitRemovalPlan{Repository: source.Repository}}
	defer func() { access.credentials = GitCredentials{} }()
	readDefaultBranch := gitReadEnvironment + defaultBranchCommand(source.Repository)
	// Authenticated reads pass credentials through private temporary askpass files.
	authenticatedDefaultBranch := gitAuthenticatedDefaultBranchCommand(source.Repository)
	output, err = access.run(readDefaultBranch, authenticatedDefaultBranch)
	if err != nil {
		return Receipt{}, fmt.Errorf("resolve registration repository default branch: %w", err)
	}
	branch, head, err := parseDefaultBranch(output)
	if err != nil {
		return Receipt{}, err
	}
	if registration.Status.Sync.Revision != head {
		return Receipt{}, errors.New("registration Application has not observed the current Git HEAD; cannot verify disconnection")
	}
	manifest, err := targetRegistrationManifest(source.Path, clusterName)
	if err != nil {
		return Receipt{}, err
	}
	gitPlan := GitRemovalPlan{
		RegistrationApplication: registrationApplicationName,
		Repository:              source.Repository, Branch: branch, Manifest: manifest,
		ClusterName: clusterName, ExpectedHead: head,
	}
	access.plan = gitPlan
	reportProgress(prompt, "[INFO] Checking that the target registration manifest is absent from Git (read-only; no commit or push)...")
	output, err = access.run(gitReadAbsenceCommand(gitPlan), gitAbsenceCommand(gitPlan))
	if err != nil {
		return Receipt{}, fmt.Errorf("verify target registration manifest is absent from Git: %w", err)
	}
	if err := requireGitAbsenceMarker(output, head); err != nil {
		return Receipt{}, err
	}

	// Without live Applications or a registration, the old API endpoint is no
	// longer knowable. Do not invent one; verify the exact historical name.
	cluster := clusterRegistration{SecretName: clusterName, Name: clusterName}
	reportProgress(prompt, "[INFO] Verifying that the cluster registration and Applications remain absent for 60 seconds...")
	if err := verifyRemovalWindowWithSource(session, cluster, head, true, &source); err != nil {
		return Receipt{}, err
	}
	output, err = access.run(readDefaultBranch, authenticatedDefaultBranch)
	if err != nil {
		return Receipt{}, fmt.Errorf("recheck registration repository HEAD: %w", err)
	}
	finalBranch, finalHead, err := parseDefaultBranch(output)
	if err != nil {
		return Receipt{}, err
	}
	if finalBranch != branch || finalHead != head {
		return Receipt{}, errors.New("registration repository HEAD changed during absence verification; rerun preflight")
	}

	// The evidence commit is the observed synced HEAD with no registration
	// manifest, not a new removal commit. Reuse the existing receipt contract.
	return Receipt{
		SchemaVersion: receiptSchemaVersion, SiteID: detection.SiteID,
		ClusterName: clusterName, Applications: append([]string(nil), detection.Applications...),
		CompletedAt: time.Now().UTC(), RegistrationApplication: registrationApplicationName,
		RegistrationRepository: source.Repository, RegistrationBranch: branch,
		RegistrationManifest: manifest, RegistrationCommit: head,
	}, nil
}
