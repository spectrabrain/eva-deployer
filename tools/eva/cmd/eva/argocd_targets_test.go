package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"eva-deployer/tools/eva/internal/argocd"
	"eva-deployer/tools/eva/internal/plan"
	"eva-deployer/tools/eva/internal/release"
	"eva-deployer/tools/eva/internal/workspace"
)

type testArgoCDPreflightSession struct {
	run func(plan.Document, string) error
}

func (session *testArgoCDPreflightSession) Run(document plan.Document, root string) error {
	return session.run(document, root)
}
func (*testArgoCDPreflightSession) Close() error { return nil }

type testArgoCDManagementSession struct{ closed int }

func (*testArgoCDManagementSession) Run(string) (string, error)                  { return "", nil }
func (*testArgoCDManagementSession) RunWithInput(string, string) (string, error) { return "", nil }
func (session *testArgoCDManagementSession) Close() error                        { session.closed++; return nil }

func targetPreflightDocument(t *testing.T) (plan.Document, string) {
	t.Helper()
	config := remoteControllerConfig(t)
	previous := defaultArgoCDReceiptRoot
	defaultArgoCDReceiptRoot = t.TempDir()
	t.Cleanup(func() { defaultArgoCDReceiptRoot = previous })
	resolved, err := workspace.Resolve(workspace.Options{ConfigFile: config, CacheRoot: defaultWorkspaceCacheRoot})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	document := plan.Build(resolved, release.Resolved{Root: root}, time.Now())
	return document, root
}

func writeTargetArgoCDReport(t *testing.T, root, site, host string, applications []string) {
	t.Helper()
	directory := filepath.Join(root, "out/work/config", site, host)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	contents := "argocd_tracking:\n  status: ok\n  applications: []\n"
	if len(applications) > 0 {
		contents = "argocd_tracking:\n  status: ok\n  applications:\n"
		for _, app := range applications {
			contents += "    - " + app + "\n"
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "precondition.yaml"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func completeTargetArgoCDReceipt(site, cluster, app string) argocd.Receipt {
	return argocd.Receipt{SchemaVersion: "v1", SiteID: site, ClusterName: cluster, ClusterServer: "https://" + cluster + ":6443", Applications: []string{app}, CompletedAt: time.Now().UTC(),
		RegistrationApplication: "registration", RegistrationRepository: "https://git.example.com/registration.git", RegistrationBranch: "main",
		RegistrationManifest: "clusters/" + cluster + ".yaml", RegistrationCommit: strings.Repeat("a", 40)}
}

func TestArgoCDManagementConnectionIsPreparedOnceAndReusedAcrossTargets(t *testing.T) {
	document, root := targetPreflightDocument(t)
	writeTargetArgoCDReport(t, root, document.SiteID, "192.0.2.10", []string{"legacy-a-eva-app"})
	writeTargetArgoCDReport(t, root, document.SiteID, "192.0.2.11", []string{"legacy-b-eva-app"})
	writeTargetArgoCDReport(t, root, document.SiteID, "stale", []string{"unrelated-eva-app"})
	previousRead, previousConnect, previousRun := readArgoCDManagementCredentials, connectArgoCDManagement, runDetectedArgoCDPreflight
	t.Cleanup(func() {
		readArgoCDManagementCredentials, connectArgoCDManagement, runDetectedArgoCDPreflight = previousRead, previousConnect, previousRun
	})
	reads, connections := 0, 0
	management := &testArgoCDManagementSession{}
	readArgoCDManagementCredentials = func(*argoCDPrompter) (argocd.Credentials, error) {
		reads++
		return argocd.Credentials{Address: "management", User: "eva", Password: "private-ssh"}, nil
	}
	connectArgoCDManagement = func(credentials argocd.Credentials) (argocd.Session, error) {
		connections++
		if credentials.Password != "private-ssh" {
			t.Fatal("missing connection credential")
		}
		return management, nil
	}
	var names, roots []string
	runDetectedArgoCDPreflight = func(detection argocd.Detection, supplied argocd.Prompter, connect argocd.Connector) error {
		prompt := supplied.(*argoCDPrompter)
		credentials, err := prompt.Credentials()
		if err != nil || credentials.Password != "" || credentials.Address != "management" {
			t.Fatal("cached credentials retained secret or lost address")
		}
		shared, err := connect(credentials)
		if err != nil {
			t.Fatal(err)
		}
		if err := shared.Close(); err != nil || management.closed != 0 {
			t.Fatal("one target closed shared management SSH")
		}
		want := map[string]string{"node-a": "legacy-a-eva-app", "node-b": "legacy-b-eva-app"}[detection.Target]
		if !reflect.DeepEqual(detection.Applications, []string{want}) {
			t.Fatalf("mixed target applications: %#v", detection)
		}
		names, roots = append(names, detection.Target), append(roots, prompt.receiptRoot())
		return nil
	}
	preflight, err := prepareArgoCDPreflight()
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 || connections != 1 {
		t.Fatal("management input/connection was not prepared up front")
	}
	if err := preflight.Run(document, root); err != nil {
		t.Fatal(err)
	}
	if reads != 1 || connections != 1 || !reflect.DeepEqual(names, []string{"node-a", "node-b"}) || roots[0] == roots[1] {
		t.Fatal("management reuse or target isolation failed")
	}
	if err := preflight.Close(); err != nil || management.closed != 1 {
		t.Fatal("management connection was not closed once")
	}
}

func TestArgoCDPreflightValidatesAllReportsBeforeHandoff(t *testing.T) {
	document, root := targetPreflightDocument(t)
	writeTargetArgoCDReport(t, root, document.SiteID, "192.0.2.10", []string{"legacy-a-eva-app"})
	previous := runDetectedArgoCDPreflight
	t.Cleanup(func() { runDetectedArgoCDPreflight = previous })
	runDetectedArgoCDPreflight = func(argocd.Detection, argocd.Prompter, argocd.Connector) error {
		t.Fatal("handoff started before all reports were validated")
		return nil
	}
	preflight := &preparedArgoCDPreflight{prompt: &argoCDPrompter{}, session: &testArgoCDManagementSession{}}
	if err := preflight.Run(document, root); err == nil || !strings.Contains(err.Error(), "node-b") {
		t.Fatalf("missing report accepted: %v", err)
	}
}

func TestArgoCDInstallGuardRequiresMatchingTargetReceipts(t *testing.T) {
	document, root := targetPreflightDocument(t)
	writeTargetArgoCDReport(t, root, document.SiteID, "192.0.2.10", []string{"legacy-a-eva-app"})
	writeTargetArgoCDReport(t, root, document.SiteID, "192.0.2.11", []string{"legacy-b-eva-app"})
	targets, _, err := argoCDTargets(document)
	if err != nil {
		t.Fatal(err)
	}
	for index, target := range targets {
		cluster := []string{"legacy-a", "legacy-b"}[index]
		receipt := completeTargetArgoCDReceipt(document.SiteID, cluster, cluster+"-eva-app")
		if err := argocd.WriteReceipt(target.StateRoot, receipt); err != nil {
			t.Fatal(err)
		}
	}
	if err := requireNoArgoCDTracking(document, root); err != nil {
		t.Fatal(err)
	}
	wrong := completeTargetArgoCDReceipt(document.SiteID, "legacy-a", "legacy-a-eva-app")
	if err := argocd.WriteReceipt(targets[1].StateRoot, wrong); err != nil {
		t.Fatal(err)
	}
	if err := requireNoArgoCDTracking(document, root); err == nil || !strings.Contains(err.Error(), "node-b") {
		t.Fatalf("wrong target approval accepted: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "out/work/config", document.SiteID, targets[1].Host, "precondition.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := requireNoArgoCDTracking(document, root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing report ignored: %v", err)
	}
}

func TestArgoCDTargetSelectionUsesSameScopeInOnlineBastionAndRemote(t *testing.T) {
	for _, mode := range []string{"online", "remote"} {
		t.Run(mode, func(t *testing.T) {
			config := remoteControllerConfig(t)
			contents, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			contents = []byte(strings.ReplaceAll(string(contents), "mode: remote", "mode: "+mode))
			contents = []byte(strings.ReplaceAll(string(contents), "  node-b:\n", "  node-b:\n    components:\n      infra: true\n"))
			if err := os.WriteFile(config, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			resolved, err := workspace.Resolve(workspace.Options{ConfigFile: config, CacheRoot: defaultWorkspaceCacheRoot})
			if err != nil {
				t.Fatal(err)
			}
			document := plan.Build(resolved, release.Resolved{Root: t.TempDir()}, time.Now())
			targets, configured, err := argoCDTargets(document)
			if err != nil || !configured || len(targets) != 1 || targets[0].Name != "node-a" {
				t.Fatalf("wrong Solution target scope: %#v %v", targets, err)
			}
		})
	}
}

func TestRemoteLocalTargetUsesTransferredReceiptRoot(t *testing.T) {
	config := remoteControllerConfig(t)
	resolved, err := workspace.Resolve(workspace.Options{ConfigFile: config, CacheRoot: defaultWorkspaceCacheRoot})
	if err != nil {
		t.Fatal(err)
	}
	input, err := workspace.InstallerFromWorkspace(resolved)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := input.LocalTargetConfig("node-a")
	if err != nil {
		t.Fatal(err)
	}
	localConfig := filepath.Join(t.TempDir(), "local.yaml")
	if err := os.WriteFile(localConfig, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	local, err := workspace.Resolve(workspace.Options{ConfigFile: localConfig, CacheRoot: defaultWorkspaceCacheRoot})
	if err != nil {
		t.Fatal(err)
	}
	document := plan.Build(local, release.Resolved{Root: t.TempDir()}, time.Now())
	targets, _, err := argoCDTargets(document)
	if err != nil || len(targets) != 1 || targets[0].StateRoot != defaultArgoCDReceiptRoot {
		t.Fatalf("Target guard does not use transferred receipt location: %#v %v", targets, err)
	}
}
