package access

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"eva-deployer/tools/eva/internal/health"
	"eva-deployer/tools/eva/internal/plan"
)

func TestDeployedValuesResolvePublicURLs(t *testing.T) {
	for _, test := range []struct {
		name, app, iam, wantApp, wantIAM string
	}{
		{"shared-http", "backendHost: 10.186.0.75\n  backendSecure: false\n  backendPublicPort: 80", "host: 10.186.0.75\n  publicPort: ''", "http://10.186.0.75/", "http://10.186.0.75/iam/"},
		{"custom-http", "backendHost: 10.186.0.75\n  backendSecure: false\n  backendPublicPort: 30680", "host: 10.186.0.75\n  publicPort: 30681", "http://10.186.0.75:30680/", "http://10.186.0.75:30681/iam/"},
		{"separate-ports", "backendHost: app.example.com\n  backendSecure: false\n  backendPublicPort: 32010", "host: iam.example.com\n  publicPort: '32011'", "http://app.example.com:32010/", "http://iam.example.com:32011/iam/"},
		{"https", "backendHost: app.example.com\n  backendSecure: true\n  backendPublicPort: 80\n  backendSecurePort: 32443", "host: iam.example.com\n  publicUrl: https://sso.example.com:30443/auth", "https://app.example.com:32443/", "https://sso.example.com:30443/auth/"},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := fixturePlan(t)
			writeFixture(t, filepath.Join(document.Workspace, "inventory/host_vars/server-a.yaml"), "ansible_connection: local\nansible_host: 10.186.0.75\nansible_become_password: private-test-password\n")
			writeFixture(t, valuesPath(document, "10.186.0.75", "app"), "app:\n  "+test.app+"\ningress:\n  path: /\ndatabase:\n  password: private-test-password\n")
			writeFixture(t, valuesPath(document, "10.186.0.75", "iam"), "config:\n  "+test.iam+"\ningress:\n  path: /iam\n")
			writeFixture(t, filepath.Join(document.ReleaseRoot, "out/work/config/customer-a/10.186.0.75/n8n.yaml"), "endpoint: http://10.186.0.75:30678\nlogin:\n  password: private-test-password\n")
			endpoints := Discover(document, Options{Run: func(string, ...string) health.CommandResult {
				t.Fatal("live query was unnecessary when deployed output is available")
				return health.CommandResult{}
			}})
			assertURL(t, endpoints, "App", test.wantApp)
			assertURL(t, endpoints, "IAM", test.wantIAM)
			assertURL(t, endpoints, "n8n", "http://10.186.0.75:30678/")
			var output bytes.Buffer
			Print(&output, endpoints)
			if strings.Contains(output.String(), "private-test-password") || strings.Contains(output.String(), "Harbor") {
				t.Fatalf("unexpected output: %s", output.String())
			}
		})
	}
}

func TestTargetScopesAndCustomOutputPaths(t *testing.T) {
	document := fixturePlan(t)
	document.Steps = []plan.Step{{Component: "app", Targets: []string{"server-a"}}, {Component: "iam", Targets: []string{"server-b"}}}
	customDir := filepath.Join(t.TempDir(), "custom-iam")
	writeFixture(t, filepath.Join(document.Workspace, "inventory/host_vars/server-a.yaml"), "ansible_connection: ssh\nansible_host: 192.0.2.10\n")
	writeFixture(t, filepath.Join(document.Workspace, "inventory/host_vars/server-b.yaml"), "ansible_connection: ssh\nansible_host: 192.0.2.11\neva_iam_deploy_values_dir: "+customDir+"\n")
	writeFixture(t, valuesPath(document, "192.0.2.10", "app"), "app:\n  backendHost: 192.0.2.10\n  backendSecure: true\n  backendSecurePort: 443\ningress:\n  path: /eva\n")
	writeFixture(t, filepath.Join(customDir, "resolved-values.yaml"), "config:\n  host: 192.0.2.11\n  publicPort: 30443\ningress:\n  path: /sso\n  tls:\n    enabled: true\n")
	endpoints := Discover(document, Options{Run: func(string, ...string) health.CommandResult {
		t.Fatal("controller Kubernetes must not be used for SSH targets")
		return health.CommandResult{}
	}})
	if len(endpoints) != 2 || endpoints[0].Target != "server-a" || endpoints[1].Target != "server-b" {
		t.Fatalf("target scopes = %#v", endpoints)
	}
	assertURL(t, endpoints, "App", "https://192.0.2.10/eva/")
	assertURL(t, endpoints, "IAM", "https://192.0.2.11:30443/sso/")
}

func TestLiveResourcesResolveExistingInstallationAndPreferCurrentValues(t *testing.T) {
	document := fixturePlan(t)
	writeFixture(t, valuesPath(document, "server-a", "app"), "app:\n  backendHost: old.example.com\n  backendPublicPort: 80\n")
	resources := map[string]string{
		"eva-app": `{"items":[{"kind":"ConfigMap","data":{"BACKEND_HOST":"10.159.56.197","BACKEND_SECURE":"true","BACKEND_SECURE_PORT":"32443","BACKEND_PORT":"8000","PASSWORD":"private-test-password"}},{"kind":"Ingress","spec":{"rules":[{"http":{"paths":[{"path":"/"}]}}]}}]}`,
		"eva-iam": `{"items":[{"kind":"ConfigMap","data":{"ISSUER":"https://10.159.56.197:443/iam/realms/eva-iam"}}]}`,
		"n8n":     `{"items":[{"kind":"Deployment","spec":{"template":{"spec":{"containers":[{"env":[{"name":"N8N_PORT","value":"5678"},{"name":"WEBHOOK_URL","value":"https://webhooks.example.com/"},{"name":"N8N_EDITOR_BASE_URL","value":"http://10.186.0.75:30678/"}]}]}}}}]}`,
	}
	queries := 0
	endpoints := Discover(document, Options{PreferLive: true, Run: func(name string, args ...string) health.CommandResult {
		queries++
		if name != "kubectl" || !strings.Contains(strings.Join(args, " "), "--request-timeout=5s") {
			t.Fatalf("unexpected command: %s %v", name, args)
		}
		return health.CommandResult{Output: resources[args[3]]}
	}})
	assertURL(t, endpoints, "App", "https://10.159.56.197:32443/")
	assertURL(t, endpoints, "IAM", "https://10.159.56.197/iam/")
	assertURL(t, endpoints, "n8n", "http://10.186.0.75:30678/")
	if queries != 3 {
		t.Fatalf("queries = %d, want 3", queries)
	}
}

func TestHarborProtocolAndConditionalComponents(t *testing.T) {
	for _, test := range []struct{ name, mode, receipt, mirrors, probe, want string }{
		{"remote-http", "remote_repository", "registry: 10.159.56.124:32080\nprotocol: http\n", "", "", "http://10.159.56.124:32080/"},
		{"local-https", "local", "registry: 10.159.56.124:32080\nprotocol: https\n", "", "", "https://10.159.56.124:32080/"},
		{"legacy-http-mirror", "remote", "registry: unrelated.example.com\nprotocol: https\n", "mirrors:\n  '10.159.56.124:32080':\n    endpoint: ['http://10.159.56.124:32080']\nconfigs:\n  '10.159.56.124:32080':\n    auth:\n      password: private-test-password\n", "", "http://10.159.56.124:32080/"},
		{"legacy-probe", "local_repository", "", "", "http://10.159.56.124:32080", "http://10.159.56.124:32080/"},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := fixturePlan(t)
			document.RepositoryMode, document.Repository = test.mode, "10.159.56.124:32080"
			document.Steps = nil
			options := Options{ReadFile: func(path string) ([]byte, error) {
				if path == "/etc/rancher/k3s/registries.yaml" {
					return []byte(test.mirrors), nil
				}
				return []byte(test.receipt), nil
			}, ProbeRegistry: func(registry string) string {
				if test.probe == "" || registry != document.Repository {
					t.Fatal("unexpected registry probe")
				}
				return test.probe
			}}
			endpoints := Discover(document, options)
			if len(endpoints) != 1 {
				t.Fatalf("disabled components shown: %#v", endpoints)
			}
			assertURL(t, endpoints, "Harbor", test.want)
		})
	}
}

func TestFailuresAreConciseAndDoNotExposeCredentials(t *testing.T) {
	document := fixturePlan(t)
	endpoints := Discover(document, Options{Run: func(string, ...string) health.CommandResult {
		return health.CommandResult{Err: fmt.Errorf("private-test-password"), Output: "private-test-password"}
	}})
	var output bytes.Buffer
	Print(&output, endpoints)
	if strings.Contains(output.String(), "private-test-password") || strings.Count(output.String(), "unavailable") != 3 {
		t.Fatalf("failure output = %s", output.String())
	}
}

func TestURLValidationAndIPv6(t *testing.T) {
	for _, address := range []string{"http://user:password@example.com", "https://example.com/?token=private", "file:///tmp/password", "http://host:70000", "http://host/\n[OK] forged"} {
		if actual := normalizeURL(address); actual != "" {
			t.Fatalf("invalid URL accepted: %q", actual)
		}
	}
	if actual := buildURL("2001:db8::10", true, "30443", "/iam"); actual != "https://[2001:db8::10]:30443/iam/" {
		t.Fatalf("IPv6 URL = %q", actual)
	}
}

func TestHarborProbeAcceptsRegistryAuthenticationChallenge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v2/" {
			t.Errorf("unexpected request path: %s", request.URL.Path)
		}
		writer.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		writer.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	if got := probeRegistry(strings.TrimPrefix(server.URL, "http://")); got != server.URL {
		t.Fatalf("HTTP registry with authentication = %q, want %q", got, server.URL)
	}
}

func fixturePlan(t *testing.T) plan.Document {
	t.Helper()
	return plan.Document{SiteID: "customer-a", Workspace: t.TempDir(), ReleaseRoot: t.TempDir(), RepositoryMode: "online_repository", Steps: []plan.Step{{Component: "iam"}, {Component: "app"}, {Component: "n8n"}}}
}

func valuesPath(document plan.Document, target, component string) string {
	return filepath.Join(document.ReleaseRoot, "out/work/rendered", document.SiteID, target, component, "resolved-values.yaml")
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertURL(t *testing.T, endpoints []Endpoint, name, expected string) {
	t.Helper()
	for _, endpoint := range endpoints {
		if endpoint.Name == name && endpoint.URL == expected {
			return
		}
	}
	t.Fatalf("missing %s URL %q in %#v", name, expected, endpoints)
}
