package workspace

import (
	"bytes"
	"embed"
	"errors"

	"gopkg.in/yaml.v3"
)

//go:embed installer.yaml.sample samples/*.yaml
var installerSamples embed.FS

type SampleOptions struct {
	Mode     string
	Address  string // shared: one domain/IP with paths; separate: distinct domains
	Protocol string
	IAM      string // install (default): same-target handoff; external: explicit App SSO
}

func (options SampleOptions) Validate() error {
	if _, ok := ansibleRepositoryModes[NormalizeRepositoryMode(options.Mode)]; !ok {
		return errors.New("sample mode must be online, remote or local")
	}
	if options.IAM != "" && options.IAM != "install" && options.IAM != "external" {
		return errors.New("sample IAM mode must be install or external")
	}
	if options.IAM == "external" && options.Address != "" && options.Address != "separate" {
		return errors.New("external IAM uses its own endpoint; omit --address or use separate")
	}
	if options.Address != "shared" && options.Address != "separate" && !(options.IAM == "external" && options.Address == "") {
		return errors.New("sample address must be shared or separate")
	}
	if options.Protocol != "http" && options.Protocol != "https" {
		return errors.New("sample protocol must be http or https")
	}
	return nil
}

// InstallerSample is embedded in the Tool, so installers do not need a source
// checkout, an additional artifact, or an existing Workspace to get started.
func InstallerSample(mode string) ([]byte, error) {
	return InstallerSampleFor(SampleOptions{Mode: mode, Address: "separate", Protocol: "https"})
}

// InstallerSampleFor composes independently maintained repository and endpoint
// templates. YAML nodes preserve the required-input comments in every mode.
func InstallerSampleFor(options SampleOptions) ([]byte, error) {
	options.Mode = NormalizeRepositoryMode(options.Mode)
	if err := options.Validate(); err != nil {
		return nil, err
	}
	basePath := "installer.yaml.sample"
	if options.Mode != "online" {
		basePath = "samples/" + options.Mode + ".yaml"
	}
	base, err := readSampleNode(basePath)
	if err != nil {
		return nil, err
	}
	profileName := options.Address
	if options.IAM == "external" {
		profileName = "external-iam"
	}
	profile, err := readSampleNode("samples/" + profileName + "-" + options.Protocol + ".yaml")
	if err != nil {
		return nil, err
	}
	target := sampleMappingValue(sampleMappingValue(base.Content[0], "targets"), "server-a")
	values := sampleMappingValue(target, "values")
	if values == nil {
		values = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		target.Content = append(target.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "values"}, values)
	}
	if options.IAM == "external" {
		removeSampleMappingValue(values, "iam")
		iamComponent := sampleMappingValue(sampleMappingValue(base.Content[0], "components"), "iam")
		if iamComponent == nil {
			return nil, errors.New("embedded installer sample is missing the IAM component")
		}
		iamComponent.Value = "false"
		iamComponent.LineComment = "외부 IAM 연동만 사용하며 이 target에는 IAM을 설치하지 않습니다."
	}
	mergeSampleMapping(values, profile.Content[0])
	license := sampleMappingValue(sampleMappingValue(sampleMappingValue(values, "app"), "app"), "license")
	activation := sampleMappingValue(license, "activation_mode")
	if activation == nil {
		return nil, errors.New("embedded installer sample is missing the license activation mode")
	}
	if options.Mode == "online" {
		activation.Value = "online"
	} else {
		activation.Value = "offline"
	}
	defaults, err := readSampleNode("samples/app-sso.yaml")
	if err != nil {
		return nil, err
	}
	mergeSampleMapping(values, defaults.Content[0])
	if options.IAM == "external" {
		sso, err := readSampleNode("samples/external-iam.yaml")
		if err != nil {
			return nil, err
		}
		mergeSampleMapping(values, sso.Content[0])
	}
	base.HeadComment = "EVA 설치 틀: " + options.Mode + " / " + profileName + " / " + options.Protocol + "\nv3.3 게시 App 3.3.0-beta.9 / IAM 3.3.0-beta.1 기준. 사용하는 Release catalog와 일치하는지 확인합니다. 실제 설치 E2E는 별도 검증합니다.\n필수 입력: <UPPER_CASE> placeholder를 모두 교체합니다. 반복된 placeholder는 같은 값을 사용합니다. 예시 Secret은 사용하지 않습니다."
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(base); err != nil {
		return nil, err
	}
	return output.Bytes(), encoder.Close()
}

func readSampleNode(path string) (*yaml.Node, error) {
	contents, err := installerSamples.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(contents, &node); err != nil {
		return nil, errors.New("invalid embedded installer sample")
	}
	return &node, nil
}

func sampleMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node != nil {
		for index := 0; index+1 < len(node.Content); index += 2 {
			if node.Content[index].Value == key {
				return node.Content[index+1]
			}
		}
	}
	return nil
}

func removeSampleMappingValue(node *yaml.Node, key string) {
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			node.Content = append(node.Content[:index], node.Content[index+2:]...)
			return
		}
	}
}

func mergeSampleMapping(destination, source *yaml.Node) {
	if source.HeadComment != "" {
		destination.HeadComment = source.HeadComment
	}
	for index := 0; index+1 < len(source.Content); index += 2 {
		key, value := source.Content[index], source.Content[index+1]
		previous := sampleMappingValue(destination, key.Value)
		if previous == nil {
			destination.Content = append(destination.Content, key, value)
		} else if previous.Kind == yaml.MappingNode && value.Kind == yaml.MappingNode {
			for previousIndex := 0; previousIndex+1 < len(destination.Content); previousIndex += 2 {
				if destination.Content[previousIndex].Value == key.Value && key.HeadComment != "" {
					destination.Content[previousIndex].HeadComment = key.HeadComment
					break
				}
			}
			mergeSampleMapping(previous, value)
		}
	}
}
