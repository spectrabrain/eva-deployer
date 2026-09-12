package operation

import (
	"crypto/rand"
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
	ID             string    `yaml:"id"`
	Status         string    `yaml:"status"`
	CreatedAt      time.Time `yaml:"created_at"`
	UpdatedAt      time.Time `yaml:"updated_at"`
	StartedAt      time.Time `yaml:"started_at,omitempty"`
	CompletedAt    time.Time `yaml:"completed_at,omitempty"`
	SiteID         string    `yaml:"site_id"`
	ReleaseVersion string    `yaml:"release_version"`
	PlanPath       string    `yaml:"plan_path"`
	ResultPath     string    `yaml:"result_path,omitempty"`
	LogDirectory   string    `yaml:"log_directory,omitempty"`
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

	document.OperationID = id
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
		PlanPath:       planPath,
	}
	contents, err := yaml.Marshal(record)
	if err != nil {
		return Record{}, fmt.Errorf("marshal operation metadata: %w", err)
	}
	if err := writePrivateFile(filepath.Join(directory, "operation.yaml"), contents); err != nil {
		return Record{}, err
	}
	return record, nil
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
