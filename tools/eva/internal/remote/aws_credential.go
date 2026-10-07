package remote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	DefaultAWSRegion                = "ap-northeast-2"
	DefaultManagedAWSCredentialPath = "/var/lib/eva/credentials/aws_key.ini"
)

type AWSCredential struct {
	AccessKeyID     string
	SecretAccessKey string
	Region          string
}

func ParseAWSCredential(contents []byte) (AWSCredential, error) {
	credential := AWSCredential{Region: DefaultAWSRegion}
	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return AWSCredential{}, errors.New("AWS credential format is invalid")
		}
		switch strings.TrimSpace(key) {
		case "aws_access_key_id":
			credential.AccessKeyID = strings.TrimSpace(value)
		case "aws_secret_access_key":
			credential.SecretAccessKey = strings.TrimSpace(value)
		case "region":
			credential.Region = strings.TrimSpace(value)
		default:
			return AWSCredential{}, errors.New("AWS credential contains an unsupported field")
		}
	}
	if credential.Region == "" {
		credential.Region = DefaultAWSRegion
	}
	if credential.AccessKeyID == "" || credential.SecretAccessKey == "" {
		return AWSCredential{}, errors.New("AWS credential requires access key ID and secret access key")
	}
	return credential, nil
}

func LoadAWSCredential(path string) (AWSCredential, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return AWSCredential{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return AWSCredential{}, errors.New("AWS credential must be a regular non-symlink file")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return AWSCredential{}, err
	}
	return ParseAWSCredential(contents)
}

func WriteAWSCredential(path string, credential AWSCredential) error {
	if _, err := ParseAWSCredential(formatAWSCredential(credential)); err != nil {
		return err
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return errors.New("create AWS credential directory")
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		return errors.New("set AWS credential directory permissions")
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(parent, 0, 0); err != nil {
			return errors.New("set AWS credential directory ownership")
		}
	}
	temporary, err := os.CreateTemp(parent, ".aws-key-")
	if err != nil {
		return errors.New("create temporary AWS credential")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	contents := formatAWSCredential(credential)
	if err := temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(contents)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return errors.New("write AWS credential")
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return errors.New("publish AWS credential")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return errors.New("set AWS credential permissions")
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(path, 0, 0); err != nil {
			return errors.New("set AWS credential ownership")
		}
	}
	return nil
}

func formatAWSCredential(credential AWSCredential) []byte {
	region := strings.TrimSpace(credential.Region)
	if region == "" {
		region = DefaultAWSRegion
	}
	return []byte("aws_access_key_id = " + strings.TrimSpace(credential.AccessKeyID) + "\naws_secret_access_key = " + strings.TrimSpace(credential.SecretAccessKey) + "\nregion = " + region + "\n")
}

func ValidateAWSCredential(ctx context.Context, credential AWSCredential) error {
	if _, err := ParseAWSCredential(formatAWSCredential(credential)); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "aws", "sts", "get-caller-identity", "--output", "json")
	command.Env = mergedEnvironment(os.Environ(), map[string]string{"AWS_ACCESS_KEY_ID": credential.AccessKeyID, "AWS_SECRET_ACCESS_KEY": credential.SecretAccessKey, "AWS_DEFAULT_REGION": credential.Region, "AWS_REGION": credential.Region})
	if err := command.Run(); err != nil {
		return fmt.Errorf("AWS access validation failed")
	}
	return nil
}
