package workspace

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Opt-in contract check against unreleased local sources, never a deployment.
// Both chart directories must be supplied; ordinary Go tests stay self-contained.
func TestInstallerSamplesAgainstLocalCharts(t *testing.T) {
	charts := map[string]string{
		"iam": os.Getenv("EVA_IAM_CHART_DIR"),
		"app": os.Getenv("EVA_APP_CHART_DIR"),
	}
	if charts["iam"] == "" && charts["app"] == "" {
		t.Skip("set EVA_IAM_CHART_DIR and EVA_APP_CHART_DIR to verify local Chart contracts")
	}
	for component, directory := range charts {
		if directory == "" {
			t.Fatalf("%s Chart directory must also be supplied", component)
		}
	}
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"cloud", "remote", "local"} {
		for _, address := range []string{"shared", "separate"} {
			for _, protocol := range []string{"http", "https"} {
				hostKinds := []string{"dns"}
				if address == "shared" {
					hostKinds = append(hostKinds, "ip")
				}
				for _, hostKind := range hostKinds {
					t.Run(mode+"/"+address+"/"+protocol+"/"+hostKind, func(t *testing.T) {
						contents, err := InstallerSampleFor(SampleOptions{Mode: mode, Address: address, Protocol: protocol})
						if err != nil {
							t.Fatal(err)
						}
						contents = fillSample(contents)
						if address == "shared" && hostKind == "dns" {
							contents = bytes.ReplaceAll(contents, []byte("192.0.2.10"), []byte("eva.example.com"))
						}
						var input Installer
						if err := yaml.Unmarshal(contents, &input); err != nil {
							t.Fatal(err)
						}
						for _, component := range []string{"iam", "app"} {
							values := input.Targets["server-a"].Values[component]
							resources := renderSampleChart(t, helm, charts[component], component, values)
							assertSampleChartProtocol(t, component, protocol, hostKind, values, resources)
							if component == "iam" {
								assertSampleRedisTLS(t, protocol == "https", resources)
							}
						}
					})
				}
			}
		}
	}
	assertExternalIAMSampleCharts(t, helm, charts["app"])
}

func assertExternalIAMSampleCharts(t *testing.T, helm, chart string) {
	t.Helper()
	for _, mode := range []string{"cloud", "remote", "local"} {
		for _, appProtocol := range []string{"http", "https"} {
			for _, iamProtocol := range []string{"http", "https"} {
				for _, redisScheme := range []string{"derived", "redis", "rediss"} {
					t.Run(mode+"/external-iam/app-"+appProtocol+"/iam-"+iamProtocol+"/"+redisScheme, func(t *testing.T) {
						contents, err := InstallerSampleFor(SampleOptions{Mode: mode, Protocol: appProtocol, IAM: "external"})
						if err != nil {
							t.Fatal(err)
						}
						contents = fillSample(contents)
						baseURL := iamProtocol + "://iam.external.example.com/iam"
						contents = bytes.ReplaceAll(contents, []byte("https://iam.external.example.com/iam"), []byte(baseURL))
						var input Installer
						if err := yaml.Unmarshal(contents, &input); err != nil {
							t.Fatal(err)
						}
						values := input.Targets["server-a"].Values["app"]
						broker := redisScheme == "rediss"
						hint := ""
						if broker {
							sso := nestedSampleValue(values, "app", "sso").(map[string]any)
							hint = "company-sso"
							sso["brokerMode"], sso["brokerIdpHint"] = true, hint
						}
						redisURL := ""
						if redisScheme != "derived" {
							redisURL = redisScheme + "://:private-redis-password@redis.external.example.com:32079/0"
							nestedSampleValue(values, "app", "sso", "redis").(map[string]any)["url"] = redisURL
						}
						resources := renderSampleChart(t, helm, chart, "app", values)
						assertSampleChartProtocol(t, "app", appProtocol, "dns", values, resources)
						for _, resource := range resources {
							if resource["kind"] != "ConfigMap" || nestedSampleValue(resource, "data", "BACKEND_SECURE") == nil {
								continue
							}
							data := resource["data"].(map[string]any)
							for key, want := range map[string]string{
								"SSO_BASE_URL": baseURL, "SSO_REALM": "eva-iam", "SSO_CLIENT_ID": "eva-app",
								"SSO_ADMIN_CLIENT_ID": "eva-admin-api", "SSO_ADMIN_CLIENT_SECRET": "private-external-secret",
								"SSO_REDIS_URL":  redisURL,
								"SSO_REDIS_PORT": "32079", "SSO_REDIS_DB": "0", "SSO_REDIS_PASSWORD": "eva-redis-pass",
								"SSO_BROKER_MODE": strconv.FormatBool(broker), "SSO_BROKER_IDP_HINT": hint,
							} {
								assertSampleRenderedValue(t, data, key, want)
							}
						}
					})
				}
			}
		}
	}
}

func renderSampleChart(t *testing.T, helm, chart, component string, values map[string]any) []map[string]any {
	t.Helper()
	contents, err := yaml.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(helm, "template", "sample-"+component, chart, "--namespace", "eva-"+component, "--values", path)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("render %s local Chart: %v: %s", component, err, stderr.String())
	}
	decoder := yaml.NewDecoder(bytes.NewReader(output))
	var resources []map[string]any
	for {
		var resource map[string]any
		err := decoder.Decode(&resource)
		if err == io.EOF {
			return resources
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(resource) > 0 {
			resources = append(resources, resource)
		}
	}
}

func assertSampleChartProtocol(t *testing.T, component, protocol, hostKind string, values map[string]any, resources []map[string]any) {
	t.Helper()
	tlsEnabled := protocol == "https"
	host := nestedSampleValue(values, "config", "host")
	if component == "app" {
		host = nestedSampleValue(values, "app", "backendHost")
	}
	path := nestedSampleValue(values, "ingress", "path").(string)
	baseURL := protocol + "://" + host.(string) + strings.TrimSuffix(path, "/")
	foundIngress, foundConfig, foundTLSJob, foundRealmPatch := false, false, false, false
	foundAdminEmail := false
	for _, resource := range resources {
		switch resource["kind"] {
		case "Secret":
			if component == "iam" && strings.HasSuffix(fmt.Sprint(nestedSampleValue(resource, "metadata", "name")), "-realm-config-env") {
				foundAdminEmail = true
				assertSampleRenderedValue(t, resource["stringData"].(map[string]any), "EVA_IAM_ADMIN_EMAIL", "admin@eva.com")
			}
		case "Ingress":
			foundIngress = true
			if (nestedSampleValue(resource, "spec", "tls") != nil) != tlsEnabled {
				t.Fatalf("%s Ingress TLS does not match %s", component, protocol)
			}
			rules := nestedSampleValue(resource, "spec", "rules").([]any)
			rule := rules[0].(map[string]any)
			if (rule["host"] == nil) != (hostKind == "ip") || (hostKind != "ip" && rule["host"] != host) {
				t.Fatalf("%s Ingress host does not match sample", component)
			}
			paths := nestedSampleValue(rule, "http", "paths").([]any)
			if paths[0].(map[string]any)["path"] != path {
				t.Fatalf("%s Ingress path does not match sample", component)
			}
			if component == "iam" && !tlsEnabled && nestedSampleValue(resource, "metadata", "annotations", "traefik.ingress.kubernetes.io/router.entrypoints") != "web" {
				t.Fatal("HTTP IAM Ingress lost web entrypoint")
			}
		case "ConfigMap":
			data, _ := resource["data"].(map[string]any)
			if component == "app" && data["BACKEND_SECURE"] != nil {
				foundConfig = true
				assertSampleRenderedValue(t, data, "BACKEND_SECURE", strconv.FormatBool(tlsEnabled))
				assertSampleRenderedValue(t, data, "BACKEND_PUBLIC_PORT", "80")
				assertSampleRenderedValue(t, data, "BACKEND_SECURE_PORT", "443")
			}
			if component == "iam" && data["KC_HOSTNAME_STRICT_HTTPS"] != nil {
				foundConfig = true
				assertSampleRenderedValue(t, data, "KC_HOSTNAME_STRICT_HTTPS", strconv.FormatBool(tlsEnabled))
				assertSampleRenderedValue(t, data, "KC_HTTP_RELATIVE_PATH", path)
				assertSampleRenderedValue(t, data, "KC_HOSTNAME_ADMIN_URL", baseURL)
				assertSampleRenderedValue(t, data, "ISSUER", baseURL+"/realms/eva-iam")
				assertSampleRenderedValue(t, data, "JWKS_URL", baseURL+"/realms/eva-iam/protocol/openid-connect/certs")
			}
		case "Job":
			name := fmt.Sprint(nestedSampleValue(resource, "metadata", "name"))
			if strings.HasSuffix(name, "-tls-job") {
				foundTLSJob = true
			}
			if component == "iam" && strings.HasSuffix(name, "-realm-patch") {
				foundRealmPatch = true
				containers := nestedSampleValue(resource, "spec", "template", "spec", "containers").([]any)
				env := containers[0].(map[string]any)["env"].([]any)
				environment := make(map[string]any)
				for _, entry := range env {
					item := entry.(map[string]any)
					environment[item["name"].(string)] = item["value"]
				}
				sslRequired := "none"
				if tlsEnabled {
					sslRequired = "external"
				}
				assertSampleRenderedValue(t, environment, "SSL_REQUIRED", sslRequired)
				assertSampleRenderedValue(t, environment, "CLEAR_STRICT_TRANSPORT_SECURITY", strconv.FormatBool(!tlsEnabled))
			}
		}
	}
	if !foundIngress || !foundConfig || foundTLSJob != tlsEnabled || (component == "iam" && !foundRealmPatch) {
		t.Fatalf("%s resource contract missing: ingress=%v config=%v tlsJob=%v realmPatch=%v", component, foundIngress, foundConfig, foundTLSJob, foundRealmPatch)
	}
	if component == "iam" && !foundAdminEmail {
		t.Fatal("IAM realm-config substitution Secret must contain base admin email")
	}
}

func assertSampleRedisTLS(t *testing.T, enabled bool, resources []map[string]any) {
	t.Helper()
	foundConfig, foundDeployment := false, false
	foundExternalService := false
	var redisSecret, ingressSecret any
	for _, resource := range resources {
		switch resource["kind"] {
		case "Service":
			if !strings.HasSuffix(fmt.Sprint(nestedSampleValue(resource, "metadata", "name")), "-redis-external") {
				continue
			}
			foundExternalService = true
			ports := nestedSampleValue(resource, "spec", "ports").([]any)
			port := ports[0].(map[string]any)
			wantPort, wantTarget := 6379, "redis"
			if enabled {
				wantPort, wantTarget = 6380, "redis-tls"
			}
			if nestedSampleValue(resource, "spec", "type") != "NodePort" || port["nodePort"] != 32079 ||
				port["port"] != wantPort || port["targetPort"] != wantTarget {
				t.Fatal("Redis external service must expose the listener selected by HTTP/HTTPS profile")
			}
		case "Ingress":
			if enabled {
				tls := nestedSampleValue(resource, "spec", "tls").([]any)
				ingressSecret = tls[0].(map[string]any)["secretName"]
			}
		case "ConfigMap":
			config, ok := nestedSampleValue(resource, "data", "redis.conf").(string)
			if !ok {
				continue
			}
			foundConfig = true
			if strings.Contains(config, "tls-port ") != enabled ||
				(enabled && (!strings.Contains(config, "tls-port 6380") || !strings.Contains(config, "tls-cert-file /tls/tls.crt") || !strings.Contains(config, "tls-key-file /tls/tls.key"))) {
				t.Fatal("Redis listener configuration does not match HTTP/HTTPS profile")
			}
		case "Deployment":
			containers := nestedSampleValue(resource, "spec", "template", "spec", "containers").([]any)
			for _, entry := range containers {
				container := entry.(map[string]any)
				if container["name"] != "redis" {
					continue
				}
				foundDeployment = true
				foundPort, foundMount := false, false
				for _, entry := range container["ports"].([]any) {
					port := entry.(map[string]any)
					if port["name"] == "redis-tls" {
						foundPort = true
						if port["containerPort"] != 6380 {
							t.Fatal("Redis TLS container port must match Chart default")
						}
					}
				}
				for _, entry := range container["volumeMounts"].([]any) {
					if entry.(map[string]any)["mountPath"] == "/tls" {
						foundMount = true
					}
				}
				if foundPort != enabled || foundMount != enabled {
					t.Fatal("Redis TLS listener and certificate mount must follow profile")
				}
				volumes := nestedSampleValue(resource, "spec", "template", "spec", "volumes").([]any)
				for _, entry := range volumes {
					volume := entry.(map[string]any)
					if volume["name"] == "tls" {
						redisSecret = nestedSampleValue(volume, "secret", "secretName")
					}
				}
			}
		}
	}
	if !foundConfig || !foundDeployment || !foundExternalService || (redisSecret != nil) != enabled || (enabled && (ingressSecret == nil || redisSecret != ingressSecret)) {
		t.Fatal("Redis resources must expose external service, follow TLS profile and reuse IAM Ingress certificate")
	}
}

func assertSampleRenderedValue(t *testing.T, values map[string]any, key, want string) {
	t.Helper()
	if values[key] != want {
		t.Errorf("rendered %s = %v, want %s", key, values[key], want)
	}
}
