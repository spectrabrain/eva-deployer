package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"eva-deployer/tools/eva/internal/remote"
)

// Airgap and Remote preparation share the same interactive credential input.
// The secret reader is separate so it can disable terminal echo, and its
// contents and errors are never echoed into prompts or diagnostics.
func promptPreparationAWSCredential(input io.Reader, output io.Writer, readSecret func() (string, error)) (remote.AWSCredential, error) {
	reader := bufio.NewReader(input)
	read := func(label string) (string, error) {
		fmt.Fprint(output, label)
		value, err := reader.ReadString('\n')
		if err != nil && len(value) == 0 {
			return "", err
		}
		return strings.TrimSpace(value), nil
	}
	accessKey, err := read("AWS Access Key ID: ")
	if err != nil || accessKey == "" {
		return remote.AWSCredential{}, errors.New("AWS Access Key ID is required")
	}
	fmt.Fprint(output, "AWS Secret Access Key: ")
	secret, err := readSecret()
	fmt.Fprintln(output)
	if err != nil || strings.TrimSpace(secret) == "" {
		return remote.AWSCredential{}, errors.New("AWS Secret Access Key is required")
	}
	region, err := read("AWS Region [ap-northeast-2]: ")
	if err != nil {
		return remote.AWSCredential{}, errors.New("read AWS Region")
	}
	if region == "" {
		region = remote.DefaultAWSRegion
	}
	return remote.AWSCredential{AccessKeyID: accessKey, SecretAccessKey: strings.TrimSpace(secret), Region: region}, nil
}
