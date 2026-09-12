package runtime

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultRoot = "/opt/eva/runtime"
const descriptorName = "runtime.yaml"
const schemaVersion = "v1"

var requiredTools = map[string]struct{}{
	"ansible-playbook": {},
	"helm":             {},
	"kubectl":          {},
	"kustomize":        {},
	"oras":             {},
}

type Descriptor struct {
	SchemaVersion string            `yaml:"schema_version"`
	Version       string            `yaml:"version"`
	Tools         map[string]string `yaml:"tools"`
}

type Resolved struct {
	Root           string
	DescriptorPath string
	Descriptor     Descriptor
	toolPaths      map[string]string
}

func Resolve(root string) (Resolved, error) {
	if root == "" {
		root = DefaultRoot
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve runtime root: %w", err)
	}
	info, err := os.Stat(absRoot)
	if err != nil {
		return Resolved{}, fmt.Errorf("read runtime root %s: %w", absRoot, err)
	}
	if !info.IsDir() {
		return Resolved{}, fmt.Errorf("runtime root is not a directory: %s", absRoot)
	}
	canonicalRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve runtime root %s: %w", absRoot, err)
	}

	descriptorPath := filepath.Join(canonicalRoot, descriptorName)
	descriptor, err := loadDescriptor(descriptorPath)
	if err != nil {
		return Resolved{}, err
	}
	toolPaths, err := validateDescriptor(canonicalRoot, descriptor)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{
		Root:           canonicalRoot,
		DescriptorPath: descriptorPath,
		Descriptor:     descriptor,
		toolPaths:      toolPaths,
	}, nil
}

// Install validates an extracted Runtime payload, then replaces the managed
// Runtime directory only after a complete staged copy passes validation.
func Install(source, root string) (Resolved, error) {
	sourceResolved, err := Resolve(source)
	if err != nil {
		return Resolved{}, fmt.Errorf("validate runtime source: %w", err)
	}
	if root == "" {
		root = DefaultRoot
	}
	destination, err := filepath.Abs(root)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve runtime destination: %w", err)
	}
	if existingRoot, err := filepath.EvalSymlinks(destination); err == nil && existingRoot == sourceResolved.Root {
		return Resolved{}, errors.New("runtime source and destination must differ")
	}

	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return Resolved{}, fmt.Errorf("create runtime parent: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".eva-runtime-")
	if err != nil {
		return Resolved{}, fmt.Errorf("create runtime staging directory: %w", err)
	}
	defer os.RemoveAll(staging)
	if err := copyTree(sourceResolved.Root, staging); err != nil {
		return Resolved{}, fmt.Errorf("stage runtime payload: %w", err)
	}
	if _, err := Resolve(staging); err != nil {
		return Resolved{}, fmt.Errorf("validate staged runtime: %w", err)
	}

	backup, err := moveExistingRuntime(destination, parent)
	if err != nil {
		return Resolved{}, err
	}
	if err := os.Rename(staging, destination); err != nil {
		if backup != "" {
			if restoreErr := os.Rename(backup, destination); restoreErr != nil {
				return Resolved{}, fmt.Errorf("publish runtime: %w; restore previous runtime: %v", err, restoreErr)
			}
		}
		return Resolved{}, fmt.Errorf("publish runtime: %w", err)
	}
	if backup != "" {
		if err := os.RemoveAll(backup); err != nil {
			return Resolved{}, fmt.Errorf("remove previous runtime %s: %w", backup, err)
		}
	}
	return Resolve(destination)
}

// BootstrapOffline extracts the runtime/ subtree from an EVA Offline payload
// and publishes it through the same validation and atomic replacement path as
// a directly supplied Runtime payload.
func BootstrapOffline(payload, root string) (Resolved, error) {
	payloadPath, err := filepath.Abs(payload)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve Offline payload: %w", err)
	}
	info, err := os.Stat(payloadPath)
	if err != nil {
		return Resolved{}, fmt.Errorf("read Offline payload %s: %w", payloadPath, err)
	}
	if !info.Mode().IsRegular() {
		return Resolved{}, fmt.Errorf("Offline payload is not a regular file: %s", payloadPath)
	}
	if root == "" {
		root = DefaultRoot
	}
	destination, err := filepath.Abs(root)
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve runtime destination: %w", err)
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return Resolved{}, fmt.Errorf("create runtime parent: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".eva-offline-")
	if err != nil {
		return Resolved{}, fmt.Errorf("create Offline Runtime staging directory: %w", err)
	}
	defer os.RemoveAll(staging)
	if err := extractOfflineRuntime(payloadPath, staging); err != nil {
		return Resolved{}, err
	}
	return Install(staging, destination)
}

func (resolved Resolved) ToolPath(name string) (string, error) {
	path, ok := resolved.toolPaths[name]
	if !ok {
		return "", fmt.Errorf("%q is not an EVA managed runtime command", name)
	}
	return path, nil
}

func (resolved Resolved) ToolNames() []string {
	names := make([]string, 0, len(resolved.toolPaths))
	for name := range resolved.toolPaths {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ToolDirectories returns unique directories containing validated Runtime
// tools. They can be prepended to PATH for an interactive EVA shell.
func (resolved Resolved) ToolDirectories() []string {
	directories := make(map[string]struct{}, len(resolved.toolPaths))
	for _, path := range resolved.toolPaths {
		directories[filepath.Dir(path)] = struct{}{}
	}
	paths := make([]string, 0, len(directories))
	for directory := range directories {
		paths = append(paths, directory)
	}
	sort.Strings(paths)
	return paths
}

func loadDescriptor(path string) (Descriptor, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return Descriptor{}, fmt.Errorf("read runtime descriptor %s: %w", path, err)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(contents)))
	decoder.KnownFields(true)
	var descriptor Descriptor
	if err := decoder.Decode(&descriptor); err != nil {
		return Descriptor{}, fmt.Errorf("parse runtime descriptor %s: %w", path, err)
	}
	return descriptor, nil
}

func validateDescriptor(root string, descriptor Descriptor) (map[string]string, error) {
	if descriptor.SchemaVersion != schemaVersion {
		return nil, fmt.Errorf("runtime descriptor schema_version must be %q", schemaVersion)
	}
	if descriptor.Version == "" {
		return nil, errors.New("runtime descriptor version is required")
	}
	if len(descriptor.Tools) == 0 {
		return nil, errors.New("runtime descriptor tools are required")
	}
	for name := range descriptor.Tools {
		if _, ok := requiredTools[name]; !ok {
			return nil, fmt.Errorf("runtime descriptor has unsupported tool %q", name)
		}
	}

	tools := make(map[string]string, len(requiredTools))
	for name := range requiredTools {
		relativePath, ok := descriptor.Tools[name]
		if !ok || relativePath == "" {
			return nil, fmt.Errorf("runtime descriptor is missing tool %q", name)
		}
		path, err := resolveToolPath(root, relativePath)
		if err != nil {
			return nil, fmt.Errorf("runtime tool %q: %w", name, err)
		}
		tools[name] = path
	}
	return tools, nil
}

func resolveToolPath(root, relativePath string) (string, error) {
	if filepath.IsAbs(relativePath) {
		return "", errors.New("tool path must be relative to the runtime root")
	}
	path := filepath.Clean(filepath.Join(root, relativePath))
	if !isWithin(root, path) {
		return "", errors.New("tool path escapes the runtime root")
	}
	canonicalPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve executable %s: %w", path, err)
	}
	if !isWithin(root, canonicalPath) {
		return "", errors.New("tool executable resolves outside the runtime root")
	}
	info, err := os.Stat(canonicalPath)
	if err != nil {
		return "", fmt.Errorf("read executable %s: %w", canonicalPath, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("tool executable is not a regular file: %s", canonicalPath)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("tool executable is not executable: %s", canonicalPath)
	}
	return canonicalPath, nil
}

func isWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func moveExistingRuntime(destination, parent string) (string, error) {
	if _, err := os.Lstat(destination); errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("read existing runtime: %w", err)
	}
	backup, err := os.MkdirTemp(parent, ".eva-runtime-previous-")
	if err != nil {
		return "", fmt.Errorf("reserve previous runtime path: %w", err)
	}
	if err := os.Remove(backup); err != nil {
		return "", fmt.Errorf("prepare previous runtime path: %w", err)
	}
	if err := os.Rename(destination, backup); err != nil {
		return "", fmt.Errorf("preserve existing runtime: %w", err)
	}
	return backup, nil
}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		target := filepath.Join(destination, relative)
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported runtime payload entry: %s", path)
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	return nil
}

func extractOfflineRuntime(payload, destination string) error {
	file, err := os.Open(payload)
	if err != nil {
		return err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open Offline payload gzip stream: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read Offline payload archive: %w", err)
		}
		name, err := offlineArchiveName(header.Name)
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeDir && header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return fmt.Errorf("Offline payload archive entry type is not allowed: %s", header.Name)
		}
		if name != "runtime" && !strings.HasPrefix(name, "runtime/") {
			if header.Typeflag != tar.TypeDir {
				if _, err := io.Copy(io.Discard, tarReader); err != nil {
					return err
				}
			}
			continue
		}
		relative := strings.TrimPrefix(name, "runtime")
		relative = strings.TrimPrefix(relative, "/")
		if relative == "" {
			if header.Typeflag != tar.TypeDir {
				return fmt.Errorf("Offline payload runtime root is not a directory")
			}
			if err := os.MkdirAll(destination, 0o750); err != nil {
				return err
			}
			continue
		}
		target := filepath.Join(destination, filepath.FromSlash(relative))
		if !isWithin(destination, target) {
			return fmt.Errorf("Offline Runtime path escapes staging directory: %s", header.Name)
		}
		if header.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, header.FileInfo().Mode().Perm()); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := copyArchiveFile(tarReader, target, header.FileInfo().Mode().Perm()); err != nil {
			return err
		}
	}
	if _, err := Resolve(destination); err != nil {
		return fmt.Errorf("validate Offline Runtime payload: %w", err)
	}
	return nil
}

func offlineArchiveName(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("Offline payload has invalid path %q", name)
	}
	clean := pathpkg.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("Offline payload path escapes archive root: %q", name)
	}
	return clean, nil
}

func copyArchiveFile(source io.Reader, destination string, mode os.FileMode) error {
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, source); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}
