package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"

	"eva-deployer/tools/eva/internal/workspace"
	"golang.org/x/term"
)

func runInstallConfigureCommand(args []string) error {
	// Help must work without a terminal or resolving a home directory.
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			fmt.Fprintln(os.Stdout, "Usage: eva install configure [--output FILE]\n대화형으로 설치 틀을 만듭니다. 기본 위치: $HOME/eva-install.yaml (0600, 기존 파일 덮어쓰기 금지).\n설치나 원격 접속은 실행하지 않습니다. sudo 없이 실행하세요.")
			return nil
		}
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("install configure requires an interactive terminal; use eva config sample --non-interactive for automation")
	}
	if os.Geteuid() == 0 && os.Getenv("SUDO_USER") != "" && os.Getenv("SUDO_USER") != "root" {
		return errors.New("설정은 사용자 홈에 보관합니다. sudo 없이 eva install configure를 실행하세요")
	}
	account, err := user.Current()
	if err != nil {
		return errors.New("cannot determine current installer account")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	readPassword := func() (string, error) {
		value, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", errors.New("비밀번호 입력을 완료하지 못했습니다; 설정 파일을 생성하지 않습니다")
		}
		return string(value), nil
	}
	return runInstallConfigure(args, os.Stdin, os.Stderr, home, account.Username, readPassword)
}

type configurePrompts struct {
	input    io.Reader
	output   io.Writer
	password func() (string, error)
}

// Read only through the newline: bufio read-ahead can consume bytes intended
// for the subsequent no-echo password read on the same terminal descriptor.
func (prompt configurePrompts) line(label, fallback string) (string, error) {
	if fallback == "" {
		fmt.Fprint(prompt.output, label+": ")
	} else {
		fmt.Fprintf(prompt.output, "%s [%s]: ", label, fallback)
	}
	var value strings.Builder
	var next [1]byte
	for value.Len() <= 4096 {
		_, err := io.ReadFull(prompt.input, next[:])
		if err != nil {
			return "", errors.New("설정 구성이 취소되었습니다 (입력 종료); 파일을 생성하지 않습니다")
		}
		if next[0] == '\n' {
			text := strings.TrimSpace(value.String())
			if text == "" {
				text = fallback
			}
			return text, nil
		}
		if next[0] == 0 || next[0] == 0x1b {
			return "", errors.New("입력에 사용할 수 없는 제어 문자가 있습니다")
		}
		value.WriteByte(next[0])
	}
	return "", errors.New("입력이 너무 깁니다; 설정 파일을 생성하지 않습니다")
}

func (prompt configurePrompts) choice(menu, fallback string, options ...string) (string, error) {
	for {
		fmt.Fprintln(prompt.output, menu)
		value, err := prompt.line("선택", fallback)
		if err != nil {
			return "", err
		}
		for _, allowed := range options {
			if value == allowed {
				return value, nil
			}
		}
		fmt.Fprintln(prompt.output, "표시된 번호를 선택하세요.")
	}
}

func runInstallConfigure(args []string, input io.Reader, output io.Writer, home, username string, password func() (string, error)) error {
	flags := flag.NewFlagSet("install configure", flag.ContinueOnError)
	flags.SetOutput(output)
	path := flags.String("output", filepath.Join(home, "eva-install.yaml"), "new private installer config")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *path == "" {
		return errors.New("install configure accepts only --output FILE")
	}
	absolutePath, err := filepath.Abs(*path)
	if err != nil {
		return fmt.Errorf("resolve output location: %w", err)
	}
	*path = absolutePath
	// Refuse before collecting any password. OpenFile(O_EXCL) checks again at
	// publication time, including dangling symlinks and concurrent creators.
	if _, err := os.Lstat(*path); err == nil {
		return fmt.Errorf("설정 파일이 이미 있습니다: %s\n기존 파일을 확인하거나 --output으로 다른 위치를 지정하세요. 기존 파일은 덮어쓰지 않습니다", *path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect output location: %w", err)
	}
	prompt := configurePrompts{input: input, output: output, password: password}
	fmt.Fprintln(output, "EVA 설치 설정 구성 — 설정 파일만 생성합니다. Ctrl+C 또는 입력 종료 시 저장하지 않습니다.")
	choice, err := prompt.choice("설치환경 (저장소 접근 방식)\n  1) online — 외부 저장소 직접 접근 (온프레미스/클라우드 공통)\n  2) remote — Main Harbor에서 공급\n  3) local — Local/Airgap Harbor에서 공급", "1", "1", "2", "3")
	if err != nil {
		return err
	}
	mode := map[string]string{"1": "online", "2": "remote", "3": "local"}[choice]
	bastion := false
	if mode == "online" {
		choice, err = prompt.choice("Online 실행 방식\n  1) 현재 서버에 설치 (local connection)\n  2) 현재 서버는 Bastion/controller, 별도 타겟에 SSH 설치", "1", "1", "2")
		if err != nil {
			return err
		}
		bastion = choice == "2"
	} else {
		fmt.Fprintln(output, "Remote/Local은 현재 Target 서버에 설치합니다. Harbor 주소와 사전 공급된 Release/자산을 설정 파일에서 확인하세요.")
	}
	if bastion {
		fmt.Fprintln(output, "타겟을 한 대씩 등록합니다. 현재 Bastion은 설치 대상이 아닙니다. SSH 비밀번호를 sudo에도 사용합니다.")
		fmt.Fprintln(output, "Bastion→Target SSH 연결과 각 Target의 외부 저장소 직접 접근이 필요합니다. Bastion만 인터넷에 연결된 환경은 online이 아닙니다. 이 과정에서는 연결을 시험하지 않습니다.")
	}
	var targets []workspace.ConfigureTarget
	names, hosts := make(map[string]bool), make(map[string]bool)
	for {
		target := workspace.ConfigureTarget{Name: "server-a", Ansible: map[string]any{"ansible_connection": "local", "ansible_user": username}}
		if bastion {
			fmt.Fprintf(output, "\n타겟 #%d 등록\n", len(targets)+1)
			if err := prompt.sshTarget(&target, len(targets)+1, names, hosts, username); err != nil {
				return err
			}
			username = target.Ansible["ansible_user"].(string)
		}
		fmt.Fprintf(output, "\n[%s] 설치 구성\n", target.Name)
		reuse := false
		if len(targets) > 0 {
			previous := targets[len(targets)-1]
			fmt.Fprintf(output, "이전 타겟 선택: %s\n", configureTargetSummary(previous))
			choice, err = prompt.choice("이전 타겟의 설치 옵션 재사용 (주소/Secret은 복사하지 않음)\n  1) 재사용\n  2) 이 타겟은 별도 선택", "1", "1", "2")
			if err != nil {
				return err
			}
			if choice == "1" {
				target.Solution, target.IAM, target.Protocol, target.Address = previous.Solution, previous.IAM, previous.Protocol, previous.Address
				target.Agent, target.Vision = previous.Agent, previous.Vision
				reuse = true
			}
		}
		if !reuse {
			if err := prompt.solution(&target); err != nil {
				return err
			}
		}
		targets = append(targets, target)
		fmt.Fprintf(output, "[%s] %s\n", target.Name, configureTargetSummary(target))
		if !bastion {
			break
		}
		choice, err = prompt.choice("타겟 추가\n  1) 한 대 더 등록\n  2) 등록 완료, 설정 파일 생성", "2", "1", "2")
		if err != nil {
			return err
		}
		if choice == "2" {
			break
		}
	}
	contents, err := workspace.ConfigureSample(mode, targets)
	if err != nil {
		return err
	}
	if err := writePrivateSample(*path, contents); err != nil {
		return err
	}
	fmt.Fprintf(output, "\n설정 샘플 생성: %s (0600)\n", *path)
	fmt.Fprintln(output, "파일의 <UPPER_CASE> 항목과 주석을 따라 site ID, credential, license, 주소·인증서를 채우세요.")
	fmt.Fprintln(output, "타겟별 components와 SSO/Redis 값을 확인하고, 기본 Redis 비밀번호는 IAM과 동일하게 변경하세요. 파일에는 비밀번호가 있으므로 공유/커밋하지 마세요.")
	for _, target := range targets {
		if target.Solution {
			fmt.Fprintln(output, "[v3.3] 게시 App 3.3.0-beta.3 / IAM 3.3.0-beta.1 기준 틀입니다. HTTP는 인증서 검사·TLS 리소스 생성을 생략하고 HTTPS는 유지합니다. 실제 설치 E2E는 별도 검증하세요.")
			break
		}
	}
	fmt.Fprintf(output, "입력 확인: sudo eva workspace validate --config %s\n", shellQuoteConfigurePath(*path))
	if bastion {
		fmt.Fprintln(output, "설치/preflight는 설정된 SSH 계정·비밀번호로 연결하고 최초 host key를 자동 등록합니다. 이미 등록된 키가 변경되면 중단합니다.")
	}
	fmt.Fprintf(output, "검토 후 설치: sudo eva install --config %s\n", shellQuoteConfigurePath(*path))
	if bastion {
		fmt.Fprintln(output, "Bastion의 eva check/preflight gpu는 현재 서버 기준입니다. 타겟 상태는 타겟별로 확인하세요.")
	}
	return nil
}

var configureSSHUser = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*[$]?$`)
var configurePlaceholder = regexp.MustCompile(`<[A-Z][A-Z0-9_]*>`)

func (prompt configurePrompts) sshTarget(target *workspace.ConfigureTarget, index int, names, hosts map[string]bool, username string) error {
	for {
		name, err := prompt.line("타겟 닉네임", fmt.Sprintf("server-%d", index))
		if err != nil {
			return err
		}
		if workspace.ValidateConfigureTargetName(name) != nil || names[name] {
			fmt.Fprintln(prompt.output, "사용 가능한 고유 닉네임을 입력하세요 (영문/숫자/점/밑줄/하이픈, 예약 이름 제외).")
			continue
		}
		target.Name = name
		break
	}
	var host string
	for {
		value, err := prompt.line("타겟 IP 주소", "")
		if err != nil {
			return err
		}
		ip := net.ParseIP(value)
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || hosts[ip.String()] {
			fmt.Fprintln(prompt.output, "다른 타겟과 겹치지 않는 실제 IP를 입력하세요 (loopback/unspecified 제외).")
			continue
		}
		host = ip.String()
		break
	}
	for {
		value, err := prompt.line("SSH 계정", username)
		if err != nil {
			return err
		}
		if !configureSSHUser.MatchString(value) {
			fmt.Fprintln(prompt.output, "올바른 SSH 계정을 입력하세요.")
			continue
		}
		username = value
		break
	}
	var secret string
	for {
		fmt.Fprint(prompt.output, "SSH 비밀번호 (숨김 입력, sudo에도 동일 사용): ")
		if prompt.password == nil {
			return errors.New("secure password input is unavailable")
		}
		value, err := prompt.password()
		if err != nil {
			// Do not include reader errors, which may contain secret input.
			return errors.New("비밀번호 입력이 취소되었습니다; 파일을 생성하지 않습니다")
		}
		if value == "" || strings.ContainsAny(value, "\r\n\x00") {
			fmt.Fprintln(prompt.output, "빈 값이나 줄바꿈 없는 비밀번호를 입력하세요.")
			continue
		}
		if configurePlaceholder.MatchString(value) {
			// Required placeholder syntax is reserved throughout installer YAML.
			fmt.Fprintln(prompt.output, "<...> placeholder 형식은 비밀번호로 사용할 수 없습니다.")
			continue
		}
		secret = value
		break
	}
	target.Ansible = map[string]any{"ansible_connection": "ssh", "ansible_host": host, "ansible_user": username, "ansible_password": secret, "ansible_become": true, "ansible_become_password": secret}
	names[target.Name], hosts[host] = true, true
	return nil
}

func (prompt configurePrompts) solution(target *workspace.ConfigureTarget) error {
	choice, err := prompt.choice("설치 범위\n  1) Infra만 설치\n  2) Infra + Solution 설치", "2", "1", "2")
	if err != nil || choice == "1" {
		return err
	}
	target.Solution = true
	choice, err = prompt.choice("Solution 접속 프로토콜\n  1) HTTP — 암호화되지 않는 격리 검증 환경\n  2) HTTPS — 인증서 필요, 권장", "2", "1", "2")
	if err != nil {
		return err
	}
	target.Protocol = map[string]string{"1": "http", "2": "https"}[choice]
	choice, err = prompt.choice("Solution 주소 구성\n  1) 동일 도메인 (App /, 함께 설치 시 IAM /iam)\n  2) 별도 도메인 (App/IAM 각각 /)\n  3) IP 직접 접속 (App /, 함께 설치 시 IAM /iam)", "1", "1", "2", "3")
	if err != nil {
		return err
	}
	target.Address = map[string]string{"1": "shared", "2": "separate", "3": "ip"}[choice]
	choice, err = prompt.choice("IAM/APP 구성\n  1) IAM + APP 함께 설치 (SSO 자동 handoff)\n  2) APP만 설치 (기존 외부 IAM 연결값 직접 입력)", "1", "1", "2")
	if err != nil {
		return err
	}
	target.IAM = map[string]string{"1": "install", "2": "external"}[choice]
	if target.IAM == "external" {
		if target.Address != "ip" {
			target.Address = "separate"
		}
		fmt.Fprintln(prompt.output, "외부 IAM은 설치하지 않습니다. 위 주소/프로토콜은 App용이며 IAM baseUrl과 Redis 연결값은 파일에서 별도 설정합니다.")
	}
	fallback := "1"
	if target.IAM == "external" {
		fallback = "4"
	}
	choice, err = prompt.choice("추가 Solution 구성 (IAM/APP 선택과 별개)\n  1) Agent + Vision\n  2) Agent만\n  3) Vision만\n  4) 추가 설치 없음", fallback, "1", "2", "3", "4")
	if err != nil {
		return err
	}
	target.Agent, target.Vision = choice == "1" || choice == "2", choice == "1" || choice == "3"
	if target.Address == "ip" && target.Protocol == "https" {
		fmt.Fprintln(prompt.output, "IP 직접 HTTPS는 해당 IP SAN을 포함한 인증서를 타겟에 준비해야 합니다.")
	}
	return nil
}

func configureTargetSummary(target workspace.ConfigureTarget) string {
	if !target.Solution {
		return "Infra만"
	}
	return fmt.Sprintf("Infra + Solution / %s / %s / IAM=%s / Agent=%t / Vision=%t", target.Protocol, target.Address, target.IAM, target.Agent, target.Vision)
}

func shellQuoteConfigurePath(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "'\"'\"'") + "'"
}
