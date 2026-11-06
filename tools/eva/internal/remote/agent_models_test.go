package remote

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateAgentModelsAcrossStorageLayouts(t *testing.T) {
	for _, test := range []struct {
		name, marker       string
		agent, vllm, valid bool
	}{
		{"new-vllm-only", "agent_models_required: false\n", false, true, true},
		{"new-vllm-missing", "agent_models_required: false\n", true, false, false},
		{"legacy-both", "", true, true, true},
		{"legacy-agent-missing", "", false, true, false},
		{"explicit-legacy", "agent_models_required: true\n", false, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			manifest := test.marker + "files:\n"
			for tree, present := range map[string]bool{"agent": test.agent, "vllm": test.vllm} {
				if !present {
					continue
				}
				directory := filepath.Join(root, "models", tree, "hf")
				if err := os.MkdirAll(directory, 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, "weights.bin"), []byte("model"), 0o640); err != nil {
					t.Fatal(err)
				}
				manifest += "models/" + tree + "/hf/weights.bin\n"
			}
			if err := os.WriteFile(filepath.Join(root, "models", "manifest.txt"), []byte(manifest), 0o640); err != nil {
				t.Fatal(err)
			}
			if err := ValidateModels(root); (err == nil) != test.valid {
				t.Fatalf("ValidateModels() = %v, want valid=%v", err, test.valid)
			}
		})
	}
}
