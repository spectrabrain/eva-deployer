package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func nestedSampleValue(values map[string]any, keys ...string) any {
	var current any = values
	for _, key := range keys {
		mapping, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = mapping[key]
	}
	return current
}

func assertPlaceholderExamples(t *testing.T, contents []byte) {
	t.Helper()
	var document yaml.Node
	if err := yaml.Unmarshal(contents, &document); err != nil {
		t.Fatal(err)
	}
	var visit func(*yaml.Node)
	visit = func(node *yaml.Node) {
		if node.Kind == yaml.ScalarNode && requiredPlaceholder.MatchString(node.Value) && !strings.Contains(node.LineComment, "ex)") {
			t.Errorf("placeholder at sample line %d has no inline example comment", node.Line)
		}
		for _, child := range node.Content {
			visit(child)
		}
	}
	visit(&document)
}

func fillSample(contents []byte) []byte {
	return []byte(strings.NewReplacer(
		"<SITE_ID>", "customer-a", "<TARGET_USER>", "eva", "<SUDO_PASSWORD>", "private-sudo-password",
		"<HARBOR_HOST>", "harbor.example.com",
		"<PUBLIC_HOST>", "192.0.2.10", "<APP_HOST>", "app.example.com", "<IAM_HOST>", "iam.example.com",
		"<TLS_CERT_DIRECTORY>", "/etc/eva/certs", "<APP_TLS_CERT_DIRECTORY>", "/etc/eva/app-certs", "<IAM_TLS_CERT_DIRECTORY>", "/etc/eva/iam-certs",
		"<AWS_ACCESS_KEY_ID>", "private-access", "<AWS_SECRET_ACCESS_KEY>", "private-secret",
		"<REALM_ADMIN_PASSWORD>", "private-realm-password", "<LICENSE_PRODUCT_CODE>", "eva-dev",
		"<LICENSE_API_KEY>", "private-license", "<LICENSE_SHARED_KEY>", "private-shared",
		"<EXTERNAL_IAM_BASE_URL>", "https://iam.external.example.com/iam",
		"<SSO_REALM>", "eva-iam", "<SSO_CLIENT_ID>", "eva-app", "<SSO_ADMIN_CLIENT_ID>", "eva-admin-api",
		"<SSO_ADMIN_CLIENT_SECRET>", "private-external-secret",
	).Replace(string(contents)))
}

func TestAllInstallerSampleCombinations(t *testing.T) {
	for _, mode := range []string{"cloud", "remote", "local"} {
		for _, address := range []string{"shared", "separate"} {
			for _, protocol := range []string{"https", "http"} {
				t.Run(mode+"/"+address+"/"+protocol, func(t *testing.T) {
					contents, err := InstallerSampleFor(SampleOptions{Mode: mode, Address: address, Protocol: protocol})
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(contents), "필수") || !strings.Contains(string(contents), "<LICENSE_API_KEY>") || !strings.Contains(string(contents), "<SITE_ID>") {
						t.Fatal("sample lost required-input comments or markers")
					}
					assertPlaceholderExamples(t, contents)
					var input Installer
					if err := yaml.Unmarshal(fillSample(contents), &input); err != nil {
						t.Fatal(err)
					}
					if input.Repository.Mode != mode || (input.Credentials.AWS != nil) != (mode == "cloud") {
						t.Fatal("sample repository/credential mismatch")
					}
					if mode != "cloud" && (strings.Contains(string(contents), "imagePullSecrets") || strings.Contains(string(contents), "aws_access_key_id")) {
						t.Fatal("offline sample contains AWS settings")
					}
					values := input.Targets["server-a"].Values
					assertSampleLicense(t, contents, values["app"], mode)
					assertSampleSSODefaults(t, values["app"])
					if nestedSampleValue(values["iam"], "keycloak", "realmConfig", "substitutions", "EVA_IAM_ADMIN_EMAIL") != "admin@eva.com" {
						t.Fatal("sample lost base IAM admin email while merging endpoint profile")
					}
					if strings.Contains(string(contents), "imagePullSecrets") {
						t.Fatal("samples must leave image pull credential settings to repository and target-user defaults")
					}
					iamHost := nestedSampleValue(values["iam"], "config", "host")
					appHost := nestedSampleValue(values["app"], "app", "backendHost")
					wantPath := "/"
					if address == "shared" {
						wantPath = "/iam"
						if iamHost != appHost {
							t.Fatal("shared template has different hosts")
						}
					} else if iamHost == appHost {
						t.Fatal("separate template has identical hosts")
					}
					if nestedSampleValue(values["iam"], "ingress", "path") != wantPath || nestedSampleValue(values["app"], "ingress", "path") != "/" {
						t.Fatal("sample ingress paths mismatch")
					}
					uris := nestedSampleValue(values["iam"], "keycloak", "realmPatch", "evaApp", "redirectUris").([]any)
					if len(uris) != 1 || uris[0] != protocol+"://"+appHost.(string)+"/*" {
						t.Fatal("IAM redirect URI does not match App external endpoint")
					}
					if nestedSampleValue(values["app"], "app", "backendSecure") != (protocol == "https") {
						t.Fatal("App backend scheme mismatch")
					}
					for _, component := range []string{"iam", "app"} {
						if nestedSampleValue(values[component], "ingress", "tls", "enabled") != (protocol == "https") ||
							nestedSampleValue(values[component], "ingress", "tls", "create") != (protocol == "https") {
							t.Fatalf("%s TLS settings do not match protocol", component)
						}
						certDirectory := nestedSampleValue(values[component], "ingress", "tls", "hostPath")
						if (certDirectory != nil) != (protocol == "https") {
							t.Fatalf("%s certificate input does not match protocol", component)
						}
					}
					if nestedSampleValue(values["iam"], "config", "publicPort") != "" ||
						!strings.Contains(string(contents), "기본 HTTP 80 / HTTPS 443") ||
						!strings.Contains(string(contents), "노출 포트를 변경하지 않습니다") {
						t.Fatal("IAM must provide an empty publicPort with default and exposure guidance")
					}
					if nestedSampleValue(values["iam"], "config", "publicUrl") != nil ||
						nestedSampleValue(values["iam"], "keycloak", "config", "hostnameStrictHttps") != nil ||
						nestedSampleValue(values["iam"], "keycloak", "config", "realmSslRequired") != nil {
						t.Fatal("IAM must derive URL and realm SSL mode from TLS and path")
					}
					if nestedSampleValue(values["iam"], "redis", "tls", "enabled") != (protocol == "https") {
						t.Fatal("IAM Redis TLS must follow HTTP/HTTPS profile")
					}
					if nestedSampleValue(values["iam"], "redis", "external", "enabled") != true {
						t.Fatal("IAM Redis external access must always be enabled in samples")
					}
					if nestedSampleValue(values["app"], "app", "sso", "redis", "tls") != nil {
						t.Fatal("App Redis TLS must keep automatic derivation from SSO URL")
					}
					portKey, wantPort := "backendSecurePort", 443
					if protocol == "http" {
						portKey, wantPort = "backendPublicPort", 80
						if nestedSampleValue(values["iam"], "ingress", "annotations", "traefik.ingress.kubernetes.io/router.entrypoints") != "web" {
							t.Fatal("HTTP IAM must use Traefik web entrypoint")
						}
					}
					if nestedSampleValue(values["app"], "app", portKey) != wantPort {
						t.Fatal("App public port mismatch")
					}
					path := writeInstaller(t, string(fillSample(contents)))
					resolved, err := Resolve(Options{ConfigFile: path, CacheRoot: filepath.Join(t.TempDir(), "cache")})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := os.Stat(filepath.Join(resolved.Root, "site-values/app.yaml")); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}

func TestExternalIAMInstallerSamples(t *testing.T) {
	for _, mode := range []string{"cloud", "remote", "local"} {
		for _, protocol := range []string{"http", "https"} {
			t.Run(mode+"/"+protocol, func(t *testing.T) {
				contents, err := InstallerSampleFor(SampleOptions{Mode: mode, Protocol: protocol, IAM: "external"})
				if err != nil {
					t.Fatal(err)
				}
				assertPlaceholderExamples(t, contents)
				for _, unexpected := range []string{"<IAM_HOST>", "<PUBLIC_HOST>", "<REALM_ADMIN_PASSWORD>", "<IAM_TLS_CERT_DIRECTORY>", "EVA_IAM_ADMIN_EMAIL", "SSO handoff가", "imagePullSecrets"} {
					if strings.Contains(string(contents), unexpected) {
						t.Fatalf("external IAM sample contains co-install setting %s", unexpected)
					}
				}
				resolved, err := Resolve(Options{ConfigFile: writeInstaller(t, string(fillSample(contents))), CacheRoot: filepath.Join(t.TempDir(), "cache")})
				if err != nil {
					t.Fatal(err)
				}
				if resolved.Config.Components["iam"] || !resolved.Config.Components["app"] {
					t.Fatal("external IAM sample must install App without IAM")
				}
				if _, err := os.Stat(filepath.Join(resolved.Root, "site-values/iam.yaml")); !os.IsNotExist(err) {
					t.Fatal("external IAM sample materialized local IAM values")
				}
				input, err := LoadInstaller(writeInstaller(t, string(fillSample(contents))), "")
				if err != nil {
					t.Fatal(err)
				}
				if _, exists := input.Targets["server-a"].Values["iam"]; exists {
					t.Fatal("external IAM must not retain base IAM values")
				}
				app := input.Targets["server-a"].Values["app"]
				assertSampleLicense(t, contents, app, mode)
				assertSampleSSODefaults(t, app)
				for _, key := range []string{"baseUrl", "realm", "clientId", "adminClientId", "adminClientSecret"} {
					if value, ok := nestedSampleValue(app, "app", "sso", key).(string); !ok || value == "" {
						t.Fatalf("external IAM missing explicit sso.%s", key)
					}
				}
				if nestedSampleValue(app, "ingress", "tls", "enabled") != (protocol == "https") || nestedSampleValue(app, "app", "backendSecure") != (protocol == "https") {
					t.Fatal("external IAM sample lost App HTTP/HTTPS selection")
				}
				var materialized map[string]map[string]any
				appContents, err := os.ReadFile(filepath.Join(resolved.Root, "site-values/app.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				if err := yaml.Unmarshal(appContents, &materialized); err != nil {
					t.Fatal(err)
				}
				if nestedSampleValue(materialized["server-a"], "app", "sso", "adminClientSecret") != "private-external-secret" {
					t.Fatal("external SSO values lost while materializing workspace")
				}
			})
		}
	}
}

func assertSampleLicense(t *testing.T, contents []byte, app map[string]any, mode string) {
	t.Helper()
	want := "offline"
	if mode == "cloud" {
		want = "online"
	}
	if nestedSampleValue(app, "app", "license", "activation_mode") != want {
		t.Fatalf("%s sample license default does not match repository mode", mode)
	}
	if !strings.Contains(string(contents), "사전 결정한 라이선스") || !strings.Contains(string(contents), "online은 라이선스 서버 연결이 필요") {
		t.Fatal("license defaults must retain guidance for prearranged online/offline activation")
	}
	if !strings.Contains(string(contents), "eva-dev 또는 eva-prod") {
		t.Fatal("license product examples must use actual product codes")
	}
}

func TestInstallerSampleLicenseOverrides(t *testing.T) {
	for _, mode := range []string{"cloud", "remote", "local"} {
		for _, activation := range []string{"online", "offline"} {
			for _, iam := range []string{"install", "external"} {
				t.Run(mode+"/"+activation+"/"+iam, func(t *testing.T) {
					contents, err := InstallerSampleFor(SampleOptions{Mode: mode, Address: "separate", Protocol: "http", IAM: iam})
					if err != nil {
						t.Fatal(err)
					}
					var document yaml.Node
					if err := yaml.Unmarshal(contents, &document); err != nil {
						t.Fatal(err)
					}
					target := sampleMappingValue(sampleMappingValue(document.Content[0], "targets"), "server-a")
					app := sampleMappingValue(sampleMappingValue(sampleMappingValue(target, "values"), "app"), "app")
					sampleMappingValue(sampleMappingValue(app, "license"), "activation_mode").Value = activation
					contents, err = yaml.Marshal(&document)
					if err != nil {
						t.Fatal(err)
					}
					input, err := LoadInstaller(writeInstaller(t, string(fillSample(contents))), "")
					if err != nil {
						t.Fatal(err)
					}
					if nestedSampleValue(input.Targets["server-a"].Values["app"], "app", "license", "activation_mode") != activation {
						t.Fatal("repository mode overrode the explicit license selection")
					}
				})
			}
		}
	}
}

func assertSampleSSODefaults(t *testing.T, app map[string]any) {
	t.Helper()
	if nestedSampleValue(app, "app", "sso", "brokerMode") != false || nestedSampleValue(app, "app", "sso", "brokerIdpHint") != "" {
		t.Fatal("samples must provide broker defaults")
	}
	for key, want := range map[string]any{"port": 32079, "db": 0, "password": "eva-redis-pass", "url": ""} {
		if nestedSampleValue(app, "app", "sso", "redis", key) != want {
			t.Fatalf("unexpected Redis default for %s", key)
		}
	}
}

func TestExternalIAMSampleRejectsUnfilledConnectionInputs(t *testing.T) {
	contents, err := InstallerSampleFor(SampleOptions{Mode: "local", Protocol: "https", IAM: "external"})
	if err != nil {
		t.Fatal(err)
	}
	contents = []byte(strings.NewReplacer("private-external-secret", "<SSO_ADMIN_CLIENT_SECRET>", "eva-redis-pass", "private-redis-password").Replace(string(fillSample(contents))))
	_, err = LoadInstaller(writeInstaller(t, string(contents)), "")
	if err == nil || !strings.Contains(err.Error(), "targets.server-a.values.app.app.sso.adminClientSecret") || strings.Contains(err.Error(), "private-redis-password") {
		t.Fatalf("missing external client secret must report only its field path: %v", err)
	}
}

func TestCloudInstallerSampleUsesAutomaticCredentialPath(t *testing.T) {
	contents, err := InstallerSample("cloud")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "imagePullSecrets") {
		t.Fatal("cloud sample must not require a credential path override")
	}
	if _, err := LoadInstaller(writeInstaller(t, string(fillSample(contents))), ""); err != nil {
		t.Fatalf("cloud sample must allow automatic credential path: %v", err)
	}
}

func TestEmbeddedSampleFormattingAndPlaceholderExamples(t *testing.T) {
	paths := []string{"installer.yaml.sample"}
	entries, err := installerSamples.ReadDir("samples")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		paths = append(paths, "samples/"+entry.Name())
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			contents, err := installerSamples.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(contents) == 0 || contents[len(contents)-1] != '\n' {
				t.Fatal("embedded YAML sample must end with a newline for CI yamllint")
			}
			assertPlaceholderExamples(t, contents)
		})
	}
}

func TestInstallerSampleRejectsUnfilledRequiredInputsWithoutLeakingSecrets(t *testing.T) {
	contents, err := InstallerSample("cloud")
	if err != nil {
		t.Fatal(err)
	}
	contents = []byte(strings.Replace(string(contents), "<AWS_SECRET_ACCESS_KEY>", "do-not-disclose-credential", 1))
	cache := filepath.Join(t.TempDir(), "cache")
	_, err = Resolve(Options{ConfigFile: writeInstaller(t, string(contents)), CacheRoot: cache})
	if err == nil || !strings.Contains(err.Error(), "site.id") || !strings.Contains(err.Error(), "targets.server-a.values.app.app.license.api_key") {
		t.Fatalf("required fields were not identified: %v", err)
	}
	if strings.Contains(err.Error(), "do-not-disclose-credential") || strings.Contains(err.Error(), "<LICENSE_API_KEY>") {
		t.Fatal("validation printed a field value instead of its path")
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatal("unfilled sample created a workspace")
	}
}

func TestInstallerSampleRejectsInvalidChoices(t *testing.T) {
	for _, options := range []SampleOptions{
		{Mode: "../cloud", Address: "shared", Protocol: "https"},
		{Mode: "cloud", Address: "../shared", Protocol: "https"},
		{Mode: "cloud", Address: "shared", Protocol: "ftp"},
		{Mode: "cloud", Address: "separate", Protocol: "https", IAM: "invalid"},
		{Mode: "cloud", Address: "shared", Protocol: "https", IAM: "external"},
	} {
		if _, err := InstallerSampleFor(options); err == nil {
			t.Fatal("invalid sample choice accepted")
		}
	}
}
