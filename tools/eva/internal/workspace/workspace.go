package workspace

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultRoot = "/etc/eva/sites"

var siteIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

var ansibleRepositoryModes = map[string]string{
	"online": "online_repository",
	"remote": "remote_repository",
	"local":  "local_repository",
}

// NormalizeRepositoryMode keeps legacy inputs usable while new configuration
// and plans describe connectivity, not the target's hosting environment.
func NormalizeRepositoryMode(mode string) string {
	switch mode = strings.ToLower(mode); mode {
	case "cloud", "cloud_repository", "online_repository":
		return "online"
	case "remote_repository":
		return "remote"
	case "local_repository":
		return "local"
	default:
		return mode
	}
}

type Options struct {
	SiteID     string
	Workspace  string
	ConfigFile string
	CacheRoot  string
}

type Config struct {
	Site struct {
		ID string `yaml:"id"`
	} `yaml:"site"`
	Repository struct {
		Mode     string `yaml:"mode"`
		Registry string `yaml:"registry"`
		Project  string `yaml:"project"`
	} `yaml:"repository"`
	Components map[string]bool `yaml:"components"`
	// Generated scope metadata contains aliases and switches only, never credentials.
	TargetComponents map[string]map[string]bool `yaml:"target_components,omitempty"`
}

type Resolved struct {
	SiteID      string
	Root        string
	ConfigPath  string
	Config      Config
	AnsibleMode string
}

func Resolve(options Options) (Resolved, error) {
	if err := validateRequestedSiteID(options.SiteID); err != nil {
		return Resolved{}, err
	}
	if options.ConfigFile != "" {
		if options.Workspace != "" {
			return Resolved{}, errors.New("use either --config or --workspace, not both")
		}
		input, err := LoadInstaller(options.ConfigFile, options.SiteID)
		if err != nil {
			return Resolved{}, err
		}
		return input.Materialize(options.CacheRoot)
	}

	root, err := resolveRoot(options)
	if err != nil {
		return Resolved{}, err
	}

	configPath := filepath.Join(root, "site-values", "site.yaml")
	config, err := loadConfig(configPath)
	if err != nil {
		return Resolved{}, err
	}
	if err := validateConfig(config, options.SiteID); err != nil {
		return Resolved{}, err
	}

	mode := NormalizeRepositoryMode(config.Repository.Mode)
	return Resolved{
		SiteID:      config.Site.ID,
		Root:        root,
		ConfigPath:  configPath,
		Config:      config,
		AnsibleMode: ansibleRepositoryModes[mode],
	}, nil
}

func (resolved Resolved) AnsibleExtraVars() []string {
	enabled := selectedComponents(resolved.Config.Components)
	if len(resolved.Config.TargetComponents) != 0 {
		// Extra vars outrank host vars: intersect the global CLI selection with
		// each target's scope lazily in Ansible, rather than replacing host scope.
		enabled = "{{ (eva_target_enabled_components | default('" + enabled + "')).split(',') | intersect('" + enabled + "'.split(',')) | sort | join(',') }}"
	}
	values := []string{
		"eva_workspace_root=" + resolved.Root,
		"eva_site_id=" + resolved.SiteID,
		"repository_mode=" + resolved.AnsibleMode,
		"eva_enabled_components=" + enabled,
	}
	if registry := resolved.Config.Repository.Registry; registry != "" {
		values = append(values, "repository_registry="+registry)
	}
	if project := resolved.Config.Repository.Project; project != "" {
		values = append(values, "repository_project="+project)
	}
	return values
}

func selectedComponents(components map[string]bool) string {
	selected := make([]string, 0, len(components))
	for component, enabled := range components {
		if enabled {
			selected = append(selected, component)
		}
	}
	sort.Strings(selected)
	return strings.Join(selected, ",")
}

func (resolved Resolved) Environment() map[string]string {
	return map[string]string{
		"EVA_SITE_ID":        resolved.SiteID,
		"EVA_WORKSPACE_ROOT": resolved.Root,
	}
}

// SelectComponents returns a copy of the workspace scoped to requested
// components. Named components must already be enabled in site.yaml; "all"
// explicitly selects every enabled component.
func (resolved Resolved) SelectComponents(requested []string) (Resolved, error) {
	components := make(map[string]bool, len(resolved.Config.Components))
	for name, enabled := range resolved.Config.Components {
		components[name] = enabled
	}
	if len(requested) == 0 {
		resolved.Config.Components = components
		return resolved, nil
	}

	selected := make(map[string]bool, len(requested))
	for _, name := range requested {
		if name == "all" {
			if len(requested) != 1 {
				return Resolved{}, errors.New("--component all cannot be combined with named components")
			}
			continue
		}
		if !validComponent(name) {
			return Resolved{}, fmt.Errorf("unknown component %q", name)
		}
		if selected[name] {
			return Resolved{}, fmt.Errorf("component %q was selected more than once", name)
		}
		if !components[name] {
			return Resolved{}, fmt.Errorf("component %q is not enabled in site configuration", name)
		}
		selected[name] = true
	}

	if requested[0] != "all" {
		for name := range components {
			components[name] = selected[name]
		}
	}
	resolved.Config.Components = components
	return resolved, nil
}

func validateRequestedSiteID(siteID string) error {
	if siteID == "" {
		return nil
	}
	if !siteIDPattern.MatchString(siteID) {
		return fmt.Errorf("invalid site ID %q: use letters, numbers, dots, underscores, and hyphens; it must start with a letter or number", siteID)
	}
	return nil
}

func resolveRoot(options Options) (string, error) {
	if options.Workspace != "" {
		root, err := filepath.Abs(options.Workspace)
		if err != nil {
			return "", fmt.Errorf("resolve workspace path: %w", err)
		}
		return root, nil
	}
	if options.SiteID == "" {
		return "", errors.New("provide --config FILE, --site ID, or --workspace PATH containing site-values/site.yaml")
	}
	return filepath.Join(DefaultRoot, options.SiteID), nil
}

func loadConfig(path string) (Config, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, fmt.Errorf("workspace input is missing: %s", path)
		}
		return Config{}, fmt.Errorf("read workspace input %s: %w", path, err)
	}

	decoder := yaml.NewDecoder(strings.NewReader(string(contents)))
	decoder.KnownFields(true)
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("parse workspace input %s: %w", path, err)
	}
	if config.Repository.Project == "" {
		config.Repository.Project = "eva"
	}
	config.Repository.Mode = NormalizeRepositoryMode(config.Repository.Mode)
	return config, nil
}

func validateConfig(config Config, requestedSiteID string) error {
	if config.Site.ID == "" {
		return errors.New("site.id is required in site configuration")
	}
	if err := validateRequestedSiteID(config.Site.ID); err != nil {
		return err
	}
	if requestedSiteID != "" && requestedSiteID != config.Site.ID {
		return fmt.Errorf("--site %q does not match site.id %q", requestedSiteID, config.Site.ID)
	}

	mode := NormalizeRepositoryMode(config.Repository.Mode)
	if _, ok := ansibleRepositoryModes[mode]; !ok {
		return fmt.Errorf("repository.mode must be online, remote, or local; got %q", config.Repository.Mode)
	}
	if mode == "remote" || mode == "local" {
		if err := validateRegistry(config.Repository.Registry); err != nil {
			return err
		}
	}
	if len(config.Components) == 0 {
		return errors.New("at least one component must be selected in components")
	}
	selected := false
	for component := range config.Components {
		if !validComponent(component) {
			return fmt.Errorf("unknown component %q", component)
		}
		selected = selected || config.Components[component]
	}
	if !selected {
		return errors.New("at least one component must be enabled in components")
	}
	for name, components := range config.TargetComponents {
		if !siteIDPattern.MatchString(name) || name == "all" || name == "ungrouped" || name == "eva" {
			return errors.New("target_components contains an unsafe inventory alias")
		}
		if err := validateTargetComponents(name, components, config.Components); err != nil {
			return err
		}
	}
	return nil
}

func validateTargetComponents(name string, components, global map[string]bool) error {
	selected := false
	for component, enabled := range components {
		if !validComponent(component) || (enabled && !global[component]) {
			return fmt.Errorf("target %s components must be a subset of enabled global components", name)
		}
		selected = selected || enabled
	}
	if !selected {
		return fmt.Errorf("target %s requires at least one enabled component", name)
	}
	return nil
}

func validateRegistry(registry string) error {
	if registry == "" {
		return errors.New("repository.registry is required for remote and local modes")
	}
	host, _, err := net.SplitHostPort(registry)
	if err != nil || host == "" {
		return fmt.Errorf("repository.registry must be a reachable host:port; got %q", registry)
	}
	if strings.EqualFold(host, "localhost") || host == "127.0.0.1" || host == "::1" {
		return fmt.Errorf("repository.registry must not use loopback address %q", registry)
	}
	return nil
}

func validComponent(component string) bool {
	switch component {
	case "infra", "iam", "agent", "vision", "app", "n8n":
		return true
	default:
		return false
	}
}
