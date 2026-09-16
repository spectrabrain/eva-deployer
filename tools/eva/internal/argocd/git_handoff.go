package argocd

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

const registrationApplicationName = "registration"
const registrationNamespace = "argocd"

var commitSHAExpression = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

type trackingIdentity struct {
	Application string
	Kind        string
	Namespace   string
	Name        string
}

type registrationSource struct {
	Repository string
	Revision   string
	Path       string
}

type registrationResource struct {
	Group     string `json:"group"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type registrationApplication struct {
	Spec struct {
		Source *struct {
			RepoURL        string `json:"repoURL"`
			TargetRevision string `json:"targetRevision"`
			Path           string `json:"path"`
		} `json:"source"`
		Sources []json.RawMessage `json:"sources"`
	} `json:"spec"`
	Status struct {
		Resources []registrationResource `json:"resources"`
		Sync      struct {
			Revision string `json:"revision"`
		} `json:"sync"`
	} `json:"status"`
}

type liveClusterSecret struct {
	Metadata struct {
		Name        string            `json:"name"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
}

// GitRemovalPlan is deliberately metadata-only: it is safe to show to an
// operator and to persist in the handoff receipt.
type GitRemovalPlan struct {
	RegistrationApplication string
	Repository              string
	Branch                  string
	Manifest                string
	ClusterName             string
	ClusterServer           string
	ExpectedHead            string
}

func parseTrackingIdentity(value string) (trackingIdentity, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 3 || parts[0] == "" {
		return trackingIdentity{}, errors.New("tracking ID must have application, kind, namespace, and name")
	}
	if strings.Contains(parts[0], "/") || parts[1] != "/Secret" {
		return trackingIdentity{}, errors.New("tracking ID must identify a Secret managed by one Application")
	}
	namespace, name, found := strings.Cut(parts[2], "/")
	if !found || namespace == "" || name == "" || strings.Contains(name, "/") {
		return trackingIdentity{}, errors.New("tracking ID Secret namespace/name is invalid")
	}
	return trackingIdentity{
		Application: parts[0], Kind: strings.TrimPrefix(parts[1], "/"), Namespace: namespace, Name: name,
	}, nil
}

func parseRegistrationApplication(output string) (registrationApplication, error) {
	var application registrationApplication
	if err := json.Unmarshal([]byte(output), &application); err != nil {
		return registrationApplication{}, err
	}
	if application.Spec.Source == nil || len(application.Spec.Sources) != 0 {
		return registrationApplication{}, errors.New("registration Application must use exactly one spec.source")
	}
	if _, err := validatedRegistrationSource(application); err != nil {
		return registrationApplication{}, err
	}
	return application, nil
}

func validatedRegistrationSource(application registrationApplication) (registrationSource, error) {
	if application.Spec.Source == nil || len(application.Spec.Sources) != 0 {
		return registrationSource{}, errors.New("registration Application must use exactly one spec.source")
	}
	source := registrationSource{
		Repository: strings.TrimSpace(application.Spec.Source.RepoURL),
		Revision:   strings.TrimSpace(application.Spec.Source.TargetRevision),
		Path:       strings.TrimSpace(application.Spec.Source.Path),
	}
	if source.Repository == "" || source.Revision == "" || source.Path == "" {
		return registrationSource{}, errors.New("registration Application source requires repoURL, targetRevision, and path")
	}
	if source.Revision != "HEAD" {
		return registrationSource{}, fmt.Errorf("registration targetRevision %q is not the required HEAD", source.Revision)
	}
	if strings.ContainsAny(source.Repository, "\r\n\x00") {
		return registrationSource{}, errors.New("registration repository URL is unsafe")
	}
	if !safeRelativePath(source.Path) {
		return registrationSource{}, fmt.Errorf("registration source path %q is not a safe relative path", source.Path)
	}
	return source, nil
}

func safeRelativePath(value string) bool {
	if value == "" || path.IsAbs(value) || strings.Contains(value, "\\") || strings.Contains(value, "\x00") {
		return false
	}
	cleaned := path.Clean(value)
	return cleaned != ".." && !strings.HasPrefix(cleaned, "../") && cleaned == value
}

func targetRegistrationManifest(sourcePath, clusterName string) (string, error) {
	if !safeRelativePath(sourcePath) || !safeClusterName(clusterName) {
		return "", errors.New("registration source path or cluster name is unsafe")
	}
	if sourcePath == "." {
		return "clusters/" + clusterName + ".yaml", nil
	}
	return sourcePath + "/clusters/" + clusterName + ".yaml", nil
}

func safeClusterName(value string) bool {
	if value == "" || len(value) > 253 {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') && !(character >= '0' && character <= '9') && character != '.' && character != '-' {
			return false
		}
	}
	return true
}

func registrationContainsSecret(application registrationApplication, secretName string) bool {
	for _, resource := range application.Status.Resources {
		if resource.Group == "" && resource.Kind == "Secret" && resource.Namespace == registrationNamespace && resource.Name == secretName {
			return true
		}
	}
	return false
}

func parseDefaultBranch(output string) (branch, head string, err error) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "ref:" && fields[2] == "HEAD" && strings.HasPrefix(fields[1], "refs/heads/") {
			branch = strings.TrimPrefix(fields[1], "refs/heads/")
		}
		if len(fields) == 2 && fields[1] == "HEAD" && commitSHAExpression.MatchString(fields[0]) {
			head = fields[0]
		}
	}
	if branch == "" || head == "" {
		return "", "", errors.New("could not resolve repository default branch and HEAD")
	}
	return branch, head, nil
}

func validCommitSHA(value string) bool { return commitSHAExpression.MatchString(value) }

func registrationApplicationCommand() string {
	return "timeout 30s kubectl get application " + shellQuote(registrationApplicationName) + " -n " + shellQuote(registrationNamespace) + " -o json"
}

func defaultBranchCommand(repository string) string {
	return "timeout 30s git ls-remote --symref " + shellQuote(repository) + " HEAD"
}

func branchHeadCommand(repository, branch string) string {
	return "timeout 30s git ls-remote " + shellQuote(repository) + " " + shellQuote("refs/heads/"+branch)
}

func parseBranchHead(output string) (string, error) {
	fields := strings.Fields(output)
	if len(fields) != 2 || !validCommitSHA(fields[0]) || !strings.HasPrefix(fields[1], "refs/heads/") {
		return "", errors.New("remote branch HEAD response is invalid")
	}
	return fields[0], nil
}

func gitRemovalCommand(plan GitRemovalPlan, server string) string {
	// The script never emits the Secret content. kubectl's client-side parser
	// validates the single YAML document and exposes only its metadata.
	return "set -euo pipefail; base=\"$HOME/tmp\"; mkdir -p \"$base\"; work=$(mktemp -d \"$base/eva-argocd-handoff.XXXXXX\"); trap 'rm -rf \"$work\"' EXIT; " +
		"timeout 60s git clone --depth 1 --branch " + shellQuote(plan.Branch) + " " + shellQuote(plan.Repository) + " \"$work\"; cd \"$work\"; " +
		"[ \"$(timeout 30s git rev-parse HEAD)\" = " + shellQuote(plan.ExpectedHead) + " ]; " +
		"timeout 30s git ls-files --error-unmatch -- " + shellQuote(plan.Manifest) + " >/dev/null; " +
		"[ -f " + shellQuote(plan.Manifest) + " ] && [ ! -L " + shellQuote(plan.Manifest) + " ]; " +
		"[ \"$(timeout 30s kubectl create --dry-run=client -f " + shellQuote(plan.Manifest) + " -o name | wc -l)\" -eq 1 ]; " +
		"metadata=$(timeout 30s kubectl create --dry-run=client -f " + shellQuote(plan.Manifest) + " -o jsonpath='{.apiVersion}|{.kind}|{.metadata.namespace}|{.metadata.name}|{.metadata.labels.argocd\\.argoproj\\.io/secret-type}|{.stringData.name}|{.stringData.server}|{.data.name}|{.data.server}'); " +
		"expected_string_data=" + shellQuote("v1|Secret|argocd|"+plan.ClusterName+"|cluster|"+plan.ClusterName+"|"+plan.ClusterServer+"||") + "; " +
		"expected_data=" + shellQuote("v1|Secret|argocd|"+plan.ClusterName+"|cluster|||"+base64.StdEncoding.EncodeToString([]byte(plan.ClusterName))+"|"+base64.StdEncoding.EncodeToString([]byte(plan.ClusterServer))) + "; " +
		"if [ \"$metadata\" != \"$expected_string_data\" ] && [ \"$metadata\" != \"$expected_data\" ]; then echo 'EVA_GIT_STAGE_FAILED=validate-manifest-identity' >&2; false; fi; " +
		"timeout 30s git rm -- " + shellQuote(plan.Manifest) + "; " +
		"[ \"$(timeout 30s git diff --cached --name-status | wc -l)\" -eq 1 ]; " +
		"[ \"$(timeout 30s git diff --cached --name-status)\" = " + shellQuote("D\t"+plan.Manifest) + " ]; " +
		"timeout 30s git config user.name >/dev/null || timeout 30s git config user.name 'EVA Deployer'; timeout 30s git config user.email >/dev/null || timeout 30s git config user.email 'eva-deployer@localhost'; " +
		"timeout 60s git commit -m " + shellQuote("[DEPLOYER] remove "+plan.ClusterName+" from Argo CD registration") + "; commit=$(timeout 30s git rev-parse HEAD); " +
		"remote=$(timeout 30s git ls-remote " + shellQuote(plan.Repository) + " " + shellQuote("refs/heads/"+plan.Branch) + " | awk 'NR==1 {print $1}'); [ \"$remote\" = " + shellQuote(plan.ExpectedHead) + " ]; " +
		"timeout 60s git push origin HEAD:" + shellQuote("refs/heads/"+plan.Branch) + "; printf 'EVA_GIT_COMMIT=%s\\n' \"$commit\""
}

func parseGitRemovalCommit(output string) (string, error) {
	const marker = "EVA_GIT_COMMIT="
	commit := ""
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, marker) {
			continue
		}
		if commit != "" {
			return "", errors.New("Git removal output contains multiple commit markers")
		}
		commit = strings.TrimPrefix(line, marker)
	}
	if commit == "" {
		return "", errors.New("Git removal output has no commit marker")
	}
	if !validCommitSHA(commit) {
		return "", errors.New("Git removal output contains an invalid commit marker")
	}
	return commit, nil
}

func clusterSecretCommand(name string) string {
	return "timeout 30s kubectl get secret " + shellQuote(name) + " -n " + shellQuote(registrationNamespace) + " -o json"
}

func prepareGitRemovalPlan(session Session, target handoffTarget) (GitRemovalPlan, error) {
	if target.Cluster.SecretName != target.Cluster.Name {
		return GitRemovalPlan{}, errors.New("target cluster registration Secret name must exactly match the cluster name")
	}
	secretOutput, err := session.Run(clusterSecretCommand(target.Cluster.SecretName))
	if err != nil {
		return GitRemovalPlan{}, fmt.Errorf("read target cluster registration Secret: %w", err)
	}
	var secret liveClusterSecret
	if err := json.Unmarshal([]byte(secretOutput), &secret); err != nil {
		return GitRemovalPlan{}, fmt.Errorf("parse target cluster registration Secret: %w", err)
	}
	if secret.Metadata.Name != target.Cluster.SecretName {
		return GitRemovalPlan{}, errors.New("target cluster registration Secret name changed during validation")
	}
	tracking, err := parseTrackingIdentity(secret.Metadata.Annotations["argocd.argoproj.io/tracking-id"])
	if err != nil {
		return GitRemovalPlan{}, fmt.Errorf("validate cluster registration tracking ID: %w", err)
	}
	if tracking.Application != registrationApplicationName || tracking.Kind != "Secret" || tracking.Namespace != registrationNamespace || tracking.Name != target.Cluster.SecretName {
		return GitRemovalPlan{}, errors.New("cluster registration Secret is not managed by Application/argocd/registration")
	}
	if name, err := decodeSecretData(secret.Data, "name"); err != nil || name != target.Cluster.Name {
		return GitRemovalPlan{}, errors.New("live cluster registration Secret name does not match the selected destination")
	}
	if server, err := decodeSecretData(secret.Data, "server"); err != nil || server != target.Cluster.Server {
		return GitRemovalPlan{}, errors.New("live cluster registration Secret server does not match the selected destination")
	}
	registrationOutput, err := session.Run(registrationApplicationCommand())
	if err != nil {
		return GitRemovalPlan{}, fmt.Errorf("read Application/argocd/registration: %w", err)
	}
	registration, err := parseRegistrationApplication(registrationOutput)
	if err != nil {
		return GitRemovalPlan{}, fmt.Errorf("validate Application/argocd/registration: %w", err)
	}
	if !registrationContainsSecret(registration, target.Cluster.SecretName) {
		return GitRemovalPlan{}, errors.New("registration Application does not desire the exact target cluster Secret")
	}
	source, err := validatedRegistrationSource(registration)
	if err != nil {
		return GitRemovalPlan{}, err
	}
	branchOutput, err := session.Run(defaultBranchCommand(source.Repository))
	if err != nil {
		return GitRemovalPlan{}, fmt.Errorf("resolve registration repository default branch: %w", err)
	}
	branch, _, err := parseDefaultBranch(branchOutput)
	if err != nil {
		return GitRemovalPlan{}, err
	}
	headOutput, err := session.Run(branchHeadCommand(source.Repository, branch))
	if err != nil {
		return GitRemovalPlan{}, fmt.Errorf("read registration repository branch HEAD: %w", err)
	}
	head, err := parseBranchHead(headOutput)
	if err != nil {
		return GitRemovalPlan{}, err
	}
	manifest, err := targetRegistrationManifest(source.Path, target.Cluster.Name)
	if err != nil {
		return GitRemovalPlan{}, err
	}
	return GitRemovalPlan{
		RegistrationApplication: registrationApplicationName,
		Repository:              source.Repository, Branch: branch, Manifest: manifest,
		ClusterName: target.Cluster.Name, ClusterServer: target.Cluster.Server, ExpectedHead: head,
	}, nil
}

func waitForRegistrationRevision(session Session, commit string) error {
	for attempt := 0; attempt < 12; attempt++ {
		output, err := session.Run(registrationApplicationCommand())
		if err != nil {
			return err
		}
		application, err := parseRegistrationApplication(output)
		if err != nil {
			return err
		}
		if application.Status.Sync.Revision == commit {
			return nil
		}
		if attempt < 11 {
			if _, err := session.Run("sleep 5"); err != nil {
				return err
			}
		}
	}
	return errors.New("registration Application did not observe the pushed revision")
}

func verifyStableRemoval(session Session, cluster clusterRegistration, commit string) error {
	// Thirteen observations separated by twelve five-second waits prove a
	// full sixty-second absence window rather than a one-shot deletion.
	for attempt := 0; attempt < 13; attempt++ {
		registrationOutput, err := session.Run(registrationApplicationCommand())
		if err != nil {
			return err
		}
		registration, err := parseRegistrationApplication(registrationOutput)
		if err != nil {
			return err
		}
		if registration.Status.Sync.Revision != commit || registrationContainsSecret(registration, cluster.SecretName) {
			return errors.New("registration Application again desires the target cluster Secret")
		}
		secretsOutput, err := session.Run(clusterSecretListCommand())
		if err != nil {
			return err
		}
		registrations, err := parseClusterRegistrations(secretsOutput)
		if err != nil {
			return err
		}
		for _, registration := range registrations {
			if registration.SecretName == cluster.SecretName || registration.Name == cluster.Name || registration.Server == cluster.Server {
				return errors.New("target cluster registration Secret was recreated")
			}
		}
		applicationsOutput, err := session.Run(applicationListCommand())
		if err != nil {
			return err
		}
		applications, err := parseApplications(applicationsOutput)
		if err != nil {
			return err
		}
		if err := requireApplicationsAbsent(cluster.Name, applications); err != nil {
			return err
		}
		if attempt < 12 {
			if _, err := session.Run("sleep 5"); err != nil {
				return err
			}
		}
	}
	return nil
}

func recoverGitBackedHandoff(session Session, detection Detection, applications []applicationRecord, registrations []clusterRegistration) (Receipt, bool, error) {
	receipt, err := LoadReceipt(DefaultReceiptRoot, detection.SiteID)
	if err != nil {
		return Receipt{}, false, nil
	}
	if receipt.RegistrationCommit == "" {
		return Receipt{}, false, nil
	}
	if err := ReceiptCovers(receipt, detection); err != nil {
		return Receipt{}, false, err
	}
	if !detectedApplicationsAbsent(detection.Applications, applications) {
		return Receipt{}, false, nil
	}
	for _, registration := range registrations {
		if registration.Name == receipt.ClusterName || registration.Server == receipt.ClusterServer || registration.SecretName == receipt.ClusterName {
			return Receipt{}, false, nil
		}
	}
	cluster := clusterRegistration{SecretName: receipt.ClusterName, Name: receipt.ClusterName, Server: receipt.ClusterServer}
	if err := verifyStableRemoval(session, cluster, receipt.RegistrationCommit); err != nil {
		return Receipt{}, false, err
	}
	return receipt, true, nil
}
