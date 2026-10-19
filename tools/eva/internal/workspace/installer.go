package workspace

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

const DefaultCacheRoot = "/var/lib/eva/workspaces"
const maxInstallerBytes = 8 << 20

var requiredPlaceholder = regexp.MustCompile(`<[A-Z][A-Z0-9_]*>`)

// Installer is the installer-facing input. Chart values stay verbatim, but
// their target keys are generated using Ansible's ansible_host identity.
type Installer struct {
	SchemaVersion string `yaml:"schema_version"`
	Config        `yaml:",inline"`
	Credentials   struct {
		AWS *AWSCredentials `yaml:"aws,omitempty"`
	} `yaml:"credentials,omitempty"`
	Targets map[string]InstallerTarget `yaml:"targets"`
}

type AWSCredentials struct {
	AccessKeyID     string `yaml:"aws_access_key_id"`
	SecretAccessKey string `yaml:"aws_secret_access_key"`
	Region          string `yaml:"region"`
}

type InstallerTarget struct {
	Components map[string]bool           `yaml:"components,omitempty"`
	Ansible    map[string]any            `yaml:"ansible"`
	Values     map[string]map[string]any `yaml:"values,omitempty"`
}

// LoadInstaller validates without writing anything. Decoder diagnostics are
// deliberately not echoed: YAML type errors can contain credential values.
func LoadInstaller(path, requestedSiteID string) (Installer, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Installer{}, fmt.Errorf("inspect installer config: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Installer{}, errors.New("installer config must be a regular non-symlink file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return Installer{}, errors.New("installer config may contain credentials; restrict its permissions to 0600 (chmod 600)")
	}
	if info.Size() > maxInstallerBytes {
		return Installer{}, errors.New("installer config exceeds the 8 MiB limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return Installer{}, fmt.Errorf("open installer config: %w", err)
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maxInstallerBytes+1))
	if err != nil || len(contents) > maxInstallerBytes {
		return Installer{}, errors.New("cannot read installer config within the 8 MiB limit")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	var input Installer
	if err := decoder.Decode(&input); err != nil {
		return Installer{}, errors.New("invalid installer config: check YAML syntax, duplicate keys, field names and types")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Installer{}, errors.New("installer config must contain exactly one YAML document")
	}
	if input.Repository.Project == "" {
		input.Repository.Project = "eva"
	}
	if err := input.validate(requestedSiteID); err != nil {
		return Installer{}, err
	}
	// SSH identity files are controller-side paths. Resolve them relative to
	// the config, not the caller's working directory or generated workspace.
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Installer{}, fmt.Errorf("resolve installer config path: %w", err)
	}
	for _, target := range input.Targets {
		for _, key := range []string{"ansible_private_key_file", "ansible_ssh_private_key_file"} {
			if value, ok := target.Ansible[key].(string); ok && value != "" && !filepath.IsAbs(value) {
				if strings.HasPrefix(value, "~") {
					return Installer{}, fmt.Errorf("%s must use an absolute path or a path relative to the config, not ~", key)
				}
				target.Ansible[key] = filepath.Join(filepath.Dir(absolute), value)
			}
		}
	}
	return input, nil
}

func (input Installer) validate(requestedSiteID string) error {
	if input.SchemaVersion != "v1" {
		return errors.New("installer config schema_version must be v1")
	}
	if len(input.Config.TargetComponents) != 0 {
		return errors.New("installer config uses targets.<alias>.components, not internal target_components")
	}
	if err := validateInstallerPlaceholders(input); err != nil {
		return err
	}
	if err := validateConfig(input.Config, requestedSiteID); err != nil {
		return fmt.Errorf("installer config: %w", err)
	}
	if len(input.Targets) == 0 {
		return errors.New("installer config requires at least one target")
	}
	identities := make(map[string]bool)
	for name, target := range input.Targets {
		if !siteIDPattern.MatchString(name) || name == "all" || name == "ungrouped" || name == "eva" {
			return errors.New("target names must be safe inventory aliases, not all, ungrouped or eva")
		}
		if len(target.Ansible) == 0 {
			return fmt.Errorf("target %s requires an ansible mapping (for example ansible_connection: local)", name)
		}
		if target.Components != nil {
			if err := validateTargetComponents(name, target.Components, input.Components); err != nil {
				return err
			}
			for component := range target.Values {
				if !target.Components[component] {
					return fmt.Errorf("target %s has values for a disabled component", name)
				}
			}
		}
		if _, exists := target.Ansible["eva_target_enabled_components"]; exists {
			return errors.New("eva_target_enabled_components is managed internally; use targets.<alias>.components")
		}
		identity := name
		if host, exists := target.Ansible["ansible_host"]; exists {
			value, ok := host.(string)
			if !ok || (!siteIDPattern.MatchString(value) && net.ParseIP(value) == nil) {
				return fmt.Errorf("target %s ansible_host must be a literal IP address or hostname without path separators", name)
			}
			identity = value
		}
		if identities[identity] {
			return errors.New("targets must have distinct effective ansible_host identities")
		}
		identities[identity] = true
		for component := range target.Values {
			switch component {
			case "iam", "app", "agent", "vision":
			default:
				return errors.New("target values support only iam, app, agent and vision; use ansible variables for Infra/n8n")
			}
		}
	}
	for component, enabled := range input.Components {
		if !enabled {
			continue
		}
		found := false
		for _, target := range input.Targets {
			found = found || target.Components == nil || target.Components[component]
		}
		if !found {
			return fmt.Errorf("enabled component %s has no selected target", component)
		}
	}
	if aws := input.Credentials.AWS; aws != nil {
		if !strings.EqualFold(input.Repository.Mode, "cloud") {
			return errors.New("credentials.aws is only supported in cloud mode; do not distribute AWS credentials to Remote/Local targets")
		}
		if aws.AccessKeyID == "" || aws.SecretAccessKey == "" {
			return errors.New("credentials.aws requires aws_access_key_id and aws_secret_access_key")
		}
		for _, value := range []string{aws.AccessKeyID, aws.SecretAccessKey, aws.Region} {
			if strings.ContainsAny(value, "\r\n\x00") || strings.TrimSpace(value) != value {
				return errors.New("AWS credential entries must be single-line values without surrounding whitespace")
			}
		}
	}
	return nil
}

// Placeholder values are never printed: report field paths only, including
// repeated required entries. This runs before creating a managed Workspace.
func validateInstallerPlaceholders(input Installer) error {
	contents, err := yaml.Marshal(input)
	if err != nil {
		return errors.New("cannot validate installer required inputs")
	}
	var document yaml.Node
	if err := yaml.Unmarshal(contents, &document); err != nil {
		return errors.New("cannot validate installer required inputs")
	}
	var missing []string
	var visit func(*yaml.Node, string)
	visit = func(node *yaml.Node, path string) {
		switch node.Kind {
		case yaml.DocumentNode:
			for _, child := range node.Content {
				visit(child, path)
			}
		case yaml.MappingNode:
			for index := 0; index+1 < len(node.Content); index += 2 {
				key := node.Content[index].Value
				if path != "" {
					key = path + "." + key
				}
				visit(node.Content[index+1], key)
			}
		case yaml.SequenceNode:
			for index, child := range node.Content {
				visit(child, fmt.Sprintf("%s[%d]", path, index))
			}
		case yaml.ScalarNode:
			if requiredPlaceholder.MatchString(node.Value) {
				missing = append(missing, path)
			}
		}
	}
	visit(&document, "")
	if len(missing) != 0 {
		return fmt.Errorf("required installer inputs are still placeholders; fill these fields: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Materialize publishes an immutable, content-addressed workspace. Plans and
// retries refer to this persistent snapshot, never back to the user's config.
func (input Installer) Materialize(cacheRoot string) (Resolved, error) {
	if err := input.validate(""); err != nil {
		return Resolved{}, err
	}
	files, err := input.workspaceFiles()
	if err != nil {
		return Resolved{}, err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		fmt.Fprintf(hash, "%s\x00%d\x00", name, len(files[name]))
		hash.Write(files[name])
	}
	if cacheRoot == "" {
		cacheRoot = DefaultCacheRoot
	}
	cacheRoot, err = filepath.Abs(cacheRoot)
	if err != nil {
		return Resolved{}, err
	}
	siteRoot := filepath.Join(cacheRoot, input.Site.ID)
	for _, directory := range []string{cacheRoot, siteRoot} {
		if err := privateDirectory(directory); err != nil {
			return Resolved{}, err
		}
	}
	root := filepath.Join(siteRoot, fmt.Sprintf("%x", hash.Sum(nil)))
	if _, err := os.Lstat(root); err == nil {
		if err := verifyWorkspace(root, files); err != nil {
			return Resolved{}, err
		}
		return Resolve(Options{Workspace: root})
	} else if !errors.Is(err, os.ErrNotExist) {
		return Resolved{}, err
	}
	temporary, err := os.MkdirTemp(siteRoot, ".workspace-*")
	if err != nil {
		return Resolved{}, fmt.Errorf("create private workspace: %w", err)
	}
	defer os.RemoveAll(temporary)
	for _, name := range names {
		path := filepath.Join(temporary, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return Resolved{}, err
		}
		if err := os.WriteFile(path, files[name], 0o600); err != nil {
			return Resolved{}, fmt.Errorf("write generated workspace input: %w", err)
		}
	}
	if err := os.Rename(temporary, root); err != nil {
		// Another invocation may have published the same complete snapshot.
		if _, statErr := os.Lstat(root); statErr != nil {
			return Resolved{}, fmt.Errorf("publish generated workspace: %w", err)
		}
	}
	if err := verifyWorkspace(root, files); err != nil {
		return Resolved{}, err
	}
	return Resolve(Options{Workspace: root})
}

func (input Installer) workspaceFiles() (map[string][]byte, error) {
	config := input.Config
	for _, target := range input.Targets {
		if target.Components != nil {
			config.TargetComponents = make(map[string]map[string]bool, len(input.Targets))
			break
		}
	}
	if config.TargetComponents != nil {
		for name, target := range input.Targets {
			selected := target.Components
			if selected == nil {
				selected = input.Components
			}
			config.TargetComponents[name] = selected
		}
	}
	site, err := yaml.Marshal(config)
	if err != nil {
		return nil, errors.New("cannot encode installer site settings")
	}
	files := map[string][]byte{"site-values/site.yaml": site}
	names := make([]string, 0, len(input.Targets))
	components := make(map[string]map[string]any)
	for name, target := range input.Targets {
		names = append(names, name)
		for component := range target.Values {
			components[component] = make(map[string]any)
		}
	}
	sort.Strings(names)
	var inventory strings.Builder
	inventory.WriteString("[eva]\n")
	for _, name := range names {
		target := input.Targets[name]
		inventory.WriteString(name + "\n")
		// Host vars avoid INI quoting/type conversion bugs for passwords,
		// lists, booleans, whitespace, quotes and literal numeric strings.
		ansible := make(map[string]any, len(target.Ansible)+1)
		for key, value := range target.Ansible {
			ansible[key] = value
		}
		if config.TargetComponents != nil {
			ansible["eva_target_enabled_components"] = selectedComponents(config.TargetComponents[name])
		}
		vars, err := yaml.Marshal(ansible)
		if err != nil {
			return nil, errors.New("cannot encode target ansible variables")
		}
		files["inventory/host_vars/"+name+".yaml"] = vars
		identity := name
		if host, ok := target.Ansible["ansible_host"].(string); ok {
			identity = host
		}
		for component, values := range components {
			block := target.Values[component]
			if block == nil {
				block = map[string]any{}
			}
			// Include empty blocks too: roles otherwise treat all other
			// targets' blocks as this target's unkeyed Chart values.
			values[identity] = block
		}
	}
	files["inventory/inventory.ini"] = []byte(inventory.String())
	for component, values := range components {
		contents, err := yaml.Marshal(values)
		if err != nil {
			return nil, errors.New("cannot encode target chart values")
		}
		files["site-values/"+component+".yaml"] = contents
	}
	if aws := input.Credentials.AWS; aws != nil {
		region := aws.Region
		if region == "" {
			region = "ap-northeast-2"
		}
		files["credentials/aws_key.ini"] = []byte(fmt.Sprintf("aws_access_key_id = %s\naws_secret_access_key = %s\nregion = %s\n", aws.AccessKeyID, aws.SecretAccessKey, region))
	}
	return files, nil
}

func privateDirectory(path string) error {
	// Inspect each ancestor before MkdirAll, rather than following a symlink
	// and discovering only afterwards that credentials escaped the cache.
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return fmt.Errorf("managed workspace directory must not contain symlinks or non-directories: %s", current)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create managed workspace directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("managed workspace directory must have private permissions (0700): %s", path)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("managed workspace directory must belong to the current effective user: %s", path)
	}
	return nil
}

func verifyWorkspace(root string, files map[string][]byte) error {
	seen := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
			return errors.New("generated workspace contains a symlink or non-private permissions")
		}
		if entry.IsDir() {
			return nil
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		expected, ok := files[filepath.ToSlash(name)]
		if !ok || !info.Mode().IsRegular() {
			return errors.New("generated workspace contains unexpected files; refusing to overwrite it")
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(contents, expected) {
			return errors.New("generated workspace was modified; refusing to overwrite it")
		}
		seen++
		return nil
	})
	if err != nil {
		return fmt.Errorf("verify generated workspace: %w", err)
	}
	if seen != len(files) {
		return errors.New("generated workspace is incomplete; refusing to overwrite it")
	}
	return nil
}
