package fieldoverride

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestParseBuildsRedactedPublicMetadata(t *testing.T) {
	root := t.TempDir()
	chart := writeChart(t, root, "eva-app", "9.9.9", "9.9.9")
	values := writeInput(t, root, "app.yaml", "replicaCount: 2\n")
	request, err := Parse([]string{"app=" + chart}, []string{"app=" + values}, []string{"replicaCount=2", "app:image.tag=next"}, map[string]bool{"app": true})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	component := request.Components["app"]
	if component.Chart == nil || component.Chart.ChartMetadata == nil || component.Chart.ChartMetadata.Version != "9.9.9" || component.Values == nil || len(component.SetValues) != 2 {
		t.Fatalf("parsed component = %#v", component)
	}
	public := request.Public()["app"]
	if len(public.SetValues) != 0 || len(public.SetKeys) != 2 {
		t.Fatalf("public component = %#v", public)
	}
}

func TestParseRejectsUnselectedOrUnsupportedComponents(t *testing.T) {
	path := writeInput(t, t.TempDir(), "app.yaml", "key: value\n")
	for _, test := range []struct {
		name string
		call func() error
	}{
		{"unselected", func() error {
			_, err := Parse([]string{"app=" + path}, nil, nil, map[string]bool{"app": false})
			return err
		}},
		{"unsupported", func() error {
			_, err := Parse([]string{"n8n=" + path}, nil, nil, map[string]bool{"n8n": true})
			return err
		}},
		{"ambiguous set", func() error {
			_, err := Parse(nil, nil, []string{"replicaCount=2"}, map[string]bool{"app": true, "agent": true})
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); err == nil {
				t.Fatal("Parse() succeeded")
			}
		})
	}
}

func TestParseSupportsAgentAndVision(t *testing.T) {
	path := writeInput(t, t.TempDir(), "override.yaml", "replicaCount: 2\n")
	chart := writeChart(t, t.TempDir(), "eva-agent", "1.2.3", "1.2.3")
	request, err := Parse(
		[]string{"agent=" + chart}, []string{"vision=" + path}, []string{"agent:replicaCount=2", "vision:replicaCount=3"},
		map[string]bool{"agent": true, "vision": true},
	)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(request.Components) != 2 || request.Components["agent"].Chart == nil || request.Components["vision"].Values == nil {
		t.Fatalf("parsed components = %#v", request.Components)
	}
}

func TestParseRecordsUnexpectedChartNameWithoutRejectingOverride(t *testing.T) {
	chart := writeChart(t, t.TempDir(), "customer-app", "1.0.0", "1.0.0")
	request, err := Parse([]string{"app=" + chart}, nil, nil, map[string]bool{"app": true})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	metadata := request.Components["app"].Chart.ChartMetadata
	if metadata.NameMatches || metadata.ExpectedName != "eva-app" {
		t.Fatalf("chart metadata = %#v", metadata)
	}
}

func writeInput(t *testing.T, root, name, contents string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeChart(t *testing.T, root, name, version, appVersion string) string {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(gzipWriter)
	contents := []byte("apiVersion: v2\nname: " + name + "\nversion: " + version + "\nappVersion: " + appVersion + "\n")
	if err := archive.WriteHeader(&tar.Header{Name: name + "/Chart.yaml", Mode: 0o644, Size: int64(len(contents)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return writeInput(t, root, name+".tgz", buffer.String())
}
