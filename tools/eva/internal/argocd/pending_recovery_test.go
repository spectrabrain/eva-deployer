package argocd

import (
	"strings"
	"testing"
	"time"
)

func TestValidatePendingRecoveryRejectsUnrecordedApplication(
	t *testing.T,
) {
	pending := testPendingHandoff(time.Now().UTC())

	applications, err := parseApplications(
		applicationsJSON(
			applicationFixture{
				Name:   "legacy-a-eva-agent",
				Server: testClusterServer,
			},
			applicationFixture{
				Name:   "legacy-a-eva-vision",
				Server: testClusterServer,
			},
		),
	)
	if err != nil {
		t.Fatal(err)
	}

	err = validatePendingRecovery(
		Detection{
			SiteID: testWorkspaceSite,
			Applications: []string{
				"legacy-a-eva-agent",
			},
		},
		applications,
		nil,
		pending,
	)

	if err == nil {
		t.Fatal(
			"unrecorded target Application was accepted",
		)
	}
}

func TestPendingRemainingApplicationsSelectsOnlyRecorded(
	t *testing.T,
) {
	pending := testPendingHandoff(time.Now().UTC())

	applications, err := parseApplications(
		applicationsJSON(
			applicationFixture{
				Name:   "legacy-a-eva-agent",
				Server: testClusterServer,
			},
			applicationFixture{
				Name:   "legacy-b-eva-app",
				Server: "https://10.0.0.20:6443",
			},
		),
	)
	if err != nil {
		t.Fatal(err)
	}

	remaining := pendingRemainingApplications(
		pending,
		applications,
	)

	if len(remaining) != 1 ||
		remaining[0].Metadata.Name !=
			"legacy-a-eva-agent" {
		t.Fatalf("remaining = %#v", remaining)
	}
}

func TestValidatePendingRecoveryAcceptsMissingLiveResources(
	t *testing.T,
) {
	pending := testPendingHandoff(time.Now().UTC())

	err := validatePendingRecovery(
		Detection{
			SiteID: testWorkspaceSite,
			Applications: []string{
				"legacy-a-eva-agent",
				"legacy-a-eva-app",
			},
		},
		nil,
		nil,
		pending,
	)

	if err != nil {
		t.Fatalf(
			"validatePendingRecovery() error = %v",
			err,
		)
	}
}

func TestVerifyPendingLiveSecretAcceptsExactIdentity(
	t *testing.T,
) {
	pending := testPendingHandoff(time.Now().UTC())

	session := &fakeSession{
		outputs: map[string]string{
			clusterSecretCommand(pending.ClusterName): liveClusterSecretJSON(
				pending.ClusterName,
				pending.ClusterName,
				pending.ClusterServer,
			),
		},
	}

	if err := verifyPendingLiveSecret(
		session,
		pending,
	); err != nil {
		t.Fatalf(
			"verifyPendingLiveSecret() error = %v",
			err,
		)
	}
}

func TestVerifyPendingLiveSecretRejectsTrackingOwnerMismatch(
	t *testing.T,
) {
	pending := testPendingHandoff(time.Now().UTC())

	output := liveClusterSecretJSON(
		pending.ClusterName,
		pending.ClusterName,
		pending.ClusterServer,
	)

	output = strings.Replace(
		output,
		"registration:/Secret:argocd/legacy-a",
		"other:/Secret:argocd/legacy-a",
		1,
	)

	session := &fakeSession{
		outputSequences: map[string][]string{
			clusterSecretCommand(pending.ClusterName): {
				output,
			},
		},
	}

	err := verifyPendingLiveSecret(
		session,
		pending,
	)

	if err == nil ||
		!strings.Contains(
			err.Error(),
			"recorded registration Application",
		) {
		t.Fatalf(
			"verifyPendingLiveSecret() error = %v",
			err,
		)
	}
}

func TestVerifyPendingLiveSecretRejectsServerMismatch(
	t *testing.T,
) {
	pending := testPendingHandoff(time.Now().UTC())

	session := &fakeSession{
		outputSequences: map[string][]string{
			clusterSecretCommand(pending.ClusterName): {
				liveClusterSecretJSON(
					pending.ClusterName,
					pending.ClusterName,
					"https://10.0.0.99:6443",
				),
			},
		},
	}

	err := verifyPendingLiveSecret(
		session,
		pending,
	)

	if err == nil ||
		!strings.Contains(
			err.Error(),
			"does not match pending state",
		) {
		t.Fatalf(
			"verifyPendingLiveSecret() error = %v",
			err,
		)
	}
}

func TestRequireReceiptMatchesPendingAcceptsExactState(
	t *testing.T,
) {
	pending := testPendingHandoff(time.Now().UTC())

	receipt := pendingToReceipt(
		pending,
		time.Now().UTC(),
	)

	if err := requireReceiptMatchesPending(
		receipt,
		pending,
	); err != nil {
		t.Fatalf(
			"requireReceiptMatchesPending() error = %v",
			err,
		)
	}
}

func TestRequireReceiptMatchesPendingRejectsCommitMismatch(
	t *testing.T,
) {
	pending := testPendingHandoff(time.Now().UTC())

	receipt := pendingToReceipt(
		pending,
		time.Now().UTC(),
	)
	receipt.RegistrationCommit =
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	err := requireReceiptMatchesPending(
		receipt,
		pending,
	)

	if err == nil ||
		!strings.Contains(
			err.Error(),
			"does not match pending",
		) {
		t.Fatalf(
			"requireReceiptMatchesPending() error = %v",
			err,
		)
	}
}

func TestRequireReceiptMatchesPendingRejectsApplicationsMismatch(
	t *testing.T,
) {
	pending := testPendingHandoff(time.Now().UTC())

	receipt := pendingToReceipt(
		pending,
		time.Now().UTC(),
	)
	receipt.Applications = []string{
		"legacy-a-eva-agent",
	}

	err := requireReceiptMatchesPending(
		receipt,
		pending,
	)

	if err == nil ||
		!strings.Contains(
			err.Error(),
			"Applications do not match",
		) {
		t.Fatalf(
			"requireReceiptMatchesPending() error = %v",
			err,
		)
	}
}

func TestLegacyReceiptIsNotCompletedGitBackedEvidence(
	t *testing.T,
) {
	pending := testPendingHandoff(time.Now().UTC())

	legacyReceipt := Receipt{
		SchemaVersion: receiptSchemaVersion,
		SiteID:        pending.SiteID,
		ClusterName:   pending.ClusterName,
		ClusterServer: pending.ClusterServer,
		Applications: append(
			[]string(nil),
			pending.Applications...,
		),
		CompletedAt: time.Now().UTC(),
	}

	if hasCompleteGitReceiptMetadata(legacyReceipt) {
		t.Fatal(
			"legacy receipt was accepted as complete " +
				"Git-backed handoff evidence",
		)
	}

	gitBackedReceipt := pendingToReceipt(
		pending,
		time.Now().UTC(),
	)

	if !hasCompleteGitReceiptMetadata(gitBackedReceipt) {
		t.Fatal(
			"complete Git-backed receipt was not recognized",
		)
	}
}
