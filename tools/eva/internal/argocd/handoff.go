// Package argocd performs the narrowly scoped Argo CD ownership handoff that
// is required before the Deployer takes over an existing EVA site.
package argocd

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"eva-deployer/tools/eva/internal/plan"
	"gopkg.in/yaml.v3"
)

type Detection struct {
	SiteID       string
	Applications []string
}

type preconditionReport struct {
	ArgoCDTracking struct {
		Status       string   `yaml:"status"`
		Applications []string `yaml:"applications"`
	} `yaml:"argocd_tracking"`
}

func LoadDetection(releaseRoot, siteID string) (Detection, error) {
	paths, err := filepath.Glob(filepath.Join(
		releaseRoot,
		"out",
		"work",
		"config",
		siteID,
		"*",
		"precondition.yaml",
	))
	if err != nil {
		return Detection{}, fmt.Errorf(
			"find precondition reports: %w",
			err,
		)
	}
	applications := map[string]struct{}{}
	for _, path := range paths {
		contents, err := os.ReadFile(path)
		if err != nil {
			return Detection{}, fmt.Errorf(
				"read precondition report %s: %w",
				path,
				err,
			)
		}
		var report preconditionReport
		if err := yaml.Unmarshal(contents, &report); err != nil {
			return Detection{}, fmt.Errorf(
				"parse precondition report %s: %w",
				path,
				err,
			)
		}
		if report.ArgoCDTracking.Status == "error" {
			return Detection{}, fmt.Errorf(
				"Argo CD tracking detection failed on target reported by %s; resolve the target kubectl access before installing Solution components",
				path,
			)
		}
		for _, application := range report.ArgoCDTracking.Applications {
			if !validLegacyApplication(application) {
				return Detection{}, fmt.Errorf(
					"precondition report %s contains unsafe Argo CD application name %q",
					path,
					application,
				)
			}
			applications[application] = struct{}{}
		}
	}
	result := Detection{
		SiteID:       siteID,
		Applications: make([]string, 0, len(applications)),
	}
	for application := range applications {
		result.Applications = append(
			result.Applications,
			application,
		)
	}
	sort.Strings(result.Applications)
	return result, nil
}

func RequiresHandoff(document plan.Document) bool {
	for _, step := range document.Steps {
		switch step.Component {
		case "config", "iam", "agent", "vision", "app", "n8n":
			return true
		}
	}
	return false
}

type Credentials struct {
	Address        string
	User           string
	Password       string
	ApproveHostKey HostKeyApprover
}

type GitCredentials struct {
	Username string
	Secret   string
}

type Prompter interface {
	Confirm(Detection) (bool, error)
	Credentials() (Credentials, error)
	ConfirmGitRemoval(GitRemovalPlan) (bool, error)
	GitCredentials(GitRemovalPlan) (GitCredentials, error)
}

type progressReporter interface {
	Progress(string)
}

type receiptRecorder interface {
	RecordReceipt(Receipt) error
}

type pendingStateManager interface {
	LoadPending(siteID string) (PendingHandoff, bool, error)
	LoadCompletedReceipt(siteID string) (Receipt, bool, error)
	RecordPending(PendingHandoff) error
	RemovePending(siteID string) error
}

func reportProgress(prompt Prompter, message string) {
	reporter, ok := prompt.(progressReporter)
	if ok {
		reporter.Progress(message)
	}
}

func recordReceipt(
	prompt Prompter,
	receipt Receipt,
) error {
	recorder, ok := prompt.(receiptRecorder)
	if !ok {
		return nil
	}

	return recorder.RecordReceipt(receipt)
}

func loadPending(
	prompt Prompter,
	siteID string,
) (PendingHandoff, bool, error) {
	manager, ok := prompt.(pendingStateManager)
	if !ok {
		return PendingHandoff{}, false, nil
	}

	return manager.LoadPending(siteID)
}

func loadCompletedReceipt(
	prompt Prompter,
	siteID string,
) (Receipt, bool, error) {
	manager, ok := prompt.(pendingStateManager)
	if !ok {
		return Receipt{}, false, nil
	}

	return manager.LoadCompletedReceipt(siteID)
}

func recordPending(
	prompt Prompter,
	pending PendingHandoff,
) error {
	manager, ok := prompt.(pendingStateManager)
	if !ok {
		return nil
	}

	return manager.RecordPending(pending)
}

func removePending(
	prompt Prompter,
	siteID string,
) error {
	manager, ok := prompt.(pendingStateManager)
	if !ok {
		return nil
	}

	return manager.RemovePending(siteID)
}

func validateGitCredentials(
	credentials GitCredentials,
) error {
	if strings.TrimSpace(credentials.Username) == "" {
		return errors.New(
			"Git repository username is required",
		)
	}

	if credentials.Secret == "" {
		return errors.New(
			"Git repository password or access token is required",
		)
	}

	if strings.ContainsAny(
		credentials.Username,
		"\r\n\x00",
	) {
		return errors.New(
			"Git repository username contains unsafe characters",
		)
	}

	if strings.ContainsAny(
		credentials.Secret,
		"\r\n\x00",
	) {
		return errors.New(
			"Git repository password or access token " +
				"contains unsafe characters",
		)
	}

	return nil
}

func gitCredentialInput(
	credentials GitCredentials,
) string {
	return credentials.Username +
		"\n" +
		credentials.Secret +
		"\n"
}

type Session interface {
	Run(command string) (string, error)
	RunWithInput(command string, input string) (string, error)
	Close() error
}

type Connector func(Credentials) (Session, error)

type clusterRegistration struct {
	SecretName string
	Name       string
	Server     string
}

type applicationRecord struct {
	Metadata struct {
		Name            string `json:"name"`
		OwnerReferences []struct {
			Kind       string `json:"kind"`
			Name       string `json:"name"`
			Controller bool   `json:"controller"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		Destination struct {
			Name   string `json:"name"`
			Server string `json:"server"`
		} `json:"destination"`
	} `json:"spec"`
}

type applicationList struct {
	Items []applicationRecord `json:"items"`
}

type clusterSecretList struct {
	Items []struct {
		Metadata struct {
			Name        string            `json:"name"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	} `json:"items"`
}

type handoffTarget struct {
	Cluster         clusterRegistration
	Applications    []applicationRecord
	ApplicationSets []string
}

func Run(document plan.Document, releaseRoot string, prompt Prompter, connect Connector) error {
	if !RequiresHandoff(document) {
		return nil
	}
	return RunPreflight(
		document.SiteID,
		releaseRoot,
		prompt,
		connect,
	)
}

func RunPreflight(
	siteID string,
	releaseRoot string,
	prompt Prompter,
	connect Connector,
) error {
	detection, err := LoadDetection(releaseRoot, siteID)
	if err != nil {
		return err
	}
	if len(detection.Applications) == 0 {
		return nil
	}

	approved, err := prompt.Confirm(detection)
	if err != nil {
		return err
	}
	if !approved {
		return manualActionError(detection)
	}

	credentials, err := prompt.Credentials()
	if err != nil {
		return err
	}
	if strings.TrimSpace(credentials.Address) == "" ||
		strings.TrimSpace(credentials.User) == "" {
		return errors.New(
			"Argo CD management server address and SSH user are required",
		)
	}

	reportProgress(
		prompt,
		"[INFO] Connecting to the Argo CD management server...",
	)

	session, err := connect(credentials)
	credentials.Password = ""
	if err != nil {
		return fmt.Errorf(
			"connect to Argo CD management server: %w",
			err,
		)
	}
	defer session.Close()

	reportProgress(
		prompt,
		"[INFO] Verifying Argo CD management resources...",
	)

	if _, err := session.Run(
		"command -v timeout >/dev/null 2>&1 && " +
			"command -v kubectl >/dev/null 2>&1 && " +
			"command -v git >/dev/null 2>&1",
	); err != nil {
		return fmt.Errorf(
			"validate timeout, kubectl, and git on "+
				"Argo CD management server: %w",
			err,
		)
	}

	applicationsOutput, err := session.Run(
		applicationListCommand(),
	)
	if err != nil {
		return fmt.Errorf(
			"read Argo CD Applications before handoff: %w",
			err,
		)
	}

	applications, err := parseApplications(applicationsOutput)
	if err != nil {
		return fmt.Errorf(
			"parse Argo CD Applications before handoff: %w",
			err,
		)
	}

	secretsOutput, err := session.Run(
		clusterSecretListCommand(),
	)
	if err != nil {
		return fmt.Errorf(
			"read Argo CD cluster registrations before handoff: %w",
			err,
		)
	}

	registrations, err := parseClusterRegistrations(secretsOutput)
	if err != nil {
		return fmt.Errorf(
			"parse Argo CD cluster registrations before handoff: %w",
			err,
		)
	}

	pending, pendingFound, err := loadPending(
		prompt,
		detection.SiteID,
	)
	if err != nil {
		return fmt.Errorf(
			"load pending Argo CD handoff: %w",
			err,
		)
	}

	if pendingFound {
		completed, completedFound, err := loadCompletedReceipt(
			prompt,
			detection.SiteID,
		)
		if err != nil {
			return fmt.Errorf(
				"load completed Argo CD handoff receipt "+
					"while pending state exists: %w",
				err,
			)
		}

		if completedFound &&
			hasCompleteGitReceiptMetadata(completed) {
			if err := cleanupCompletedPending(
				session,
				prompt,
				detection,
				applications,
				registrations,
				pending,
				completed,
			); err != nil {
				return fmt.Errorf(
					"clean stale completed Argo CD "+
						"pending state: %w",
					err,
				)
			}

			reportProgress(
				prompt,
				"[OK] Cleared stale completed Argo CD "+
					"pending state.",
			)

			return nil
		}

		reportProgress(
			prompt,
			"[INFO] Resuming recorded pending Argo CD handoff...",
		)

		if err := resumePendingHandoff(
			session,
			prompt,
			detection,
			applications,
			registrations,
			pending,
		); err != nil {
			return fmt.Errorf(
				"resume pending Argo CD handoff: %w",
				err,
			)
		}

		reportProgress(
			prompt,
			"[OK] Pending Argo CD handoff completed.",
		)

		return nil
	}

	if recovery, recovered, err := recoverGitBackedHandoff(session, detection, applications, registrations); err != nil {
		return fmt.Errorf("verify recorded Git-backed Argo CD handoff: %w", err)
	} else if recovered {
		if err := recordReceipt(prompt, recovery); err != nil {
			return fmt.Errorf("record recovered Argo CD handoff receipt: %w", err)
		}
		reportProgress(prompt, "[OK] Verified recorded Git-backed Argo CD ownership handoff.")
		return nil
	}

	target, err := resolveHandoffTarget(
		detection.Applications,
		applications,
		registrations,
	)
	if err != nil {
		return err
	}

	if _, err := session.Run(
		applicationSetExistenceCommand(target.ApplicationSets),
	); err != nil {
		return fmt.Errorf(
			"verify owner ApplicationSets before handoff: %w",
			err,
		)
	}

	gitPlan, err := prepareGitRemovalPlan(session, target)
	if err != nil {
		return err
	}
	approvedGit, err := prompt.ConfirmGitRemoval(gitPlan)
	if err != nil {
		return err
	}
	if !approvedGit {
		return errors.New("Git-backed cluster registration removal was not approved; no Git or Kubernetes mutation was performed")
	}

	gitCredentials, err := prompt.GitCredentials(gitPlan)
	if err != nil {
		return fmt.Errorf(
			"read Git repository credentials: %w; "+
				"no Git or Kubernetes mutation was performed",
			err,
		)
	}

	if err := validateGitCredentials(
		gitCredentials,
	); err != nil {
		gitCredentials.Secret = ""

		return fmt.Errorf(
			"validate Git repository credentials: %w; "+
				"no Git or Kubernetes mutation was performed",
			err,
		)
	}

	credentialInput := gitCredentialInput(
		gitCredentials,
	)

	reportProgress(prompt, "[INFO] Removing only the target cluster registration manifest from Git...")
	commitOutput, err := session.RunWithInput(
		gitRemovalCommand(gitPlan, target.Cluster.Server),
		credentialInput,
	)

	gitCredentials.Secret = ""
	credentialInput = ""

	if err != nil {
		return fmt.Errorf("commit and push target cluster registration removal: %w; no live Kubernetes resource was deleted", err)
	}
	commit, err := parseGitRemovalCommit(commitOutput)
	if err != nil {
		return fmt.Errorf("parse Git removal result: %w; no live Kubernetes resource was deleted", err)
	}
	pendingApplications := make(
		[]string,
		0,
		len(target.Applications),
	)

	for _, application := range target.Applications {
		pendingApplications = append(
			pendingApplications,
			application.Metadata.Name,
		)
	}

	pendingState := PendingHandoff{
		SchemaVersion:           pendingSchemaVersion,
		SiteID:                  detection.SiteID,
		ClusterName:             target.Cluster.Name,
		ClusterServer:           target.Cluster.Server,
		Applications:            pendingApplications,
		RegistrationApplication: registrationApplicationName,
		RegistrationRepository:  gitPlan.Repository,
		RegistrationBranch:      gitPlan.Branch,
		RegistrationManifest:    gitPlan.Manifest,
		RegistrationCommit:      commit,
		CreatedAt:               time.Now().UTC(),
	}

	if err := recordPending(prompt, pendingState); err != nil {
		return fmt.Errorf(
			"record pending Argo CD handoff after Git push "+
				"and before live deletion: %w; "+
				"Git removal commit %s was already pushed, "+
				"but no live Kubernetes resource was deleted",
			err,
			commit,
		)
	}

	reportProgress(
		prompt,
		"[OK] Recorded pending Argo CD handoff state.",
	)

	if err := waitForRegistrationRevision(
		session,
		prompt,
		commit,
	); err != nil {
		return fmt.Errorf(
			"verify registration Application observed "+
				"Git removal commit %s: %w; "+
				"pending handoff state remains for recovery "+
				"and no live Kubernetes resource was deleted",
			commit,
			err,
		)
	}

	reportProgress(
		prompt,
		fmt.Sprintf(
			"[OK] Verified legacy cluster: %s "+
				"server=%s applications=%d",
			target.Cluster.Name,
			target.Cluster.Server,
			len(target.Applications),
		),
	)

	reportProgress(
		prompt,
		fmt.Sprintf(
			"[INFO] Removing cluster registration %s "+
				"to stop Application recreation...",
			target.Cluster.Name,
		),
	)

	if _, err := session.Run(
		deleteClusterSecretCommand(target.Cluster.SecretName),
	); err != nil {
		return fmt.Errorf(
			"remove Argo CD cluster registration %q: %w; "+
				"no Application deletion was started",
			target.Cluster.Name,
			err,
		)
	}

	verifySecretsOutput, err := session.Run(
		clusterSecretListCommand(),
	)
	if err != nil {
		return fmt.Errorf(
			"verify Argo CD cluster registration %q removal: %w",
			target.Cluster.Name,
			err,
		)
	}

	remainingRegistrations, err := parseClusterRegistrations(
		verifySecretsOutput,
	)
	if err != nil {
		return fmt.Errorf(
			"parse Argo CD cluster registrations after removal: %w",
			err,
		)
	}

	for _, registration := range remainingRegistrations {
		if registration.Name == target.Cluster.Name ||
			registration.Server == target.Cluster.Server ||
			registration.SecretName == target.Cluster.SecretName {
			return fmt.Errorf(
				"Argo CD cluster registration %q still exists; "+
					"Application deletion was not started",
				target.Cluster.Name,
			)
		}
	}

	reportProgress(
		prompt,
		fmt.Sprintf(
			"[OK] Cluster registration removed: %s",
			target.Cluster.Name,
		),
	)

	reportProgress(
		prompt,
		"[INFO] Removing legacy Applications without "+
			"deleting deployed workloads...",
	)

	for index, application := range target.Applications {
		name := application.Metadata.Name

		reportProgress(
			prompt,
			fmt.Sprintf(
				"[INFO] Removing Application %d/%d: %s",
				index+1,
				len(target.Applications),
				name,
			),
		)

		if _, err := session.Run(
			deleteApplicationCommand(name),
		); err != nil {
			return fmt.Errorf(
				"remove Argo CD Application %q "+
					"without cascading: %w; "+
					"cluster registration %q is already absent, "+
					"rerun eva preflight argocd",
				name,
				err,
				target.Cluster.Name,
			)
		}

		reportProgress(
			prompt,
			fmt.Sprintf(
				"[OK] Removed Application %d/%d: %s",
				index+1,
				len(target.Applications),
				name,
			),
		)
	}

	reportProgress(
		prompt,
		"[INFO] Verifying that Applications are not recreated...",
	)

	verifyApplicationsOutput, err := session.Run(
		applicationListCommand(),
	)
	if err != nil {
		return fmt.Errorf(
			"verify Argo CD Applications after deletion: %w",
			err,
		)
	}

	remainingApplications, err := parseApplications(
		verifyApplicationsOutput,
	)
	if err != nil {
		return fmt.Errorf(
			"parse Argo CD Applications after deletion: %w",
			err,
		)
	}

	if err := requireApplicationsAbsent(
		target.Cluster.Name,
		remainingApplications,
	); err != nil {
		return err
	}

	stableApplicationsOutput, err := session.Run(
		"sleep 5 && " + applicationListCommand(),
	)
	if err != nil {
		return fmt.Errorf(
			"verify Argo CD Applications remain absent: %w",
			err,
		)
	}

	stableApplications, err := parseApplications(
		stableApplicationsOutput,
	)
	if err != nil {
		return fmt.Errorf(
			"parse stabilized Argo CD Applications: %w",
			err,
		)
	}

	if err := requireApplicationsAbsent(
		target.Cluster.Name,
		stableApplications,
	); err != nil {
		return err
	}
	if err := verifyStableRemoval(session, target.Cluster, commit); err != nil {
		return err
	}

	if err := recordReceipt(
		prompt,
		pendingToReceipt(
			pendingState,
			time.Now().UTC(),
		),
	); err != nil {
		return fmt.Errorf(
			"record Argo CD handoff receipt: %w; "+
				"pending handoff state remains for recovery",
			err,
		)
	}

	reportProgress(
		prompt,
		"[OK] Recorded completed Argo CD ownership handoff.",
	)

	if err := removePending(
		prompt,
		detection.SiteID,
	); err != nil {
		return fmt.Errorf(
			"remove completed Argo CD pending handoff: %w; "+
				"the completed receipt was already recorded",
			err,
		)
	}

	reportProgress(
		prompt,
		"[OK] Cleared pending Argo CD handoff state.",
	)
	reportProgress(
		prompt,
		"[OK] Argo CD ownership handoff completed.",
	)

	return nil
}

func applicationListCommand() string {
	return "timeout 30s kubectl get applications.argoproj.io " +
		"-n argocd -o json"
}

func clusterSecretListCommand() string {
	return "timeout 30s kubectl get secrets -n argocd " +
		"-l argocd.argoproj.io/secret-type=cluster " +
		"-o json"
}

func applicationSetExistenceCommand(names []string) string {
	quoted := make([]string, 0, len(names))

	for _, name := range names {
		quoted = append(quoted, shellQuote(name))
	}

	return "timeout 30s kubectl get applicationsets.argoproj.io " +
		strings.Join(quoted, " ") +
		" -n argocd -o name"
}

func deleteClusterSecretCommand(name string) string {
	return "timeout 60s kubectl delete secret " +
		shellQuote(name) +
		" -n argocd --wait=true --timeout=45s"
}

func deleteApplicationCommand(name string) string {
	quotedName := shellQuote(name)
	finalizerPatch := shellQuote(
		`{"metadata":{"finalizers":null}}`,
	)

	return "if timeout 30s kubectl get application " +
		quotedName +
		" -n argocd >/dev/null 2>&1; then " +
		"timeout 30s kubectl patch application " +
		quotedName +
		" -n argocd --type=merge -p " +
		finalizerPatch +
		" >/dev/null && " +
		"timeout 30s kubectl delete application " +
		quotedName +
		" -n argocd --wait=false >/dev/null && " +
		"timeout 30s kubectl patch application " +
		quotedName +
		" -n argocd --type=merge -p " +
		finalizerPatch +
		" >/dev/null 2>&1 || true; " +
		"timeout 60s kubectl wait --for=delete " +
		"application/" +
		quotedName +
		" -n argocd --timeout=45s; " +
		"fi"
}

func requireApplicationsAbsent(
	clusterName string,
	applications []applicationRecord,
) error {
	prefix := clusterName + "-"

	for _, application := range applications {
		if strings.HasPrefix(
			application.Metadata.Name,
			prefix,
		) {
			return fmt.Errorf(
				"Argo CD Application %q was recreated or "+
					"still exists after cluster registration removal",
				application.Metadata.Name,
			)
		}
	}

	return nil
}

func detectedApplicationsAbsent(
	detected []string,
	applications []applicationRecord,
) bool {
	remote := make(map[string]struct{}, len(applications))

	for _, application := range applications {
		remote[application.Metadata.Name] = struct{}{}
	}

	for _, application := range detected {
		if _, exists := remote[application]; exists {
			return false
		}
	}

	return true
}

func parseApplications(output string) ([]applicationRecord, error) {
	var list applicationList
	if err := json.Unmarshal([]byte(output), &list); err == nil &&
		list.Items != nil {
		return list.Items, nil
	}

	var applications []applicationRecord
	if err := json.Unmarshal(
		[]byte(output),
		&applications,
	); err != nil {
		return nil, err
	}

	return applications, nil
}

func parseClusterRegistrations(
	output string,
) ([]clusterRegistration, error) {
	var list clusterSecretList

	if err := json.Unmarshal([]byte(output), &list); err != nil {
		return nil, err
	}

	registrations := make(
		[]clusterRegistration,
		0,
		len(list.Items),
	)

	for _, item := range list.Items {
		name, err := decodeSecretData(item.Data, "name")
		if err != nil {
			return nil, fmt.Errorf(
				"decode cluster Secret %q name: %w",
				item.Metadata.Name,
				err,
			)
		}

		server, err := decodeSecretData(item.Data, "server")
		if err != nil {
			return nil, fmt.Errorf(
				"decode cluster Secret %q server: %w",
				item.Metadata.Name,
				err,
			)
		}

		if name == "" || server == "" {
			return nil, fmt.Errorf(
				"cluster Secret %q requires name and server",
				item.Metadata.Name,
			)
		}

		registrations = append(
			registrations,
			clusterRegistration{
				SecretName: item.Metadata.Name,
				Name:       name,
				Server:     server,
			},
		)
	}

	return registrations, nil
}

func decodeSecretData(
	data map[string]string,
	key string,
) (string, error) {
	encoded := data[key]
	if encoded == "" {
		return "", errors.New("value is missing")
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}

	return string(decoded), nil
}

func resolveHandoffTarget(
	detected []string,
	applications []applicationRecord,
	registrations []clusterRegistration,
) (handoffTarget, error) {
	remoteByName := make(
		map[string]applicationRecord,
		len(applications),
	)

	for _, application := range applications {
		remoteByName[application.Metadata.Name] = application
	}

	destinationName := ""
	destinationServer := ""

	for _, name := range detected {
		if !validLegacyApplication(name) {
			return handoffTarget{}, fmt.Errorf(
				"unsafe detected Argo CD Application name %q",
				name,
			)
		}

		application, found := remoteByName[name]
		if !found {
			return handoffTarget{}, fmt.Errorf(
				"locally detected Argo CD Application %q "+
					"is absent from the management server",
				name,
			)
		}

		destination := application.Spec.Destination
		if destination.Name == "" && destination.Server == "" {
			return handoffTarget{}, fmt.Errorf(
				"Argo CD Application %q has no destination",
				name,
			)
		}

		if destination.Name != "" {
			if destinationName != "" &&
				destinationName != destination.Name {
				return handoffTarget{}, errors.New(
					"detected Argo CD Applications have " +
						"different destination names",
				)
			}
			destinationName = destination.Name
		}

		if destination.Server != "" {
			if destinationServer != "" &&
				destinationServer != destination.Server {
				return handoffTarget{}, errors.New(
					"detected Argo CD Applications have " +
						"different destination servers",
				)
			}
			destinationServer = destination.Server
		}
	}

	matches := make([]clusterRegistration, 0, 1)

	for _, registration := range registrations {
		nameMatches := destinationName != "" &&
			registration.Name == destinationName

		serverMatches := destinationServer != "" &&
			registration.Server == destinationServer

		if nameMatches || serverMatches {
			matches = append(matches, registration)
		}
	}

	if len(matches) != 1 {
		return handoffTarget{}, fmt.Errorf(
			"expected exactly one Argo CD cluster registration "+
				"for detected Applications, found %d",
			len(matches),
		)
	}

	cluster := matches[0]

	if destinationName != "" &&
		cluster.Name != destinationName {
		return handoffTarget{}, fmt.Errorf(
			"Argo CD cluster name mismatch: "+
				"destination=%q registration=%q",
			destinationName,
			cluster.Name,
		)
	}

	if destinationServer != "" &&
		cluster.Server != destinationServer {
		return handoffTarget{}, fmt.Errorf(
			"Argo CD cluster server mismatch: "+
				"destination=%q registration=%q",
			destinationServer,
			cluster.Server,
		)
	}

	prefix := cluster.Name + "-"
	selected := make([]applicationRecord, 0)

	for _, application := range applications {
		name := application.Metadata.Name

		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if !validLegacyApplication(name) {
			continue
		}

		destination := application.Spec.Destination
		if destination.Name != cluster.Name &&
			destination.Server != cluster.Server {
			return handoffTarget{}, fmt.Errorf(
				"Argo CD Application %q destination does not "+
					"match cluster registration %q",
				name,
				cluster.Name,
			)
		}

		selected = append(selected, application)
	}

	if len(selected) == 0 {
		return handoffTarget{}, errors.New(
			"no Argo CD Applications matched the verified cluster",
		)
	}

	sort.Slice(selected, func(left, right int) bool {
		return selected[left].Metadata.Name <
			selected[right].Metadata.Name
	})

	applicationSets := make([]string, 0, len(selected))
	seenApplicationSets := map[string]bool{}

	for _, application := range selected {
		owners := application.Metadata.OwnerReferences
		controllerOwners := make([]string, 0, 1)

		for _, owner := range owners {
			if owner.Controller {
				if owner.Kind != "ApplicationSet" ||
					owner.Name == "" {
					return handoffTarget{}, fmt.Errorf(
						"Argo CD Application %q has unsupported "+
							"controller owner %s/%s",
						application.Metadata.Name,
						owner.Kind,
						owner.Name,
					)
				}

				controllerOwners = append(
					controllerOwners,
					owner.Name,
				)
			}
		}

		if len(controllerOwners) != 1 {
			return handoffTarget{}, fmt.Errorf(
				"Argo CD Application %q must have exactly one "+
					"ApplicationSet controller owner, found %d",
				application.Metadata.Name,
				len(controllerOwners),
			)
		}

		ownerName := controllerOwners[0]

		if !seenApplicationSets[ownerName] {
			applicationSets = append(
				applicationSets,
				ownerName,
			)
			seenApplicationSets[ownerName] = true
		}
	}

	sort.Strings(applicationSets)

	return handoffTarget{
		Cluster:         cluster,
		Applications:    selected,
		ApplicationSets: applicationSets,
	}, nil
}

func validLegacyApplication(application string) bool {
	if application == "" {
		return false
	}

	for _, character := range application {
		if !(character >= 'a' && character <= 'z') &&
			!(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') &&
			character != '.' &&
			character != '-' {
			return false
		}
	}

	for _, suffix := range []string{
		"-eva-iam",
		"-eva-app",
		"-eva-vision",
		"-eva-agent",
		"-eva-agent-init",
		"-eva-agent-qdrant",
		"-eva-agent-vllm",
	} {
		if strings.HasSuffix(application, suffix) &&
			len(application) > len(suffix) {
			return true
		}
	}

	return false
}

func RequireNoTracking(
	document plan.Document,
	releaseRoot string,
) error {
	return RequireNoTrackingWithReceiptRoot(
		document,
		releaseRoot,
		DefaultReceiptRoot,
	)
}

func RequireNoTrackingWithReceiptRoot(
	document plan.Document,
	releaseRoot string,
	receiptRoot string,
) error {
	if !RequiresHandoff(document) {
		return nil
	}

	detection, err := LoadDetection(
		releaseRoot,
		document.SiteID,
	)
	if err != nil {
		return err
	}

	if len(detection.Applications) == 0 {
		return nil
	}

	receipt, receiptErr := LoadReceipt(
		receiptRoot,
		document.SiteID,
	)
	if receiptErr == nil {
		if err := ReceiptCovers(
			receipt,
			detection,
		); err == nil && hasCompleteGitReceiptMetadata(receipt) {
			return nil
		}
	}

	return fmt.Errorf(
		"Argo CD-managed EVA resources are still present "+
			"for site %q. Run "+
			"`eva preflight argocd --workspace %s` "+
			"from the verified Release root to review "+
			"and disconnect them before eva install",
		document.SiteID,
		document.Workspace,
	)
}

func manualActionError(detection Detection) error {
	return fmt.Errorf(
		"Argo CD handoff was not approved. " +
			"Before installing EVA Solution, remove the detected " +
			"Applications with non-cascade deletion and remove " +
			"their exact destination cluster registration from " +
			"the Argo CD management server",
	)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(
		value,
		"'",
		"'\"'\"'",
	) + "'"
}
