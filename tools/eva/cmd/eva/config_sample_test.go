package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"eva-deployer/tools/eva/internal/workspace"
	"gopkg.in/yaml.v3"
)

func TestConfigSampleInteractiveChoices(t *testing.T) {
	for _, test := range []struct {
		name, answers, address, protocol, iam string
	}{
		{"shared HTTPS", "1\n1\n2\n", "shared", "https", "install"},
		{"separate HTTPS", "1\n2\n2\n", "separate", "https", "install"},
		{"shared HTTP", "1\n1\n1\n", "shared", "http", "install"},
		{"separate HTTP", "1\n2\n1\n", "separate", "http", "install"},
		{"external IAM App HTTPS", "2\n2\n", "external-iam", "https", "external"},
		{"external IAM App HTTP", "2\n1\n", "external-iam", "http", "external"},
		{"defaults", "\n\n\n", "shared", "https", "install"},
		{"invalid then valid", "wrong\n1\nwrong\n1\nwrong\n2\n", "shared", "https", "install"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var yamlOutput, messages bytes.Buffer
			if err := runConfigSample([]string{"sample", "--mode", "remote"}, strings.NewReader(test.answers), &yamlOutput, &messages, true); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(messages.String(), "IAM 구성") || strings.Contains(messages.String(), "주소 구성") != (test.iam == "install") {
				t.Fatal("IAM/address choices do not match selected installation mode")
			}
			if !strings.Contains(messages.String(), "외부 접속 환경") {
				t.Fatal("missing protocol choices")
			}
			var input workspace.Installer
			if err := yaml.Unmarshal(yamlOutput.Bytes(), &input); err != nil {
				t.Fatal("menu contaminated YAML output")
			}
			if input.Components["iam"] != (test.iam == "install") {
				t.Fatal("IAM installation component does not match menu selection")
			}
			license := input.Targets["server-a"].Values["app"]["app"].(map[string]any)["license"].(map[string]any)
			if license["activation_mode"] != "offline" || strings.Contains(messages.String(), "라이선스 서버 인터넷 연결") {
				t.Fatal("Remote must default to offline without an additional license prompt")
			}
			if strings.Contains(yamlOutput.String(), "선택 [") || !strings.Contains(yamlOutput.String(), "remote / "+test.address+" / "+test.protocol) {
				t.Fatal("selected template mismatch or prompts mixed with YAML")
			}
			if !strings.Contains(messages.String(), "v3.3") || !strings.Contains(yamlOutput.String(), "v3.3") {
				t.Fatal("v3.3 sample target not displayed")
			}
			for _, version := range []string{"3.3.0-beta.3", "3.3.0-beta.1"} {
				if !strings.Contains(messages.String(), version) || !strings.Contains(yamlOutput.String(), version) {
					t.Fatal("published App/IAM version baseline not displayed")
				}
			}
			if test.protocol == "http" && (!strings.Contains(messages.String(), "생략") || !strings.Contains(messages.String(), "E2E") || strings.Contains(messages.String(), "미지원")) {
				t.Fatal("HTTP must explain TLS skipping without claiming live E2E validation")
			}
		})
	}
}

func TestConfigSampleNoninteractiveAndExplicitOptions(t *testing.T) {
	for _, args := range [][]string{
		{"sample", "--mode", "local", "--address", "shared", "--protocol", "https"},
		{"sample", "--mode", "local", "--non-interactive"},
		{"sample", "--mode", "local", "--iam", "external", "--protocol", "https"},
	} {
		var output, messages bytes.Buffer
		if err := runConfigSample(args, strings.NewReader(""), &output, &messages, true); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(messages.String(), "선택 [") || output.Len() == 0 {
			t.Fatal("explicit/noninteractive generation prompted")
		}
	}
	var output, messages bytes.Buffer
	if err := runConfigSample([]string{"sample", "--address", "shared"}, strings.NewReader(""), &output, &messages, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "online / shared / https") {
		t.Fatal("non-terminal defaults ignored explicit address")
	}
	output.Reset()
	if err := runConfigSample([]string{"sample"}, strings.NewReader(""), &output, &messages, true); err == nil || output.Len() != 0 {
		t.Fatal("interactive EOF silently selected a template")
	}
	for _, args := range [][]string{{"sample", "--address", "invalid"}, {"sample", "--protocol", "invalid"}, {"sample", "--iam", "invalid"}, {"sample", "--iam", "external", "--address", "shared"}} {
		if err := runConfigSample(args, strings.NewReader(""), &output, &messages, true); err == nil {
			t.Fatal("invalid explicit option accepted")
		}
	}
}

func TestConfigSampleActivationMode(t *testing.T) {
	for _, test := range []struct {
		mode, wantMode, want string
	}{
		{"online", "online", "online"},
		{"remote", "remote", "offline"},
		{"local", "local", "offline"},
		{"cloud", "online", "online"},
		{"cloud_repository", "online", "online"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			args := []string{"sample", "--mode", test.mode, "--address", "shared", "--protocol", "http"}
			var output, messages bytes.Buffer
			if err := runConfigSample(args, strings.NewReader(""), &output, &messages, true); err != nil {
				t.Fatal(err)
			}
			var input workspace.Installer
			if err := yaml.Unmarshal(output.Bytes(), &input); err != nil {
				t.Fatal(err)
			}
			if input.Repository.Mode != test.wantMode || !strings.Contains(output.String(), test.wantMode+" / shared / http") {
				t.Fatalf("sample repository mode/header must use %s, got %s", test.wantMode, input.Repository.Mode)
			}
			license := input.Targets["server-a"].Values["app"]["app"].(map[string]any)["license"].(map[string]any)
			if license["activation_mode"] != test.want {
				t.Fatalf("activation_mode = %v, want %s", license["activation_mode"], test.want)
			}
			if strings.Contains(messages.String(), "선택 [") {
				t.Fatal("license defaults must not add prompts")
			}
		})
	}
}

func TestExternalIAMNoninteractiveDefaults(t *testing.T) {
	var output, messages bytes.Buffer
	if err := runConfigSample([]string{"sample", "--iam", "external", "--non-interactive"}, strings.NewReader(""), &output, &messages, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "online / external-iam / https") || strings.Contains(messages.String(), "선택 [") || !strings.Contains(messages.String(), "[외부 IAM]") {
		t.Fatal("external IAM noninteractive defaults or guidance missing")
	}
}

func TestConfigSampleOutputIsPrivateAndNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "eva-install.yaml")
	args := []string{"sample", "--mode", "online", "--address", "shared", "--protocol", "https", "--output", path}
	var output, messages bytes.Buffer
	if err := runConfigSample(args, strings.NewReader(""), &output, &messages, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 || output.Len() != 0 {
		t.Fatalf("sample file is not private or YAML leaked to stdout: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := runConfigSample(args, strings.NewReader(""), &output, &messages, false); err == nil {
		t.Fatal("existing sample file overwritten")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("existing file changed")
	}
	link := filepath.Join(t.TempDir(), "sample.yaml")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	args[len(args)-1] = link
	if err := runConfigSample(args, strings.NewReader(""), &output, &messages, false); err == nil {
		t.Fatal("symlink destination accepted")
	}
}
