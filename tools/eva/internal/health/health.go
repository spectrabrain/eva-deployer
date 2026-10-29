package health

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type Component struct {
	Name      string
	Namespace string
}

type Input struct {
	RuntimeVersion string
	RuntimeError   error
	OperationState string
	OperationError error
	Components     []Component
}

type CommandResult struct {
	Output string
	Err    error
}

type Runner func(string, ...string) CommandResult

type Entry struct {
	Name    string
	Detail  string
	Healthy bool
}

type Detail struct {
	Name          string
	UnhealthyPods []string
	Workloads     []string
	Services      []string
	Events        []string
}

type Report struct {
	Entries  []Entry
	Details  []Detail
	Healthy  bool
	CanCheck bool
}

// Check gathers concise health signals without exposing Kubernetes objects in
// the default report. Callers may render Details only for verbose output.
func Check(input Input, run Runner) Report {
	report := Report{Healthy: true}
	if input.RuntimeError != nil {
		report.add("Runtime", input.RuntimeError.Error(), false)
	} else {
		report.add("Runtime", "version="+input.RuntimeVersion, true)
		report.CanCheck = true
	}

	checkService(&report, run, "Docker", "docker")
	checkService(&report, run, "k3s", "k3s")
	if report.CanCheck {
		checkNodes(&report, run)
		if requiresAccelerator(input.Components) {
			checkAcceleratorInfrastructure(&report, run)
		}
		for _, component := range input.Components {
			checkComponent(&report, run, component)
		}
	}

	if input.OperationError != nil {
		report.add("Operation", input.OperationError.Error(), false)
	} else if input.OperationState == "succeeded" {
		report.add("Operation", "succeeded", true)
	} else {
		report.add("Operation", input.OperationState, false)
	}
	return report
}

func (report *Report) add(name, detail string, healthy bool) {
	report.Entries = append(report.Entries, Entry{Name: name, Detail: detail, Healthy: healthy})
	if !healthy {
		report.Healthy = false
	}
}

func checkService(report *Report, run Runner, label, service string) {
	result := run("systemctl", "is-active", service)
	if result.Err != nil || strings.TrimSpace(result.Output) != "active" {
		detail := strings.TrimSpace(result.Output)
		if detail == "" && result.Err != nil {
			detail = result.Err.Error()
		}
		report.add(label, detail, false)
		return
	}
	report.add(label, "active", true)
}

func checkNodes(report *Report, run Runner) {
	result := run("kubectl", "get", "nodes", "-o", "json")
	if result.Err != nil {
		report.add("Kubernetes", commandFailure(result), false)
		return
	}
	var nodes nodeList
	if err := json.Unmarshal([]byte(result.Output), &nodes); err != nil {
		report.add("Kubernetes", "invalid kubectl response", false)
		return
	}
	ready := 0
	for _, node := range nodes.Items {
		for _, condition := range node.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready++
				break
			}
		}
	}
	report.add("Kubernetes", fmt.Sprintf("nodes=%d ready=%d", len(nodes.Items), ready), len(nodes.Items) > 0 && ready == len(nodes.Items))
}

func requiresAccelerator(components []Component) bool {
	for _, component := range components {
		if component.Name == "agent" || component.Name == "vision" {
			return true
		}
	}
	return false
}

func checkAcceleratorInfrastructure(report *Report, run Runner) {
	nodesResult := run("kubectl", "get", "nodes", "-o", "json")
	if nodesResult.Err != nil {
		report.add("NVIDIA GPU", commandFailure(nodesResult), false)
	} else {
		var nodes nodeList
		if err := json.Unmarshal([]byte(nodesResult.Output), &nodes); err != nil {
			report.add("NVIDIA GPU", "invalid kubectl response", false)
		} else {
			allocatable := nodes.acceleratorResources()
			report.add("NVIDIA GPU", "allocatable="+strings.Join(allocatable, ","), len(allocatable) > 0)
		}
	}

	pluginResult := run("kubectl", "get", "daemonsets", "-A", "-o", "json")
	if pluginResult.Err != nil {
		report.add("NVIDIA Device Plugin", commandFailure(pluginResult), false)
		return
	}
	var resources resourceList
	if err := json.Unmarshal([]byte(pluginResult.Output), &resources); err != nil {
		report.add("NVIDIA Device Plugin", "invalid kubectl response", false)
		return
	}
	for _, resource := range resources.Items {
		if resource.Kind == "DaemonSet" && strings.Contains(resource.Metadata.Name, "nvidia-device-plugin") {
			healthy := resource.Status.DesiredNumberScheduled > 0 && resource.Status.DesiredNumberScheduled == resource.Status.NumberReady
			report.add("NVIDIA Device Plugin", fmt.Sprintf("ready=%d/%d", resource.Status.NumberReady, resource.Status.DesiredNumberScheduled), healthy)
			return
		}
	}
	report.add("NVIDIA Device Plugin", "not found", false)
}

func checkComponent(report *Report, run Runner, component Component) {
	result := run("kubectl", "get", "pods,deployments,statefulsets,daemonsets,services,ingresses", "-n", component.Namespace, "-o", "json")
	if result.Err != nil {
		report.add("eva-"+component.Name, commandFailure(result), false)
		return
	}
	var resources resourceList
	if err := json.Unmarshal([]byte(result.Output), &resources); err != nil {
		report.add("eva-"+component.Name, "invalid kubectl response", false)
		return
	}

	workloads, ready, workloadDetails := resources.workloads()
	unhealthyPods := resources.unhealthyPods()
	ingresses := resources.count("Ingress")
	detail := fmt.Sprintf("workloads=%d ready=%d", workloads, ready)
	if ingresses > 0 {
		detail += fmt.Sprintf(" ingress=%d", ingresses)
	}
	healthy := len(unhealthyPods) == 0 && workloads == ready
	acceleratorPods := resources.acceleratorPods()
	if component.Name == "agent" || component.Name == "vision" {
		healthy = healthy && acceleratorPods > 0
		if acceleratorPods == 0 && len(unhealthyPods) == 0 {
			detail = "gpu_allocations=0"
		}
	}
	if !healthy && len(unhealthyPods) > 0 {
		detail = fmt.Sprintf("unhealthy_pods=%d", len(unhealthyPods))
	}
	report.add("eva-"+component.Name, detail, healthy)
	if !healthy {
		componentDetail := Detail{
			Name:          "eva-" + component.Name,
			UnhealthyPods: unhealthyPods,
			Workloads:     workloadDetails,
			Services:      resources.names("Service"),
		}
		events := run("kubectl", "get", "events", "-n", component.Namespace, "--sort-by=.metadata.creationTimestamp", "-o", "json")
		if events.Err == nil {
			componentDetail.Events = recentEvents(events.Output)
		}
		report.Details = append(report.Details, componentDetail)
	}
}

func commandFailure(result CommandResult) string {
	if output := strings.TrimSpace(result.Output); output != "" {
		return output
	}
	if result.Err != nil {
		return result.Err.Error()
	}
	return "command failed"
}

type nodeList struct {
	Items []struct {
		Status struct {
			Allocatable map[string]string `json:"allocatable"`
			Conditions  []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

func (nodes nodeList) acceleratorResources() []string {
	resources := make([]string, 0)
	seen := map[string]bool{}
	for _, node := range nodes.Items {
		for name, quantity := range node.Status.Allocatable {
			if (name == "nvidia.com/gpu" || strings.HasPrefix(name, "nvidia.com/mig-")) && quantity != "0" && !seen[name] {
				resources = append(resources, name+"="+quantity)
				seen[name] = true
			}
		}
	}
	sort.Strings(resources)
	return resources
}

type resourceList struct {
	Items []resource `json:"items"`
}

type resource struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Replicas   *int `json:"replicas"`
		Containers []struct {
			Resources struct {
				Limits map[string]string `json:"limits"`
			} `json:"resources"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase                  string `json:"phase"`
		Replicas               int    `json:"replicas"`
		ReadyReplicas          int    `json:"readyReplicas"`
		DesiredNumberScheduled int    `json:"desiredNumberScheduled"`
		NumberReady            int    `json:"numberReady"`
		ContainerStatuses      []struct {
			Name  string `json:"name"`
			Ready bool   `json:"ready"`
			State struct {
				Waiting struct {
					Reason string `json:"reason"`
				} `json:"waiting"`
				Terminated struct {
					Reason string `json:"reason"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

func (resources resourceList) acceleratorPods() int {
	count := 0
	for _, resource := range resources.Items {
		if resource.Kind != "Pod" || resource.Status.Phase == "Succeeded" {
			continue
		}
		for _, container := range resource.Spec.Containers {
			for name, quantity := range container.Resources.Limits {
				if (name == "nvidia.com/gpu" || strings.HasPrefix(name, "nvidia.com/mig-")) && quantity != "0" {
					count++
					goto nextPod
				}
			}
		}
	nextPod:
	}
	return count
}

func (resources resourceList) count(kind string) int {
	count := 0
	for _, resource := range resources.Items {
		if resource.Kind == kind {
			count++
		}
	}
	return count
}

func (resources resourceList) names(kind string) []string {
	var names []string
	for _, resource := range resources.Items {
		if resource.Kind == kind {
			names = append(names, strings.ToLower(kind)+"/"+resource.Metadata.Name)
		}
	}
	return names
}

func (resources resourceList) workloads() (int, int, []string) {
	count, ready := 0, 0
	var details []string
	for _, resource := range resources.Items {
		var desired, available int
		switch resource.Kind {
		case "Deployment", "StatefulSet":
			desired = resource.Status.Replicas
			if resource.Spec.Replicas != nil {
				desired = *resource.Spec.Replicas
			}
			available = resource.Status.ReadyReplicas
		case "DaemonSet":
			desired = resource.Status.DesiredNumberScheduled
			available = resource.Status.NumberReady
		default:
			continue
		}
		count++
		if desired == available {
			ready++
		}
		details = append(details, fmt.Sprintf("%s/%s %d/%d", strings.ToLower(resource.Kind), resource.Metadata.Name, available, desired))
	}
	return count, ready, details
}

func (resources resourceList) unhealthyPods() []string {
	var pods []string
	for _, resource := range resources.Items {
		if resource.Kind != "Pod" || resource.Status.Phase == "Succeeded" {
			continue
		}
		reason := ""
		if resource.Status.Phase == "Failed" || resource.Status.Phase == "Pending" {
			reason = resource.Status.Phase
		}
		for _, container := range resource.Status.ContainerStatuses {
			if container.Ready {
				continue
			}
			if container.State.Waiting.Reason != "" {
				reason = container.State.Waiting.Reason
			} else if container.State.Terminated.Reason != "" {
				reason = container.State.Terminated.Reason
			}
			break
		}
		if reason != "" {
			pods = append(pods, resource.Metadata.Name+" "+reason)
		}
	}
	return pods
}

func recentEvents(output string) []string {
	var events struct {
		Items []struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(output), &events) != nil {
		return nil
	}
	start := 0
	if len(events.Items) > 5 {
		start = len(events.Items) - 5
	}
	entries := make([]string, 0, len(events.Items)-start)
	for _, event := range events.Items[start:] {
		message := strings.TrimSpace(event.Message)
		if message == "" {
			message = strings.TrimSpace(event.Reason)
		}
		if message != "" {
			entries = append(entries, message)
		}
	}
	return entries
}
