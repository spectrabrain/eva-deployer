package argocd

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"eva-deployer/tools/eva/internal/plan"
)

const (
	testWorkspaceSite = "customer-a"
	testLegacyCluster = "legacy-a"
	testClusterServer = "https://10.0.0.10:6443"
	testClusterSecret = "legacy-a-secret"
)

func TestRunSkipsInfraOnlyAndNoTracking(t *testing.T) {
	root := t.TempDir()
	writeReport(
		t,
		root,
		testWorkspaceSite,
		"host-a",
		"ok",
		nil,
	)

	called := false
	connect := func(Credentials) (Session, error) {
		called = true
		return nil, errors.New("unexpected connection")
	}

	err := Run(
		plan.Document{
			SiteID: testWorkspaceSite,
			Steps: []plan.Step{
				{Component: "infra"},
			},
		},
		root,
		fakePrompt{},
		connect,
	)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal(
			"infra-only operation attempted an Argo CD handoff",
		)
	}

	err = Run(
		plan.Document{
			SiteID: testWorkspaceSite,
			Steps: []plan.Step{
				{Component: "app"},
			},
		},
		root,
		fakePrompt{},
		connect,
	)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal(
			"untracked site attempted an Argo CD handoff",
		)
	}
}

func TestRunResolvesLegacyIdentityAndUsesKubectl(t *testing.T) {
	root := t.TempDir()

	detected := []string{
		"legacy-a-eva-agent",
		"legacy-a-eva-app",
		"legacy-a-eva-vision",
	}

	writeReport(
		t,
		root,
		testWorkspaceSite,
		"host-a",
		"ok",
		detected,
	)

	applicationsCommand := applicationListCommand()
	secretsCommand := clusterSecretListCommand()

	session := &fakeSession{
		outputSequences: map[string][]string{
			applicationsCommand: {
				applicationsJSON(
					applicationFixture{
						Name:   "legacy-a-eva-agent",
						Server: testClusterServer,
					},
					applicationFixture{
						Name:   "legacy-a-eva-app",
						Server: testClusterServer,
					},
					applicationFixture{
						Name:   "legacy-a-eva-vision",
						Server: testClusterServer,
					},
					applicationFixture{
						Name:   "legacy-a-eva-agent-init",
						Server: testClusterServer,
					},
					applicationFixture{
						Name:   "legacy-b-eva-app",
						Server: "https://10.0.0.20:6443",
					},
					applicationFixture{
						Name:   "legacy-aa-eva-app",
						Server: testClusterServer,
					},
				),
				applicationsJSON(
					applicationFixture{
						Name:   "legacy-b-eva-app",
						Server: "https://10.0.0.20:6443",
					},
					applicationFixture{
						Name:   "legacy-aa-eva-app",
						Server: testClusterServer,
					},
				),
			},
			secretsCommand: {
				clusterSecretsJSON(
					clusterSecretFixture{
						SecretName: testClusterSecret,
						Name:       testLegacyCluster,
						Server:     testClusterServer,
					},
					clusterSecretFixture{
						SecretName: "legacy-b-secret",
						Name:       "legacy-b",
						Server:     "https://10.0.0.20:6443",
					},
				),
				clusterSecretsJSON(
					clusterSecretFixture{
						SecretName: "legacy-b-secret",
						Name:       "legacy-b",
						Server:     "https://10.0.0.20:6443",
					},
				),
			},
		},
	}

	err := Run(
		plan.Document{
			SiteID: testWorkspaceSite,
			Steps: []plan.Step{
				{Component: "app"},
			},
		},
		root,
		approvedPrompt(),
		func(Credentials) (Session, error) {
			return session, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !session.closed {
		t.Fatal("session was not closed")
	}

	wantOrder := []string{
		"command -v timeout >/dev/null 2>&1 && " +
			"command -v kubectl >/dev/null 2>&1",
		applicationsCommand,
		secretsCommand,
		applicationSetExistenceCommand([]string{
			"appset-legacy-a-eva-agent",
			"appset-legacy-a-eva-agent-init",
			"appset-legacy-a-eva-app",
			"appset-legacy-a-eva-vision",
		}),
		deleteClusterSecretCommand(testClusterSecret),
		secretsCommand,
		deleteApplicationCommand(
			"legacy-a-eva-agent",
		),
		deleteApplicationCommand(
			"legacy-a-eva-agent-init",
		),
		deleteApplicationCommand(
			"legacy-a-eva-app",
		),
		deleteApplicationCommand(
			"legacy-a-eva-vision",
		),
		applicationsCommand,
		"sleep 5 && " + applicationsCommand,
	}

	assertExactCommandOrder(
		t,
		session.commands,
		wantOrder,
	)

	for _, command := range session.commands {
		if strings.Contains(command, "legacy-b-eva-app") ||
			strings.Contains(command, "legacy-aa-eva-app") {
			t.Fatalf(
				"unrelated Application was modified: %s",
				command,
			)
		}
	}

	for _, command := range session.commands {
		if strings.HasPrefix(command, "argocd ") ||
			strings.Contains(command, "argocd account") {
			t.Fatalf(
				"Argo CD CLI command remains: %s",
				command,
			)
		}
	}
}

func TestRunDestinationMismatchDoesNotMutate(
	t *testing.T,
) {
	root := t.TempDir()

	writeReport(
		t,
		root,
		testWorkspaceSite,
		"host-a",
		"ok",
		[]string{"legacy-a-eva-app"},
	)

	session := &fakeSession{
		outputs: map[string]string{
			applicationListCommand(): applicationsJSON(
				applicationFixture{
					Name:   "legacy-a-eva-app",
					Server: "https://10.0.0.99:6443",
				},
			),
			clusterSecretListCommand(): clusterSecretsJSON(
				clusterSecretFixture{
					SecretName: testClusterSecret,
					Name:       testLegacyCluster,
					Server:     testClusterServer,
				},
			),
		},
	}

	err := Run(
		plan.Document{
			SiteID: testWorkspaceSite,
			Steps: []plan.Step{
				{Component: "app"},
			},
		},
		root,
		approvedPrompt(),
		func(Credentials) (Session, error) {
			return session, nil
		},
	)
	if err == nil ||
		!strings.Contains(
			err.Error(),
			"exactly one Argo CD cluster registration",
		) {
		t.Fatalf("Run() error = %v", err)
	}

	assertNoMutation(t, session.commands)
}

func TestRunMissingDetectedApplicationDoesNotMutate(
	t *testing.T,
) {
	root := t.TempDir()

	writeReport(
		t,
		root,
		testWorkspaceSite,
		"host-a",
		"ok",
		[]string{"legacy-a-eva-app"},
	)

	session := &fakeSession{
		outputs: map[string]string{
			applicationListCommand(): applicationsJSON(),
			clusterSecretListCommand(): clusterSecretsJSON(
				clusterSecretFixture{
					SecretName: testClusterSecret,
					Name:       testLegacyCluster,
					Server:     testClusterServer,
				},
			),
		},
	}

	err := Run(
		plan.Document{
			SiteID: testWorkspaceSite,
			Steps: []plan.Step{
				{Component: "app"},
			},
		},
		root,
		approvedPrompt(),
		func(Credentials) (Session, error) {
			return session, nil
		},
	)
	if err == nil ||
		!strings.Contains(err.Error(), "absent") {
		t.Fatalf("Run() error = %v", err)
	}

	assertNoMutation(t, session.commands)
}

func TestRunAmbiguousClusterRegistrationDoesNotMutate(
	t *testing.T,
) {
	root := t.TempDir()

	writeReport(
		t,
		root,
		testWorkspaceSite,
		"host-a",
		"ok",
		[]string{"legacy-a-eva-app"},
	)

	session := &fakeSession{
		outputs: map[string]string{
			applicationListCommand(): applicationsJSON(
				applicationFixture{
					Name:   "legacy-a-eva-app",
					Server: testClusterServer,
				},
			),
			clusterSecretListCommand(): clusterSecretsJSON(
				clusterSecretFixture{
					SecretName: testClusterSecret,
					Name:       testLegacyCluster,
					Server:     testClusterServer,
				},
				clusterSecretFixture{
					SecretName: "duplicate-secret",
					Name:       "duplicate-name",
					Server:     testClusterServer,
				},
			),
		},
	}

	err := Run(
		plan.Document{
			SiteID: testWorkspaceSite,
			Steps: []plan.Step{
				{Component: "app"},
			},
		},
		root,
		approvedPrompt(),
		func(Credentials) (Session, error) {
			return session, nil
		},
	)
	if err == nil ||
		!strings.Contains(
			err.Error(),
			"exactly one Argo CD cluster registration",
		) {
		t.Fatalf("Run() error = %v", err)
	}

	assertNoMutation(t, session.commands)
}

func TestRunApplicationDeletionFailureReportsRegistrationAbsent(
	t *testing.T,
) {
	root := t.TempDir()

	writeReport(
		t,
		root,
		testWorkspaceSite,
		"host-a",
		"ok",
		[]string{
			"legacy-a-eva-agent",
			"legacy-a-eva-app",
		},
	)

	failingCommand := deleteApplicationCommand(
		"legacy-a-eva-app",
	)

	applicationsCommand := applicationListCommand()
	secretsCommand := clusterSecretListCommand()

	session := &fakeSession{
		outputSequences: map[string][]string{
			applicationsCommand: {
				applicationsJSON(
					applicationFixture{
						Name:   "legacy-a-eva-agent",
						Server: testClusterServer,
					},
					applicationFixture{
						Name:   "legacy-a-eva-app",
						Server: testClusterServer,
					},
				),
			},
			secretsCommand: {
				clusterSecretsJSON(
					clusterSecretFixture{
						SecretName: testClusterSecret,
						Name:       testLegacyCluster,
						Server:     testClusterServer,
					},
				),
				clusterSecretsJSON(),
			},
		},
		failures: map[string]error{
			failingCommand: errors.New("delete failed"),
		},
	}

	err := Run(
		plan.Document{
			SiteID: testWorkspaceSite,
			Steps: []plan.Step{
				{Component: "app"},
			},
		},
		root,
		approvedPrompt(),
		func(Credentials) (Session, error) {
			return session, nil
		},
	)

	if err == nil ||
		!strings.Contains(
			err.Error(),
			"cluster registration \"legacy-a\" is already absent",
		) {
		t.Fatalf("Run() error = %v", err)
	}

	secretDeleteIndex := commandIndex(
		session.commands,
		deleteClusterSecretCommand(testClusterSecret),
	)
	applicationDeleteIndex := commandIndex(
		session.commands,
		failingCommand,
	)

	if secretDeleteIndex < 0 {
		t.Fatal("cluster Secret deletion was not attempted")
	}

	if applicationDeleteIndex < 0 {
		t.Fatal("failing Application deletion was not attempted")
	}

	if secretDeleteIndex >= applicationDeleteIndex {
		t.Fatalf(
			"cluster Secret deletion index=%d, "+
				"Application deletion index=%d",
			secretDeleteIndex,
			applicationDeleteIndex,
		)
	}
}

func commandIndex(
	commands []string,
	expected string,
) int {
	for index, command := range commands {
		if command == expected {
			return index
		}
	}

	return -1
}

func TestRunDeclinedStopsBeforeConnection(t *testing.T) {
	root := t.TempDir()

	writeReport(
		t,
		root,
		testWorkspaceSite,
		"host-a",
		"ok",
		[]string{"legacy-a-eva-app"},
	)

	called := false

	err := Run(
		plan.Document{
			SiteID: testWorkspaceSite,
			Steps: []plan.Step{
				{Component: "app"},
			},
		},
		root,
		fakePrompt{},
		func(Credentials) (Session, error) {
			called = true
			return nil, nil
		},
	)
	if err == nil ||
		!strings.Contains(err.Error(), "non-cascade") {
		t.Fatalf("Run() error = %v", err)
	}
	if called {
		t.Fatal(
			"declined handoff connected to Argo CD",
		)
	}
}

func TestRequireNoTrackingUsesSimplifiedPreflightCommand(
	t *testing.T,
) {
	root := t.TempDir()

	writeReport(
		t,
		root,
		testWorkspaceSite,
		"host-a",
		"ok",
		[]string{"legacy-a-eva-app"},
	)

	err := RequireNoTracking(
		plan.Document{
			SiteID:    testWorkspaceSite,
			Workspace: "/work/customer-a",
			Steps: []plan.Step{
				{Component: "app"},
			},
		},
		root,
	)
	if err == nil ||
		!strings.Contains(
			err.Error(),
			"eva preflight argocd --workspace /work/customer-a",
		) {
		t.Fatalf(
			"RequireNoTracking() error = %v",
			err,
		)
	}
	if strings.Contains(err.Error(), "--release") {
		t.Fatalf(
			"RequireNoTracking() contains stale --release: %v",
			err,
		)
	}
}

func TestResolveCompletedHandoffRequiresApplicationsAndRegistrationAbsent(
	t *testing.T,
) {
	detection := Detection{
		SiteID: testWorkspaceSite,
		Applications: []string{
			"legacy-a-eva-agent",
			"legacy-a-eva-app",
		},
	}

	receipt, ok := resolveCompletedHandoff(
		detection,
		applicationsJSONRecords(t),
		nil,
	)
	if ok {
		t.Fatalf(
			"resolveCompletedHandoff() unexpectedly succeeded: %#v",
			receipt,
		)
	}

	receipt, ok = resolveCompletedHandoff(
		detection,
		nil,
		[]clusterRegistration{
			{
				Name:   testLegacyCluster,
				Server: testClusterServer,
			},
		},
	)
	if ok {
		t.Fatalf(
			"resolveCompletedHandoff() accepted existing registration: %#v",
			receipt,
		)
	}

	receipt, ok = resolveCompletedHandoff(
		detection,
		nil,
		nil,
	)
	if !ok {
		t.Fatal(
			"resolveCompletedHandoff() did not recover absent state",
		)
	}

	if receipt.ClusterName != testLegacyCluster ||
		receipt.SiteID != testWorkspaceSite ||
		len(receipt.Applications) != 2 {
		t.Fatalf("recovered receipt = %#v", receipt)
	}
}

func TestRequireNoTrackingAcceptsCoveringReceipt(
	t *testing.T,
) {
	releaseRoot := t.TempDir()
	receiptRoot := t.TempDir()

	writeReport(
		t,
		releaseRoot,
		testWorkspaceSite,
		"host-a",
		"ok",
		[]string{"legacy-a-eva-app"},
	)

	if err := WriteReceipt(
		receiptRoot,
		Receipt{
			SchemaVersion: receiptSchemaVersion,
			SiteID:        testWorkspaceSite,
			ClusterName:   testLegacyCluster,
			Applications: []string{
				"legacy-a-eva-agent",
				"legacy-a-eva-app",
			},
			CompletedAt: time.Now().UTC(),
		},
	); err != nil {
		t.Fatal(err)
	}

	err := RequireNoTrackingWithReceiptRoot(
		plan.Document{
			SiteID:    testWorkspaceSite,
			Workspace: "/work/customer-a",
			Steps: []plan.Step{
				{Component: "app"},
			},
		},
		releaseRoot,
		receiptRoot,
	)

	if err != nil {
		t.Fatalf(
			"RequireNoTrackingWithReceiptRoot() error = %v",
			err,
		)
	}
}

func TestRequireNoTrackingRejectsNonCoveringReceipt(
	t *testing.T,
) {
	releaseRoot := t.TempDir()
	receiptRoot := t.TempDir()

	writeReport(
		t,
		releaseRoot,
		testWorkspaceSite,
		"host-a",
		"ok",
		[]string{"legacy-a-eva-vision"},
	)

	if err := WriteReceipt(
		receiptRoot,
		Receipt{
			SchemaVersion: receiptSchemaVersion,
			SiteID:        testWorkspaceSite,
			ClusterName:   testLegacyCluster,
			Applications: []string{
				"legacy-a-eva-app",
			},
			CompletedAt: time.Now().UTC(),
		},
	); err != nil {
		t.Fatal(err)
	}

	err := RequireNoTrackingWithReceiptRoot(
		plan.Document{
			SiteID:    testWorkspaceSite,
			Workspace: "/work/customer-a",
			Steps: []plan.Step{
				{Component: "vision"},
			},
		},
		releaseRoot,
		receiptRoot,
	)

	if err == nil ||
		!strings.Contains(
			err.Error(),
			"eva preflight argocd",
		) {
		t.Fatalf(
			"RequireNoTrackingWithReceiptRoot() error = %v",
			err,
		)
	}
}

func applicationsJSONRecords(
	t *testing.T,
) []applicationRecord {
	t.Helper()

	records, err := parseApplications(
		applicationsJSON(
			applicationFixture{
				Name:   "legacy-a-eva-agent",
				Server: testClusterServer,
			},
		),
	)
	if err != nil {
		t.Fatal(err)
	}

	return records
}

func TestLoadDetectionFailsClosedWhenKubectlQueryFailed(
	t *testing.T,
) {
	root := t.TempDir()

	writeReport(
		t,
		root,
		testWorkspaceSite,
		"host-a",
		"error",
		nil,
	)

	_, err := LoadDetection(
		root,
		testWorkspaceSite,
	)
	if err == nil ||
		!strings.Contains(err.Error(), "detection failed") {
		t.Fatalf("LoadDetection() error = %v", err)
	}
}

type applicationFixture struct {
	Name            string
	DestinationName string
	Server          string
	OwnerKind       string
	OwnerName       string
	OwnerController *bool
}

type clusterSecretFixture struct {
	SecretName string
	Name       string
	Server     string
}

type fakePrompt struct {
	approved    bool
	credentials Credentials
}

func approvedPrompt() fakePrompt {
	return fakePrompt{
		approved: true,
		credentials: Credentials{
			Address:  "argo.example",
			User:     "operator",
			Password: "not-persisted",
		},
	}
}

func (prompt fakePrompt) Confirm(
	Detection,
) (bool, error) {
	return prompt.approved, nil
}

func (prompt fakePrompt) Credentials() (
	Credentials,
	error,
) {
	return prompt.credentials, nil
}

type fakeSession struct {
	commands        []string
	outputs         map[string]string
	outputSequences map[string][]string
	outputIndexes   map[string]int
	failures        map[string]error
	closed          bool
}

func (session *fakeSession) Run(
	command string,
) (string, error) {
	session.commands = append(
		session.commands,
		command,
	)

	if err := session.failures[command]; err != nil {
		return "", err
	}

	fixtureCommand := command
	if strings.HasPrefix(
		fixtureCommand,
		"sleep 5 && ",
	) {
		fixtureCommand = strings.TrimPrefix(
			fixtureCommand,
			"sleep 5 && ",
		)
	}

	sequence := session.outputSequences[fixtureCommand]
	if len(sequence) > 0 {
		if session.outputIndexes == nil {
			session.outputIndexes = map[string]int{}
		}

		index := session.outputIndexes[fixtureCommand]
		if index >= len(sequence) {
			index = len(sequence) - 1
		}

		session.outputIndexes[fixtureCommand]++

		return sequence[index], nil
	}

	return session.outputs[fixtureCommand], nil
}

func (session *fakeSession) Close() error {
	session.closed = true
	return nil
}

func applicationsJSON(
	fixtures ...applicationFixture,
) string {
	var builder strings.Builder

	builder.WriteString(`{"items":[`)

	for index, fixture := range fixtures {
		if index > 0 {
			builder.WriteByte(',')
		}

		ownerKind := fixture.OwnerKind
		if ownerKind == "" {
			ownerKind = "ApplicationSet"
		}

		ownerName := fixture.OwnerName
		if ownerName == "" {
			ownerName = "appset-" + fixture.Name
		}

		ownerController := true
		if fixture.OwnerController != nil {
			ownerController = *fixture.OwnerController
		}

		builder.WriteString(
			`{"metadata":{"name":"` +
				fixture.Name +
				`","ownerReferences":[{"kind":"` +
				ownerKind +
				`","name":"` +
				ownerName +
				`","controller":`,
		)

		if ownerController {
			builder.WriteString("true")
		} else {
			builder.WriteString("false")
		}

		builder.WriteString(
			`}]},"spec":{"destination":{`,
		)

		written := false

		if fixture.DestinationName != "" {
			builder.WriteString(
				`"name":"` +
					fixture.DestinationName +
					`"`,
			)
			written = true
		}

		if fixture.Server != "" {
			if written {
				builder.WriteByte(',')
			}

			builder.WriteString(
				`"server":"` +
					fixture.Server +
					`"`,
			)
		}

		builder.WriteString(`}}}`)
	}

	builder.WriteString(`]}`)

	return builder.String()
}

func clusterSecretsJSON(
	fixtures ...clusterSecretFixture,
) string {
	var builder strings.Builder

	builder.WriteString(`{"items":[`)

	for index, fixture := range fixtures {
		if index > 0 {
			builder.WriteByte(',')
		}

		builder.WriteString(
			`{"metadata":{"name":"` +
				fixture.SecretName +
				`"},"data":{"name":"` +
				base64Value(fixture.Name) +
				`","server":"` +
				base64Value(fixture.Server) +
				`"}}`,
		)
	}

	builder.WriteString(`]}`)

	return builder.String()
}

func base64Value(value string) string {
	return base64.StdEncoding.EncodeToString(
		[]byte(value),
	)
}

func assertNoMutation(
	t *testing.T,
	commands []string,
) {
	t.Helper()

	for _, command := range commands {
		if strings.Contains(
			command,
			" patch application ",
		) ||
			strings.Contains(
				command,
				" delete application ",
			) ||
			strings.Contains(
				command,
				" delete secret ",
			) {
			t.Fatalf(
				"mutation command was called: %s",
				command,
			)
		}
	}
}

func assertExactCommandOrder(
	t *testing.T,
	actual []string,
	expected []string,
) {
	t.Helper()

	if len(actual) != len(expected) {
		t.Fatalf(
			"command count = %d, want %d\nactual=%#v",
			len(actual),
			len(expected),
			actual,
		)
	}

	for index := range expected {
		if actual[index] != expected[index] {
			t.Fatalf(
				"command[%d] = %q, want %q",
				index,
				actual[index],
				expected[index],
			)
		}
	}
}

func writeReport(
	t *testing.T,
	root string,
	siteID string,
	host string,
	status string,
	applications []string,
) {
	t.Helper()

	directory := filepath.Join(
		root,
		"out",
		"work",
		"config",
		siteID,
		host,
	)

	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}

	contents := "argocd_tracking:\n  status: " +
		status +
		"\n  applications:\n"

	for _, application := range applications {
		contents += "    - " + application + "\n"
	}

	if err := os.WriteFile(
		filepath.Join(
			directory,
			"precondition.yaml",
		),
		[]byte(contents),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
}
