package argocd

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const evaOperatorHome = "/home/eva"

type HostKey struct {
	Address     string
	Fingerprint string
}

type HostKeyApprover func(HostKey) (bool, error)

// ConnectSSH uses password authentication and strict SSH host-key verification.
// A previously unknown host key may be registered only after explicit approval.
// A changed host key is always rejected.
func ConnectSSH(credentials Credentials) (Session, error) {
	address, err := sshAddress(credentials.Address)
	if err != nil {
		return nil, err
	}
	knownHostsPath, err := prepareKnownHosts()
	if err != nil {
		return nil, err
	}
	knownHostsCallback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf(
			"load SSH host keys from %s: %w",
			knownHostsPath,
			err,
		)
	}
	hostKeyCallback := func(
		hostname string,
		remote net.Addr,
		key ssh.PublicKey,
	) error {
		verifyErr := knownHostsCallback(hostname, remote, key)
		if verifyErr == nil {
			return nil
		}
		var keyErr *knownhosts.KeyError
		if !errors.As(verifyErr, &keyErr) {
			return verifyErr
		}
		if len(keyErr.Want) > 0 {
			return fmt.Errorf(
				"SSH host key changed for %s; expected a known key but received fingerprint %s",
				address,
				ssh.FingerprintSHA256(key),
			)
		}
		if credentials.ApproveHostKey == nil {
			return fmt.Errorf(
				"SSH host key for %s is unknown; fingerprint=%s",
				address,
				ssh.FingerprintSHA256(key),
			)
		}
		approved, err := credentials.ApproveHostKey(HostKey{
			Address:     address,
			Fingerprint: ssh.FingerprintSHA256(key),
		})
		if err != nil {
			return err
		}
		if !approved {
			return fmt.Errorf(
				"SSH host key registration was not approved for %s",
				address,
			)
		}
		if err := appendKnownHost(
			knownHostsPath,
			address,
			key,
		); err != nil {
			return err
		}
		return nil
	}

	connection, err := net.DialTimeout("tcp", address, 20*time.Second)
	if err != nil {
		return nil, fmt.Errorf(
			"connect to Argo CD management server %s: %w",
			address,
			err,
		)
	}
	config := &ssh.ClientConfig{
		User: credentials.User,
		Auth: []ssh.AuthMethod{
			ssh.Password(credentials.Password),
		},
		HostKeyCallback: hostKeyCallback,
		Timeout:         20 * time.Second,
	}
	clientConnection, channels, requests, err := ssh.NewClientConn(
		connection,
		address,
		config,
	)
	if err != nil {
		connection.Close()
		return nil, fmt.Errorf(
			"authenticate to Argo CD management server %s: %w",
			address,
			err,
		)
	}
	return &sshSession{
		client: ssh.NewClient(clientConnection, channels, requests),
	}, nil
}

func prepareKnownHosts() (string, error) {
	account, err := user.Lookup("eva")
	if err != nil {
		return "", fmt.Errorf("resolve EVA operator account: %w", err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return "", fmt.Errorf("parse EVA operator uid: %w", err)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return "", fmt.Errorf("parse EVA operator gid: %w", err)
	}

	sshDirectory := filepath.Join(evaOperatorHome, ".ssh")
	if err := ensureSafeDirectory(sshDirectory, 0o700, uid, gid); err != nil {
		return "", err
	}
	knownHostsPath := filepath.Join(sshDirectory, "known_hosts")
	if err := ensureSafeFile(knownHostsPath, 0o600, uid, gid); err != nil {
		return "", err
	}
	return knownHostsPath, nil
}

func ensureSafeDirectory(
	path string,
	mode os.FileMode,
	uid int,
	gid int,
) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, mode); err != nil {
			return fmt.Errorf("create SSH directory %s: %w", path, err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("inspect SSH directory %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf(
			"SSH directory must be a non-symlink directory: %s",
			path,
		)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("set SSH directory mode %s: %w", path, err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(path, uid, gid); err != nil {
			return fmt.Errorf("set SSH directory owner %s: %w", path, err)
		}
	}
	return nil
}

func ensureSafeFile(
	path string,
	mode os.FileMode,
	uid int,
	gid int,
) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		file, createErr := os.OpenFile(
			path,
			os.O_WRONLY|os.O_CREATE|os.O_EXCL,
			mode,
		)
		if createErr != nil {
			return fmt.Errorf(
				"create SSH known_hosts %s: %w",
				path,
				createErr,
			)
		}
		if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf(
				"close SSH known_hosts %s: %w",
				path,
				closeErr,
			)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("inspect SSH known_hosts %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf(
			"SSH known_hosts must be a regular non-symlink file: %s",
			path,
		)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("set SSH known_hosts mode %s: %w", path, err)
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(path, uid, gid); err != nil {
			return fmt.Errorf("set SSH known_hosts owner %s: %w", path, err)
		}
	}
	return nil
}

func appendKnownHost(
	path string,
	address string,
	key ssh.PublicKey,
) error {
	file, err := os.OpenFile(
		path,
		os.O_WRONLY|os.O_APPEND,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("open SSH known_hosts %s: %w", path, err)
	}
	defer file.Close()

	if err := syscall.Flock(
		int(file.Fd()),
		syscall.LOCK_EX,
	); err != nil {
		return fmt.Errorf("lock SSH known_hosts %s: %w", path, err)
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)

	line := knownhosts.Line(
		[]string{knownhosts.Normalize(address)},
		key,
	)
	if _, err := file.WriteString(line + "\n"); err != nil {
		return fmt.Errorf("append SSH known_hosts %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync SSH known_hosts %s: %w", path, err)
	}
	return nil
}

type sshSession struct {
	client *ssh.Client
}

func (session *sshSession) Run(command string) (string, error) {
	remote, err := session.client.NewSession()
	if err != nil {
		return "", err
	}
	defer remote.Close()

	output, err := remote.CombinedOutput(
		"bash -lc " + shellQuote(command),
	)
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message != "" {
			return string(output), fmt.Errorf("%w: %s", err, message)
		}
		return string(output), err
	}
	return string(output), nil
}

func (session *sshSession) Close() error {
	return session.client.Close()
}

func sshAddress(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf(
			"Argo CD management server address is required",
		)
	}
	if host, port, err := net.SplitHostPort(value); err == nil {
		if host == "" || port == "" {
			return "", fmt.Errorf(
				"invalid Argo CD management server address %q",
				value,
			)
		}
		return value, nil
	}
	return net.JoinHostPort(strings.Trim(value, "[]"), "22"), nil
}
