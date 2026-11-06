package remote

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type HarborCredential struct {
	Username string
	Password string
}

var ErrHarborCredentialRequired = errors.New("Harbor publication credential is required")

func dockerConfigPath() (string, error) {
	root := os.Getenv("DOCKER_CONFIG")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", ErrHarborCredentialRequired
		}
		root = filepath.Join(home, ".docker")
	}
	return filepath.Join(root, "config.json"), nil
}

func readHarborDockerCredential(ctx context.Context, registry string) (HarborCredential, error) {
	path, err := dockerConfigPath()
	if err != nil {
		return HarborCredential{}, err
	}
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return HarborCredential{}, ErrHarborCredentialRequired
	}
	var config struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
		Helpers map[string]string `json:"credHelpers"`
		Store   string            `json:"credsStore"`
	}
	if err != nil || json.Unmarshal(contents, &config) != nil {
		return HarborCredential{}, errors.New("cannot read Docker registry credential configuration")
	}
	for _, key := range []string{registry, "https://" + registry, "http://" + registry} {
		if auth := config.Auths[key].Auth; auth != "" {
			decoded, err := base64.StdEncoding.DecodeString(auth)
			username, password, found := strings.Cut(string(decoded), ":")
			if err == nil && found && username != "" && password != "" {
				return HarborCredential{Username: username, Password: password}, nil
			}
		}
	}
	helper := config.Helpers[registry]
	if helper == "" {
		helper = config.Store
	}
	if helper == "" {
		return HarborCredential{}, ErrHarborCredentialRequired
	}
	for _, char := range helper {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-", char) {
			return HarborCredential{}, errors.New("Docker credential helper name is invalid")
		}
	}
	helperContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(helperContext, "docker-credential-"+helper, "get")
	command.Stdin = strings.NewReader(registry + "\n")
	contents, err = command.Output()
	var credential struct{ Username, Secret string }
	if err != nil || json.Unmarshal(contents, &credential) != nil || credential.Username == "" || credential.Secret == "" {
		return HarborCredential{}, ErrHarborCredentialRequired
	}
	return HarborCredential{Username: credential.Username, Password: credential.Secret}, nil
}

// Prepare only the selected EVA project. Existing repositories and artifacts
// are preserved; the token scope verifies publication without pushing a probe.
func ensureHarborPublication(ctx context.Context, receipt HarborReceipt, credential HarborCredential) error {
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(request *http.Request, previous []*http.Request) error {
		if request.URL.Host != previous[0].URL.Host || request.URL.Scheme != previous[0].URL.Scheme {
			return errors.New("Harbor publication request cannot change endpoint or protocol")
		}
		if len(previous) >= 10 {
			return errors.New("too many Harbor redirects")
		}
		return nil
	}}
	return ensureHarborPublicationWithClient(ctx, client, receipt, credential)
}

func ensureHarborPublicationWithClient(ctx context.Context, client *http.Client, receipt HarborReceipt, credential HarborCredential) error {
	if credential.Username == "" || credential.Password == "" {
		return ErrHarborCredentialRequired
	}
	endpoint := receipt.Protocol + "://" + receipt.Registry
	request := func(method, path string, body []byte) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, method, endpoint+path, bytes.NewReader(body))
		if err != nil {
			return nil, errors.New("cannot create Harbor publication request")
		}
		req.SetBasicAuth(credential.Username, credential.Password)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		response, err := client.Do(req)
		if err != nil {
			return nil, errors.New("Harbor publication API is unavailable; check connectivity and trusted CA")
		}
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			response.Body.Close()
			return nil, ErrHarborCredentialRequired
		}
		return response, nil
	}
	projectPath := "/api/v2.0/projects/" + url.PathEscape(receipt.Project)
	response, err := request(http.MethodGet, projectPath, nil)
	if err != nil {
		return err
	}
	status := response.StatusCode
	var project struct {
		Metadata map[string]string `json:"metadata"`
	}
	if status == http.StatusOK {
		err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&project)
	}
	response.Body.Close()
	if status == http.StatusNotFound {
		body, _ := json.Marshal(map[string]any{"project_name": receipt.Project, "metadata": map[string]string{"public": "false"}})
		response, err = request(http.MethodPost, "/api/v2.0/projects", body)
		if err != nil {
			return err
		}
		status = response.StatusCode
		response.Body.Close()
		if status != http.StatusCreated && status != http.StatusConflict {
			return errors.New("Harbor EVA project creation failed")
		}
		// Re-read even after a concurrent creation, so the private setting is verified.
		response, err = request(http.MethodGet, projectPath, nil)
		if err != nil {
			return err
		}
		status = response.StatusCode
		err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&project)
		response.Body.Close()
	}
	if status != http.StatusOK || err != nil {
		return errors.New("Harbor EVA project could not be validated")
	}
	if project.Metadata["public"] == "true" {
		body := []byte(`{"metadata":{"public":"false"}}`)
		response, err = request(http.MethodPut, projectPath, body)
		if err != nil {
			return err
		}
		status = response.StatusCode
		response.Body.Close()
		if status != http.StatusOK {
			return errors.New("Harbor EVA project private setting could not be applied")
		}
		response, err = request(http.MethodGet, projectPath, nil)
		if err != nil {
			return err
		}
		status = response.StatusCode
		err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&project)
		response.Body.Close()
		if status != http.StatusOK || err != nil || project.Metadata["public"] != "false" {
			return errors.New("Harbor EVA project private setting did not become effective")
		}
	} else if project.Metadata["public"] != "false" {
		return errors.New("Harbor EVA project private setting could not be confirmed")
	}
	repository := receipt.Project + "/eva-bootstrap-check"
	query := url.Values{"service": {"harbor-registry"}, "scope": {"repository:" + repository + ":pull,push"}}
	response, err = request(http.MethodGet, "/service/token?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	var token struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	status = response.StatusCode
	err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&token)
	response.Body.Close()
	if status != http.StatusOK || err != nil {
		return errors.New("Harbor publication token could not be obtained")
	}
	if token.Token == "" {
		token.Token = token.AccessToken
	}
	parts := strings.Split(token.Token, ".")
	if len(parts) != 3 {
		return errors.New("Harbor publication token is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Access []struct {
			Type, Name string
			Actions    []string
		} `json:"access"`
	}
	if err != nil || json.Unmarshal(payload, &claims) != nil {
		return errors.New("Harbor publication token is invalid")
	}
	for _, access := range claims.Access {
		if access.Type != "repository" || access.Name != repository {
			continue
		}
		pull, push := false, false
		for _, action := range access.Actions {
			pull = pull || action == "pull"
			push = push || action == "push"
		}
		if pull && push {
			return nil
		}
	}
	return fmt.Errorf("%w: the account needs image and OCI artifact pull/push access to project %s", ErrHarborCredentialRequired, receipt.Project)
}
