package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"eva-deployer/tools/eva/internal/workspace"
	"golang.org/x/term"
)

func runConfig(args []string) error {
	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	return runConfigSample(args, os.Stdin, os.Stdout, os.Stderr, interactive)
}

func runConfigSample(args []string, input io.Reader, output, messages io.Writer, interactive bool) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(output, "Usage: eva config sample [--mode cloud|remote|local] [--iam install|external] [--address shared|separate] [--protocol http|https] [--output FILE] [--non-interactive]")
		fmt.Fprintln(output, "Terminal input prompts for missing choices; menus go to stderr and YAML goes to stdout.")
		fmt.Fprintln(output, "Non-interactive defaults: cloud / install / separate / https. External IAM skips address selection; --protocol controls App only.")
		fmt.Fprintln(output, "--output creates a new 0600 file without overwriting.")
		fmt.Fprintln(output, "HTTP/HTTPS profiles target v3.3 local App/IAM sources. Release catalog and HTTP execution integration are separate.")
		return nil
	}
	if args[0] != "sample" {
		return fmt.Errorf("unknown config command %q", args[0])
	}
	flags := flag.NewFlagSet("config sample", flag.ContinueOnError)
	flags.SetOutput(messages)
	mode := flags.String("mode", "cloud", "repository mode: cloud, remote or local")
	iam := flags.String("iam", "", "IAM connection: install (same-target handoff) or external (explicit App SSO)")
	address := flags.String("address", "", "address pattern: shared or separate")
	protocol := flags.String("protocol", "", "external protocol: http or https")
	path := flags.String("output", "", "create a new private sample file (never overwrite)")
	nonInteractive := flags.Bool("non-interactive", false, "do not prompt; defaults to co-installed IAM, separate domains and HTTPS")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("config sample accepts options only; see eva config help")
	}
	options := workspace.SampleOptions{Mode: *mode, Address: *address, Protocol: *protocol, IAM: *iam}
	// Preserve the existing fully specified invocation: address+protocol implies
	// co-installation unless an IAM choice was explicitly requested.
	if options.IAM == "" && options.Address != "" && options.Protocol != "" {
		options.IAM = "install"
	}
	// Validate explicit choices before asking questions or creating files.
	check := options
	if check.IAM == "" {
		check.IAM = "install"
	}
	if check.Address == "" {
		check.Address = "separate"
	}
	if check.Protocol == "" {
		check.Protocol = "https"
	}
	if err := check.Validate(); err != nil {
		return err
	}
	if interactive && !*nonInteractive {
		reader := bufio.NewReader(input)
		if options.IAM == "" {
			choice, err := sampleChoice(reader, messages,
				"IAM 구성\n  1) IAM 함께 설치 (동일 target SSO handoff)\n  2) 외부 IAM 연동만 사용 (App SSO 직접 입력)\n선택 [1]: ", "1", "1", "2")
			if err != nil {
				return err
			}
			options.IAM = map[string]string{"1": "install", "2": "external"}[choice]
		}
		if options.IAM == "external" {
			if options.Address == "" {
				options.Address = "separate"
			}
			if options.Address != "separate" {
				return errors.New("external IAM uses its own endpoint; omit --address or use separate")
			}
		} else if options.Address == "" {
			choice, err := sampleChoice(reader, messages,
				"주소 구성\n  1) EVA APP과 EVA IAM 동일 도메인/IP, path 분리 (App /, IAM /iam)\n  2) EVA APP과 EVA IAM 다른 도메인 (각각 /)\n선택 [1]: ", "1", "1", "2")
			if err != nil {
				return err
			}
			options.Address = map[string]string{"1": "shared", "2": "separate"}[choice]
		}
		if options.Protocol == "" {
			choice, err := sampleChoice(reader, messages,
				"외부 접속 환경 (v3.3 대상)\n  1) HTTP (인증서 불필요, 격리된 검증 환경용)\n  2) HTTPS (인증서 필요, 권장)\n선택 [2]: ", "2", "1", "2")
			if err != nil {
				return err
			}
			options.Protocol = map[string]string{"1": "http", "2": "https"}[choice]
		}
	} else {
		options = check
	}
	contents, err := workspace.InstallerSampleFor(options)
	if err != nil {
		return err
	}
	profileName := options.Address
	if options.IAM == "external" {
		profileName = "external-iam"
	}
	fmt.Fprintf(messages, "설치 틀: %s / %s / %s\n", options.Mode, profileName, options.Protocol)
	if options.IAM == "external" {
		fmt.Fprintln(messages, "[외부 IAM] IAM은 설치하지 않습니다. --protocol은 App 접속 방식이며 IAM baseUrl/client 연결값과 Redis port/db/password를 확인합니다. Redis url은 선택 override입니다.")
	}
	fmt.Fprintln(messages, "[v3.3] 로컬 App/IAM 소스 기준 샘플입니다. Release catalog 버전은 변경하지 않습니다.")
	if options.Protocol == "http" {
		fmt.Fprintln(messages, "[HTTP] 암호화되지 않는 검증 환경용입니다. 실제 설치 전 Release Chart 전환과 Deployer TLS SAN/TLSStore 실행 연동이 필요합니다.")
	}
	fmt.Fprintln(messages, "[필수] <UPPER_CASE> 입력을 모두 교체하세요. validate가 미입력 필드 경로를 안내합니다. Secret 값은 출력하지 않습니다.")
	if *path == "" {
		_, err := output.Write(contents)
		return err
	}
	if err := writePrivateSample(*path, contents); err != nil {
		return err
	}
	fmt.Fprintf(messages, "설정 샘플 생성: %s (0600)\n", *path)
	return nil
}

func writePrivateSample(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create sample file (existing files are never overwritten): %w", err)
	}
	complete := false
	defer func() {
		file.Close()
		if !complete {
			os.Remove(path)
		}
	}()
	if _, err := file.Write(contents); err != nil {
		return fmt.Errorf("write sample file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close sample file: %w", err)
	}
	complete = true
	return nil
}

func sampleChoice(reader *bufio.Reader, output io.Writer, prompt, defaultChoice string, choices ...string) (string, error) {
	for {
		fmt.Fprint(output, prompt)
		line, err := reader.ReadString('\n')
		if err != nil && (err != io.EOF || len(line) == 0) {
			return "", errors.New("sample selection cancelled: no terminal input; specify --iam, --address and --protocol or --non-interactive")
		}
		choice := strings.TrimSpace(line)
		if choice == "" {
			choice = defaultChoice
		}
		for _, allowed := range choices {
			if choice == allowed {
				return choice, nil
			}
		}
		fmt.Fprintln(output, "표시된 번호를 선택하세요.")
	}
}
