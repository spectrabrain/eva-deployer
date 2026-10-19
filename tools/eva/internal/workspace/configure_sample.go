package workspace

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

type configuredSampleTarget struct {
	node       *yaml.Node
	components map[string]bool
}

// ConfigureTarget separates connection inputs from reusable installation choices.
// Address "ip" selects path separation and uses the SSH IP when available.
type ConfigureTarget struct {
	Name     string
	Ansible  map[string]any
	Solution bool
	IAM      string
	Protocol string
	Address  string
	Agent    bool
	Vision   bool
}

func ValidateConfigureTargetName(name string) error {
	if !siteIDPattern.MatchString(name) || name == "all" || name == "ungrouped" || name == "eva" {
		return errors.New("닉네임은 영문/숫자로 시작하고 영문/숫자/점/밑줄/하이픈만 사용합니다 (all, ungrouped, eva 제외)")
	}
	return nil
}

// ConfigureSample composes target-specific scaffolds without serializing a typed
// Installer, preserving the embedded examples and required-input comments.
func ConfigureSample(mode string, targets []ConfigureTarget) ([]byte, error) {
	if _, ok := ansibleRepositoryModes[mode]; !ok || len(targets) == 0 {
		return nil, errors.New("configure requires a repository mode and at least one target")
	}
	basePath := "installer.yaml.sample"
	if mode != "cloud" {
		basePath = "samples/" + mode + ".yaml"
	}
	document, err := readSampleNode(basePath)
	if err != nil {
		return nil, err
	}
	root := document.Content[0]
	targetMapping := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setSampleMappingValue(root, "targets", targetMapping)
	global := map[string]bool{"infra": true, "iam": false, "app": false, "agent": false, "vision": false, "n8n": false}
	configured := make(map[string]configuredSampleTarget, len(targets))
	seen := make(map[string]bool)
	for index, options := range targets {
		if err := ValidateConfigureTargetName(options.Name); err != nil {
			return nil, err
		}
		if seen[options.Name] {
			return nil, errors.New("configure target nicknames must be distinct")
		}
		seen[options.Name] = true
		profile := SampleOptions{Mode: mode, IAM: options.IAM, Address: options.Address, Protocol: options.Protocol}
		if options.Address == "ip" {
			profile.Address = "shared"
			if options.IAM == "external" {
				profile.Address = "separate"
			}
		}
		var target *yaml.Node
		components := map[string]bool{"infra": true, "iam": false, "app": false, "agent": false, "vision": false, "n8n": false}
		if options.Solution {
			contents, err := InstallerSampleFor(profile)
			if err != nil {
				return nil, err
			}
			var selected yaml.Node
			if err := yaml.Unmarshal(contents, &selected); err != nil {
				return nil, errors.New("cannot compose selected installer sample")
			}
			target = sampleMappingValue(sampleMappingValue(selected.Content[0], "targets"), "server-a")
			components["iam"], components["app"] = options.IAM != "external", true
			components["agent"], components["vision"] = options.Agent, options.Vision
		} else {
			base, err := readSampleNode(basePath)
			if err != nil {
				return nil, err
			}
			target = sampleMappingValue(sampleMappingValue(base.Content[0], "targets"), "server-a")
			removeSampleMappingValue(target, "values")
		}
		ansible := sampleMappingValue(target, "ansible")
		for key, value := range options.Ansible {
			node := new(yaml.Node)
			if err := node.Encode(value); err != nil {
				return nil, errors.New("cannot encode target connection settings")
			}
			setSampleMappingValue(ansible, key, node)
		}
		componentNode := new(yaml.Node)
		if err := componentNode.Encode(components); err != nil {
			return nil, err
		}
		for component, enabled := range components {
			global[component] = global[component] || enabled
		}
		if options.Address == "ip" && options.Solution {
			ip, _ := options.Ansible["ansible_host"].(string)
			if net.ParseIP(ip) == nil {
				ip = "<TARGET_IP>"
			}
			replaceSampleScalars(sampleMappingValue(target, "values"), func(value string) string {
				if net.ParseIP(ip) != nil && strings.Contains(ip, ":") {
					value = strings.ReplaceAll(value, "://<PUBLIC_HOST>", "://["+ip+"]")
					value = strings.ReplaceAll(value, "://<APP_HOST>", "://["+ip+"]")
				}
				value = strings.ReplaceAll(value, "<PUBLIC_HOST>", ip)
				return strings.ReplaceAll(value, "<APP_HOST>", ip)
			})
		}
		if len(targets) > 1 {
			// Never rewrite supplied passwords, or reuse host placeholders across
			// targets. Prefix only values in Values and the remaining base inputs.
			prefix := fmt.Sprintf("<TARGET_%d_", index+1)
			values := sampleMappingValue(target, "values")
			replaceSampleScalars(values, func(value string) string {
				return requiredPlaceholder.ReplaceAllStringFunc(value, func(marker string) string { return prefix + marker[1:] })
			})
		}
		configured[options.Name] = configuredSampleTarget{node: target, components: components}
	}
	names := make([]string, 0, len(configured))
	for name := range configured {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		selected := configured[name]
		if !equalBoolMap(selected.components, global) {
			componentNode := new(yaml.Node)
			if err := componentNode.Encode(selected.components); err != nil {
				return nil, err
			}
			componentNode.HeadComment = "이 target에 설치할 component만 활성화합니다. 생략된 component는 설치하지 않습니다."
			setSampleMappingValue(selected.node, "components", componentNode)
		}
		targetMapping.Content = append(targetMapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, selected.node)
	}
	globalNode := new(yaml.Node)
	if err := globalNode.Encode(global); err != nil {
		return nil, err
	}
	globalNode.HeadComment = "전체 target의 합집합입니다. 실제 실행 대상은 각 target의 components로 제한됩니다."
	setSampleMappingValue(root, "components", globalNode)
	document.HeadComment = "EVA 대화형 설치 설정: " + mode + "\n필수 <UPPER_CASE> 입력을 채운 뒤 workspace validate로 확인합니다.\n여러 target의 TARGET_번호_ placeholder는 각각 독립된 입력입니다.\n이 명령은 설정만 생성하며 설치/SSH 접속/서버 검증은 수행하지 않습니다."
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	return output.Bytes(), encoder.Close()
}

func equalBoolMap(left, right map[string]bool) bool {
	if len(left) != len(right) {
		return false
	}
	for key, leftValue := range left {
		if rightValue, ok := right[key]; !ok || rightValue != leftValue {
			return false
		}
	}
	return true
}

func setSampleMappingValue(mapping *yaml.Node, key string, value *yaml.Node) {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content[index+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func replaceSampleScalars(node *yaml.Node, replace func(string) string) {
	if node == nil {
		return
	}
	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
		node.Value = replace(node.Value)
	}
	for _, child := range node.Content {
		replaceSampleScalars(child, replace)
	}
}
