package workspace

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Keep the selected context in the immutable Workspace, so retries and all
// role helpers use it even if the original current-context changes later.
func pinKubeconfig(path, contextName string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxInstallerBytes {
		return nil, errors.New("cannot read deployment.kubeconfig as one regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read deployment.kubeconfig")
	}
	var config map[string]any
	if yaml.Unmarshal(raw, &config) != nil {
		return nil, errors.New("invalid deployment.kubeconfig")
	}
	if contextName == "" {
		contextName, _ = config["current-context"].(string)
	}
	find := func(section, name, field string) map[string]any {
		entries, _ := config[section].([]any)
		for _, entry := range entries {
			record, _ := entry.(map[string]any)
			if record["name"] == name {
				value, _ := record[field].(map[string]any)
				return value
			}
		}
		return nil
	}
	context := find("contexts", contextName, "context")
	if context == nil {
		return nil, errors.New("selected context is missing from deployment.kubeconfig")
	}
	clusterName, _ := context["cluster"].(string)
	userName, _ := context["user"].(string)
	cluster := find("clusters", clusterName, "cluster")
	user := find("users", userName, "user")
	if cluster == nil || user == nil {
		return nil, errors.New("selected kubeconfig context requires a cluster and an authenticated user")
	}
	server, _ := cluster["server"].(string)
	endpoint, err := url.Parse(server)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || cluster["insecure-skip-tls-verify"] == true {
		return nil, errors.New("Cloud Kubernetes requires an HTTPS API endpoint with TLS verification")
	}
	for _, block := range []map[string]any{cluster, user} {
		for _, key := range []string{"certificate-authority", "client-certificate", "client-key"} {
			if value, ok := block[key].(string); ok && value != "" && !filepath.IsAbs(value) {
				block[key] = filepath.Join(filepath.Dir(path), value)
			}
		}
	}
	if plugin, ok := user["exec"].(map[string]any); ok {
		if command, ok := plugin["command"].(string); ok && strings.Contains(command, "/") && !filepath.IsAbs(command) {
			plugin["command"] = filepath.Join(filepath.Dir(path), command)
		}
	}
	config["current-context"] = contextName
	config["contexts"] = []any{map[string]any{"name": contextName, "context": context}}
	config["clusters"] = []any{map[string]any{"name": clusterName, "cluster": cluster}}
	config["users"] = []any{map[string]any{"name": userName, "user": user}}
	encoded, err := yaml.Marshal(config)
	if err != nil {
		return nil, errors.New("cannot snapshot deployment.kubeconfig")
	}
	return encoded, nil
}
