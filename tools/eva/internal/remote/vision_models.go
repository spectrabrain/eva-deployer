package remote

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"
)

// Keep this contract aligned with scripts/lib/vision_models.py. Model names
// retain their original case in the cache; only OCI repository names fold case.
const defaultVisionModels = "Owl-v2,OmDet,LLMDet,RT-DETRV2,DINOv2Small,E5EmbeddingSmall,Qwen3VLEmbed2B"
const visionModelsSource = "s3://spectrabrain-eva-vision/model_dir"

var visionModelName = regexp.MustCompile(`^[A-Za-z0-9]+(?:(?:__?|-+)[A-Za-z0-9]+)*$`)
var visionComponent = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var visionProject = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

type visionFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type visionModel struct {
	Name  string       `json:"name"`
	Files []visionFile `json:"files"`
}

type visionManifest struct {
	SchemaVersion string        `json:"schema_version"`
	Source        string        `json:"source"`
	Models        []visionModel `json:"models"`
}

func visionModelSelection() ([]string, error) {
	value, set := os.LookupEnv("EVA_VISION_MODELS")
	if !set {
		value = defaultVisionModels
	}
	names := strings.Split(value, ",")
	seen := map[string]bool{}
	for i, name := range names {
		name = strings.TrimSpace(name)
		if len(name) > 128 || !visionModelName.MatchString(name) || !safeVisionComponent(name) || seen[strings.ToLower(name)] {
			return nil, errors.New("invalid or duplicate Vision model selection")
		}
		names[i], seen[strings.ToLower(name)] = name, true
	}
	return names, nil
}

func safeVisionComponent(name string) bool {
	lower := strings.ToLower(name)
	if !visionComponent.MatchString(name) || name == "." || name == ".." || strings.HasPrefix(lower, ".env.") {
		return false
	}
	switch lower {
	case ".aws", ".docker", ".ssh", ".git", ".cache", ".locks", ".env", ".netrc", ".npmrc",
		"credentials", "credentials.json", "credentials.yaml", "credentials.yml", "aws_key.ini",
		"harbor.yml", "harbor.yaml", "id_rsa", "id_ed25519", "id_ecdsa", "authorized_keys":
		return false
	}
	for _, suffix := range []string{".lock", ".incomplete", ".pem", ".key", ".p12", ".pfx"} {
		if strings.HasSuffix(lower, suffix) {
			return false
		}
	}
	return true
}

// visionParents checks before cleaning so traversal and symlinked ancestors
// cannot be hidden by Abs/Join. Errors deliberately contain no input data.
func visionParents(name string) error {
	for _, part := range strings.Split(name, string(filepath.Separator)) {
		if part == ".." {
			return errors.New("parent traversal in Vision cache or report path")
		}
	}
	full, err := filepath.Abs(name)
	if err != nil {
		return errors.New("invalid Vision cache or report path")
	}
	for {
		info, err := os.Lstat(full)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("missing or symlinked Vision cache or report path")
		}
		parent := filepath.Dir(full)
		if parent == full {
			return nil
		}
		full = parent
	}
}

func visionDirectory(name string) error {
	info, err := os.Lstat(name)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("expected a real Vision model directory")
	}
	return nil
}

func visionRegular(info os.FileInfo, nonempty bool) bool {
	if !info.Mode().IsRegular() || nonempty && info.Size() <= 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

func openVisionFile(name string, nonempty bool) (*os.File, error) {
	info, err := os.Lstat(name)
	if err != nil || !visionRegular(info, nonempty) {
		return nil, errors.New("expected a safe regular Vision file")
	}
	file, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("unable to open Vision file")
	}
	actual, err := file.Stat()
	if err != nil || !visionRegular(actual, nonempty) || !os.SameFile(info, actual) || info.Size() != actual.Size() {
		file.Close()
		return nil, errors.New("Vision file changed during validation")
	}
	return file, nil
}

func readVisionFile(name string, nonempty bool) ([]byte, error) {
	file, err := openVisionFile(name, nonempty)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, errors.New("unable to read Vision metadata")
	}
	return data, nil
}

func scanVisionFiles(directory, relative string) ([]visionFile, error) {
	if err := visionDirectory(directory); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, errors.New("unable to read Vision directory")
	}
	var files []visionFile
	for _, entry := range entries {
		if !safeVisionComponent(entry.Name()) {
			return nil, errors.New("unsafe Vision file or directory name")
		}
		full := filepath.Join(directory, entry.Name())
		key := entry.Name()
		if relative != "" {
			key = relative + "/" + key
		}
		if entry.IsDir() {
			children, err := scanVisionFiles(full, key)
			if err != nil {
				return nil, err
			}
			files = append(files, children...)
			continue
		}
		file, err := openVisionFile(full, true)
		if err != nil {
			return nil, err
		}
		hash := sha256.New()
		size, copyErr := io.Copy(hash, file)
		info, statErr := file.Stat()
		file.Close()
		if copyErr != nil || statErr != nil || !visionRegular(info, true) || size != info.Size() {
			return nil, errors.New("unable to checksum Vision model file")
		}
		files = append(files, visionFile{Path: key, SHA256: hex.EncodeToString(hash.Sum(nil)), Size: size})
	}
	if len(files) == 0 {
		return nil, errors.New("empty Vision model directories are not valid")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// uniqueVisionJSON preserves numeric types and rejects duplicate keys at every
// depth (encoding/json otherwise silently accepts the last duplicate value).
func uniqueVisionJSON(decoder *json.Decoder, depth int) (any, error) {
	if depth > 100 {
		return nil, errors.New("Vision manifest nesting is too deep")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		values := map[string]any{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, errors.New("invalid Vision manifest key")
			}
			if _, exists := values[name]; exists {
				return nil, errors.New("duplicate Vision manifest key")
			}
			values[name], err = uniqueVisionJSON(decoder, depth+1)
			if err != nil {
				return nil, err
			}
		}
		_, err = decoder.Token()
		return values, err
	case json.Delim('['):
		values := []any{}
		for decoder.More() {
			value, err := uniqueVisionJSON(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		_, err = decoder.Token()
		return values, err
	}
	return token, nil
}

func canonicalVisionJSON(data []byte) ([]byte, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("Vision manifest must be UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := uniqueVisionJSON(decoder, 0)
	if err != nil {
		return nil, errors.New("invalid Vision manifest JSON or duplicate key")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing Vision manifest content")
	}
	return json.Marshal(value)
}

// ValidateVisionModels verifies the exact selected cache tree, schema, file
// sizes and SHA256 hashes without Python, network access or filesystem writes.
func ValidateVisionModels(cacheRoot string) error {
	_, err := validateVisionModels(cacheRoot)
	return err
}

func validateVisionModels(cacheRoot string) (string, error) {
	names, err := visionModelSelection()
	if err != nil {
		return "", err
	}
	if err := visionParents(cacheRoot); err != nil {
		return "", err
	}
	cache := filepath.Join(cacheRoot, "vision-models")
	if err := visionDirectory(cache); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		return "", errors.New("unable to read Vision cache")
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "model_dir", "manifest.json":
		case "harbor-artifacts.txt":
			file, err := openVisionFile(filepath.Join(cache, entry.Name()), false)
			if err != nil {
				return "", err
			}
			file.Close()
		default:
			return "", errors.New("unexpected Vision cache root entry")
		}
	}
	data, err := readVisionFile(filepath.Join(cache, "manifest.json"), true)
	if err != nil {
		return "", err
	}
	actual := visionManifest{SchemaVersion: "v1", Source: visionModelsSource}
	modelDir := filepath.Join(cache, "model_dir")
	if err := visionDirectory(modelDir); err != nil {
		return "", err
	}
	models, err := os.ReadDir(modelDir)
	if err != nil || len(models) != len(names) {
		return "", errors.New("Vision cache does not match exact model selection")
	}
	selected := map[string]bool{}
	for _, name := range names {
		selected[name] = true
	}
	for _, model := range models {
		if !selected[model.Name()] {
			return "", errors.New("Vision cache does not match exact model selection")
		}
	}
	for _, name := range names {
		files, err := scanVisionFiles(filepath.Join(modelDir, name), "")
		if err != nil {
			return "", err
		}
		actual.Models = append(actual.Models, visionModel{Name: name, Files: files})
	}
	want, _ := json.Marshal(actual)
	want, _ = canonicalVisionJSON(want)
	got, err := canonicalVisionJSON(data)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(got, want) {
		return "", errors.New("Vision manifest schema, selection or file checksum mismatch")
	}
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// ValidateVisionArtifacts binds each exact OCI reference to its original-case
// selected model and the checksum of the exact cache manifest bytes, not an OCI
// digest. No duplicate, extra, stale, partial or credential-bearing report passes.
func ValidateVisionArtifacts(cacheRoot, preparationRoot, registry, project string) error {
	if _, err := ValidateRegistry(registry); err != nil {
		return errors.New("invalid Vision artifact registry")
	}
	if project == "" {
		project = defaultRemoteProject
	}
	if !visionProject.MatchString(project) {
		return errors.New("invalid Vision artifact project")
	}
	checksum, err := validateVisionModels(cacheRoot)
	if err != nil {
		return err
	}
	if err := visionParents(preparationRoot); err != nil {
		return err
	}
	report := filepath.Join(preparationRoot, "reports/vision-artifacts.txt")
	if err := visionParents(report); err != nil {
		return err
	}
	data, err := readVisionFile(report, true)
	if err != nil {
		return err
	}
	names, err := visionModelSelection()
	if err != nil {
		return err
	}
	expected := map[string]bool{}
	for _, name := range names {
		expected[strings.ToLower(registry)+"/"+project+"/"+strings.ToLower(name)+":latest|"+name+"|"+checksum] = true
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	for _, line := range lines {
		if !expected[line] {
			return errors.New("invalid or duplicate Vision artifact mapping")
		}
		delete(expected, line)
	}
	if len(expected) != 0 {
		return errors.New("Vision artifact mappings are incomplete")
	}
	return nil
}
