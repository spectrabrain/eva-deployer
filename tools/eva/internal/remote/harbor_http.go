package remote

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"eva-deployer/tools/eva/internal/runtime"
	"gopkg.in/yaml.v3"
)

type harborHTTPCommand func(context.Context, string, ...string) ([]byte, error)

type storedHarborRelease struct {
	Name, Namespace string
	Manifest        string
	Version         int
	Info            struct{ Status string }
	Config          map[string]any
	Chart           struct {
		Metadata struct {
			Name, Version string
			Dependencies  []any
		}
		Values           map[string]any
		Schema           []byte
		Files, Templates []struct {
			Name string
			Data []byte
		}
	}
}

func ensureExistingHarborHTTP(ctx context.Context, registry string) error {
	if err := ensureManagedRuntime(ctx); err != nil {
		return err
	}
	resolved, err := runtime.Resolve(runtime.DefaultRoot)
	if err != nil {
		return err
	}
	kubectl, err := resolved.ToolPath("kubectl")
	if err != nil {
		return err
	}
	helm, err := resolved.ToolPath("helm")
	if err != nil {
		return err
	}
	run := func(ctx context.Context, tool string, args ...string) ([]byte, error) {
		path := kubectl
		if tool == "helm" {
			path = helm
		}
		command := exec.CommandContext(ctx, path, args...)
		command.Env = append(os.Environ(), "KUBECONFIG=/etc/rancher/k3s/k3s.yaml")
		// Chart values, manifests and subprocess errors may contain credentials.
		return command.Output()
	}
	return convertK3sHarborHTTP(ctx, registry, run, mainRegistryHostIsLocal,
		"/var/lib/eva/preparation/main/harbor-backups", func(ctx context.Context) error {
			deadline, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			for {
				present, err := probeExistingHarbor(deadline, registry, "http")
				if present && err == nil {
					return nil
				}
				select {
				case <-deadline.Done():
					return errors.New("Harbor HTTP endpoint did not become healthy")
				case <-time.After(time.Second):
				}
			}
		})
}

func mainRegistryHostIsLocal(ctx context.Context, host string) bool {
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return false
	}
	interfaces, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, address := range addresses {
		for _, iface := range interfaces {
			ip, _, _ := net.ParseCIDR(iface.String())
			if address.IP.Equal(ip) {
				return true
			}
		}
	}
	return false
}

// Reuse the exact stored chart and user values, rather than downloading a
// possibly changed chart with the same version or installing another Harbor.
func convertK3sHarborHTTP(ctx context.Context, registry string, run harborHTTPCommand, local func(context.Context, string) bool, backupRoot string, verify func(context.Context) error) error {
	host, portText, err := net.SplitHostPort(registry)
	port, portErr := strconv.Atoi(portText)
	if err != nil || portErr != nil || !local(ctx, host) {
		return errors.New("automatic Harbor HTTP conversion requires a local Main k3s NodePort endpoint")
	}
	contents, err := run(ctx, "kubectl", "get", "services", "-A", "-o", "json")
	var services struct {
		Items []struct {
			Metadata struct {
				Namespace           string
				Annotations, Labels map[string]string
			}
			Spec struct {
				Type  string
				Ports []struct{ NodePort int }
			}
		}
	}
	if err != nil || json.Unmarshal(contents, &services) != nil {
		return errors.New("cannot inspect Main k3s Harbor services")
	}
	name, namespace := "", ""
	for _, service := range services.Items {
		for _, candidate := range service.Spec.Ports {
			if service.Spec.Type != "NodePort" || candidate.NodePort != port {
				continue
			}
			annotations, labels := service.Metadata.Annotations, service.Metadata.Labels
			if name != "" || labels["app"] != "harbor" && labels["app.kubernetes.io/name"] != "harbor" {
				return errors.New("Main registry service could not be identified as a unique Helm Harbor deployment")
			}
			name, namespace = annotations["meta.helm.sh/release-name"], annotations["meta.helm.sh/release-namespace"]
			if !regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,52}$`).MatchString(name) || !regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`).MatchString(namespace) || namespace != service.Metadata.Namespace {
				return errors.New("Main Harbor service has no matching Helm release ownership")
			}
		}
	}
	if name == "" {
		return errors.New("Main HTTPS Harbor is not a recognized k3s Helm NodePort deployment; existing Harbor was preserved")
	}
	contents, err = run(ctx, "kubectl", "-n", namespace, "get", "secrets", "-l", "owner=helm,name="+name+",status=deployed", "-o", "json")
	var secrets struct {
		Items []struct{ Data map[string]string }
	}
	if err != nil || json.Unmarshal(contents, &secrets) != nil || len(secrets.Items) != 1 {
		return errors.New("cannot read the single deployed Harbor Helm revision")
	}
	encoded, err := base64.StdEncoding.DecodeString(secrets.Items[0].Data["release"])
	if err != nil {
		return errors.New("stored Harbor Helm release is invalid")
	}
	compressed, err := base64.StdEncoding.DecodeString(string(encoded))
	if err != nil {
		return errors.New("stored Harbor Helm release is invalid")
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return errors.New("stored Harbor Helm release is invalid")
	}
	releaseJSON, err := io.ReadAll(io.LimitReader(reader, 32<<20))
	reader.Close()
	var release storedHarborRelease
	if err != nil || json.Unmarshal(releaseJSON, &release) != nil || release.Name != name || release.Namespace != namespace || release.Info.Status != "deployed" || release.Version < 1 || release.Chart.Metadata.Name != "harbor" || release.Chart.Metadata.Version == "" || release.Manifest == "" || len(release.Chart.Metadata.Dependencies) != 0 || len(release.Chart.Templates) == 0 {
		return errors.New("stored chart is not a supported deployed Harbor release; existing Harbor was preserved")
	}
	if err := os.MkdirAll(backupRoot, 0o700); err != nil {
		return errors.New("cannot prepare private Harbor configuration backup")
	}
	backup, err := os.CreateTemp(backupRoot, "helm-release-*.json")
	if err != nil {
		return errors.New("cannot back up existing Harbor configuration")
	}
	_, writeErr := backup.Write(releaseJSON)
	if writeErr == nil {
		writeErr = backup.Sync()
	}
	closeErr := backup.Close()
	if writeErr != nil || closeErr != nil {
		return errors.New("cannot back up existing Harbor configuration")
	}
	staging, err := os.MkdirTemp("", "eva-harbor-http-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	chart := filepath.Join(staging, "harbor")
	if err := os.Mkdir(chart, 0o700); err != nil {
		return err
	}
	var stored struct {
		Chart struct{ Metadata map[string]any }
	}
	if json.Unmarshal(releaseJSON, &stored) != nil {
		return errors.New("stored Harbor metadata is invalid")
	}
	metadata, _ := yaml.Marshal(stored.Chart.Metadata)
	defaults, _ := yaml.Marshal(release.Chart.Values)
	files := map[string][]byte{"Chart.yaml": metadata, "values.yaml": defaults}
	if len(release.Chart.Schema) != 0 {
		files["values.schema.json"] = release.Chart.Schema
	}
	for _, file := range append(release.Chart.Templates, release.Chart.Files...) {
		path := strings.TrimPrefix(file.Name, "harbor/")
		if filepath.IsAbs(path) || filepath.Clean(path) != path || path == "." || strings.HasPrefix(path, "../") || strings.Contains(path, "\\") {
			return errors.New("stored Harbor chart contains an invalid file path")
		}
		if _, duplicate := files[path]; duplicate {
			return errors.New("stored Harbor chart contains duplicate files")
		}
		files[path] = file.Data
	}
	for path, data := range files {
		target := filepath.Join(chart, path)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o600); err != nil {
			return err
		}
	}
	if release.Config == nil {
		release.Config = map[string]any{}
	}
	setHarborHTTPValues(release.Config, registry, port)
	values, err := yaml.Marshal(release.Config)
	if err != nil {
		return errors.New("cannot prepare Harbor HTTP values")
	}
	valuesPath := filepath.Join(staging, "values.yaml")
	if err := os.WriteFile(valuesPath, values, 0o600); err != nil {
		return err
	}
	rendered, renderErr := run(ctx, "helm", "template", name, chart, "--namespace", namespace, "--values", valuesPath, "--is-upgrade")
	if renderErr != nil {
		return errors.New("cannot render the original Harbor chart for HTTP conversion; existing deployment was preserved")
	}
	if err := validateHarborStorageUnchanged(release.Manifest, string(rendered)); err != nil {
		return err
	}
	_, upgradeErr := run(ctx, "helm", "upgrade", name, chart, "--namespace", namespace,
		"--reuse-values", "--values", valuesPath, "--wait", "--timeout", "10m")
	if upgradeErr == nil {
		upgradeErr = verify(ctx)
	}
	if upgradeErr != nil {
		if _, rollbackErr := run(ctx, "helm", "rollback", name, strconv.Itoa(release.Version), "--namespace", namespace, "--wait", "--timeout", "10m"); rollbackErr != nil {
			return errors.New("Harbor HTTP conversion and Helm rollback failed; original configuration is in the private Main Harbor backup")
		}
		return errors.New("Harbor HTTP conversion failed; original Helm revision was restored")
	}
	return nil
}

func validateHarborStorageUnchanged(original, rendered string) error {
	storage := func(manifest string) (map[string]any, error) {
		result := map[string]any{}
		decoder := yaml.NewDecoder(strings.NewReader(manifest))
		for {
			var resource struct {
				Kind     string
				Metadata struct{ Name string }
				Spec     map[string]any
			}
			err := decoder.Decode(&resource)
			if errors.Is(err, io.EOF) {
				return result, nil
			}
			if err != nil {
				return nil, err
			}
			key := resource.Kind + "/" + resource.Metadata.Name
			switch resource.Kind {
			case "PersistentVolume", "PersistentVolumeClaim":
				result[key] = resource.Spec
			case "StatefulSet":
				if claims, exists := resource.Spec["volumeClaimTemplates"]; exists {
					result[key] = claims
				}
			}
		}
	}
	before, err := storage(original)
	if err != nil {
		return errors.New("cannot validate original Harbor storage")
	}
	after, err := storage(rendered)
	if err != nil || !reflect.DeepEqual(before, after) {
		return errors.New("HTTP conversion would change Harbor persistent storage; existing deployment was preserved")
	}
	return nil
}

func setHarborHTTPValues(values map[string]any, registry string, port int) {
	child := func(parent map[string]any, key string) map[string]any {
		value, ok := parent[key].(map[string]any)
		if !ok {
			value = map[string]any{}
			parent[key] = value
		}
		return value
	}
	expose := child(values, "expose")
	expose["type"] = "nodePort"
	child(expose, "tls")["enabled"] = false
	child(child(child(expose, "nodePort"), "ports"), "http")["nodePort"] = port
	values["externalURL"] = "http://" + registry
}
