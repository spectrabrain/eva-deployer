// Package prune removes per-version Release and Remote payload trees that
// nothing current still needs. Nothing else in the tool deletes them, so they
// accumulate on Main and on every Remote target with each delivered version.
package prune

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"eva-deployer/tools/eva/internal/operation"
	"eva-deployer/tools/eva/internal/release"
	"eva-deployer/tools/eva/internal/remote"

	"gopkg.in/yaml.v3"
)

// Location is a directory whose direct children are named by Release version.
type Location struct {
	Label string
	Root  string
}

// DefaultLocations are the version-keyed trees on Main and Remote targets.
// Shared, version-independent state (the Remote cache, runtime, Harbor,
// workspaces, credentials, backups and operation history) is deliberately
// not listed: removing it breaks the current version, not only old ones.
func DefaultLocations() []Location {
	return []Location{
		{Label: "delivered release", Root: release.DefaultRemoteInboxRoot},
		{Label: "prepared release", Root: release.DefaultInstallRoot},
		{Label: "target payload", Root: remote.DefaultTargetPayloadRoot},
		{Label: "remote preparation", Root: remote.DefaultPreparationRoot},
	}
}

type Options struct {
	Locations          []Location
	CurrentReceiptPath string
	OperationRoot      string
	// Keep is how many of the newest versions to retain in addition to the
	// protected ones. At least 1, so a version being prepared or delivered
	// right now (newer than current) is never removed.
	Keep int
}

type Entry struct {
	Location Location
	Version  string
	Path     string
	Bytes    int64
	Reasons  []string
}

type Plan struct {
	CurrentVersion string
	CurrentRoot    string
	// Protected maps a normalized version (no "v" prefix) to why it must stay.
	Protected map[string][]string
	Keep      []Entry
	Delete    []Entry
	Warnings  []string
}

func (p Plan) DeleteBytes() int64 {
	var total int64
	for _, entry := range p.Delete {
		total += entry.Bytes
	}
	return total
}

// Build decides what to remove without touching anything. Any doubt about
// what is in use (unreadable receipt or operation, running operation) is an
// error rather than a guess.
func Build(options Options) (Plan, error) {
	if options.Keep < 1 {
		return Plan{}, errors.New("--keep must be at least 1")
	}
	plan := Plan{Protected: map[string][]string{}}
	protect := func(version, reason string) {
		if version == "" {
			return
		}
		key := normalizeVersion(version)
		for _, existing := range plan.Protected[key] {
			if existing == reason {
				return
			}
		}
		plan.Protected[key] = append(plan.Protected[key], reason)
	}

	receipt, found, err := readCurrentReceipt(options.CurrentReceiptPath)
	if err != nil {
		return Plan{}, err
	}
	if found {
		plan.CurrentVersion = receipt.ReleaseVersion
		plan.CurrentRoot = filepath.Clean(receipt.ReleaseRoot)
		protect(receipt.ReleaseVersion, "current release")
	}

	records, err := listOperations(options.OperationRoot)
	if err != nil {
		return Plan{}, err
	}
	var latest, lastSucceeded *operation.Record
	for index := range records {
		record := &records[index]
		switch record.Status {
		case operation.Running:
			liveness, err := operationLiveness(options.OperationRoot, *record)
			if err != nil {
				return Plan{}, err
			}
			switch liveness {
			case operation.LivenessActive:
				return Plan{}, fmt.Errorf("operation %s is running; prune after it finishes", record.ID)
			case operation.LivenessInterrupted:
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("operation %s is recorded as running but its process has exited (interrupted); its release %s is kept", record.ID, record.ReleaseVersion))
				protect(record.ReleaseVersion, "interrupted operation "+record.ID)
			default:
				// Written by a tool without a run lock: cannot tell whether it
				// is still alive, so keep everything it may be using.
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("operation %s is recorded as running by an older eva without a run lock; if it is no longer running (e.g. stopped with Ctrl+C) this is safe, and its release %s is kept", record.ID, record.ReleaseVersion))
				protect(record.ReleaseVersion, "running operation "+record.ID)
			}
		case operation.Planned:
			protect(record.ReleaseVersion, "planned operation "+record.ID)
		}
		if latest == nil || record.UpdatedAt.After(latest.UpdatedAt) {
			latest = record
		}
		if record.Status == operation.Succeeded && (lastSucceeded == nil || record.UpdatedAt.After(lastSucceeded.UpdatedAt)) {
			lastSucceeded = record
		}
	}
	if latest != nil {
		protect(latest.ReleaseVersion, "latest operation "+latest.ID)
	}
	if lastSucceeded != nil {
		protect(lastSucceeded.ReleaseVersion, "last succeeded operation "+lastSucceeded.ID)
	}

	entries := []Entry{}
	versions := map[string]bool{}
	for _, location := range options.Locations {
		found, err := versionDirectories(location)
		if err != nil {
			return Plan{}, err
		}
		for _, entry := range found {
			versions[entry.Version] = true
			entries = append(entries, entry)
		}
	}

	ordered := make([]string, 0, len(versions))
	for version := range versions {
		ordered = append(ordered, version)
	}
	sort.Slice(ordered, func(left, right int) bool { return compareVersions(ordered[left], ordered[right]) > 0 })
	newest := map[string]bool{}
	for index := 0; index < len(ordered) && index < options.Keep; index++ {
		newest[ordered[index]] = true
	}

	for _, entry := range entries {
		entry.Reasons = append(entry.Reasons, plan.Protected[normalizeVersion(entry.Version)]...)
		if plan.CurrentRoot != "" && (entry.Path == plan.CurrentRoot || strings.HasPrefix(plan.CurrentRoot, entry.Path+string(filepath.Separator))) {
			entry.Reasons = append(entry.Reasons, "contains current release root")
		}
		if newest[entry.Version] {
			entry.Reasons = append(entry.Reasons, fmt.Sprintf("newest %d", options.Keep))
		}
		if len(entry.Reasons) > 0 {
			plan.Keep = append(plan.Keep, entry)
			continue
		}
		bytes, err := treeSize(entry.Path)
		if err != nil {
			return Plan{}, err
		}
		entry.Bytes = bytes
		plan.Delete = append(plan.Delete, entry)
	}
	sortEntries(plan.Keep)
	sortEntries(plan.Delete)
	return plan, nil
}

// Execute removes the planned entries. It re-checks each path is still a real
// directory directly under its location before removing it.
func Execute(plan Plan, removed func(Entry)) error {
	for _, entry := range plan.Delete {
		if filepath.Dir(entry.Path) != filepath.Clean(entry.Location.Root) || !validVersionName(filepath.Base(entry.Path)) {
			return fmt.Errorf("refusing to remove unexpected path %s", entry.Path)
		}
		info, err := os.Lstat(entry.Path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect %s: %w", entry.Path, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("refusing to remove non-directory %s", entry.Path)
		}
		if err := os.RemoveAll(entry.Path); err != nil {
			return fmt.Errorf("remove %s: %w", entry.Path, err)
		}
		if removed != nil {
			removed(entry)
		}
	}
	return nil
}

func readCurrentReceipt(path string) (release.CurrentReceipt, bool, error) {
	if path == "" {
		path = release.DefaultCurrentReceiptPath
	}
	contents, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return release.CurrentReceipt{}, false, nil
	}
	if err != nil {
		return release.CurrentReceipt{}, false, fmt.Errorf("read current release receipt %s: %w", path, err)
	}
	var receipt release.CurrentReceipt
	if err := yaml.Unmarshal(contents, &receipt); err != nil {
		return release.CurrentReceipt{}, false, fmt.Errorf("parse current release receipt %s: %w", path, err)
	}
	if receipt.ReleaseVersion == "" || receipt.ReleaseRoot == "" {
		return release.CurrentReceipt{}, false, fmt.Errorf("current release receipt %s has no release_version or release_root", path)
	}
	return receipt, true, nil
}

func operationLiveness(root string, record operation.Record) (string, error) {
	if root == "" {
		root = operation.DefaultRoot
	}
	return operation.RunLiveness(root, record)
}

func listOperations(root string) ([]operation.Record, error) {
	if root == "" {
		root = operation.DefaultRoot
	}
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	records, err := operation.List(root, "")
	if err != nil {
		return nil, fmt.Errorf("read operation history: %w", err)
	}
	return records, nil
}

func versionDirectories(location Location) ([]Entry, error) {
	children, err := os.ReadDir(location.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", location.Root, err)
	}
	entries := []Entry{}
	for _, child := range children {
		// Only real directories named like a version; hidden staging/probe
		// directories and symlinks belong to in-flight or foreign work.
		if child.Type()&fs.ModeSymlink != 0 || !child.IsDir() || !validVersionName(child.Name()) {
			continue
		}
		entries = append(entries, Entry{
			Location: location,
			Version:  child.Name(),
			Path:     filepath.Join(filepath.Clean(location.Root), child.Name()),
		})
	}
	return entries, nil
}

func validVersionName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return false
	}
	if name[0] < '0' || name[0] > '9' {
		if !(name[0] == 'v' && len(name) > 1 && name[1] >= '0' && name[1] <= '9') {
			return false
		}
	}
	for _, character := range name {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || strings.ContainsRune(".-+_", character)) {
			return false
		}
	}
	return true
}

func treeSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measure %s: %w", root, err)
	}
	return total, nil
}

func sortEntries(entries []Entry) {
	sort.SliceStable(entries, func(left, right int) bool {
		if entries[left].Version != entries[right].Version {
			return compareVersions(entries[left].Version, entries[right].Version) > 0
		}
		return entries[left].Path < entries[right].Path
	})
}

// compareVersions orders SemVer-like names ("3.4.0", "v3.2.0",
// "3.4.0-alpha.02"); a release sorts after its pre-releases.
func compareVersions(left, right string) int {
	leftCore, leftPre := splitVersion(left)
	rightCore, rightPre := splitVersion(right)
	if result := compareIdentifiers(strings.Split(leftCore, "."), strings.Split(rightCore, "."), true); result != 0 {
		return result
	}
	switch {
	case leftPre == "" && rightPre == "":
		return strings.Compare(left, right)
	case leftPre == "":
		return 1
	case rightPre == "":
		return -1
	}
	if result := compareIdentifiers(strings.Split(leftPre, "."), strings.Split(rightPre, "."), false); result != 0 {
		return result
	}
	return strings.Compare(left, right)
}

func normalizeVersion(version string) string { return strings.TrimPrefix(version, "v") }

func splitVersion(version string) (string, string) {
	version = strings.TrimPrefix(version, "v")
	version, _, _ = strings.Cut(version, "+")
	core, pre, _ := strings.Cut(version, "-")
	return core, pre
}

func compareIdentifiers(left, right []string, padNumeric bool) int {
	for index := 0; index < len(left) || index < len(right); index++ {
		if index >= len(left) || index >= len(right) {
			if padNumeric {
				var value string
				sign := 1
				if index >= len(left) {
					value, sign = right[index], -1
				} else {
					value = left[index]
				}
				if number, err := strconv.Atoi(value); err == nil && number == 0 {
					continue
				}
				return sign
			}
			if index >= len(left) {
				return -1
			}
			return 1
		}
		leftNumber, leftErr := strconv.Atoi(left[index])
		rightNumber, rightErr := strconv.Atoi(right[index])
		switch {
		case leftErr == nil && rightErr == nil:
			if leftNumber != rightNumber {
				if leftNumber > rightNumber {
					return 1
				}
				return -1
			}
		case leftErr == nil:
			return -1
		case rightErr == nil:
			return 1
		default:
			if result := strings.Compare(left[index], right[index]); result != 0 {
				return result
			}
		}
	}
	return 0
}
