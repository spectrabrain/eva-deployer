package remote

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func harborHTTPFixture(t *testing.T, change func(map[string]any)) ([]byte, []byte) {
	t.Helper()
	services, _ := json.Marshal(map[string]any{"items": []any{map[string]any{
		"metadata": map[string]any{"namespace": "default", "labels": map[string]string{"app": "harbor"},
			"annotations": map[string]string{"meta.helm.sh/release-name": "harbor", "meta.helm.sh/release-namespace": "default"}},
		"spec": map[string]any{"type": "NodePort", "ports": []any{map[string]any{"nodePort": 32080}}},
	}}})
	release := map[string]any{"name": "harbor", "namespace": "default", "version": 7, "info": map[string]any{"status": "deployed"},
		"manifest": "apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: original-registry-pvc\nspec:\n  storageClassName: local-path\n",
		"config":   map[string]any{"harborAdminPassword": "private-original-password", "persistence": map[string]any{"existingClaim": "original-registry-pvc"}},
		"chart": map[string]any{"metadata": map[string]any{"apiVersion": "v2", "name": "harbor", "version": "1.18.0", "appVersion": "2.14.0"},
			"values":    map[string]any{"registry": map[string]any{"image": map[string]any{"tag": "original-image-tag"}}},
			"templates": []any{map[string]any{"name": "harbor/templates/registry.yaml", "data": base64.StdEncoding.EncodeToString([]byte("original chart template"))}}}}
	if change != nil {
		change(release)
	}
	encoded, _ := json.Marshal(release)
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	_, _ = writer.Write(encoded)
	_ = writer.Close()
	secrets, _ := json.Marshal(map[string]any{"items": []any{map[string]any{"data": map[string]string{
		"release": base64.StdEncoding.EncodeToString([]byte(base64.StdEncoding.EncodeToString(buffer.Bytes()))),
	}}}})
	return services, secrets
}

func TestHarborHTTPConversionReusesExactChartValuesAndPVC(t *testing.T) {
	for _, failure := range []string{"", "upgrade", "health"} {
		t.Run(failure, func(t *testing.T) {
			services, secrets := harborHTTPFixture(t, nil)
			backupRoot := t.TempDir()
			upgrades, rollbacks := 0, 0
			run := func(_ context.Context, tool string, args ...string) ([]byte, error) {
				if tool == "kubectl" {
					if args[0] == "get" {
						return services, nil
					}
					return secrets, nil
				}
				if args[0] == "template" {
					return []byte("apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: original-registry-pvc\nspec:\n  storageClassName: local-path\n"), nil
				}
				if args[0] == "rollback" {
					rollbacks++
					if args[1] != "harbor" || args[2] != "7" {
						t.Fatal("rollback did not restore the original release revision")
					}
					return nil, nil
				}
				if args[0] != "upgrade" || args[1] != "harbor" || strings.Contains(strings.Join(args, " "), "--install") {
					t.Fatal("conversion attempted to install another Harbor")
				}
				upgrades++
				chart := args[2]
				metadata, _ := os.ReadFile(filepath.Join(chart, "Chart.yaml"))
				var parsedMetadata map[string]any
				_ = yaml.Unmarshal(metadata, &parsedMetadata)
				if parsedMetadata["name"] != "harbor" || parsedMetadata["version"] != "1.18.0" || parsedMetadata["appVersion"] != "2.14.0" {
					t.Fatal("conversion changed the deployed chart or application version")
				}
				template, _ := os.ReadFile(filepath.Join(chart, "templates/registry.yaml"))
				if string(template) != "original chart template" {
					t.Fatal("conversion replaced the original chart templates")
				}
				valuesPath := args[7]
				contents, _ := os.ReadFile(valuesPath)
				var values map[string]any
				if err := yaml.Unmarshal(contents, &values); err != nil {
					t.Fatal(err)
				}
				if values["harborAdminPassword"] != "private-original-password" || values["persistence"].(map[string]any)["existingClaim"] != "original-registry-pvc" {
					t.Fatal("conversion changed existing credentials or persistent storage")
				}
				expose := values["expose"].(map[string]any)
				if values["externalURL"] != "http://harbor.main:32080" || expose["tls"].(map[string]any)["enabled"] != false || expose["nodePort"].(map[string]any)["ports"].(map[string]any)["http"].(map[string]any)["nodePort"] != 32080 {
					t.Fatal("conversion did not select HTTP on the existing NodePort and token endpoint")
				}
				info, _ := os.Stat(valuesPath)
				if info.Mode().Perm() != 0o600 || strings.Contains(strings.Join(args, " "), "private-original-password") {
					t.Fatal("conversion exposed private values")
				}
				if failure == "upgrade" {
					return nil, errors.New("private-original-password must not escape")
				}
				return nil, nil
			}
			verify := func(context.Context) error {
				if failure == "health" {
					return errors.New("HTTP not healthy")
				}
				return nil
			}
			err := convertK3sHarborHTTP(context.Background(), "harbor.main:32080", run, func(context.Context, string) bool { return true }, backupRoot, verify)
			if (err != nil) != (failure != "") || upgrades != 1 || (rollbacks == 1) != (failure != "") {
				t.Fatalf("unexpected conversion/rollback result: upgrades=%d rollbacks=%d err=%v", upgrades, rollbacks, err)
			}
			if err != nil && strings.Contains(err.Error(), "private-original-password") {
				t.Fatal("subprocess credentials escaped into conversion errors")
			}
			backups, _ := filepath.Glob(filepath.Join(backupRoot, "*.json"))
			if len(backups) != 1 {
				t.Fatal("original Helm configuration was not backed up")
			}
			info, _ := os.Stat(backups[0])
			if info.Mode().Perm() != 0o600 {
				t.Fatal("Harbor configuration backup is not private")
			}
		})
	}
}

func TestHarborHTTPConversionRejectsStorageChanges(t *testing.T) {
	original := "apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n  name: harbor-database\nspec:\n  volumeClaimTemplates:\n    - metadata:\n        name: database-data\n      spec:\n        storageClassName: local-path\n"
	if err := validateHarborStorageUnchanged(original, original); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []string{"", strings.ReplaceAll(original, "local-path", "other-class"), strings.ReplaceAll(original, "database-data", "new-data")} {
		if err := validateHarborStorageUnchanged(original, changed); err == nil {
			t.Fatal("persistent storage change was accepted")
		}
	}
}

func TestHarborHTTPConversionPreservesUnknownOrUnsafeDeployments(t *testing.T) {
	for _, condition := range []string{"remote", "wrong-service", "wrong-chart", "traversal"} {
		t.Run(condition, func(t *testing.T) {
			services, secrets := harborHTTPFixture(t, func(release map[string]any) {
				chart := release["chart"].(map[string]any)
				if condition == "wrong-chart" {
					chart["metadata"].(map[string]any)["name"] = "another-chart"
				}
				if condition == "traversal" {
					chart["templates"].([]any)[0].(map[string]any)["name"] = "../escaped"
				}
			})
			if condition == "wrong-service" {
				services = []byte(`{"items":[]}`)
			}
			run := func(_ context.Context, tool string, args ...string) ([]byte, error) {
				if tool == "helm" {
					t.Fatal("unknown deployment was mutated")
				}
				if args[0] == "get" {
					return services, nil
				}
				return secrets, nil
			}
			err := convertK3sHarborHTTP(context.Background(), "harbor.main:32080", run, func(context.Context, string) bool { return condition != "remote" }, t.TempDir(), func(context.Context) error { return nil })
			if err == nil {
				t.Fatal("unsupported deployment was accepted for conversion")
			}
		})
	}
}

func TestBootstrapConvertsHTTPSHarborAndThenReusesHTTP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbor.yaml")
	receipt := testReceipt()
	receipt.ManagedBy, receipt.Protocol = "external", "https"
	if err := WriteHarborReceipt(path, receipt); err != nil {
		t.Fatal(err)
	}
	protocol, conversions := "https", 0
	service := BootstrapService{
		DetectExistingHarbor:     func(context.Context, string, string) (string, bool, error) { return protocol, true, nil },
		EnsureExistingHarborHTTP: func(context.Context, string) error { conversions++; protocol = "http"; return nil },
		Credential:               func(string) bool { return true },
		CheckHarbor: func(_ context.Context, receipt HarborReceipt) error {
			if receipt.Protocol != "http" {
				t.Fatal("converted protocol was not passed to publication checks")
			}
			return nil
		},
	}
	for i := 0; i < 2; i++ {
		result, err := service.Bootstrap(context.Background(), BootstrapOptions{Yes: true, ReceiptPath: path})
		if err != nil || result.Protocol != "http" || conversions != 1 {
			t.Fatalf("conversion or reuse failed: %#v %v", result, err)
		}
	}
	stored, err := LoadHarborReceipt(path)
	if err != nil || stored.Protocol != "http" {
		t.Fatal("HTTP configuration was not recorded for target installation")
	}
}
