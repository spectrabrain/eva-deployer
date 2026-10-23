package remote

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func writeVisionFixture(t *testing.T, cache string, names []string) {
	t.Helper()
	if names == nil {
		names = strings.Split(defaultVisionModels, ",")
	}
	manifest := visionManifest{SchemaVersion: "v1", Source: visionModelsSource}
	for _, name := range names {
		body := "tiny model " + name + "\n"
		mustWrite(t, filepath.Join(cache, "vision-models/model_dir", name, "weights/model.bin"), body)
		manifest.Models = append(manifest.Models, visionModel{Name: name, Files: []visionFile{{Path: "weights/model.bin", Size: int64(len(body)), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(body)))}}})
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(cache, "vision-models/manifest.json"), string(data)+"\n")
}

func writeVisionArtifactFixture(t *testing.T, cache, preparation, registry, project string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cache, "vision-models/manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest visionManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	var report strings.Builder
	for _, model := range manifest.Models {
		fmt.Fprintf(&report, "%s/%s/%s:latest|%s|sha256:%x\n", strings.ToLower(registry), project, strings.ToLower(model.Name), model.Name, sha256.Sum256(data))
	}
	mustWrite(t, filepath.Join(preparation, "reports/vision-artifacts.txt"), report.String())
}

func mutateVisionManifest(t *testing.T, root string, mutate func(map[string]any)) {
	t.Helper()
	path := filepath.Join(root, "vision-models/manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	mutate(manifest)
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, string(data))
}

func TestValidateVisionModelsExactSafeCache(t *testing.T) {
	root := t.TempDir()
	writeVisionFixture(t, root, nil)
	before := preparationFingerprint(t, root)
	if err := ValidateVisionModels(root); err != nil {
		t.Fatal(err)
	}
	if after := preparationFingerprint(t, root); before != after {
		t.Fatal("Vision validation mutated the cache")
	}
	mustWrite(t, filepath.Join(root, "vision-models/harbor-artifacts.txt"), "")
	if err := ValidateVisionModels(root); err != nil {
		t.Fatalf("optional empty regular artifact report rejected: %v", err)
	}
}

func TestValidateVisionModelsRejectsUnsafeOrMismatchedCaches(t *testing.T) {
	model := "vision-models/model_dir/Owl-v2/weights/model.bin"
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{"same size corruption", func(t *testing.T, root string) { mustWrite(t, filepath.Join(root, model), "Tiny model Owl-v2\n") }},
		{"size mismatch", func(t *testing.T, root string) { mustWrite(t, filepath.Join(root, model), "truncated") }},
		{"empty file", func(t *testing.T, root string) { mustWrite(t, filepath.Join(root, model), "") }},
		{"missing file", func(t *testing.T, root string) { mustRemove(t, filepath.Join(root, model)) }},
		{"missing model", func(t *testing.T, root string) { mustRemove(t, filepath.Join(root, "vision-models/model_dir/OmDet")) }},
		{"extra model", func(t *testing.T, root string) {
			mustWrite(t, filepath.Join(root, "vision-models/model_dir/Other/model.bin"), "extra")
		}},
		{"extra safe file", func(t *testing.T, root string) {
			mustWrite(t, filepath.Join(root, "vision-models/model_dir/Owl-v2/extra.bin"), "extra")
		}},
		{"extra root file", func(t *testing.T, root string) { mustWrite(t, filepath.Join(root, "vision-models/extra.txt"), "extra") }},
		{"empty nested directory", func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, "vision-models/model_dir/Owl-v2/empty"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"file symlink", func(t *testing.T, root string) {
			mustRemove(t, filepath.Join(root, model))
			if err := os.Symlink("/etc/passwd", filepath.Join(root, model)); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory symlink", func(t *testing.T, root string) {
			mustRemove(t, filepath.Join(root, "vision-models/model_dir/Owl-v2"))
			if err := os.Symlink(t.TempDir(), filepath.Join(root, "vision-models/model_dir/Owl-v2")); err != nil {
				t.Fatal(err)
			}
		}},
		{"hardlink", func(t *testing.T, root string) {
			if err := os.Link(filepath.Join(root, model), filepath.Join(t.TempDir(), "linked")); err != nil {
				t.Fatal(err)
			}
		}},
		{"fifo", func(t *testing.T, root string) {
			mustRemove(t, filepath.Join(root, model))
			if err := syscall.Mkfifo(filepath.Join(root, model), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"manifest symlink", func(t *testing.T, root string) {
			mustRemove(t, filepath.Join(root, "vision-models/manifest.json"))
			if err := os.Symlink(filepath.Join(root, model), filepath.Join(root, "vision-models/manifest.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{"report hardlink", func(t *testing.T, root string) {
			if err := os.Link(filepath.Join(root, model), filepath.Join(root, "vision-models/harbor-artifacts.txt")); err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong schema", func(t *testing.T, root string) {
			mutateVisionManifest(t, root, func(m map[string]any) { m["schema_version"] = "v2" })
		}},
		{"wrong source", func(t *testing.T, root string) {
			mutateVisionManifest(t, root, func(m map[string]any) { m["source"] = "s3://other" })
		}},
		{"unknown key", func(t *testing.T, root string) {
			mutateVisionManifest(t, root, func(m map[string]any) { m["other"] = "x" })
		}},
		{"case folded key", func(t *testing.T, root string) {
			mutateVisionManifest(t, root, func(m map[string]any) { m["Source"] = m["source"]; delete(m, "source") })
		}},
		{"model order", func(t *testing.T, root string) {
			mutateVisionManifest(t, root, func(m map[string]any) { models := m["models"].([]any); models[0], models[1] = models[1], models[0] })
		}},
		{"duplicate JSON key", func(t *testing.T, root string) {
			path := filepath.Join(root, "vision-models/manifest.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			mustWrite(t, path, strings.Replace(string(data), `"size":`, `"size": 0, "size":`, 1))
		}},
		{"trailing JSON", func(t *testing.T, root string) {
			path := filepath.Join(root, "vision-models/manifest.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			mustWrite(t, path, string(data)+"{}")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeVisionFixture(t, root, nil)
			tc.mutate(t, root)
			if err := ValidateVisionModels(root); err == nil {
				t.Fatal("invalid Vision cache accepted")
			}
		})
	}
	for _, name := range []string{".aws/credentials", "credentials.json", ".cache/asset", ".env.prod", "aws_key.ini", "harbor.yml", "model.lock", "model.incomplete", "tls.key", "tls.pem", "tls.p12", ".netrc", "id_rsa", "bad name.bin"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeVisionFixture(t, root, nil)
			mustWrite(t, filepath.Join(root, "vision-models/model_dir/Owl-v2", name), "private")
			if err := ValidateVisionModels(root); err == nil {
				t.Fatal("unsafe file name accepted")
			}
		})
	}
	for _, tc := range []struct {
		key   string
		value any
	}{
		{"size", true}, {"size", nil}, {"size", -1}, {"size", "18"},
		{"size", json.Number("18.0")}, {"size", json.Number("1.8e1")},
		{"path", "../weights/model.bin"}, {"path", "/weights/model.bin"},
		{"path", "weights\\model.bin"}, {"sha256", strings.Repeat("0", 64)},
	} {
		t.Run(fmt.Sprint(tc.key, tc.value), func(t *testing.T) {
			root := t.TempDir()
			writeVisionFixture(t, root, nil)
			mutateVisionManifest(t, root, func(m map[string]any) {
				file := m["models"].([]any)[0].(map[string]any)["files"].([]any)[0].(map[string]any)
				file[tc.key] = tc.value
			})
			if err := ValidateVisionModels(root); err == nil {
				t.Fatal("invalid file metadata accepted")
			}
		})
	}
}

func TestValidateVisionModelsSelectionAndAncestors(t *testing.T) {
	for _, selection := range []string{"", "Owl-v2,Owl-v2", "OmDet,omdet", "../Owl-v2", "credentials", "bad name"} {
		t.Run(selection, func(t *testing.T) {
			t.Setenv("EVA_VISION_MODELS", selection)
			root := t.TempDir()
			writeVisionFixture(t, root, nil)
			if err := ValidateVisionModels(root); err == nil {
				t.Fatal("invalid selection accepted")
			}
		})
	}
	t.Run("custom exact selection", func(t *testing.T) {
		t.Setenv("EVA_VISION_MODELS", " OmDet, Owl-v2 ")
		root := t.TempDir()
		writeVisionFixture(t, root, []string{"OmDet", "Owl-v2"})
		if err := ValidateVisionModels(root); err != nil {
			t.Fatal(err)
		}
		t.Setenv("EVA_VISION_MODELS", "Owl-v2")
		if err := ValidateVisionModels(root); err == nil {
			t.Fatal("selection mismatch accepted")
		}
	})
	root := t.TempDir()
	writeVisionFixture(t, root, nil)
	link := filepath.Join(t.TempDir(), "ancestor")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := ValidateVisionModels(link); err == nil {
		t.Fatal("symlinked cache ancestor accepted")
	}
	if err := ValidateVisionModels(root + "/../" + filepath.Base(root)); err == nil {
		t.Fatal("parent traversal accepted")
	}
}

func TestValidateVisionArtifactsExactMappings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(string) string
	}{
		{"valid", func(s string) string { return s }},
		{"wrong registry", func(s string) string { return strings.ReplaceAll(s, "harbor.example", "other.example") }},
		{"wrong project", func(s string) string { return strings.ReplaceAll(s, "/eva/", "/other/") }},
		{"wrong tag", func(s string) string { return strings.ReplaceAll(s, ":latest|", ":old|") }},
		{"wrong case ref", func(s string) string { return strings.Replace(s, "/owl-v2:", "/Owl-v2:", 1) }},
		{"wrong model", func(s string) string { return strings.Replace(s, "|Owl-v2|", "|owl-v2|", 1) }},
		{"wrong checksum", func(s string) string { return strings.Replace(s, "|sha256:", "|sha256:0", 1) }},
		{"duplicate", func(s string) string { return s + strings.Split(s, "\n")[0] + "\n" }},
		{"missing", func(s string) string { return strings.Join(strings.Split(s, "\n")[1:], "\n") }},
		{"extra", func(s string) string {
			return s + "harbor.example/eva/other:latest|Other|sha256:" + strings.Repeat("0", 64) + "\n"
		}},
		{"credential", func(s string) string { return s + "AWS_SECRET_ACCESS_KEY=private\n" }},
		{"blank", func(s string) string { return s + "\n" }},
		{"empty", func(string) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache, preparation := t.TempDir(), t.TempDir()
			writeVisionFixture(t, cache, nil)
			writeVisionArtifactFixture(t, cache, preparation, "harbor.example", "eva")
			path := filepath.Join(preparation, "reports/vision-artifacts.txt")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			mustWrite(t, path, tc.mutate(string(data)))
			err = ValidateVisionArtifacts(cache, preparation, "harbor.example", "eva")
			if (err == nil) != (tc.name == "valid") {
				t.Fatalf("report validation = %v", err)
			}
		})
	}
	t.Run("exact bytes not normalized manifest", func(t *testing.T) {
		cache, preparation := t.TempDir(), t.TempDir()
		writeVisionFixture(t, cache, nil)
		writeVisionArtifactFixture(t, cache, preparation, "harbor.example", "eva")
		path := filepath.Join(cache, "vision-models/manifest.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, path, string(data)+"\n")
		if err := ValidateVisionModels(cache); err != nil {
			t.Fatal(err)
		}
		if err := ValidateVisionArtifacts(cache, preparation, "harbor.example", "eva"); err == nil {
			t.Fatal("stale manifest checksum accepted")
		}
	})
	t.Run("custom exact selection", func(t *testing.T) {
		t.Setenv("EVA_VISION_MODELS", "OmDet,Owl-v2")
		cache, preparation := t.TempDir(), t.TempDir()
		writeVisionFixture(t, cache, []string{"OmDet", "Owl-v2"})
		writeVisionArtifactFixture(t, cache, preparation, "Harbor.Example:32080", "eva")
		if err := ValidateVisionArtifacts(cache, preparation, "Harbor.Example:32080", "eva"); err != nil {
			t.Fatal(err)
		}
	})
	for _, kind := range []string{"symlink", "hardlink", "symlink parent"} {
		t.Run(kind, func(t *testing.T) {
			cache, preparation := t.TempDir(), t.TempDir()
			writeVisionFixture(t, cache, nil)
			writeVisionArtifactFixture(t, cache, preparation, "harbor.example", "eva")
			path := filepath.Join(preparation, "reports/vision-artifacts.txt")
			switch kind {
			case "symlink":
				target := filepath.Join(t.TempDir(), "report")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, filepath.Join(t.TempDir(), "report")); err != nil {
					t.Fatal(err)
				}
			case "symlink parent":
				target := filepath.Join(t.TempDir(), "reports")
				if err := os.Rename(filepath.Dir(path), target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Dir(path)); err != nil {
					t.Fatal(err)
				}
			}
			if err := ValidateVisionArtifacts(cache, preparation, "harbor.example", "eva"); err == nil {
				t.Fatal("unsafe report accepted")
			}
		})
	}
}

func TestPrepareVisionStepsUseAWSEnvValidateAndPublishEvidence(t *testing.T) {
	fixture, resolved, identity := writeCompletedPreparation(t)
	cache := filepath.Join(fixture, "cache")
	preparation := t.TempDir()
	var called []string
	service := PrepareService{
		PreparationRoot: preparation, CacheRoot: cache, HarborConfigPath: "/managed/harbor.yml",
		RuntimeRoot: writeRuntimeFixture(t), Preflight: readyPreflight(t),
		ResolveBackend: func(path string) (string, error) { return path, nil },
		Run: func(_ context.Context, options ProcessOptions) error {
			step := ""
			for name, path := range remoteBackendPaths {
				if path == options.Path {
					if path == "scripts/publish/push_images_to_repository.sh" {
						step = "publish-product-images"
						if strings.Contains(options.Env["IMAGE_LIST"], "infra-") {
							step = "publish-infra-images"
						}
					} else {
						step = name
					}
				}
			}
			if step == "" {
				t.Fatalf("unexpected backend %q", options.Path)
			}
			called = append(called, step)
			if options.Env["EVA_VISION_MODELS"] != defaultVisionModels {
				t.Fatal("backend selection is not pinned")
			}
			if step == "download-vision-models" {
				if options.Env["AWS_ACCESS_KEY_ID"] != "test-access" || options.Env["AWS_SECRET_ACCESS_KEY"] != "test-private" || options.Env["AWS_REGION"] != "ap-northeast-2" {
					t.Fatal("Vision download lacks AWS env")
				}
			}
			if strings.HasPrefix(step, "publish-") {
				if options.Env["AWS_SECRET_ACCESS_KEY"] != "" || options.Env["REPOSITORY_REGISTRY"] != identity.RepositoryRegistry || options.Env["REPOSITORY_PROJECT"] != identity.RepositoryProject {
					t.Fatal("invalid publish environment")
				}
				if step == "publish-vision-models" {
					root := filepath.Join(preparation, identity.ReleaseVersion)
					if options.Env["HARBOR_ARTIFACT_MANIFEST"] != filepath.Join(root, "reports/vision-artifacts.txt") || options.Env["LOCAL_HARBOR_YML"] != "/managed/harbor.yml" {
						t.Fatal("invalid Vision publish report or credential boundary")
					}
					writeVisionArtifactFixture(t, cache, root, identity.RepositoryRegistry, identity.RepositoryProject)
				} else {
					data, err := os.ReadFile(filepath.Join(fixture, stepEvidence(step)))
					if err != nil {
						t.Fatal(err)
					}
					name := options.Env["REPOSITORY_MAPPING_FILE"]
					if step == "publish-qdrant-snapshots" {
						name = options.Env["HARBOR_ARTIFACT_MANIFEST"]
					}
					mustWrite(t, name, string(data))
				}
			}
			return nil
		},
	}
	options := PrepareOptions{Release: resolved, Registry: identity.RepositoryRegistry, AWSCredential: AWSCredential{AccessKeyID: "test-access", SecretAccessKey: "test-private", Region: "ap-northeast-2"}}
	if _, err := service.Prepare(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, name := range DefaultStepNames {
		if _, ok := remoteBackendPaths[name]; ok {
			want = append(want, name)
		}
	}
	if !reflect.DeepEqual(called, want) {
		t.Fatalf("backend order=%v want=%v", called, want)
	}
	root := filepath.Join(preparation, identity.ReleaseVersion)
	if err := nonEmptyRegular(root, "reports/cache-vision-models.yaml"); err != nil {
		t.Fatal(err)
	}
	before := preparationFingerprint(t, root)
	called = nil
	if _, err := service.Prepare(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	if len(called) != 0 || before != preparationFingerprint(t, root) {
		t.Fatal("completed prepare was not a read-only resume")
	}
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "test-private") || strings.Contains(string(data), "test-access") {
			t.Fatal("credentials persisted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "reports/vision-artifacts.txt"), "wrong report\n")
	if _, err := service.Prepare(context.Background(), options); err == nil {
		t.Fatal("resume accepted corrupt Vision report")
	}
}

func TestPrepareVisionStepsRejectFalseEvidence(t *testing.T) {
	for _, name := range []string{"corrupt download", "missing download", "wrong report", "missing report", "duplicate report"} {
		t.Run(name, func(t *testing.T) {
			_, resolved, identity := writeCompletedPreparation(t)
			root, cache := t.TempDir(), t.TempDir()
			if err := makePreparationLayout(root); err != nil {
				t.Fatal(err)
			}
			writeVisionFixture(t, cache, nil)
			service := PrepareService{
				ResolveBackend: func(path string) (string, error) { return path, nil },
				Run: func(_ context.Context, options ProcessOptions) error {
					switch name {
					case "corrupt download":
						mustWrite(t, filepath.Join(cache, "vision-models/model_dir/Owl-v2/weights/model.bin"), "corrupt")
					case "missing download":
						mustRemove(t, filepath.Join(cache, "vision-models"))
					case "wrong report":
						mustWrite(t, options.Env["HARBOR_ARTIFACT_MANIFEST"], "wrong\n")
					case "duplicate report":
						writeVisionArtifactFixture(t, cache, root, identity.RepositoryRegistry, identity.RepositoryProject)
						path := options.Env["HARBOR_ARTIFACT_MANIFEST"]
						data, err := os.ReadFile(path)
						if err != nil {
							t.Fatal(err)
						}
						mustWrite(t, path, string(data)+strings.Split(string(data), "\n")[0]+"\n")
					}
					return nil
				},
			}
			steps, err := service.steps(root, cache, "/managed/harbor.yml", resolved, identity, &Manifest{}, Streams{}, AWSCredential{})
			if err != nil {
				t.Fatal(err)
			}
			target := "publish-vision-models"
			if strings.HasSuffix(name, "download") {
				target = "download-vision-models"
			}
			found := false
			for _, step := range steps {
				if step.Name != target {
					continue
				}
				found = true
				result, err := step.Run(context.Background())
				if err == nil {
					err = step.ValidateEvidence(context.Background(), result.Evidence)
				}
				if err == nil {
					t.Fatal("Vision step accepted false evidence")
				}
			}
			if !found {
				t.Fatal("Vision step is missing")
			}
			if target == "download-vision-models" {
				if _, err := os.Lstat(filepath.Join(root, "reports/cache-vision-models.yaml")); !os.IsNotExist(err) {
					t.Fatal("invalid download recorded a validation report")
				}
			}
		})
	}
}
