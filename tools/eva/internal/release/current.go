package release

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultCurrentReceiptPath is the root-managed, persistent release context.
const DefaultCurrentReceiptPath = "/var/lib/eva/releases/current.yaml"
const DefaultRemoteInboxRoot = "/var/lib/eva/inbox/releases"

type CurrentReceipt struct {
	SchemaVersion   string `yaml:"schema_version"`
	ReleaseRoot     string `yaml:"release_root"`
	ReleaseVersion  string `yaml:"release_version"`
	ReleaseIdentity string `yaml:"release_identity"`
	SelectedBy      string `yaml:"selected_by"`
	SelectedAt      string `yaml:"selected_at"`
}

type SelectionSource string

const (
	SelectionExplicit         SelectionSource = "explicit"
	SelectionCurrent          SelectionSource = "current"
	SelectionWorkingDirectory SelectionSource = "working-directory"
)

type Selected struct {
	Resolved Resolved
	Source   SelectionSource
}

type SelectionOptions struct {
	Explicit    string
	ReceiptPath string
	InboxRoot   string
}

// ReleaseIdentity binds the two immutable release descriptors that Remote
// delivery already carries independently. It intentionally contains no
// workspace or credential data.
func ReleaseIdentity(resolved Resolved) (string, error) {
	releaseDigest, err := regularFileDigest(filepath.Join(resolved.Root, "release.yaml"))
	if err != nil {
		return "", fmt.Errorf("digest release metadata: %w", err)
	}
	checksumsDigest, err := regularFileDigest(filepath.Join(resolved.Root, "checksums.sha256"))
	if err != nil {
		return "", fmt.Errorf("digest checksum manifest: %w", err)
	}
	digest := sha256.Sum256([]byte(releaseDigest + ":" + checksumsDigest))
	return hex.EncodeToString(digest[:]), nil
}

func WriteCurrentReceipt(path string, resolved Resolved, selectedBy string, now time.Time) error {
	if err := validateCurrentRoot(resolved.Root); err != nil {
		return err
	}
	validated, err := Resolve(resolved.Root)
	if err != nil {
		return fmt.Errorf("validate Current Release before writing receipt: %w", err)
	}
	resolved = validated
	identity, err := ReleaseIdentity(resolved)
	if err != nil {
		return err
	}
	if selectedBy == "" {
		return errors.New("current Release selected_by is required")
	}
	receipt := CurrentReceipt{
		SchemaVersion: "v1", ReleaseRoot: resolved.Root, ReleaseVersion: resolved.Metadata.Version,
		ReleaseIdentity: identity, SelectedBy: selectedBy, SelectedAt: now.UTC().Format(time.RFC3339),
	}
	contents, err := yaml.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("encode current Release receipt: %w", err)
	}
	return publishCurrentReceipt(path, contents)
}

// RestoreCurrentReceipt restores a previously validated receipt through the
// same atomic writer used for new receipts. The backup contents are preserved
// verbatim so a failed installer transaction returns to its exact receipt.
func RestoreCurrentReceipt(path, backupPath string) error {
	if _, _, err := LoadCurrentRelease(backupPath); err != nil {
		return fmt.Errorf("validate Current Release receipt backup: %w", err)
	}
	contents, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read Current Release receipt backup: %w", err)
	}
	return publishCurrentReceipt(path, contents)
}

// ClearCurrentReceipt removes a receipt after a failed first installation.
// It rejects links and special files rather than following an untrusted path.
func ClearCurrentReceipt(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Current Release receipt: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("current Release receipt is not a regular file")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove Current Release receipt: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync Current Release receipt directory: %w", err)
	}
	return nil
}

// LoadCurrentRelease verifies both the receipt and the Release it names.
func LoadCurrentRelease(path string) (CurrentReceipt, Resolved, error) {
	receipt, err := LoadCurrentReceipt(path)
	if err != nil {
		return CurrentReceipt{}, Resolved{}, err
	}
	resolved, err := resolveCurrent(receipt)
	if err != nil {
		return CurrentReceipt{}, Resolved{}, err
	}
	return receipt, resolved, nil
}

func publishCurrentReceipt(path string, contents []byte) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("current Release receipt is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read current Release receipt: %w", err)
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("create current Release receipt directory: %w", err)
	}
	if err := os.Chmod(directory, 0o750); err != nil {
		return fmt.Errorf("set current Release receipt directory mode: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".current.yaml-")
	if err != nil {
		return fmt.Errorf("create current Release receipt temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o640); err != nil {
		temporary.Close()
		return fmt.Errorf("set current Release receipt mode: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return fmt.Errorf("write current Release receipt: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync current Release receipt: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close current Release receipt: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("publish current Release receipt: %w", err)
	}
	return syncDirectory(directory)
}

func LoadCurrentReceipt(path string) (CurrentReceipt, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return CurrentReceipt{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return CurrentReceipt{}, errors.New("current Release receipt is not a regular file")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return CurrentReceipt{}, fmt.Errorf("read current Release receipt: %w", err)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(contents)))
	decoder.KnownFields(true)
	var receipt CurrentReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return CurrentReceipt{}, fmt.Errorf("parse current Release receipt: %w", err)
	}
	if receipt.SchemaVersion != "v1" || receipt.ReleaseRoot == "" || receipt.ReleaseVersion == "" || receipt.ReleaseIdentity == "" || receipt.SelectedBy == "" || receipt.SelectedAt == "" {
		return CurrentReceipt{}, errors.New("current Release receipt is incomplete or has an unsupported schema")
	}
	if !filepath.IsAbs(receipt.ReleaseRoot) || filepath.Clean(receipt.ReleaseRoot) != receipt.ReleaseRoot {
		return CurrentReceipt{}, errors.New("current Release receipt has an unsafe release_root")
	}
	if !tagPattern.MatchString(receipt.ReleaseVersion) {
		return CurrentReceipt{}, fmt.Errorf("current Release receipt has an invalid release_version %q", receipt.ReleaseVersion)
	}
	if _, err := time.Parse(time.RFC3339, receipt.SelectedAt); err != nil {
		return CurrentReceipt{}, fmt.Errorf("current Release receipt has an invalid selected_at: %w", err)
	}
	return receipt, nil
}

func Select(options SelectionOptions) (Selected, error) {
	if options.ReceiptPath == "" {
		options.ReceiptPath = DefaultCurrentReceiptPath
	}
	if options.InboxRoot == "" {
		options.InboxRoot = DefaultRemoteInboxRoot
	}
	if options.Explicit != "" {
		resolved, err := resolveSelectionInput(options.Explicit, options.InboxRoot)
		if err != nil {
			return Selected{}, err
		}
		return Selected{Resolved: resolved, Source: SelectionExplicit}, nil
	}
	receipt, err := LoadCurrentReceipt(options.ReceiptPath)
	if err == nil {
		resolved, err := resolveCurrent(receipt)
		if err != nil {
			return Selected{}, fmt.Errorf("validate current Release receipt: %w", err)
		}
		return Selected{Resolved: resolved, Source: SelectionCurrent}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Selected{}, fmt.Errorf("load current Release receipt: %w", err)
	}
	resolved, cwdErr := Resolve(".")
	if cwdErr != nil {
		return Selected{}, fmt.Errorf("no Current Release is registered and the working directory is not a valid Release: %w", cwdErr)
	}
	return Selected{Resolved: resolved, Source: SelectionWorkingDirectory}, nil
}

func resolveSelectionInput(input, inboxRoot string) (Resolved, error) {
	if tagPattern.MatchString(input) {
		return Resolve(filepath.Join(inboxRoot, input))
	}
	return Resolve(input)
}

func resolveCurrent(receipt CurrentReceipt) (Resolved, error) {
	if err := validateCurrentRoot(receipt.ReleaseRoot); err != nil {
		return Resolved{}, err
	}
	resolved, err := Resolve(receipt.ReleaseRoot)
	if err != nil {
		return Resolved{}, err
	}
	if resolved.Metadata.Version != receipt.ReleaseVersion {
		return Resolved{}, fmt.Errorf("receipt version %q does not match Release version %q", receipt.ReleaseVersion, resolved.Metadata.Version)
	}
	identity, err := ReleaseIdentity(resolved)
	if err != nil {
		return Resolved{}, err
	}
	if identity != receipt.ReleaseIdentity {
		return Resolved{}, errors.New("receipt identity does not match Release identity")
	}
	return resolved, nil
}

func validateCurrentRoot(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fmt.Errorf("Current Release root is not an absolute clean path: %s", root)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("read Current Release root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("Current Release root is not a non-symlink directory: %s", root)
	}
	for _, name := range []string{"release.yaml", "checksums.sha256"} {
		info, err := os.Lstat(filepath.Join(root, name))
		if err != nil {
			return fmt.Errorf("read Current Release %s: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("Current Release %s is not a regular non-symlink file", name)
		}
	}
	return nil
}

func regularFileDigest(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("not a regular non-symlink file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
