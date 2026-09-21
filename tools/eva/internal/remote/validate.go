package remote

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"eva-deployer/tools/eva/internal/release"
	"gopkg.in/yaml.v3"
)

var imageReferencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)

// ValidatePreparation is intentionally read-only. It is shared by prepare's
// final step and the later verify command. The completed-manifest wrapper adds
// the final verification report after it has been written by prepare.
func ValidatePreparation(root string, resolved release.Resolved, identity PreparationIdentity, manifest Manifest) error {
	if err := EnsureIdentity(manifest, identity); err != nil {
		return err
	}
	for _, step := range manifest.Steps {
		for _, evidence := range step.Evidence {
			if err := nonEmptyRegularOrRegular(root, evidence, false); err != nil {
				return fmt.Errorf("step %q evidence: %w", step.Name, err)
			}
		}
	}
	if err := release.ValidateRemotePublish(resolved); err != nil {
		return err
	}
	if err := validateReleaseReport(root, resolved, identity); err != nil {
		return fmt.Errorf("release report: %w", err)
	}
	if err := validatePreflightReport(root, identity); err != nil {
		return fmt.Errorf("main preflight report: %w", err)
	}
	if err := validatePreparationAssets(root, identity); err != nil {
		return err
	}
	if _, err := LoadTargetPayload(TargetPayloadPath(root, identity), identity); err != nil {
		return fmt.Errorf("target payload: %w", err)
	}
	if err := validateSummaryReport(root, identity); err != nil {
		return fmt.Errorf("preparation summary: %w", err)
	}
	return nil
}

// ValidatePreparationAssets is the same read-only domain validator used while
// prepare is assembling its summary. It deliberately does not require the
// summary or final verification report to exist yet.
func ValidatePreparationAssets(root string, resolved release.Resolved, identity PreparationIdentity, manifest Manifest) error {
	if err := EnsureIdentity(manifest, identity); err != nil {
		return err
	}
	if err := release.ValidateRemotePublish(resolved); err != nil {
		return err
	}
	if err := validateReleaseReport(root, resolved, identity); err != nil {
		return fmt.Errorf("release report: %w", err)
	}
	return validatePreparationAssets(root, identity)
}

// ValidateCompletedPreparation is the read-only contract for a previously
// completed preparation. It never writes the manifest or reports.
func ValidateCompletedPreparation(root string, resolved release.Resolved, identity PreparationIdentity, manifest Manifest) error {
	if err := validateCompletedManifest(manifest, identity); err != nil {
		return err
	}
	if err := ValidatePreparation(root, resolved, identity, manifest); err != nil {
		return err
	}
	if err := validateVerificationReport(root, identity); err != nil {
		return fmt.Errorf("verification report: %w", err)
	}
	return nil
}

func validatePreparationAssets(root string, identity PreparationIdentity) error {
	if err := ValidateOfflineAssets(root); err != nil {
		return err
	}
	if err := ValidateImageLists(root, "images-all.txt", "images-pulled.txt", "images-missing.txt"); err != nil {
		return fmt.Errorf("product images: %w", err)
	}
	if err := ValidateImageLists(root, "infra-images-all.txt", "infra-images-pulled.txt", "infra-images-missing.txt"); err != nil {
		return fmt.Errorf("infra images: %w", err)
	}
	if err := ValidateRepositoryMapping(root, "cache/images/images-pulled.txt", "reports/repository-mapping-product.txt", identity.RepositoryRegistry, identity.RepositoryProject); err != nil {
		return fmt.Errorf("product mapping: %w", err)
	}
	if err := ValidateRepositoryMapping(root, "cache/images/infra-images-pulled.txt", "reports/repository-mapping-infra.txt", identity.RepositoryRegistry, identity.RepositoryProject); err != nil {
		return fmt.Errorf("infra mapping: %w", err)
	}
	if err := ValidateModels(root); err != nil {
		return err
	}
	if err := ValidateQdrantSnapshots(root); err != nil {
		return err
	}
	if err := ValidateQdrantArtifacts(root, identity.RepositoryRegistry, identity.RepositoryProject); err != nil {
		return err
	}
	return nil
}

func validateCompletedManifest(manifest Manifest, identity PreparationIdentity) error {
	if err := EnsureIdentity(manifest, identity); err != nil {
		return err
	}
	if manifest.Status != ManifestSucceeded {
		if manifest.Status == ManifestFailed {
			for _, step := range manifest.Steps {
				if step.Status == StepFailed {
					return fmt.Errorf("preparation failed at step %q", step.Name)
				}
			}
		}
		return fmt.Errorf("preparation manifest is %q, not succeeded", manifest.Status)
	}
	if manifest.CompletedAt.IsZero() {
		return errors.New("succeeded preparation manifest is missing completed_at")
	}
	if len(manifest.Steps) != len(DefaultStepNames) {
		return errors.New("preparation manifest step list does not match current contract")
	}
	for index, step := range manifest.Steps {
		if step.Name != DefaultStepNames[index] || step.Status != StepSucceeded {
			return fmt.Errorf("preparation step %q is not succeeded", step.Name)
		}
		if len(step.Evidence) == 0 {
			return fmt.Errorf("preparation step %q has no evidence", step.Name)
		}
	}
	return nil
}

func validateReleaseReport(root string, resolved release.Resolved, identity PreparationIdentity) error {
	report, err := readReport(root, "reports/release-validation.yaml")
	if err != nil {
		return err
	}
	want := map[string]string{"release_version": identity.ReleaseVersion, "release_yaml_sha256": identity.ReleaseYAMLSHA256, "checksums_sha256": identity.ChecksumsSHA256, "platform": resolved.Metadata.Platform.OS + "/" + resolved.Metadata.Platform.Arch, "offline_artifact": offlineArtifactName(resolved), "offline_artifact_sha256": offlineArtifactSHA256(resolved)}
	for key, value := range want {
		if report[key] != value {
			return fmt.Errorf("%s does not match preparation identity", key)
		}
	}
	return nil
}
func validatePreflightReport(root string, identity PreparationIdentity) error {
	report, err := readReport(root, "reports/main-preflight.yaml")
	if err != nil {
		return err
	}
	if report["schema_version"] != preflightSchemaVersion || report["release_version"] != identity.ReleaseVersion || report["registry"] != identity.RepositoryRegistry || report["project"] != identity.RepositoryProject {
		return errors.New("preflight identity does not match preparation identity")
	}
	categories, ok := report["categories"].([]any)
	if !ok || len(categories) != 6 {
		return errors.New("preflight categories are incomplete")
	}
	want := map[string]bool{"host-tools": true, "docker": true, "aws": true, "harbor": true, "storage": true, "external-sources": true}
	for _, category := range categories {
		value, ok := category.(string)
		if !ok || !want[value] {
			return errors.New("preflight categories are invalid")
		}
		delete(want, value)
	}
	if len(want) != 0 {
		return errors.New("preflight categories are incomplete")
	}
	return nil
}
func validateSummaryReport(root string, identity PreparationIdentity) error {
	report, err := readReport(root, "reports/preparation-summary.yaml")
	if err != nil {
		return err
	}
	if report["release_version"] != identity.ReleaseVersion || report["repository"] != identity.RepositoryRegistry+"/"+identity.RepositoryProject {
		return errors.New("summary identity does not match preparation identity")
	}
	assets, ok := report["assets"].([]any)
	if !ok || len(assets) != 6 {
		return errors.New("summary asset status is incomplete")
	}
	want := map[string]bool{"offline": true, "product-images": true, "infra-images": true, "models": true, "qdrant-snapshots": true, "target-payload": true}
	for _, asset := range assets {
		value, ok := asset.(string)
		if !ok || !want[value] {
			return errors.New("summary asset status is invalid")
		}
		delete(want, value)
	}
	if len(want) != 0 {
		return errors.New("summary asset status is incomplete")
	}
	return nil
}
func validateVerificationReport(root string, identity PreparationIdentity) error {
	report, err := readReport(root, "reports/verification.yaml")
	if err != nil {
		return err
	}
	if report["release_version"] != identity.ReleaseVersion || report["repository"] != identity.RepositoryRegistry+"/"+identity.RepositoryProject || report["status"] != "validated" {
		return errors.New("verification report does not match preparation identity")
	}
	return nil
}
func readReport(root, relative string) (map[string]any, error) {
	if err := nonEmptyRegular(root, relative); err != nil {
		return nil, err
	}
	contents, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil {
		return nil, err
	}
	if err := rejectSecretContent(strings.Split(string(contents), "\n")); err != nil {
		return nil, err
	}
	var report map[string]any
	decoder := yaml.NewDecoder(strings.NewReader(string(contents)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&report); err != nil {
		return nil, fmt.Errorf("parse report: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("report must contain exactly one YAML document")
		}
		return nil, fmt.Errorf("parse trailing report content: %w", err)
	}
	if len(report) == 0 {
		return nil, errors.New("report is empty")
	}
	return report, nil
}

func ValidateOfflineAssets(root string) error {
	for _, relative := range []string{"cache/manifest.txt", "cache/apt/debs/manifest.txt", "cache/docker/debs/manifest.txt", "cache/nvidia/container-toolkit-debs/manifest.txt", "cache/tools/oras"} {
		if err := nonEmptyRegular(root, relative); err != nil {
			return err
		}
	}
	for _, pattern := range []string{"cache/apt/debs/*.deb", "cache/docker/debs/*.deb", "cache/nvidia/container-toolkit-debs/*.deb", "cache/k3s/k3s-*-linux-amd64"} {
		if err := requireRegularGlob(root, pattern); err != nil {
			return err
		}
	}
	for _, pattern := range []string{"cache/eva-app/*.tgz", "cache/eva-vision/*.tgz", "cache/eva-agent/*.tgz", "cache/eva-agent/release/*/plugins/eva-agent-qdrant/post-renderer.sh", "cache/eva-agent/release/*/plugins/eva-agent-qdrant/plugin.yaml"} {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil || len(matches) == 0 {
			return fmt.Errorf("required offline asset is missing: %s", pattern)
		}
		for _, match := range matches {
			if err := nonEmptyRegular(root, mustRelative(root, match)); err != nil {
				return err
			}
		}
	}
	return nil
}

func ValidateImageLists(root, allName, pulledName, missingName string) error {
	all, err := imageList(root, filepath.Join("cache/images", allName), true)
	if err != nil {
		return err
	}
	pulled, err := imageList(root, filepath.Join("cache/images", pulledName), true)
	if err != nil {
		return err
	}
	missing, err := imageList(root, filepath.Join("cache/images", missingName), false)
	if err != nil {
		return err
	}
	if len(missing) != 0 {
		return errors.New("image missing list is not empty")
	}
	if !sameStrings(all, pulled) {
		return errors.New("all and pulled image lists differ")
	}
	return nil
}

func ValidateRepositoryMapping(root, listRelative, mappingRelative, registry, project string) error {
	sources, err := imageList(root, listRelative, true)
	if err != nil {
		return err
	}
	lines, err := readLines(root, mappingRelative, true)
	if err != nil {
		return err
	}
	mapped := make(map[string]string, len(lines))
	prefix := registry + "/" + project + "/"
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return fmt.Errorf("invalid repository mapping line %q", line)
		}
		if !imageReferencePattern.MatchString(fields[0]) || !strings.HasPrefix(fields[1], prefix) {
			return fmt.Errorf("invalid repository mapping %q", line)
		}
		if previous, exists := mapped[fields[0]]; exists && previous != fields[1] {
			return fmt.Errorf("conflicting mapping for %q", fields[0])
		}
		if _, exists := mapped[fields[0]]; exists {
			return fmt.Errorf("duplicate mapping for %q", fields[0])
		}
		mapped[fields[0]] = fields[1]
	}
	if len(mapped) != len(sources) {
		return errors.New("repository mapping count does not match image list")
	}
	for _, source := range sources {
		if _, ok := mapped[source]; !ok {
			return fmt.Errorf("repository mapping missing source %q", source)
		}
	}
	return nil
}

func ValidateModels(root string) error {
	manifest := "cache/models/manifest.txt"
	lines, err := readLines(root, manifest, true)
	if err != nil {
		return err
	}
	if err := rejectSecretContent(lines); err != nil {
		return err
	}
	for _, tree := range []string{"cache/models/agent", "cache/models/vllm"} {
		if err := secureNonEmptyTree(root, tree); err != nil {
			return err
		}
	}
	files := filesAfter(lines, "files:")
	if len(files) == 0 {
		return errors.New("model manifest has no files")
	}
	for _, name := range files {
		relative, err := manifestPathRelative(root, name)
		if err != nil {
			return fmt.Errorf("invalid model manifest file: %w", err)
		}
		if err := nonEmptyRegular(root, relative); err != nil {
			return err
		}
	}
	return nil
}

func ValidateQdrantSnapshots(root string) error {
	lines, err := readLines(root, "cache/qdrant-snapshots/manifest.txt", true)
	if err != nil {
		return err
	}
	if err := rejectSecretContent(lines); err != nil {
		return err
	}
	specs := snapshotSpecs(lines)
	if len(specs) == 0 {
		return errors.New("snapshot manifest has no snapshot specs")
	}
	seen := map[string]bool{}
	for _, spec := range specs {
		if filepath.Base(spec.file) != spec.file || seen[spec.file] {
			return fmt.Errorf("invalid or duplicate snapshot file %q", spec.file)
		}
		seen[spec.file] = true
		if err := nonEmptyRegular(root, filepath.Join("cache/qdrant-snapshots", spec.file)); err != nil {
			return err
		}
	}
	return secureTree(root, "cache/qdrant-snapshots")
}

func ValidateQdrantArtifacts(root, registry, project string) error {
	lines, err := readLines(root, "reports/qdrant-artifacts.txt", true)
	if err != nil {
		return err
	}
	if err := rejectSecretContent(lines); err != nil {
		return err
	}
	manifest, err := readLines(root, "cache/qdrant-snapshots/manifest.txt", true)
	if err != nil {
		return err
	}
	specs := snapshotSpecs(manifest)
	if len(specs) == 0 {
		return errors.New("snapshot manifest has no snapshot specs")
	}
	expected := make(map[string]snapshotSpec, len(specs))
	for _, spec := range specs {
		expected[spec.file] = spec
	}
	seenFiles, seenTags := map[string]bool{}, map[string]bool{}
	prefix := registry + "/" + project + "/qdrant-snapshots:"
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 3 || !strings.HasPrefix(parts[0], prefix) || parts[1] == "" || parts[2] == "" {
			return fmt.Errorf("invalid Qdrant artifact mapping %q", line)
		}
		tag := strings.TrimPrefix(parts[0], prefix)
		if tag == "" || seenTags[tag] || seenFiles[parts[1]] {
			return fmt.Errorf("duplicate Qdrant artifact mapping %q", line)
		}
		spec, ok := expected[parts[1]]
		if !ok || spec.collection != parts[2] {
			return fmt.Errorf("Qdrant artifact does not match snapshot spec %q", line)
		}
		seenFiles[parts[1]], seenTags[tag] = true, true
	}
	if len(seenFiles) != len(expected) {
		return errors.New("Qdrant artifact mappings are incomplete")
	}
	return nil
}

type snapshotSpec struct{ file, collection string }

func snapshotSpecs(lines []string) []snapshotSpec {
	inSpecs := false
	values := []snapshotSpec{}
	for _, line := range lines {
		if strings.TrimSpace(line) == "snapshot_specs:" {
			inSpecs = true
			continue
		}
		if strings.TrimSpace(line) == "files:" {
			break
		}
		if !inSpecs || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		parts := strings.Split(strings.TrimSpace(line), "|")
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			continue
		}
		values = append(values, snapshotSpec{file: parts[1], collection: parts[2]})
	}
	return values
}

func imageList(root, relative string, requireNonEmpty bool) ([]string, error) {
	lines, err := readLines(root, relative, false)
	if err != nil {
		return nil, err
	}
	values, seen := []string{}, map[string]bool{}
	for _, line := range lines {
		value := strings.TrimSpace(line)
		if value == "" {
			continue
		}
		if !imageReferencePattern.MatchString(value) || seen[value] {
			return nil, fmt.Errorf("invalid or duplicate image reference %q", value)
		}
		seen[value] = true
		values = append(values, value)
	}
	if requireNonEmpty && len(values) == 0 {
		return nil, fmt.Errorf("required image list is empty: %s", relative)
	}
	sort.Strings(values)
	return values, nil
}
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func readLines(root, relative string, nonEmpty bool) ([]string, error) {
	if err := nonEmptyRegularOrRegular(root, relative, nonEmpty); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n"), nil
}
func nonEmptyRegular(root, relative string) error {
	return nonEmptyRegularOrRegular(root, relative, true)
}
func nonEmptyRegularOrRegular(root, relative string, nonEmpty bool) error {
	path, err := safePath(root, relative)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("required evidence is missing: %s", relative)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || (nonEmpty && info.Size() == 0) {
		return fmt.Errorf("required evidence is not a non-empty regular file: %s", relative)
	}
	return nil
}
func requireRegularGlob(root, pattern string) error {
	matches, err := filepath.Glob(filepath.Join(root, pattern))
	if err != nil || len(matches) == 0 {
		return fmt.Errorf("required offline asset is missing: %s", pattern)
	}
	for _, match := range matches {
		if err := nonEmptyRegular(root, mustRelative(root, match)); err != nil {
			return err
		}
	}
	return nil
}
func safePath(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || filepath.Clean(relative) != relative || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == ".." {
		return "", fmt.Errorf("unsafe preparation path %q", relative)
	}
	return filepath.Join(root, relative), nil
}
func mustRelative(root, path string) string { relative, _ := filepath.Rel(root, path); return relative }
func secureNonEmptyTree(root, relative string) error {
	if err := secureTree(root, relative); err != nil {
		return err
	}
	found := false
	err := filepath.Walk(filepath.Join(root, relative), func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && info.Size() > 0 {
			found = true
		}
		return nil
	})
	if err != nil || !found {
		return fmt.Errorf("required directory is empty: %s", relative)
	}
	return nil
}
func secureTree(root, relative string) error {
	path, err := safePath(root, relative)
	if err != nil {
		return err
	}
	return filepath.Walk(path, func(name string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Errorf("unsafe file in preparation cache: %s", mustRelative(root, name))
		}
		return nil
	})
}
func filesAfter(lines []string, header string) []string {
	values := []string{}
	found := false
	for _, line := range lines {
		if strings.TrimSpace(line) == header {
			found = true
			continue
		}
		if found && strings.TrimSpace(line) != "" {
			values = append(values, strings.TrimSpace(line))
		}
	}
	return values
}
func manifestPathRelative(root, value string) (string, error) {
	if filepath.IsAbs(value) {
		relative, err := filepath.Rel(root, value)
		if err != nil {
			return "", err
		}
		value = relative
	}
	if _, err := safePath(root, value); err != nil {
		return "", err
	}
	return value, nil
}
func rejectSecretContent(lines []string) error {
	lower := strings.ToLower(strings.Join(lines, "\n"))
	for _, marker := range []string{"aws_access_key", "aws_secret", "session_token", "authorization", "password:", "credential", "bearer "} {
		if strings.Contains(lower, marker) {
			return errors.New("preparation evidence contains secret-like content")
		}
	}
	return nil
}

// report writes only non-sensitive structured facts, atomically.
func writeYAMLReport(root, relative string, value any) error {
	path, err := safePath(root, relative)
	if err != nil {
		return err
	}
	contents, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	if err := rejectSecretContent(strings.Split(string(contents), "\n")); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".report-")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err = temp.Write(contents); err == nil {
		err = temp.Chmod(0o640)
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	return err
}
