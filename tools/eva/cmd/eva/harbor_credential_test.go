package main

import (
	"bytes"
	"strings"
	"testing"

	"eva-deployer/tools/eva/internal/remote"
)

func TestHarborCredentialPromptKeepsPasswordHidden(t *testing.T) {
	for _, username := range []string{"", "robot$eva+publisher"} {
		var output bytes.Buffer
		receipt := remote.HarborReceipt{Registry: "harbor.example.com:8090", Project: "eva"}
		credential, err := promptHarborCredential(receipt, strings.NewReader(username+"\n"), &output,
			func() (string, error) { return "private-token-with-spaces", nil })
		if username == "" {
			username = "admin"
		}
		if err != nil || credential.Username != username || credential.Password != "private-token-with-spaces" {
			t.Fatal("bootstrap did not read the publication credential")
		}
		if strings.Contains(output.String(), credential.Password) || strings.Contains(output.String(), "docker login") {
			t.Fatal("bootstrap exposed the password or requested manual login")
		}
	}
}
