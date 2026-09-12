package fieldoverride

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// File identifies a user-supplied file and, once an operation is created, its
// immutable private snapshot.
type File struct {
	SourcePath string `yaml:"source_path"`
	SHA256     string `yaml:"sha256"`
	StagedPath string `yaml:"staged_path,omitempty"`
}

// Component holds the public override metadata plus private set expressions.
// SetValues are intentionally never serialized into a Plan because they may
// contain credentials.
type Component struct {
	Chart           *File    `yaml:"chart,omitempty"`
	Values          *File    `yaml:"values,omitempty"`
	SetKeys         []string `yaml:"set_keys,omitempty"`
	AnsibleVarsPath string   `yaml:"ansible_vars_path,omitempty"`
	SetValues       []string `yaml:"-"`
}

type Request struct {
	Components map[string]Component
}

func (request Request) Empty() bool {
	return len(request.Components) == 0
}

// Public returns metadata suitable for plans and logs. Raw --set values are
// omitted and are written only to the operation's private input file.
func (request Request) Public() map[string]Component {
	if len(request.Components) == 0 {
		return nil
	}
	components := make(map[string]Component, len(request.Components))
	for name, component := range request.Components {
		component.SetValues = nil
		components[name] = component
	}
	return components
}

// Parse accepts component-scoped Helm overrides. The explicit component prefix
// avoids positional-argument ambiguity.
func Parse(charts, values, sets []string, selected map[string]bool) (Request, error) {
	request := Request{Components: map[string]Component{}}
	for _, specification := range charts {
		component, path, err := parseFileSpecification("--chart", specification, selected)
		if err != nil {
			return Request{}, err
		}
		entry := request.Components[component]
		if entry.Chart != nil {
			return Request{}, fmt.Errorf("--chart was specified more than once for component %q", component)
		}
		file, err := inspectFile(path)
		if err != nil {
			return Request{}, fmt.Errorf("inspect --chart for component %q: %w", component, err)
		}
		entry.Chart = &file
		request.Components[component] = entry
	}
	for _, specification := range values {
		component, path, err := parseFileSpecification("--values", specification, selected)
		if err != nil {
			return Request{}, err
		}
		entry := request.Components[component]
		if entry.Values != nil {
			return Request{}, fmt.Errorf("--values was specified more than once for component %q", component)
		}
		file, err := inspectFile(path)
		if err != nil {
			return Request{}, fmt.Errorf("inspect --values for component %q: %w", component, err)
		}
		entry.Values = &file
		request.Components[component] = entry
	}
	for _, specification := range sets {
		component, expression, err := parseSetSpecification(specification, selected)
		if err != nil {
			return Request{}, err
		}
		entry := request.Components[component]
		key, _, _ := strings.Cut(expression, "=")
		entry.SetKeys = append(entry.SetKeys, key)
		entry.SetValues = append(entry.SetValues, expression)
		request.Components[component] = entry
	}
	if len(request.Components) == 0 {
		return Request{}, nil
	}
	return request, nil
}

func parseFileSpecification(flag, specification string, selected map[string]bool) (string, string, error) {
	component, path, found := strings.Cut(specification, "=")
	if !found || component == "" || path == "" {
		return "", "", fmt.Errorf("%s requires COMPONENT=PATH", flag)
	}
	if err := validateComponent(component, selected); err != nil {
		return "", "", err
	}
	return component, path, nil
}

func parseSetSpecification(specification string, selected map[string]bool) (string, string, error) {
	component := ""
	expression := specification
	if prefix, remainder, found := strings.Cut(specification, ":"); found {
		component, expression = prefix, remainder
	} else {
		if component, found := singleHelmComponent(selected); found {
			expression, err := validateSetExpression(expression)
			return component, expression, err
		} else {
			return "", "", errors.New("--set requires COMPONENT:KEY=VALUE unless exactly one Helm component is selected")
		}
	}
	if err := validateComponent(component, selected); err != nil {
		return "", "", err
	}
	expression, err := validateSetExpression(expression)
	return component, expression, err
}

func validateSetExpression(expression string) (string, error) {
	key, value, found := strings.Cut(expression, "=")
	if !found || key == "" || value == "" {
		return "", errors.New("--set requires KEY=VALUE")
	}
	return expression, nil
}

func validateComponent(component string, selected map[string]bool) error {
	if !helmComponent(component) {
		return fmt.Errorf("field overrides support app, agent, and vision; got %q", component)
	}
	if !selected[component] {
		return fmt.Errorf("component %q must be enabled and selected before applying field overrides", component)
	}
	return nil
}

func singleHelmComponent(selected map[string]bool) (string, bool) {
	component := ""
	for name, enabled := range selected {
		if enabled && helmComponent(name) {
			if component != "" {
				return "", false
			}
			component = name
		}
	}
	return component, component != ""
}

func helmComponent(component string) bool {
	switch component {
	case "app", "agent", "vision":
		return true
	default:
		return false
	}
}

func inspectFile(path string) (File, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return File{}, fmt.Errorf("resolve path: %w", err)
	}
	info, err := os.Lstat(absPath)
	if err != nil {
		return File{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return File{}, fmt.Errorf("must be a regular non-symlink file: %s", absPath)
	}
	file, err := os.Open(absPath)
	if err != nil {
		return File{}, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return File{}, err
	}
	return File{SourcePath: absPath, SHA256: fmt.Sprintf("%x", hash.Sum(nil))}, nil
}
