// Package access resolves operator-facing URLs from deployed values and live
// Kubernetes resources. It never renders credentials or raw command failures.
package access

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"eva-deployer/tools/eva/internal/health"
	"eva-deployer/tools/eva/internal/plan"
	"eva-deployer/tools/eva/internal/remote"
	"gopkg.in/yaml.v3"
)

type Endpoint struct {
	Name   string
	Target string
	URL    string
}

type Options struct {
	Run               health.Runner
	PreferLive        bool
	ReadFile          func(string) ([]byte, error)
	HarborReceiptPath string
	RegistryConfig    string
	ProbeRegistry     func(string) string
}

type target struct {
	name, identity string
	vars           struct {
		Host       string `yaml:"ansible_host"`
		Connection string `yaml:"ansible_connection"`
		AppDir     string `yaml:"eva_app_deploy_values_dir"`
		IAMDir     string `yaml:"eva_iam_deploy_values_dir"`
		N8NDir     string `yaml:"n8n_config_output_dir"`
		N8NNS      string `yaml:"n8n_namespace"`
	}
}

// Discover uses the resolved Helm values published by successful component
// deployments, including CLI overrides. Live resources provide a fallback for
// installations whose rendered output is no longer available on the controller.
func Discover(document plan.Document, options Options) []Endpoint {
	if options.ReadFile == nil {
		options.ReadFile = os.ReadFile
	}
	targets := deploymentTargets(document, options)
	var endpoints []Endpoint
	for _, component := range []string{"app", "iam", "n8n"} {
		for _, step := range document.Steps {
			if step.Component != component {
				continue
			}
			for _, target := range targets {
				if len(step.Targets) > 0 && !contains(step.Targets, target.name) {
					continue
				}
				address := ""
				canQuery := options.Run != nil && (target.vars.Connection == "local" || len(targets) == 1 && target.vars.Connection == "")
				if canQuery && options.PreferLive {
					address = liveURL(component, target.vars.N8NNS, options.Run)
				}
				if address == "" {
					address = savedURL(document, target, component, options)
				}
				if address == "" && canQuery && !options.PreferLive {
					address = liveURL(component, target.vars.N8NNS, options.Run)
				}
				name := map[string]string{"app": "App", "iam": "IAM", "n8n": "n8n"}[component]
				label := ""
				if len(targets) > 1 {
					label = target.name
				}
				endpoints = append(endpoints, Endpoint{Name: name, Target: label, URL: address})
			}
			break
		}
	}
	if mode := strings.TrimSuffix(strings.ToLower(document.RepositoryMode), "_repository"); mode == "remote" || mode == "local" {
		endpoints = append(endpoints, Endpoint{Name: "Harbor", URL: harborURL(document.Repository, options)})
	}
	return endpoints
}

func Print(writer io.Writer, endpoints []Endpoint) {
	if len(endpoints) == 0 {
		return
	}
	fmt.Fprintln(writer, "\nAccess URLs:")
	// Keep the display order stable regardless of deployment order.
	for _, name := range []string{"App", "IAM", "Harbor", "n8n"} {
		for _, endpoint := range endpoints {
			if endpoint.Name != name {
				continue
			}
			label := endpoint.Name
			if endpoint.Target != "" {
				label += " (" + endpoint.Target + ")"
			}
			address := endpoint.URL
			if address == "" {
				address = "unavailable (could not determine public URL)"
			}
			fmt.Fprintf(writer, "  %-12s %s\n", label+":", address)
		}
	}
}

func deploymentTargets(document plan.Document, options Options) []target {
	paths, _ := filepath.Glob(filepath.Join(document.Workspace, "inventory", "host_vars", "*.yaml"))
	var targets []target
	for _, path := range paths {
		t := target{name: strings.TrimSuffix(filepath.Base(path), ".yaml")}
		if readYAML(path, &t.vars, options) {
			t.identity = t.vars.Host
			if t.identity == "" {
				t.identity = t.name
			}
			targets = append(targets, t)
		}
	}
	if len(targets) == 0 {
		paths, _ = filepath.Glob(filepath.Join(document.ReleaseRoot, "out", "work", "rendered", document.SiteID, "*"))
		for _, path := range paths {
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				name := filepath.Base(path)
				targets = append(targets, target{name: name, identity: name})
			}
		}
	}
	if len(targets) == 0 {
		targets = append(targets, target{})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].name < targets[j].name })
	return targets
}

func savedURL(document plan.Document, target target, component string, options Options) string {
	if component == "n8n" {
		dir := target.vars.N8NDir
		if dir == "" {
			dir = filepath.Join(document.ReleaseRoot, "out", "work", "config", document.SiteID, target.identity)
		}
		var config struct {
			Endpoint string `yaml:"endpoint"`
		}
		if readYAML(filepath.Join(dir, "n8n.yaml"), &config, options) {
			return normalizeURL(config.Endpoint)
		}
		return ""
	}
	dir := target.vars.AppDir
	if component == "iam" {
		dir = target.vars.IAMDir
	}
	if dir == "" {
		dir = filepath.Join(document.ReleaseRoot, "out", "work", "rendered", document.SiteID, target.identity, component)
	}
	var values struct {
		App struct {
			Host       string `yaml:"backendHost"`
			Secure     bool   `yaml:"backendSecure"`
			PublicPort string `yaml:"backendPublicPort"`
			SecurePort string `yaml:"backendSecurePort"`
		} `yaml:"app"`
		Config struct {
			Host      string `yaml:"host"`
			PublicURL string `yaml:"publicUrl"`
			Port      string `yaml:"publicPort"`
		} `yaml:"config"`
		Ingress struct {
			Enabled *bool  `yaml:"enabled"`
			Path    string `yaml:"path"`
			TLS     struct {
				Enabled bool `yaml:"enabled"`
			} `yaml:"tls"`
		} `yaml:"ingress"`
	}
	if !readYAML(filepath.Join(dir, "resolved-values.yaml"), &values, options) || values.Ingress.Enabled != nil && !*values.Ingress.Enabled {
		return ""
	}
	if component == "iam" {
		if values.Config.PublicURL != "" {
			return normalizeURL(values.Config.PublicURL)
		}
		return buildURL(values.Config.Host, values.Ingress.TLS.Enabled, values.Config.Port, values.Ingress.Path)
	}
	port := values.App.PublicPort
	if values.App.Secure {
		port = values.App.SecurePort
	}
	return buildURL(values.App.Host, values.App.Secure, port, values.Ingress.Path)
}

func readYAML(path string, value any, options Options) bool {
	contents, err := options.ReadFile(path)
	return err == nil && yaml.Unmarshal(contents, value) == nil
}

type resourceList struct {
	Items []struct {
		Kind string            `json:"kind"`
		Data map[string]string `json:"data"`
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Env []struct{ Name, Value string } `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
			Rules []struct {
				HTTP struct {
					Paths []struct{ Path string } `json:"paths"`
				} `json:"http"`
			} `json:"rules"`
		} `json:"spec"`
	} `json:"items"`
}

func liveURL(component, n8nNamespace string, run health.Runner) string {
	namespace := "eva-" + component
	if component == "n8n" {
		namespace = n8nNamespace
		if namespace == "" {
			namespace = "n8n"
		}
	}
	result := run("kubectl", "get", "configmaps,deployments,ingresses", "-n", namespace, "-o", "json", "--request-timeout=5s")
	var resources resourceList
	if result.Err != nil || json.Unmarshal([]byte(result.Output), &resources) != nil {
		return ""
	}
	path := "/"
	for _, resource := range resources.Items {
		if resource.Kind == "Ingress" {
			for _, rule := range resource.Spec.Rules {
				if len(rule.HTTP.Paths) > 0 {
					path = rule.HTTP.Paths[0].Path
				}
			}
		}
	}
	var addresses []string
	for _, resource := range resources.Items {
		address := ""
		if component == "n8n" && resource.Kind == "Deployment" {
			for _, container := range resource.Spec.Template.Spec.Containers {
				for _, key := range []string{"N8N_EDITOR_BASE_URL", "WEBHOOK_URL"} {
					for _, env := range container.Env {
						if env.Name == key {
							address = normalizeURL(env.Value)
						}
					}
					if address != "" {
						break
					}
				}
			}
		} else if component == "app" && resource.Kind == "ConfigMap" && resource.Data["BACKEND_HOST"] != "" {
			secure := strings.EqualFold(resource.Data["BACKEND_SECURE"], "true")
			port := resource.Data["BACKEND_PUBLIC_PORT"]
			if secure {
				port = resource.Data["BACKEND_SECURE_PORT"]
			}
			address = buildURL(resource.Data["BACKEND_HOST"], secure, port, path)
		} else if component == "iam" && resource.Kind == "ConfigMap" {
			address = normalizeURL(resource.Data["KC_HOSTNAME_ADMIN_URL"])
			if address == "" {
				issuer := resource.Data["ISSUER"]
				if index := strings.LastIndex(issuer, "/realms/"); index >= 0 {
					address = normalizeURL(issuer[:index])
				}
			}
		}
		if address != "" && !contains(addresses, address) {
			addresses = append(addresses, address)
		}
	}
	if len(addresses) == 1 {
		return addresses[0]
	}
	return ""
}

func harborURL(registry string, options Options) string {
	if registry == "" {
		return ""
	}
	if strings.Contains(registry, "://") {
		return normalizeURL(registry)
	}
	receiptPath := options.HarborReceiptPath
	if receiptPath == "" {
		receiptPath = remote.DefaultHarborReceiptPath
	}
	var receipt struct{ Registry, Protocol string }
	if readYAML(receiptPath, &receipt, options) && receipt.Registry == registry && (receipt.Protocol == "http" || receipt.Protocol == "https") {
		return normalizeURL(receipt.Protocol + "://" + registry)
	}
	registryConfig := options.RegistryConfig
	if registryConfig == "" {
		registryConfig = "/etc/rancher/k3s/registries.yaml"
	}
	var mirrors struct {
		Mirrors map[string]struct{ Endpoint []string } `yaml:"mirrors"`
	}
	if readYAML(registryConfig, &mirrors, options) {
		for _, endpoint := range mirrors.Mirrors[registry].Endpoint {
			parsed, err := url.Parse(endpoint)
			if err == nil && parsed.Host == registry {
				if address := normalizeURL(parsed.Scheme + "://" + registry); address != "" {
					return address
				}
			}
		}
	}
	probe := options.ProbeRegistry
	if probe == nil {
		probe = probeRegistry
	}
	// Only probe a validated authority, never a URL carrying credentials.
	if normalizeURL("https://"+registry) == "" || strings.ContainsAny(registry, "/?#") {
		return ""
	}
	return normalizeURL(probe(registry))
}

func probeRegistry(registry string) string {
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, scheme := range []string{"https", "http"} {
		endpoint := scheme + "://" + registry
		response, err := client.Get(endpoint + "/v2/")
		if err != nil {
			continue
		}
		response.Body.Close()
		if (response.StatusCode == http.StatusOK || response.StatusCode == http.StatusUnauthorized) && response.Header.Get("Docker-Distribution-Api-Version") != "" {
			return endpoint
		}
	}
	return ""
}

func buildURL(host string, secure bool, port, path string) string {
	if host == "" || strings.ContainsAny(host, "/@?#") {
		return ""
	}
	scheme, defaultPort := "http", "80"
	if secure {
		scheme, defaultPort = "https", "443"
	}
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return ""
		}
		if port != defaultPort {
			host = net.JoinHostPort(strings.Trim(host, "[]"), port)
		}
	}
	if strings.Contains(host, ":") && net.ParseIP(host) != nil {
		host = "[" + host + "]"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return normalizeURL(scheme + "://" + host + path)
}

func normalizeURL(address string) string {
	if strings.ContainsAny(address, "\x00\x1b\r\n\t ") {
		return ""
	}
	parsed, err := url.Parse(address)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return ""
		}
		if parsed.Scheme == "http" && port == "80" || parsed.Scheme == "https" && port == "443" {
			parsed.Host = parsed.Hostname()
			if strings.Contains(parsed.Host, ":") {
				parsed.Host = "[" + parsed.Host + "]"
			}
		}
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/"
	parsed.RawPath = ""
	return parsed.String()
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}
