package airgap

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// Merge export groups from one Docker daemon. Preserve both Docker's tag
// manifest and OCI's root index; concatenating tar files or keeping just one
// index would silently omit images when Docker or containerd imports them.
func mergeDockerExports(ctx context.Context, sources []string, destination string) (resultErr error) {
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err := output.Close(); resultErr == nil {
			resultErr = err
		}
		if resultErr != nil {
			os.Remove(destination)
		}
	}()
	tw := tar.NewWriter(output)
	seen := make(map[string]dockerArchiveFile)
	var manifests []json.RawMessage
	var index map[string]json.RawMessage
	var descriptors []json.RawMessage
	repositories := make(map[string]map[string]string)
	indexCount, layoutCount := 0, 0
	for _, source := range sources {
		file, err := openArchiveSource(source, nil)
		if err != nil {
			return err
		}
		err = func() error {
			defer file.Close()
			tr := tar.NewReader(file)
			local := make(map[string]bool)
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				header, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
				name, err := checkArchiveHeader(header)
				if err != nil {
					return err
				}
				if local[name] {
					return fmt.Errorf("duplicate Docker group member %q", name)
				}
				local[name] = true
				if name == "manifest.json" || name == "index.json" || name == "repositories" {
					if header.Size > archiveJSONLimit || header.Typeflag == tar.TypeDir {
						return errors.New("invalid Docker export metadata")
					}
					body, err := io.ReadAll(tr)
					if err != nil {
						return err
					}
					switch name {
					case "manifest.json":
						var entries []json.RawMessage
						if err := json.Unmarshal(body, &entries); err != nil || len(entries) == 0 {
							return errors.New("invalid Docker group manifest")
						}
						manifests = append(manifests, entries...)
					case "index.json":
						var current map[string]json.RawMessage
						var entries []json.RawMessage
						if err := json.Unmarshal(body, &current); err != nil || string(current["schemaVersion"]) != "2" {
							return errors.New("invalid OCI group index")
						}
						if err := json.Unmarshal(current["manifests"], &entries); err != nil || len(entries) == 0 {
							return errors.New("empty or invalid OCI group descriptors")
						}
						delete(current, "manifests")
						if index != nil {
							old, _ := json.Marshal(index)
							new, _ := json.Marshal(current)
							if !bytes.Equal(old, new) {
								return errors.New("incompatible OCI group indexes")
							}
						}
						index = current
						descriptors = append(descriptors, entries...)
						indexCount++
					case "repositories":
						var current map[string]map[string]string
						if err := json.Unmarshal(body, &current); err != nil {
							return err
						}
						for repo, tags := range current {
							if repositories[repo] == nil {
								repositories[repo] = make(map[string]string)
							}
							for tag, digest := range tags {
								if old, ok := repositories[repo][tag]; ok && old != digest {
									return errors.New("conflicting Docker group repository tags")
								}
								repositories[repo][tag] = digest
							}
						}
					}
					continue
				}
				if name == "oci-layout" {
					layoutCount++
				}
				entry := dockerArchiveFile{size: header.Size, directory: header.Typeflag == tar.TypeDir}
				previous, exists := seen[name]
				if !exists {
					if err := tw.WriteHeader(cleanArchiveHeader(name, header.Mode, header.Size, entry.directory)); err != nil {
						return err
					}
				}
				if !entry.directory {
					hash := sha256.New()
					var target io.Writer = hash
					if !exists {
						target = io.MultiWriter(tw, hash)
					}
					if _, err := io.Copy(target, tr); err != nil {
						return err
					}
					entry.digest = hex.EncodeToString(hash.Sum(nil))
				}
				if exists && (previous.directory != entry.directory || previous.size != entry.size || previous.digest != entry.digest) {
					return fmt.Errorf("conflicting Docker group member %q", name)
				}
				seen[name] = entry
			}
			if !local["manifest.json"] {
				return errors.New("Docker export group is missing manifest.json")
			}
			return consumeArchivePadding(file)
		}()
		if err != nil {
			return err
		}
	}
	if indexCount != layoutCount || indexCount != 0 && indexCount != len(sources) {
		return errors.New("Docker export groups use incompatible archive layouts")
	}
	metadata := map[string]any{"manifest.json": manifests}
	if index != nil {
		index["manifests"], err = json.Marshal(descriptors)
		if err != nil {
			return err
		}
		metadata["index.json"] = index
	}
	if len(repositories) != 0 {
		metadata["repositories"] = repositories
	}
	for _, name := range []string{"manifest.json", "index.json", "repositories"} {
		value, ok := metadata[name]
		if !ok {
			continue
		}
		body, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if len(body) > archiveJSONLimit {
			return errors.New("merged Docker metadata exceeds 16 MiB")
		}
		if err := tw.WriteHeader(cleanArchiveHeader(name, 0o644, int64(len(body)), false)); err != nil {
			return err
		}
		if _, err := tw.Write(body); err != nil {
			return err
		}
	}
	return tw.Close()
}
