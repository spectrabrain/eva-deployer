package operation

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"eva-deployer/tools/eva/internal/fieldoverride"
	"eva-deployer/tools/eva/internal/plan"
	"gopkg.in/yaml.v3"
)

const DefaultRoot = "/var/lib/eva/operations"
const Planned = "planned"
const Running = "running"
const Succeeded = "succeeded"
const Failed = "failed"

var operationIDPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[A-Za-z0-9][A-Za-z0-9._-]*-[a-f0-9]{8}$`)

type Record struct {
	ID                string    `yaml:"id"`
	Status            string    `yaml:"status"`
	SourceOperationID string    `yaml:"source_operation_id,omitempty"`
	RetryOperationID  string    `yaml:"retry_operation_id,omitempty"`
	CreatedAt         time.Time `yaml:"created_at"`
	UpdatedAt         time.Time `yaml:"updated_at"`
	StartedAt         time.Time `yaml:"started_at,omitempty"`
	CompletedAt       time.Time `yaml:"completed_at,omitempty"`
	SiteID            string    `yaml:"site_id"`
	ReleaseVersion    string    `yaml:"release_version"`
	HasOverrides      bool      `yaml:"has_field_overrides,omitempty"`
	PlanPath          string    `yaml:"plan_path"`
	ResultPath        string    `yaml:"result_path,omitempty"`
	LogDirectory      string    `yaml:"log_directory,omitempty"`
}

type StepResult struct {
	Component   string    `yaml:"component"`
	Playbook    string    `yaml:"playbook"`
	StartedAt   time.Time `yaml:"started_at"`
	CompletedAt time.Time `yaml:"completed_at"`
	ExitCode    int       `yaml:"exit_code"`
}

type Result struct {
	Status      string       `yaml:"status"`
	StartedAt   time.Time    `yaml:"started_at"`
	CompletedAt time.Time    `yaml:"completed_at"`
	Steps       []StepResult `yaml:"steps"`
}

func Create(root string, document plan.Document, now time.Time) (Record, error) {
	if root == "" {
		root = DefaultRoot
	}
	id, err := newID(document.SiteID, now, rand.Reader)
	if err != nil {
		return Record{}, err
	}
	directory := filepath.Join(root, id)
	if err := os.MkdirAll(root, 0o750); err != nil {
		return Record{}, fmt.Errorf("create operation root: %w", err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return Record{}, fmt.Errorf("create operation directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(directory)
		}
	}()

	document.OperationID = id
	if err := stageOverrideInputs(directory, &document); err != nil {
		return Record{}, err
	}
	planContents, err := plan.Marshal(document)
	if err != nil {
		return Record{}, err
	}
	planPath := filepath.Join(directory, "plan.yaml")
	if err := writePrivateFile(planPath, planContents); err != nil {
		return Record{}, err
	}

	record := Record{
		ID:             id,
		Status:         Planned,
		CreatedAt:      now.UTC(),
		UpdatedAt:      now.UTC(),
		SiteID:         document.SiteID,
		ReleaseVersion: document.ReleaseVersion,
		HasOverrides:   len(document.Overrides) > 0,
		PlanPath:       planPath,
	}
	contents, err := yaml.Marshal(record)
	if err != nil {
		return Record{}, fmt.Errorf("marshal operation metadata: %w", err)
	}
	if err := writePrivateFile(filepath.Join(directory, "operation.yaml"), contents); err != nil {
		return Record{}, err
	}
	published = true
	return record, nil
}

// Retry clones a failed Operation's immutable Plan and staged override inputs
// into a new planned Operation. The source remains failed and records the new
// retry ID; the retry records its source ID.
func Retry(root string, source Record, now time.Time) (Record, error) {
	if root == "" {
		root = DefaultRoot
	}
	if !operationIDPattern.MatchString(source.ID) {
		return Record{}, fmt.Errorf("invalid operation ID %q", source.ID)
	}
	if source.Status != Failed {
		return Record{}, fmt.Errorf("operation %s has status %q; only failed operations can be retried", source.ID, source.Status)
	}
	document, err := LoadPlan(root, source)
	if err != nil {
		return Record{}, err
	}
	if document.SiteID != source.SiteID || document.ReleaseVersion != source.ReleaseVersion {
		return Record{}, fmt.Errorf("operation plan does not match source operation %s", source.ID)
	}
	id, err := newID(document.SiteID, now, rand.Reader)
	if err != nil {
		return Record{}, err
	}
	directory := filepath.Join(root, id)
	if err := os.MkdirAll(root, 0o750); err != nil {
		return Record{}, fmt.Errorf("create operation root: %w", err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return Record{}, fmt.Errorf("create retry operation directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(directory)
		}
	}()

	document.OperationID = id
	if err := cloneOverrideInputs(filepath.Join(root, source.ID), directory, &document); err != nil {
		return Record{}, err
	}
	planContents, err := plan.Marshal(document)
	if err != nil {
		return Record{}, err
	}
	planPath := filepath.Join(directory, "plan.yaml")
	if err := writePrivateFile(planPath, planContents); err != nil {
		return Record{}, err
	}
	retry := Record{
		ID:                id,
		Status:            Planned,
		SourceOperationID: source.ID,
		CreatedAt:         now.UTC(),
		UpdatedAt:         now.UTC(),
		SiteID:            document.SiteID,
		ReleaseVersion:    document.ReleaseVersion,
		HasOverrides:      len(document.Overrides) > 0,
		PlanPath:          planPath,
	}
	contents, err := yaml.Marshal(retry)
	if err != nil {
		return Record{}, fmt.Errorf("marshal retry operation metadata: %w", err)
	}
	if err := writePrivateFile(filepath.Join(directory, "operation.yaml"), contents); err != nil {
		return Record{}, err
	}

	source.RetryOperationID = retry.ID
	if err := Update(root, source); err != nil {
		return Record{}, fmt.Errorf("record retry relationship for operation %s: %w", source.ID, err)
	}
	published = true
	return retry, nil
}

func stageOverrideInputs(operationDirectory string, document *plan.Document) error {
	if document.OverrideInputs.Empty() {
		return nil
	}
	if document.Overrides == nil {
		document.Overrides = make(map[string]fieldoverride.Component, len(document.OverrideInputs.Components))
	}
	for componentName, input := range document.OverrideInputs.Components {
		inputDirectory := filepath.Join(operationDirectory, "inputs", componentName)
		if err := os.MkdirAll(inputDirectory, 0o700); err != nil {
			return fmt.Errorf("create override input directory: %w", err)
		}
		staged := input
		if input.Chart != nil {
			chart := *input.Chart
			chart.StagedPath = filepath.Join(inputDirectory, "chart"+filepath.Ext(chart.SourcePath))
			if err := copyVerifiedInput(chart.SourcePath, chart.StagedPath, chart.SHA256, 0o644); err != nil {
				return fmt.Errorf("stage %s chart override: %w", componentName, err)
			}
			staged.Chart = &chart
		}
		if input.Values != nil {
			values := *input.Values
			values.StagedPath = filepath.Join(inputDirectory, "values"+filepath.Ext(values.SourcePath))
			if err := copyVerifiedInput(values.SourcePath, values.StagedPath, values.SHA256, 0o600); err != nil {
				return fmt.Errorf("stage %s values override: %w", componentName, err)
			}
			staged.Values = &values
		}
		staged.AnsibleVarsPath = filepath.Join(inputDirectory, "ansible-overrides.yaml")
		if err := writeOverrideVars(staged); err != nil {
			return fmt.Errorf("write %s override variables: %w", componentName, err)
		}
		staged.SetValues = nil
		document.Overrides[componentName] = staged
	}
	return nil
}

func cloneOverrideInputs(sourceDirectory, destinationDirectory string, document *plan.Document) error {
	for componentName, override := range document.Overrides {
		if !retryOverrideComponent(componentName) {
			return fmt.Errorf("operation plan has unsupported override component %q", componentName)
		}
		inputDirectory := filepath.Join(destinationDirectory, "inputs", componentName)
		if !isWithinDirectory(destinationDirectory, inputDirectory) {
			return fmt.Errorf("retry override input directory escapes operation directory: %s", componentName)
		}
		if err := os.MkdirAll(inputDirectory, 0o700); err != nil {
			return fmt.Errorf("create retry override input directory: %w", err)
		}
		cloned := override
		if override.Chart != nil {
			chart := *override.Chart
			chart.StagedPath = filepath.Join(inputDirectory, "chart"+filepath.Ext(chart.StagedPath))
			if err := copyStagedInput(sourceDirectory, override.Chart.StagedPath, chart.StagedPath, chart.SHA256, 0o644); err != nil {
				return fmt.Errorf("clone %s chart override: %w", componentName, err)
			}
			cloned.Chart = &chart
		}
		if override.Values != nil {
			values := *override.Values
			values.StagedPath = filepath.Join(inputDirectory, "values"+filepath.Ext(values.StagedPath))
			if err := copyStagedInput(sourceDirectory, override.Values.StagedPath, values.StagedPath, values.SHA256, 0o600); err != nil {
				return fmt.Errorf("clone %s values override: %w", componentName, err)
			}
			cloned.Values = &values
		}
		cloned.AnsibleVarsPath = filepath.Join(inputDirectory, "ansible-overrides.yaml")
		if err := cloneOverrideVars(sourceDirectory, override.AnsibleVarsPath, cloned); err != nil {
			return fmt.Errorf("clone %s override variables: %w", componentName, err)
		}
		cloned.SetValues = nil
		document.Overrides[componentName] = cloned
	}
	return nil
}

func retryOverrideComponent(component string) bool {
	switch component {
	case "app", "agent", "vision":
		return true
	default:
		return false
	}
}

func copyStagedInput(sourceDirectory, source, destination, expectedSHA256 string, mode os.FileMode) error {
	if !isWithinDirectory(sourceDirectory, source) {
		return errors.New("staged input escapes source operation directory")
	}
	return copyVerifiedInput(source, destination, expectedSHA256, mode)
}

func cloneOverrideVars(sourceDirectory, sourcePath string, override fieldoverride.Component) error {
	if !isWithinDirectory(sourceDirectory, sourcePath) {
		return errors.New("staged variables escape source operation directory")
	}
	contents, err := os.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	var variables map[string]any
	if err := yaml.Unmarshal(contents, &variables); err != nil {
		return err
	}
	if variables == nil {
		variables = make(map[string]any)
	}
	if override.Chart != nil {
		variables["eva_cli_chart_path"] = override.Chart.StagedPath
	}
	if override.Values != nil {
		variables["eva_cli_values_path"] = override.Values.StagedPath
	}
	encoded, err := yaml.Marshal(variables)
	if err != nil {
		return err
	}
	return writePrivateFile(override.AnsibleVarsPath, encoded)
}

func isWithinDirectory(directory, path string) bool {
	relative, err := filepath.Rel(directory, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func copyVerifiedInput(source, destination, expectedSHA256 string, mode os.FileMode) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("must be a regular non-symlink file: %s", source)
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if err := output.Chmod(mode); err != nil {
		output.Close()
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(output, hash), input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if actual := fmt.Sprintf("%x", hash.Sum(nil)); actual != expectedSHA256 {
		return fmt.Errorf("file changed while staging (sha256=%s, expected=%s)", actual, expectedSHA256)
	}
	return nil
}

func writeOverrideVars(component fieldoverride.Component) error {
	variables := make(map[string]any)
	if component.Chart != nil {
		variables["eva_cli_chart_path"] = component.Chart.StagedPath
	}
	if component.Values != nil {
		variables["eva_cli_values_path"] = component.Values.StagedPath
	}
	if len(component.SetValues) > 0 {
		variables["eva_cli_helm_set"] = component.SetValues
	}
	contents, err := yaml.Marshal(variables)
	if err != nil {
		return err
	}
	return writePrivateFile(component.AnsibleVarsPath, contents)
}

func Load(root, id string) (Record, error) {
	if root == "" {
		root = DefaultRoot
	}
	if !operationIDPattern.MatchString(id) {
		return Record{}, fmt.Errorf("invalid operation ID %q", id)
	}
	path := filepath.Join(root, id, "operation.yaml")
	contents, err := os.ReadFile(path)
	if err != nil {
		return Record{}, fmt.Errorf("read operation %s: %w", id, err)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(contents)))
	decoder.KnownFields(true)
	var record Record
	if err := decoder.Decode(&record); err != nil {
		return Record{}, fmt.Errorf("parse operation %s: %w", id, err)
	}
	if record.ID != id || record.Status == "" {
		return Record{}, fmt.Errorf("operation metadata is invalid: %s", path)
	}
	return record, nil
}

func LoadPlan(root string, record Record) (plan.Document, error) {
	if root == "" {
		root = DefaultRoot
	}
	path := filepath.Join(root, record.ID, "plan.yaml")
	contents, err := os.ReadFile(path)
	if err != nil {
		return plan.Document{}, fmt.Errorf("read operation plan %s: %w", record.ID, err)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(contents)))
	decoder.KnownFields(true)
	var document plan.Document
	if err := decoder.Decode(&document); err != nil {
		return plan.Document{}, fmt.Errorf("parse operation plan %s: %w", record.ID, err)
	}
	if document.SchemaVersion != plan.SchemaVersion || document.OperationID != record.ID {
		return plan.Document{}, fmt.Errorf("operation plan is invalid: %s", path)
	}
	return document, nil
}

func Update(root string, record Record) error {
	if root == "" {
		root = DefaultRoot
	}
	if !operationIDPattern.MatchString(record.ID) {
		return fmt.Errorf("invalid operation ID %q", record.ID)
	}
	contents, err := yaml.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal operation metadata: %w", err)
	}
	return writePrivateAtomically(filepath.Join(root, record.ID, "operation.yaml"), contents)
}

func WriteResult(root string, record Record, result Result) (string, error) {
	if root == "" {
		root = DefaultRoot
	}
	if !operationIDPattern.MatchString(record.ID) {
		return "", fmt.Errorf("invalid operation ID %q", record.ID)
	}
	contents, err := yaml.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal operation result: %w", err)
	}
	path := filepath.Join(root, record.ID, "result.yaml")
	if err := writePrivateAtomically(path, contents); err != nil {
		return "", err
	}
	return path, nil
}

func Latest(root string) (Record, error) {
	if root == "" {
		root = DefaultRoot
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return Record{}, fmt.Errorf("read operation root: %w", err)
	}
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !operationIDPattern.MatchString(entry.Name()) {
			continue
		}
		record, err := Load(root, entry.Name())
		if err != nil {
			return Record{}, err
		}
		records = append(records, record)
	}
	if len(records) == 0 {
		return Record{}, errors.New("no operations found")
	}
	sort.Slice(records, func(left, right int) bool {
		return records[left].UpdatedAt.After(records[right].UpdatedAt)
	})
	return records[0], nil
}

func newID(siteID string, now time.Time, reader io.Reader) (string, error) {
	if siteID == "" {
		return "", errors.New("operation site ID is required")
	}
	nonce := make([]byte, 4)
	if _, err := io.ReadFull(reader, nonce); err != nil {
		return "", fmt.Errorf("generate operation ID: %w", err)
	}
	return fmt.Sprintf("%s-%s-%s", now.UTC().Format("20060102T150405Z"), siteID, hex.EncodeToString(nonce)), nil
}

func writePrivateFile(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create private file %s: %w", path, err)
	}
	if _, err := file.Write(contents); err != nil {
		file.Close()
		return fmt.Errorf("write private file %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close private file %s: %w", path, err)
	}
	return nil
}

func writePrivateAtomically(path string, contents []byte) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".operation-*")
	if err != nil {
		return fmt.Errorf("create private temporary file %s: %w", path, err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("set private file permissions %s: %w", path, err)
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return fmt.Errorf("write private file %s: %w", path, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close private file %s: %w", path, err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish private file %s: %w", path, err)
	}
	return nil
}
