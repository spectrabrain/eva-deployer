package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAWSCredentialContract(t *testing.T) {
	credential, err := ParseAWSCredential([]byte("aws_access_key_id = key\naws_secret_access_key = secret-value\nregion = \n"))
	if err != nil || credential.Region != DefaultAWSRegion {
		t.Fatalf("credential=%#v err=%v", credential, err)
	}
	for _, contents := range []string{"aws_secret_access_key = secret-value\n", "aws_access_key_id = key\n", "aws_access_key_id = key\naws_secret_access_key = secret-value\naws_session_token = unsupported\n"} {
		_, err := ParseAWSCredential([]byte(contents))
		if err == nil || strings.Contains(err.Error(), "secret-value") {
			t.Fatalf("unsafe parse result: %v", err)
		}
	}
}

func TestWriteAWSCredentialUsesRestrictiveAtomicFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials", "aws_key.ini")
	credential := AWSCredential{AccessKeyID: "key", SecretAccessKey: "secret-value"}
	if err := WriteAWSCredential(path, credential); err != nil {
		t.Fatal(err)
	}
	file, err := os.Stat(path)
	if err != nil || file.Mode().Perm() != 0o600 {
		t.Fatalf("credential file=%v err=%v", file, err)
	}
	directory, err := os.Stat(filepath.Dir(path))
	if err != nil || directory.Mode().Perm() != 0o700 {
		t.Fatalf("credential directory=%v err=%v", directory, err)
	}
	loaded, err := LoadAWSCredential(path)
	if err != nil || loaded.Region != DefaultAWSRegion || loaded.SecretAccessKey != credential.SecretAccessKey {
		t.Fatalf("loaded credential=%#v err=%v", loaded, err)
	}
}
