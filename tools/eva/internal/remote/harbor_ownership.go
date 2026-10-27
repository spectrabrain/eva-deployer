package remote

import (
	"context"
	"encoding/json"
	"net"
	"os/exec"
	"path/filepath"
	"strings"
)

// A receipt alone cannot establish ownership after a service migration. Require
// a local Compose nginx container configured for this endpoint before using
// managed recovery. Include stopped containers so an owned deployment can be
// recovered, while a stale receipt cannot start an unrelated Compose stack.
func ownsManagedHarbor(ctx context.Context, receipt HarborReceipt) (bool, error) {
	if receipt.InstallRoot != "/opt/eva/harbor" || receipt.Protocol != "http" {
		return false, nil
	}
	host, port, err := net.SplitHostPort(receipt.Registry)
	if err != nil {
		host, port = receipt.Registry, "80"
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return false, nil
	}
	interfaces, err := net.InterfaceAddrs()
	if err != nil {
		return false, nil
	}
	local := false
	for _, address := range addresses {
		for _, iface := range interfaces {
			ip, _, _ := net.ParseCIDR(iface.String())
			local = local || address.IP.Equal(ip)
		}
	}
	if !local {
		return false, nil
	}
	compose := filepath.Join(receipt.InstallRoot, "harbor", "docker-compose.yml")
	if validateManagedHarborComposeFile(compose) != nil {
		return false, nil
	}
	ids, err := exec.CommandContext(ctx, "docker", "compose", "--file", compose, "ps", "--all", "--quiet", "nginx").Output()
	if err != nil || len(strings.Fields(string(ids))) != 1 {
		return false, nil
	}
	contents, err := exec.CommandContext(ctx, "docker", "inspect", strings.TrimSpace(string(ids))).Output()
	if err != nil {
		return false, nil
	}
	var containers []struct {
		Config     struct{ Labels map[string]string }
		HostConfig struct {
			PortBindings map[string][]struct{ HostIP, HostPort string }
		}
	}
	if json.Unmarshal(contents, &containers) != nil || len(containers) != 1 {
		return false, nil
	}
	container := containers[0]
	if container.Config.Labels["com.docker.compose.project.working_dir"] != filepath.Dir(compose) || container.Config.Labels["com.docker.compose.service"] != "nginx" {
		return false, nil
	}
	for _, bindings := range container.HostConfig.PortBindings {
		for _, binding := range bindings {
			if binding.HostPort != port {
				continue
			}
			if binding.HostIP == "" || binding.HostIP == "0.0.0.0" || binding.HostIP == "::" {
				return true, nil
			}
			for _, address := range addresses {
				if address.IP.Equal(net.ParseIP(binding.HostIP)) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}
