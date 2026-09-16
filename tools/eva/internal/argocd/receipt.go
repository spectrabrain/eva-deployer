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

const DefaultReceiptRoot = "/var/lib/eva/sites"

const receiptSchemaVersion = "v1"

const receiptFileName = "argocd-handoff.yaml"

// Receipt records a verified ownership handoff. Applications can remain as
// tracking metadata on deployed resources after their Argo CD Application
// objects and cluster registration have been removed.
type Receipt struct {
	SchemaVersion string    `yaml:"schema_version"`
	SiteID        string    `yaml:"site_id"`
	ClusterName   string    `yaml:"cluster_name"`
	ClusterServer string    `yaml:"cluster_server,omitempty"`
	Applications  []string  `yaml:"applications"`
	CompletedAt   time.Time `yaml:"completed_at"`
}

func ReceiptPath(root, siteID string) string {
	if root == "" {
		root = DefaultReceiptRoot
	}

	return filepath.Join(root, siteID, receiptFileName)
}

// WriteReceipt validates and atomically publishes a private site-level receipt.
func WriteReceipt(root string, receipt Receipt) error {
	if err := validateReceipt(receipt); err != nil {
		return err
	}

	if root == "" {
		root = DefaultReceiptRoot
	}

	rootPath, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve Argo CD receipt root: %w", err)
	}

	if err := ensureReceiptDirectory(rootPath, 0o750); err != nil {
		return err
	}

	siteDirectory := filepath.Join(rootPath, receipt.SiteID)
	if !isPathWithin(rootPath, siteDirectory) {
		return errors.New(
			"Argo CD receipt site directory escapes receipt root",
		)
	}

	if err := ensureReceiptDirectory(siteDirectory, 0o700); err != nil {
		return err
	}

	path := filepath.Join(siteDirectory, receiptFileName)

	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 ||
			!info.Mode().IsRegular() {
			return fmt.Errorf(
				"Argo CD handoff receipt must be a regular "+
					"non-symlink file: %s",
				path,
			)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf(
			"inspect Argo CD handoff receipt %s: %w",
			path,
			err,
		)
	}

	receipt.CompletedAt = receipt.CompletedAt.UTC()
	sort.Strings(receipt.Applications)

	contents, err := yaml.Marshal(receipt)
	if err != nil {
		return fmt.Errorf(
			"marshal Argo CD handoff receipt: %w",
			err,
		)
	}

	temporary, err := os.CreateTemp(
		siteDirectory,
		".argocd-handoff-*",
	)
	if err != nil {
		return fmt.Errorf(
			"create temporary Argo CD handoff receipt: %w",
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
			"set temporary Argo CD receipt permissions: %w",
			err,
		)
	}

	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return fmt.Errorf(
			"write temporary Argo CD handoff receipt: %w",
			err,
		)
	}

	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf(
			"sync temporary Argo CD handoff receipt: %w",
			err,
		)
	}

	if err := temporary.Close(); err != nil {
		return fmt.Errorf(
			"close temporary Argo CD handoff receipt: %w",
			err,
		)
	}

	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf(
			"publish Argo CD handoff receipt: %w",
			err,
		)
	}

	published = true
	return nil
}

// LoadReceipt loads a private receipt and fails closed on malformed,
// mismatched, unsafe, or unsupported state.
func LoadReceipt(root, siteID string) (Receipt, error) {
	if root == "" {
		root = DefaultReceiptRoot
	}

	rootPath, err := filepath.Abs(root)
	if err != nil {
		return Receipt{}, fmt.Errorf(
			"resolve Argo CD receipt root: %w",
			err,
		)
	}

	siteDirectory := filepath.Join(rootPath, siteID)
	if !isPathWithin(rootPath, siteDirectory) {
		return Receipt{}, errors.New(
			"Argo CD receipt site directory escapes receipt root",
		)
	}

	if err := inspectReceiptDirectory(siteDirectory); err != nil {
		return Receipt{}, err
	}

	path := filepath.Join(siteDirectory, receiptFileName)
	info, err := os.Lstat(path)
	if err != nil {
		return Receipt{}, fmt.Errorf(
			"read Argo CD handoff receipt %s: %w",
			path,
			err,
		)
	}

	if info.Mode()&os.ModeSymlink != 0 ||
		!info.Mode().IsRegular() {
		return Receipt{}, fmt.Errorf(
			"Argo CD handoff receipt must be a regular "+
				"non-symlink file: %s",
			path,
		)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		return Receipt{}, fmt.Errorf(
			"read Argo CD handoff receipt %s: %w",
			path,
			err,
		)
	}

	decoder := yaml.NewDecoder(
		strings.NewReader(string(contents)),
	)
	decoder.KnownFields(true)

	var receipt Receipt
	if err := decoder.Decode(&receipt); err != nil {
		return Receipt{}, fmt.Errorf(
			"parse Argo CD handoff receipt %s: %w",
			path,
			err,
		)
	}

	if err := validateReceipt(receipt); err != nil {
		return Receipt{}, fmt.Errorf(
			"validate Argo CD handoff receipt %s: %w",
			path,
			err,
		)
	}

	if receipt.SiteID != siteID {
		return Receipt{}, fmt.Errorf(
			"Argo CD handoff receipt site_id %q "+
				"does not match site %q",
			receipt.SiteID,
			siteID,
		)
	}

	sort.Strings(receipt.Applications)
	return receipt, nil
}

func ReceiptCovers(
	receipt Receipt,
	detection Detection,
) error {
	if receipt.SiteID != detection.SiteID {
		return fmt.Errorf(
			"Argo CD handoff receipt site_id %q "+
				"does not match detected site %q",
			receipt.SiteID,
			detection.SiteID,
		)
	}

	recorded := make(
		map[string]struct{},
		len(receipt.Applications),
	)
	for _, application := range receipt.Applications {
		recorded[application] = struct{}{}
	}

	for _, application := range detection.Applications {
		if _, ok := recorded[application]; !ok {
			return fmt.Errorf(
				"Argo CD handoff receipt does not cover "+
					"detected Application %q",
				application,
			)
		}
	}

	return nil
}

func validateReceipt(receipt Receipt) error {
	if receipt.SchemaVersion != receiptSchemaVersion {
		return fmt.Errorf(
			"schema_version must be %q",
			receiptSchemaVersion,
		)
	}

	if receipt.SiteID == "" {
		return errors.New("site_id is required")
	}

	if receipt.ClusterName == "" {
		return errors.New("cluster_name is required")
	}

	if receipt.CompletedAt.IsZero() {
		return errors.New("completed_at is required")
	}

	if len(receipt.Applications) == 0 {
		return errors.New("applications are required")
	}

	seen := map[string]bool{}
	for _, application := range receipt.Applications {
		if !validLegacyApplication(application) {
			return fmt.Errorf(
				"invalid legacy Application %q",
				application,
			)
		}

		if seen[application] {
			return fmt.Errorf(
				"duplicate legacy Application %q",
				application,
			)
		}

		seen[application] = true
	}

	return nil
}

func ensureReceiptDirectory(
	path string,
	mode os.FileMode,
) error {
	info, err := os.Lstat(path)

	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, mode); err != nil {
			return fmt.Errorf(
				"create Argo CD receipt directory %s: %w",
				path,
				err,
			)
		}

		info, err = os.Lstat(path)
	}

	if err != nil {
		return fmt.Errorf(
			"inspect Argo CD receipt directory %s: %w",
			path,
			err,
		)
	}

	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf(
			"Argo CD receipt directory must be a "+
				"non-symlink directory: %s",
			path,
		)
	}

	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf(
			"set Argo CD receipt directory mode %s: %w",
			path,
			err,
		)
	}

	return nil
}

func inspectReceiptDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf(
			"read Argo CD receipt directory %s: %w",
			path,
			err,
		)
	}

	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf(
			"Argo CD receipt directory must be a "+
				"non-symlink directory: %s",
			path,
		)
	}

	return nil
}

func isPathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil &&
		relative != ".." &&
		!strings.HasPrefix(
			relative,
			".."+string(filepath.Separator),
		)
}
