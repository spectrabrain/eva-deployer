package remote

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const composeLabel = "com.docker.compose."

type harborRecoveryContainer struct {
	ID     string
	Image  string
	Labels map[string]string
	State  struct {
		Status, Error string
		Running       bool
	}
	Log struct {
		Type   string
		Config map[string]string
	}
	Ports map[string][]struct{ HostIP, HostPort string }
}

// Do not retrieve environment variables, mounts or other credential-bearing
// configuration. Recovery never invokes a shell or executes a Compose file.
const harborRecoveryInspect = `{"ID":{{json .Id}},"Image":{{json .Config.Image}},"Labels":{{json .Config.Labels}},"State":{"Status":{{json .State.Status}},"Error":{{json .State.Error}},"Running":{{json .State.Running}}},"Log":{{json .HostConfig.LogConfig}},"Ports":{{json .HostConfig.PortBindings}}}`

type harborRecovery struct {
	run        func(context.Context, ...string) ([]byte, error)
	localIPs   func(context.Context, string) ([]net.IP, error)
	waitLogger func(context.Context, string) error
}

func recoverLocalHarbor(ctx context.Context, registry, protocol string) (bool, error) {
	r := harborRecovery{
		run: func(ctx context.Context, args ...string) ([]byte, error) {
			// Always inspect/start on this host, not a DOCKER_HOST/context selected
			// remote daemon. Connection probing must refer to the same host.
			args = append([]string{"--host", "unix:///var/run/docker.sock"}, args...)
			return exec.CommandContext(ctx, "docker", args...).Output()
		},
		localIPs:   localHarborIPs,
		waitLogger: waitHarborLogger,
	}
	return r.recover(ctx, registry, protocol)
}

func localHarborIPs(ctx context.Context, host string) ([]net.IP, error) {
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	interfaces, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var result []net.IP
	for _, address := range addresses {
		for _, iface := range interfaces {
			ip, _, _ := net.ParseCIDR(iface.String())
			if address.IP.Equal(ip) {
				result = append(result, address.IP)
				break
			}
		}
	}
	return result, nil
}

func (r harborRecovery) inspect(ctx context.Context, ids []string) ([]harborRecoveryContainer, error) {
	args := append([]string{"inspect", "--format", harborRecoveryInspect}, ids...)
	output, err := r.run(ctx, args...)
	if err != nil {
		return nil, errors.New("cannot inspect existing local Harbor containers; they were not replaced")
	}
	var containers []harborRecoveryContainer
	remaining := make(map[string]bool, len(ids))
	for _, id := range ids {
		remaining[id] = true
	}
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	for range ids {
		var container harborRecoveryContainer
		if err := decoder.Decode(&container); err != nil || !remaining[container.ID] {
			return nil, errors.New("cannot identify existing local Harbor containers")
		}
		delete(remaining, container.ID)
		containers = append(containers, container)
	}
	return containers, nil
}

func (r harborRecovery) recover(ctx context.Context, registry, protocol string) (bool, error) {
	if protocol != "" && protocol != "http" {
		return false, nil
	}
	host, port, err := net.SplitHostPort(registry)
	if err != nil {
		host, port = registry, "80"
	}
	ips, err := r.localIPs(ctx, host)
	if err != nil || len(ips) == 0 {
		return false, nil
	}
	output, err := r.run(ctx, "ps", "--all", "--quiet", "--no-trunc", "--filter", "label="+composeLabel+"project")
	ids := strings.Fields(string(output))
	if err != nil || len(ids) == 0 {
		return false, nil // A k3s Harbor or an unavailable Docker is not ours.
	}
	containers, err := r.inspect(ctx, ids)
	if err != nil {
		return false, err
	}
	var matches []harborRecoveryContainer
	for _, container := range containers {
		service := container.Labels[composeLabel+"service"]
		if (service == "proxy" || service == "nginx") &&
			harborImage(container.Image, "nginx-photon") && container.exposes(port, ips) {
			matches = append(matches, container)
		}
	}
	if len(matches) == 0 {
		return false, nil
	}
	if len(matches) != 1 {
		return false, errors.New("local Harbor endpoint matches multiple Compose deployments; recover it manually")
	}
	proxy := matches[0]
	project := proxy.Labels[composeLabel+"project"]
	workingDir := proxy.Labels[composeLabel+"project.working_dir"]
	files := proxy.Labels[composeLabel+"project.config_files"]
	if project == "" || !filepath.IsAbs(workingDir) || files == "" {
		return false, errors.New("local Harbor Compose identity is incomplete; recover it manually")
	}
	for _, path := range strings.Split(files, ",") {
		if !filepath.IsAbs(path) || validateManagedHarborComposeFile(path) != nil {
			return false, errors.New("original local Harbor Compose configuration is unavailable; containers were not replaced")
		}
	}
	var logger *harborRecoveryContainer
	var failed []string
	for i := range containers {
		container := &containers[i]
		if container.Labels[composeLabel+"project"] != project ||
			container.Labels[composeLabel+"project.working_dir"] != workingDir ||
			container.Labels[composeLabel+"project.config_files"] != files {
			continue
		}
		if container.Labels[composeLabel+"service"] == "log" && harborImage(container.Image, "harbor-log") {
			if logger != nil {
				return false, errors.New("local Harbor has multiple log collectors; recover it manually")
			}
			logger = container
		}
		if container.syslogStartFailed() && harborImage(container.Image, "") {
			failed = append(failed, container.ID)
		}
	}
	if len(failed) == 0 {
		// Also prevent bootstrap from installing a second Harbor when a
		// matching deployment was intentionally stopped or has another fault.
		return false, errors.New("matching local Harbor has no supported syslog start failure; existing containers were left unchanged")
	}
	if logger == nil || !logger.exposes("1514", []net.IP{net.ParseIP("127.0.0.1")}) || logger.Log.Type == "syslog" {
		return false, errors.New("local Harbor syslog collector could not be verified; recover it manually")
	}
	if !logger.State.Running {
		if !slices.Contains([]string{"created", "exited"}, logger.State.Status) {
			return false, errors.New("local Harbor log collector is not ready; inspect its state and logs")
		}
		if _, err := r.run(ctx, "start", logger.ID); err != nil {
			return false, errors.New("cannot start existing Harbor log collector; inspect its state and logs")
		}
	}
	if err := r.waitLogger(ctx, "127.0.0.1:1514"); err != nil {
		return false, errors.New("existing Harbor log collector is not accepting TCP 1514; dependent containers were not started")
	}
	// Start immutable container IDs only: no Compose up, pull, recreation or
	// configuration changes. Other stopped containers remain untouched.
	if _, err := r.run(ctx, append([]string{"start"}, failed...)...); err != nil {
		return false, errors.New("cannot start existing Harbor containers after syslog recovery; inspect their state and Docker logs")
	}
	return true, nil
}

func harborImage(image, component string) bool {
	image = strings.TrimPrefix(image, "docker.io/")
	if !strings.HasPrefix(image, "goharbor/") {
		return false
	}
	name := strings.FieldsFunc(strings.TrimPrefix(image, "goharbor/"), func(r rune) bool { return r == ':' || r == '@' })
	return len(name) != 0 && (component == "" || name[0] == component)
}

func (c harborRecoveryContainer) exposes(port string, ips []net.IP) bool {
	for containerPort, bindings := range c.Ports {
		if !strings.HasSuffix(containerPort, "/tcp") {
			continue
		}
		for _, binding := range bindings {
			if binding.HostPort != port {
				continue
			}
			if binding.HostIP == "" || binding.HostIP == "0.0.0.0" || binding.HostIP == "::" {
				return true
			}
			for _, ip := range ips {
				if ip.Equal(net.ParseIP(binding.HostIP)) {
					return true
				}
			}
		}
	}
	return false
}

func (c harborRecoveryContainer) syslogStartFailed() bool {
	address := c.Log.Config["syslog-address"]
	return !c.State.Running && slices.Contains([]string{"created", "exited"}, c.State.Status) &&
		c.Log.Type == "syslog" && (address == "tcp://127.0.0.1:1514" || address == "tcp://localhost:1514") &&
		strings.Contains(c.State.Error, "failed to initialize logging driver") &&
		strings.Contains(c.State.Error, "127.0.0.1:1514") && strings.Contains(c.State.Error, "connection refused")
}

func waitHarborLogger(ctx context.Context, address string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		connection, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", address)
		if err == nil {
			connection.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
