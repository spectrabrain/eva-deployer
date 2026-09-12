package fieldoverride

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseBuildsRedactedPublicMetadata(t *testing.T) {
	root := t.TempDir()
	chart := writeInput(t, root, "app.tgz", "chart")
	values := writeInput(t, root, "app.yaml", "replicaCount: 2\n")
	request, err := Parse([]string{"app=" + chart}, []string{"app=" + values}, []string{"replicaCount=2", "app:image.tag=next"}, map[string]bool{"app": true})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	component := request.Components["app"]
	if component.Chart == nil || component.Values == nil || len(component.SetValues) != 2 {
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
			_, err := Parse([]string{"vision=" + path}, nil, nil, map[string]bool{"vision": true})
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

func writeInput(t *testing.T, root, name, contents string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
