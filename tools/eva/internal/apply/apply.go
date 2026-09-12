package apply

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"eva-deployer/tools/eva/internal/operation"
	"eva-deployer/tools/eva/internal/plan"
	"eva-deployer/tools/eva/internal/runtime"
)

const DefaultLogRoot = "/var/log/eva/operations"

var expectedPlaybooks = map[string]string{
	"infra":  "src/infra/playbooks/site_infra.yaml",
	"config": "src/solution/playbooks/site_eva_config.yaml",
	"iam":    "src/solution/playbooks/site_eva_iam.yaml",
	"agent":  "src/solution/playbooks/site_eva_agent.yaml",
	"vision": "src/solution/playbooks/site_eva_vision.yaml",
	"app":    "src/solution/playbooks/site_eva_app.yaml",
	"n8n":    "src/solution/playbooks/site_n8n.yaml",
}

type Options struct {
	StateRoot   string
	LogRoot     string
	RuntimeRoot string
	Stdout      io.Writer
	Stderr      io.Writer
	Now         func() time.Time
}

func Execute(options Options, record operation.Record) (operation.Record, error) {
	if record.Status != operation.Planned {
		return record, fmt.Errorf("operation %s has status %q; only planned operations can be applied", record.ID, record.Status)
	}
	if options.StateRoot == "" {
		options.StateRoot = operation.DefaultRoot
	}
	if options.LogRoot == "" {
		options.LogRoot = DefaultLogRoot
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}
	if options.Stderr == nil {
		options.Stderr = io.Discard
	}

	document, err := operation.LoadPlan(options.StateRoot, record)
	if err != nil {
		return record, err
	}
	ansiblePath, inventory, releaseRoot, err := preflight(options.RuntimeRoot, document)
	if err != nil {
		return record, err
	}
	logDirectory, err := operationLogDirectory(options.LogRoot, record.ID)
	if err != nil {
		return record, err
	}
	combinedLog, err := openPrivate(filepath.Join(logDirectory, "ansible.log"))
	if err != nil {
		return record, err
	}
	defer combinedLog.Close()
	internalLogPath := filepath.Join(logDirectory, "ansible-internal.log")
	internalLog, err := openPrivate(internalLogPath)
	if err != nil {
		return record, err
	}
	if err := internalLog.Close(); err != nil {
		return record, fmt.Errorf("close Ansible internal log: %w", err)
	}

	startedAt := options.Now().UTC()
	record.Status = operation.Running
	record.StartedAt = startedAt
	record.UpdatedAt = startedAt
	record.LogDirectory = logDirectory
	if err := operation.Update(options.StateRoot, record); err != nil {
		return record, err
	}

	result := operation.Result{Status: operation.Running, StartedAt: startedAt}
	for _, step := range document.Steps {
		stepStartedAt := options.Now().UTC()
		command := exec.Command(ansiblePath, ansibleArgs(inventory, filepath.Join(releaseRoot, step.Playbook), document.AnsibleExtraVars)...)
		command.Dir = releaseRoot
		command.Env = commandEnvironment(document, releaseRoot, internalLogPath)
		output := io.MultiWriter(options.Stdout, combinedLog)
		command.Stdout = output
		command.Stderr = io.MultiWriter(options.Stderr, combinedLog)
		err := command.Run()
		stepResult := operation.StepResult{
			Component:   step.Component,
			Playbook:    step.Playbook,
			StartedAt:   stepStartedAt,
			CompletedAt: options.Now().UTC(),
			ExitCode:    exitCode(err),
		}
		result.Steps = append(result.Steps, stepResult)
		if err != nil {
			return finish(options, record, result, operation.Failed, fmt.Errorf("apply component %q: %w", step.Component, err))
		}
	}
	return finish(options, record, result, operation.Succeeded, nil)
}

func preflight(runtimeRoot string, document plan.Document) (string, string, string, error) {
	resolvedRuntime, err := runtime.Resolve(runtimeRoot)
	if err != nil {
		return "", "", "", err
	}
	ansiblePath, err := resolvedRuntime.ToolPath("ansible-playbook")
	if err != nil {
		return "", "", "", err
	}
	releaseRoot, err := filepath.Abs(document.ReleaseRoot)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve plan release root: %w", err)
	}
	if err := requireRegular(filepath.Join(releaseRoot, "ansible.cfg")); err != nil {
		return "", "", "", fmt.Errorf("prepared Release source is invalid: %w", err)
	}
	inventory := filepath.Join(document.Workspace, "inventory", "inventory.ini")
	if err := requireRegular(inventory); err != nil {
		return "", "", "", fmt.Errorf("workspace inventory is invalid: %w", err)
	}
	for _, step := range document.Steps {
		expected, ok := expectedPlaybooks[step.Component]
		if !ok || step.Playbook != expected {
			return "", "", "", fmt.Errorf("operation plan has invalid step %q (%s)", step.Component, step.Playbook)
		}
		playbook, err := playbookPath(releaseRoot, step.Playbook)
		if err != nil {
			return "", "", "", fmt.Errorf("operation step %q: %w", step.Component, err)
		}
		if err := requireRegular(playbook); err != nil {
			return "", "", "", fmt.Errorf("prepared Release source is invalid: %w", err)
		}
	}
	return ansiblePath, inventory, releaseRoot, nil
}

func finish(options Options, record operation.Record, result operation.Result, status string, applyErr error) (operation.Record, error) {
	completedAt := options.Now().UTC()
	result.Status = status
	result.CompletedAt = completedAt
	resultPath, resultErr := operation.WriteResult(options.StateRoot, record, result)
	record.Status = status
	record.CompletedAt = completedAt
	record.UpdatedAt = completedAt
	if resultErr == nil {
		record.ResultPath = resultPath
	}
	if updateErr := operation.Update(options.StateRoot, record); updateErr != nil {
		return record, updateErr
	}
	if resultErr != nil {
		return record, resultErr
	}
	return record, applyErr
}

func operationLogDirectory(root, operationID string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve operation log root: %w", err)
	}
	directory := filepath.Join(absRoot, operationID)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create operation log directory: %w", err)
	}
	return directory, nil
}

func openPrivate(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create private log %s: %w", path, err)
	}
	return file, nil
}

func ansibleArgs(inventory, playbook string, extraVars []string) []string {
	args := []string{"-i", inventory, playbook}
	for _, value := range extraVars {
		args = append(args, "-e", value)
	}
	return args
}

func commandEnvironment(document plan.Document, releaseRoot, internalLogPath string) []string {
	values := make(map[string]string, len(document.Environment)+2)
	for _, entry := range os.Environ() {
		name, value, found := strings.Cut(entry, "=")
		if found {
			values[name] = value
		}
	}
	for name, value := range document.Environment {
		values[name] = value
	}
	values["EVA_REPO_ROOT"] = releaseRoot
	values["ANSIBLE_LOG_PATH"] = internalLogPath
	environment := make([]string, 0, len(values))
	for name, value := range values {
		environment = append(environment, name+"="+value)
	}
	return environment
}

func playbookPath(root, relativePath string) (string, error) {
	if filepath.IsAbs(relativePath) {
		return "", errors.New("playbook path must be relative to the Release source")
	}
	path := filepath.Clean(filepath.Join(root, relativePath))
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("playbook path escapes the Release source")
	}
	return path, nil
}

func requireRegular(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", path)
	}
	return nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return -1
}
