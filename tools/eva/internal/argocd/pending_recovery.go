package argocd

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

func cleanupCompletedPending(
	session Session,
	prompt Prompter,
	detection Detection,
	applications []applicationRecord,
	registrations []clusterRegistration,
	pending PendingHandoff,
	receipt Receipt,
) error {
	if err := validatePendingRecovery(
		detection,
		applications,
		registrations,
		pending,
	); err != nil {
		return err
	}

	if err := requireReceiptMatchesPending(
		receipt,
		pending,
	); err != nil {
		return err
	}

	cluster := clusterRegistration{
		SecretName: pending.ClusterName,
		Name:       pending.ClusterName,
		Server:     pending.ClusterServer,
	}

	if err := verifyStableRemoval(
		session,
		cluster,
		pending.RegistrationCommit,
	); err != nil {
		return fmt.Errorf(
			"verify completed handoff before stale "+
				"pending cleanup: %w",
			err,
		)
	}

	if err := removePending(
		prompt,
		pending.SiteID,
	); err != nil {
		return err
	}

	return nil
}

func requireReceiptMatchesPending(
	receipt Receipt,
	pending PendingHandoff,
) error {
	if !hasCompleteGitReceiptMetadata(receipt) {
		return errors.New(
			"completed receipt does not contain complete " +
				"Git-backed handoff metadata",
		)
	}

	if receipt.SiteID != pending.SiteID ||
		receipt.ClusterName != pending.ClusterName ||
		receipt.ClusterServer != pending.ClusterServer ||
		receipt.RegistrationApplication !=
			pending.RegistrationApplication ||
		receipt.RegistrationRepository !=
			pending.RegistrationRepository ||
		receipt.RegistrationBranch !=
			pending.RegistrationBranch ||
		receipt.RegistrationManifest !=
			pending.RegistrationManifest ||
		receipt.RegistrationCommit !=
			pending.RegistrationCommit {
		return errors.New(
			"completed receipt does not match pending " +
				"handoff state",
		)
	}

	receiptApplications := append(
		[]string(nil),
		receipt.Applications...,
	)
	pendingApplications := append(
		[]string(nil),
		pending.Applications...,
	)

	sort.Strings(receiptApplications)
	sort.Strings(pendingApplications)

	if len(receiptApplications) !=
		len(pendingApplications) {
		return errors.New(
			"completed receipt Applications do not match " +
				"pending handoff state",
		)
	}

	for index := range receiptApplications {
		if receiptApplications[index] !=
			pendingApplications[index] {
			return errors.New(
				"completed receipt Applications do not " +
					"match pending handoff state",
			)
		}
	}

	return nil
}

func resumePendingHandoff(
	session Session,
	prompt Prompter,
	detection Detection,
	applications []applicationRecord,
	registrations []clusterRegistration,
	pending PendingHandoff,
) error {
	if err := validatePendingRecovery(
		detection,
		applications,
		registrations,
		pending,
	); err != nil {
		return err
	}

	registrationOutput, err := session.Run(
		registrationApplicationCommand(),
	)
	if err != nil {
		return fmt.Errorf(
			"read registration Application for pending recovery: %w",
			err,
		)
	}

	registration, err := parseRegistrationApplication(
		registrationOutput,
	)
	if err != nil {
		return fmt.Errorf(
			"parse registration Application for pending recovery: %w",
			err,
		)
	}

	if registration.Status.Sync.Revision !=
		pending.RegistrationCommit {
		return fmt.Errorf(
			"registration Application revision %q "+
				"does not match pending commit %q",
			registration.Status.Sync.Revision,
			pending.RegistrationCommit,
		)
	}

	source, err := parsePendingRegistrationSource(
		registrationOutput,
	)
	if err != nil {
		return err
	}

	if source.Repository != pending.RegistrationRepository {
		return errors.New(
			"registration repository does not match pending state",
		)
	}

	manifest, err := targetRegistrationManifest(
		source.Path,
		pending.ClusterName,
	)
	if err != nil {
		return fmt.Errorf(
			"resolve pending registration manifest: %w",
			err,
		)
	}

	if manifest != pending.RegistrationManifest {
		return errors.New(
			"registration manifest does not match pending state",
		)
	}

	headOutput, err := session.Run(
		branchHeadCommand(
			pending.RegistrationRepository,
			pending.RegistrationBranch,
		),
	)
	if err != nil {
		return fmt.Errorf(
			"read pending registration branch HEAD: %w",
			err,
		)
	}

	head, err := parseBranchHead(headOutput)
	if err != nil {
		return err
	}

	if head != pending.RegistrationCommit {
		return fmt.Errorf(
			"registration branch HEAD %q "+
				"does not match pending commit %q",
			head,
			pending.RegistrationCommit,
		)
	}

	cluster := clusterRegistration{
		SecretName: pending.ClusterName,
		Name:       pending.ClusterName,
		Server:     pending.ClusterServer,
	}

	secretExists := false

	for _, registration := range registrations {
		matchesTarget := registration.SecretName ==
			cluster.SecretName ||
			registration.Name == cluster.Name ||
			registration.Server == cluster.Server

		if !matchesTarget {
			continue
		}

		if registration.SecretName != cluster.SecretName ||
			registration.Name != cluster.Name ||
			registration.Server != cluster.Server {
			return errors.New(
				"live cluster registration does not match " +
					"pending state",
			)
		}

		secretExists = true
	}

	if secretExists {
		if err := verifyPendingLiveSecret(
			session,
			pending,
		); err != nil {
			return err
		}

		reportProgress(
			prompt,
			fmt.Sprintf(
				"[INFO] Removing pending cluster registration: %s",
				cluster.Name,
			),
		)

		if _, err := session.Run(
			deleteClusterSecretCommand(cluster.SecretName),
		); err != nil {
			return fmt.Errorf(
				"remove pending cluster registration %q: %w",
				cluster.Name,
				err,
			)
		}

		verifySecretsOutput, err := session.Run(
			clusterSecretListCommand(),
		)
		if err != nil {
			return fmt.Errorf(
				"verify pending cluster registration removal: %w",
				err,
			)
		}

		remainingRegistrations, err :=
			parseClusterRegistrations(verifySecretsOutput)
		if err != nil {
			return err
		}

		for _, registration := range remainingRegistrations {
			if registration.SecretName == cluster.SecretName ||
				registration.Name == cluster.Name ||
				registration.Server == cluster.Server {
				return errors.New(
					"pending cluster registration still exists",
				)
			}
		}
	}

	remaining := pendingRemainingApplications(
		pending,
		applications,
	)

	for index, application := range remaining {
		reportProgress(
			prompt,
			fmt.Sprintf(
				"[INFO] Removing pending Application %d/%d: %s",
				index+1,
				len(remaining),
				application.Metadata.Name,
			),
		)

		if _, err := session.Run(
			deleteApplicationCommand(
				application.Metadata.Name,
			),
		); err != nil {
			return fmt.Errorf(
				"remove pending Argo CD Application %q: %w",
				application.Metadata.Name,
				err,
			)
		}
	}

	if err := verifyStableRemoval(
		session,
		cluster,
		pending.RegistrationCommit,
	); err != nil {
		return err
	}

	if err := recordReceipt(
		prompt,
		pendingToReceipt(
			pending,
			time.Now().UTC(),
		),
	); err != nil {
		return fmt.Errorf(
			"record completed receipt during pending recovery: %w; "+
				"pending state remains",
			err,
		)
	}

	if err := removePending(
		prompt,
		pending.SiteID,
	); err != nil {
		return fmt.Errorf(
			"remove completed pending handoff: %w; "+
				"completed receipt was already recorded",
			err,
		)
	}

	return nil
}

func validatePendingRecovery(
	detection Detection,
	applications []applicationRecord,
	registrations []clusterRegistration,
	pending PendingHandoff,
) error {
	if err := validatePending(pending); err != nil {
		return err
	}

	if pending.SiteID != detection.SiteID {
		return errors.New(
			"pending handoff site does not match detection",
		)
	}

	recorded := make(
		map[string]struct{},
		len(pending.Applications),
	)

	for _, name := range pending.Applications {
		recorded[name] = struct{}{}
	}

	for _, name := range detection.Applications {
		if _, ok := recorded[name]; !ok {
			return fmt.Errorf(
				"pending handoff does not cover detected "+
					"Application %q",
				name,
			)
		}
	}

	prefix := pending.ClusterName + "-"

	for _, application := range applications {
		name := application.Metadata.Name

		if !strings.HasPrefix(name, prefix) {
			continue
		}

		if _, ok := recorded[name]; !ok {
			return fmt.Errorf(
				"unrecorded target Application %q exists",
				name,
			)
		}

		destination := application.Spec.Destination
		if destination.Name != pending.ClusterName &&
			destination.Server != pending.ClusterServer {
			return fmt.Errorf(
				"recorded Application %q destination "+
					"does not match pending cluster",
				name,
			)
		}

		controllerOwners := 0

		for _, owner := range application.Metadata.OwnerReferences {
			if !owner.Controller {
				continue
			}

			if owner.Kind != "ApplicationSet" ||
				owner.Name == "" {
				return fmt.Errorf(
					"recorded Application %q has unsupported "+
						"controller owner",
					name,
				)
			}

			controllerOwners++
		}

		if controllerOwners != 1 {
			return fmt.Errorf(
				"recorded Application %q must have exactly "+
					"one ApplicationSet controller owner",
				name,
			)
		}
	}

	for _, registration := range registrations {
		matchesTarget := registration.SecretName ==
			pending.ClusterName ||
			registration.Name == pending.ClusterName ||
			registration.Server == pending.ClusterServer

		if matchesTarget &&
			(registration.SecretName != pending.ClusterName ||
				registration.Name != pending.ClusterName ||
				registration.Server != pending.ClusterServer) {
			return errors.New(
				"cluster registration conflicts with pending state",
			)
		}
	}

	return nil
}

func pendingRemainingApplications(
	pending PendingHandoff,
	applications []applicationRecord,
) []applicationRecord {
	recorded := make(
		map[string]struct{},
		len(pending.Applications),
	)

	for _, name := range pending.Applications {
		recorded[name] = struct{}{}
	}

	result := make([]applicationRecord, 0)

	for _, application := range applications {
		applicationName := application.Metadata.Name

		if _, ok := recorded[applicationName]; ok {
			result = append(result, application)
		}
	}

	return result
}

func verifyPendingLiveSecret(
	session Session,
	pending PendingHandoff,
) error {
	output, err := session.Run(
		clusterSecretCommand(pending.ClusterName),
	)
	if err != nil {
		return fmt.Errorf(
			"read exact cluster registration Secret "+
				"for pending recovery: %w",
			err,
		)
	}

	var secret liveClusterSecret

	if err := json.Unmarshal(
		[]byte(output),
		&secret,
	); err != nil {
		return fmt.Errorf(
			"parse exact cluster registration Secret "+
				"for pending recovery: %w",
			err,
		)
	}

	if secret.Metadata.Name != pending.ClusterName {
		return errors.New(
			"live cluster registration Secret name " +
				"does not match pending state",
		)
	}

	trackingKey := "argocd.argoproj.io/tracking-id"
	trackingValue := secret.Metadata.Annotations[trackingKey]

	tracking, err := parseTrackingIdentity(
		trackingValue,
	)
	if err != nil {
		return fmt.Errorf(
			"validate pending cluster registration "+
				"tracking ID: %w",
			err,
		)
	}

	if tracking.Application !=
		pending.RegistrationApplication ||
		tracking.Kind != "Secret" ||
		tracking.Namespace != registrationNamespace ||
		tracking.Name != pending.ClusterName {
		return errors.New(
			"live cluster registration Secret is not " +
				"managed by the recorded registration Application",
		)
	}

	name, err := decodeSecretData(
		secret.Data,
		"name",
	)
	if err != nil {
		return fmt.Errorf(
			"decode pending cluster registration name: %w",
			err,
		)
	}

	server, err := decodeSecretData(
		secret.Data,
		"server",
	)
	if err != nil {
		return fmt.Errorf(
			"decode pending cluster registration server: %w",
			err,
		)
	}

	if name != pending.ClusterName ||
		server != pending.ClusterServer {
		return errors.New(
			"live cluster registration Secret identity " +
				"does not match pending state",
		)
	}

	return nil
}

func parsePendingRegistrationSource(
	output string,
) (registrationSource, error) {
	application, err := parseRegistrationApplication(output)
	if err != nil {
		return registrationSource{}, err
	}

	return validatedRegistrationSource(application)
}
