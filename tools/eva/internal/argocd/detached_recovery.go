package argocd

import (
	"errors"
	"fmt"
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
	// Inspect the Git tree rather than the filesystem: symlinked parents or a
	// sparse checkout must not make a tracked registration appear absent.
	command := gitCheckoutCommand(plan)
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
	output, err = session.Run(defaultBranchCommand(source.Repository))
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
	reportProgress(prompt, "[INFO] Checking that the target registration manifest is absent from Git (read-only; no commit or push)...")
	credentials, err := prompt.GitCredentials(gitPlan)
	if err != nil {
		return Receipt{}, fmt.Errorf("read Git credentials for absence verification: %w", err)
	}
	if err := validateGitCredentials(credentials); err != nil {
		return Receipt{}, err
	}
	output, err = session.RunWithInput(gitAbsenceCommand(gitPlan), gitCredentialInput(credentials))
	credentials.Secret = ""
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
	output, err = session.Run(defaultBranchCommand(source.Repository))
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
