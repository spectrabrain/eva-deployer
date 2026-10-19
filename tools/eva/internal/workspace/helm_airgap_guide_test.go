package workspace

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Read the actual guide examples, not a second copy of their Values.
// With EVA_HELM_TEST_CHART_DIR unset, only the Markdown/YAML contract is checked.
// With it set, render the published archives offline; never connect to Kubernetes.
func TestHelmAirgapGuide(t *testing.T) {
	guide := filepath.Join("..", "..", "..", "..", "docs", "development", "app-iam-helm-airgap-test-runbook.md")
	contents, err := os.ReadFile(guide)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "<REGISTRY>/eva-test/") || strings.Contains(string(contents), "$REGISTRY/eva-test/") {
		t.Fatal("guide must reuse the existing eva Harbor project")
	}
	examples := map[string]string{}
	for _, block := range strings.Split(string(contents), "```yaml\n")[1:] {
		body, _, ok := strings.Cut(block, "```")
		if !ok {
			t.Fatal("unclosed YAML code fence")
		}
		header, _, _ := strings.Cut(body, "\n")
		if !strings.HasPrefix(header, "# values: ") {
			t.Fatal("guide YAML blocks must name their Values file")
		}
		name := strings.TrimPrefix(header, "# values: ")
		if _, exists := examples[name]; exists {
			t.Fatalf("duplicate Values block %s", name)
		}
		var values map[string]any
		if err := yaml.Unmarshal([]byte(body), &values); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		examples[name] = strings.ReplaceAll(body, "<REGISTRY>", "registry.example.internal:5000")
	}
	for _, name := range []string{"iam-test.yaml", "app-test.yaml", "iam-https.yaml", "app-https.yaml", "registry-auth.yaml", "iam-update.yaml", "app-update.yaml"} {
		if examples[name] == "" {
			t.Fatalf("guide is missing %s", name)
		}
	}
	if len(examples) != 7 {
		t.Fatalf("unexpected Values blocks: %d", len(examples))
	}
	chartDir := os.Getenv("EVA_HELM_TEST_CHART_DIR")
	if chartDir == "" {
		t.Log("set EVA_HELM_TEST_CHART_DIR to also render the published 3.3.0-beta.1 archives")
		return
	}
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"iam", "app", "iam-update", "app-update"} {
		component := strings.TrimSuffix(variant, "-update")
		update := strings.HasSuffix(variant, "-update")
		for _, https := range []bool{false, true} {
			for _, auth := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/https=%t/auth=%t", variant, https, auth), func(t *testing.T) {
					chart := filepath.Join(chartDir, "eva-"+component+"-3.3.0-beta.1.tgz")
					args := []string{"template", "eva-" + component, chart, "--namespace", "eva-" + component}
					files := []string{component + "-test.yaml"}
					if update {
						files = append(files, component+"-update.yaml")
					}
					if https {
						files = append(files, component+"-https.yaml")
					}
					if auth {
						files = append(files, "registry-auth.yaml")
					}
					for _, name := range files {
						path := filepath.Join(t.TempDir(), name)
						body := examples[name]
						if update && name == component+"-test.yaml" {
							// Synthetic old-image baseline, not a real site's backup.
							// The small update file must replace tags while preserving
							// DB/storage/SSO/auth and auxiliary image settings below.
							body = strings.ReplaceAll(body, "3.3.0-beta.1", "previous-render-fixture")
						}
						if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
							t.Fatal(err)
						}
						args = append(args, "--values", path)
					}
					command := exec.Command(helm, args...)
					var stderr bytes.Buffer
					command.Stderr = &stderr
					output, err := command.Output()
					if err != nil {
						t.Fatalf("Helm render: %v: %s", err, stderr.String())
					}
					decoder := yaml.NewDecoder(bytes.NewReader(output))
					var resources []map[string]any
					for {
						var resource map[string]any
						if err := decoder.Decode(&resource); err == io.EOF {
							break
						} else if err != nil {
							t.Fatal(err)
						}
						if len(resource) > 0 {
							resources = append(resources, resource)
						}
					}
					assertAirgapGuideResources(t, component, https, auth, resources)
				})
			}
		}
	}
}

func assertAirgapGuideResources(t *testing.T, component string, https, auth bool, resources []map[string]any) {
	t.Helper()
	root := "registry.example.internal:5000/eva/"
	wantImages := []string{"eva-app:3.3.0-beta.1", "mysql:8.0.42-bookworm"}
	wantPaths := []string{"data", "mysql", ".license/LicenseSpring"}
	if component == "iam" {
		wantImages = []string{"keycloak:3.3.0-beta.1", "dispatcher:3.3.0-beta.1", "postgres:16", "redis:7", "busybox:1.38.0", "keycloak-config-cli:6.1.6-24.0.5"}
		wantPaths = []string{"keycloak", "postgres", "redis", "realm-backups"}
	}
	images, paths, hooks := map[string]bool{}, map[string]bool{}, map[string]bool{}
	foundIngress, foundConfig := false, false
	for _, resource := range resources {
		kind, name := resource["kind"], fmt.Sprint(nestedSampleValue(resource, "metadata", "name"))
		if kind == "CronJob" || (kind == "Job" && !strings.HasSuffix(name, "-realm-config") && !strings.HasSuffix(name, "-realm-patch")) {
			t.Fatalf("unexpected offline Job/CronJob: %s %s", kind, name)
		}
		if kind == "Job" {
			hooks[name] = true
			assertSampleRenderedValue(t, nestedSampleValue(resource, "metadata", "annotations").(map[string]any), "helm.sh/hook", "post-install,post-upgrade")
		}
		if kind == "Deployment" || kind == "Job" {
			pod := nestedSampleValue(resource, "spec", "template", "spec").(map[string]any)
			if component == "iam" && kind == "Deployment" && !strings.Contains(name, "postgres") {
				wantRevision := "http-test-1"
				if https {
					wantRevision = "https-test-1"
				}
				if nestedSampleValue(resource, "spec", "template", "metadata", "annotations", "eva-test/config-revision") != wantRevision {
					t.Fatalf("%s must roll before hooks when IAM config changes", name)
				}
			}
			secrets, _ := pod["imagePullSecrets"].([]any)
			if (len(secrets) > 0) != auth || (auth && secrets[0].(map[string]any)["name"] != "airgap-regcred") {
				t.Fatalf("%s does not use the expected Registry authentication", name)
			}
			for _, key := range []string{"containers", "initContainers"} {
				containers, _ := pod[key].([]any)
				for _, container := range containers {
					c := container.(map[string]any)
					image := fmt.Sprint(c["image"])
					if !strings.HasPrefix(image, root) || c["imagePullPolicy"] == "Always" {
						t.Fatalf("unexpected image/pull policy in %s: %s", name, image)
					}
					images[strings.TrimPrefix(image, root)] = true
				}
			}
			volumes, _ := pod["volumes"].([]any)
			for _, volume := range volumes {
				if path, ok := nestedSampleValue(volume.(map[string]any), "hostPath", "path").(string); ok {
					paths[path] = true
				}
			}
		}
		if kind == "Ingress" {
			foundIngress = true
			rules := nestedSampleValue(resource, "spec", "rules").([]any)
			rule := rules[0].(map[string]any)
			if (rule["host"] != nil) != https || (https && rule["host"] != "eva-test.example.internal") {
				t.Fatal("HTTP IP must be hostless; HTTPS must use the test DNS")
			}
			wantPath := "/"
			if component == "iam" {
				wantPath = "/iam"
			}
			p := nestedSampleValue(rule, "http", "paths").([]any)
			if p[0].(map[string]any)["path"] != wantPath {
				t.Fatalf("unexpected Ingress path for %s", component)
			}
			tls, _ := nestedSampleValue(resource, "spec", "tls").([]any)
			if (len(tls) > 0) != https || (https && tls[0].(map[string]any)["secretName"] != "eva-"+component+"-secret-tls") {
				t.Fatal("Ingress TLS Secret differs from the guide")
			}
		}
		if kind == "ConfigMap" && component == "app" && nestedSampleValue(resource, "data", "SSO_BASE_URL") != nil {
			foundConfig = true
			baseURL := "http://192.0.2.10/iam"
			if https {
				baseURL = "https://eva-test.example.internal/iam"
			}
			data := resource["data"].(map[string]any)
			for key, value := range map[string]string{
				"SSO_BASE_URL": baseURL, "SSO_REALM": "eva-iam", "SSO_CLIENT_ID": "eva-app",
				"SSO_ADMIN_CLIENT_ID": "eva-admin-api", "SSO_ADMIN_CLIENT_SECRET": "<IAM_ADMIN_CLIENT_SECRET>",
				"SSO_REDIS_URL": "", "SSO_REDIS_PORT": "32079", "SSO_REDIS_DB": "0", "SSO_REDIS_PASSWORD": "<REDIS_PASSWORD>",
				"SSO_BROKER_MODE": "false", "SSO_BROKER_IDP_HINT": "", "LICENSE_ACTIVATION_MODE": "offline",
				"BACKEND_SECURE": fmt.Sprint(https),
			} {
				assertSampleRenderedValue(t, data, key, value)
			}
			if data["SSO_REDIS_TLS"] != nil {
				t.Fatal("guide leaves Redis TLS derivation to the App")
			}
		}
	}
	if !foundIngress || (component == "app" && !foundConfig) {
		t.Fatal("guide is missing Ingress/App config")
	}
	if len(images) != len(wantImages) {
		t.Fatalf("unexpected image set: %v", images)
	}
	for _, image := range wantImages {
		if !images[image] {
			t.Errorf("missing image %s", image)
		}
	}
	for _, path := range wantPaths {
		if !paths["/data/eva-helm-test/"+component+"/"+path] {
			t.Errorf("missing documented hostPath %s", path)
		}
	}
	if component == "iam" {
		if !hooks["eva-iam-realm-config"] || !hooks["eva-iam-realm-patch"] {
			t.Fatal("missing IAM realm hooks")
		}
		assertSampleRedisTLS(t, https, resources)
	}
}
