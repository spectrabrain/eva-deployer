package argocd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const pendingSchemaVersion = "v1"

const pendingFileName = "argocd-handoff.pending.yaml"

// PendingHandoff records a Git removal that has been pushed and observed by
// Argo CD, but whose live Kubernetes cleanup has not yet completed.
type PendingHandoff struct {
	SchemaVersion           string    `yaml:"schema_version"`
	SiteID                  string    `yaml:"site_id"`
	ClusterName             string    `yaml:"cluster_name"`
	ClusterServer           string    `yaml:"cluster_server"`
	Applications            []string  `yaml:"applications"`
	RegistrationApplication string    `yaml:"registration_application"`
	RegistrationRepository  string    `yaml:"registration_repository"`
	RegistrationBranch      string    `yaml:"registration_branch"`
	RegistrationManifest    string    `yaml:"registration_manifest"`
	RegistrationCommit      string    `yaml:"registration_commit"`
	CreatedAt               time.Time `yaml:"created_at"`
}

func PendingPath(root, siteID string) string {
	if root == "" {
		root = DefaultReceiptRoot
	}

	return filepath.Join(
		root,
		siteID,
		pendingFileName,
	)
}

func WritePending(
	root string,
	pending PendingHandoff,
) error {
	if err := validatePending(pending); err != nil {
		return err
	}

	if root == "" {
		root = DefaultReceiptRoot
	}

	rootPath, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf(
			"resolve Argo CD pending-state root: %w",
			err,
		)
	}

	if err := ensureReceiptDirectory(
		rootPath,
		0o750,
	); err != nil {
		return err
	}

	siteDirectory := filepath.Join(
		rootPath,
		pending.SiteID,
	)

	if !isPathWithin(rootPath, siteDirectory) {
		return errors.New(
			"Argo CD pending-state site directory " +
				"escapes state root",
		)
	}

	if err := ensureReceiptDirectory(
		siteDirectory,
		0o700,
	); err != nil {
		return err
	}

	path := filepath.Join(
		siteDirectory,
		pendingFileName,
	)

	if err := inspectExistingPendingPath(path); err != nil {
		return err
	}

	pending.CreatedAt = pending.CreatedAt.UTC()
	sort.Strings(pending.Applications)

	contents, err := yaml.Marshal(pending)
	if err != nil {
		return fmt.Errorf(
			"marshal Argo CD pending handoff: %w",
			err,
		)
	}

	temporary, err := os.CreateTemp(
		siteDirectory,
		".argocd-handoff-pending-*",
	)
	if err != nil {
		return fmt.Errorf(
			"create temporary Argo CD pending handoff: %w",
			err,
		)
	}

	temporaryPath := temporary.Name()
	published := false

	defer func() {
		if !published {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()

		return fmt.Errorf(
			"set temporary Argo CD pending-state permissions: %w",
			err,
		)
	}

	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()

		return fmt.Errorf(
			"write temporary Argo CD pending handoff: %w",
			err,
		)
	}

	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()

		return fmt.Errorf(
			"sync temporary Argo CD pending handoff: %w",
			err,
		)
	}

	if err := temporary.Close(); err != nil {
		return fmt.Errorf(
			"close temporary Argo CD pending handoff: %w",
			err,
		)
	}

	if err := os.Rename(
		temporaryPath,
		path,
	); err != nil {
		return fmt.Errorf(
			"publish Argo CD pending handoff: %w",
			err,
		)
	}

	published = true
	return nil
}

func LoadPending(
	root string,
	siteID string,
) (PendingHandoff, error) {
	if root == "" {
		root = DefaultReceiptRoot
	}

	rootPath, err := filepath.Abs(root)
	if err != nil {
		return PendingHandoff{}, fmt.Errorf(
			"resolve Argo CD pending-state root: %w",
			err,
		)
	}

	siteDirectory := filepath.Join(
		rootPath,
		siteID,
	)

	if !isPathWithin(rootPath, siteDirectory) {
		return PendingHandoff{}, errors.New(
			"Argo CD pending-state site directory " +
				"escapes state root",
		)
	}

	if err := inspectReceiptDirectory(
		siteDirectory,
	); err != nil {
		return PendingHandoff{}, err
	}

	path := filepath.Join(
		siteDirectory,
		pendingFileName,
	)

	info, err := os.Lstat(path)
	if err != nil {
		return PendingHandoff{}, fmt.Errorf(
			"read Argo CD pending handoff %s: %w",
			path,
			err,
		)
	}

	if info.Mode()&os.ModeSymlink != 0 ||
		!info.Mode().IsRegular() {
		return PendingHandoff{}, fmt.Errorf(
			"Argo CD pending handoff must be a "+
				"regular non-symlink file: %s",
			path,
		)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		return PendingHandoff{}, fmt.Errorf(
			"read Argo CD pending handoff %s: %w",
			path,
			err,
		)
	}

	decoder := yaml.NewDecoder(
		strings.NewReader(string(contents)),
	)
	decoder.KnownFields(true)

	var pending PendingHandoff

	if err := decoder.Decode(&pending); err != nil {
		return PendingHandoff{}, fmt.Errorf(
			"parse Argo CD pending handoff %s: %w",
			path,
			err,
		)
	}

	if err := validatePending(pending); err != nil {
		return PendingHandoff{}, fmt.Errorf(
			"validate Argo CD pending handoff %s: %w",
			path,
			err,
		)
	}

	if pending.SiteID != siteID {
		return PendingHandoff{}, fmt.Errorf(
			"Argo CD pending handoff site_id %q "+
				"does not match site %q",
			pending.SiteID,
			siteID,
		)
	}

	sort.Strings(pending.Applications)
	return pending, nil
}

func RemovePending(
	root string,
	siteID string,
) error {
	path := PendingPath(root, siteID)

	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf(
			"inspect Argo CD pending handoff %s: %w",
			path,
			err,
		)
	}

	if info.Mode()&os.ModeSymlink != 0 ||
		!info.Mode().IsRegular() {
		return fmt.Errorf(
			"Argo CD pending handoff must be a "+
				"regular non-symlink file: %s",
			path,
		)
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf(
			"remove Argo CD pending handoff %s: %w",
			path,
			err,
		)
	}

	return nil
}

func pendingToReceipt(
	pending PendingHandoff,
	completedAt time.Time,
) Receipt {
	return Receipt{
		SchemaVersion: receiptSchemaVersion,
		SiteID:        pending.SiteID,
		ClusterName:   pending.ClusterName,
		ClusterServer: pending.ClusterServer,
		Applications: append(
			[]string(nil),
			pending.Applications...,
		),
		CompletedAt:             completedAt.UTC(),
		RegistrationApplication: pending.RegistrationApplication,
		RegistrationRepository:  pending.RegistrationRepository,
		RegistrationBranch:      pending.RegistrationBranch,
		RegistrationManifest:    pending.RegistrationManifest,
		RegistrationCommit:      pending.RegistrationCommit,
	}
}

func validatePending(
	pending PendingHandoff,
) error {
	if pending.SchemaVersion != pendingSchemaVersion {
		return fmt.Errorf(
			"schema_version must be %q",
			pendingSchemaVersion,
		)
	}

	if pending.SiteID == "" {
		return errors.New("site_id is required")
	}

	if pending.ClusterName == "" {
		return errors.New("cluster_name is required")
	}

	if pending.ClusterServer == "" {
		return errors.New("cluster_server is required")
	}

	if pending.CreatedAt.IsZero() {
		return errors.New("created_at is required")
	}

	if len(pending.Applications) == 0 {
		return errors.New("applications are required")
	}

	seenApplications := map[string]bool{}

	for _, application := range pending.Applications {
		if !validLegacyApplication(application) {
			return fmt.Errorf(
				"invalid legacy Application %q",
				application,
			)
		}

		if seenApplications[application] {
			return fmt.Errorf(
				"duplicate legacy Application %q",
				application,
			)
		}

		seenApplications[application] = true
	}

	receipt := pendingToReceipt(
		pending,
		pending.CreatedAt,
	)

	if !hasCompleteGitReceiptMetadata(receipt) {
		return errors.New(
			"pending handoff requires complete " +
				"registration Git metadata",
		)
	}

	return nil
}

func inspectExistingPendingPath(path string) error {
	info, err := os.Lstat(path)

	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf(
			"inspect Argo CD pending handoff %s: %w",
			path,
			err,
		)
	}

	if info.Mode()&os.ModeSymlink != 0 ||
		!info.Mode().IsRegular() {
		return fmt.Errorf(
			"Argo CD pending handoff must be a "+
				"regular non-symlink file: %s",
			path,
		)
	}

	return nil
}
