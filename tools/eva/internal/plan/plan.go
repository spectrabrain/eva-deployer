package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"eva-deployer/tools/eva/internal/fieldoverride"
	"eva-deployer/tools/eva/internal/release"
	"eva-deployer/tools/eva/internal/workspace"
	"gopkg.in/yaml.v3"
)

const SchemaVersion = "v1"

type Step struct {
	Component string `yaml:"component"`
	Playbook  string `yaml:"playbook"`
}

type Document struct {
	SchemaVersion     string                             `yaml:"schema_version"`
	OperationID       string                             `yaml:"operation_id,omitempty"`
	GeneratedAt       time.Time                          `yaml:"generated_at"`
	SiteID            string                             `yaml:"site_id"`
	Workspace         string                             `yaml:"workspace"`
	ReleaseVersion    string                             `yaml:"release_version"`
	ReleaseRoot       string                             `yaml:"release_root"`
	RepositoryMode    string                             `yaml:"repository_mode"`
	Repository        string                             `yaml:"repository_registry,omitempty"`
	RepositoryProject string                             `yaml:"repository_project"`
	Steps             []Step                             `yaml:"steps"`
	AnsibleExtraVars  []string                           `yaml:"ansible_extra_vars"`
	Environment       map[string]string                  `yaml:"environment"`
	Overrides         map[string]fieldoverride.Component `yaml:"overrides,omitempty"`
	OverrideInputs    fieldoverride.Request              `yaml:"-"`
}

func Build(workspaceResolved workspace.Resolved, releaseResolved release.Resolved, now time.Time) Document {
	return BuildWithOverrides(workspaceResolved, releaseResolved, fieldoverride.Request{}, now)
}

func BuildWithOverrides(workspaceResolved workspace.Resolved, releaseResolved release.Resolved, overrides fieldoverride.Request, now time.Time) Document {
	config := workspaceResolved.Config
	return Document{
		SchemaVersion:     SchemaVersion,
		GeneratedAt:       now.UTC(),
		SiteID:            workspaceResolved.SiteID,
		Workspace:         workspaceResolved.Root,
		ReleaseVersion:    releaseResolved.Metadata.Version,
		ReleaseRoot:       releaseResolved.Root,
		RepositoryMode:    workspaceResolved.AnsibleMode,
		Repository:        config.Repository.Registry,
		RepositoryProject: config.Repository.Project,
		Steps:             steps(config.Components),
		AnsibleExtraVars:  workspaceResolved.AnsibleExtraVars(),
		Environment:       workspaceResolved.Environment(),
		Overrides:         overrides.Public(),
		OverrideInputs:    overrides,
	}
}

func Marshal(document Document) ([]byte, error) {
	contents, err := yaml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("marshal plan: %w", err)
	}
	return contents, nil
}

func Write(path string, contents []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("create plan directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".plan-*")
	if err != nil {
		return fmt.Errorf("create temporary plan: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("set temporary plan permissions: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary plan: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary plan: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish plan: %w", err)
	}
	return nil
}

func steps(components map[string]bool) []Step {
	steps := make([]Step, 0, 8)
	if len(components) > 0 {
		steps = append(steps, Step{Component: "precondition", Playbook: "src/infra/playbooks/site_precondition.yaml"})
	}
	if components["infra"] {
		steps = append(steps, Step{Component: "infra", Playbook: "src/infra/playbooks/site_infra.yaml"})
	}
	if components["agent"] || components["vision"] {
		steps = append(steps, Step{Component: "config", Playbook: "src/solution/playbooks/site_eva_config.yaml"})
	}
	if components["iam"] {
		steps = append(steps, Step{Component: "iam", Playbook: "src/solution/playbooks/site_eva_iam.yaml"})
	}
	if components["agent"] {
		steps = append(steps, Step{Component: "agent", Playbook: "src/solution/playbooks/site_eva_agent.yaml"})
	}
	if components["vision"] {
		steps = append(steps, Step{Component: "vision", Playbook: "src/solution/playbooks/site_eva_vision.yaml"})
	}
	if components["app"] {
		steps = append(steps, Step{Component: "app", Playbook: "src/solution/playbooks/site_eva_app.yaml"})
	}
	if components["n8n"] {
		steps = append(steps, Step{Component: "n8n", Playbook: "src/solution/playbooks/site_n8n.yaml"})
	}
	return steps
}
