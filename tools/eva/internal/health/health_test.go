package health

import (
	"fmt"
	"strings"
	"testing"
)

func TestCheckReportsHealthySelectedComponents(t *testing.T) {
	report := Check(Input{
		RuntimeVersion: "1.0.1", OperationState: "succeeded",
		Components: []Component{{Name: "iam", Namespace: "eva-iam"}, {Name: "app", Namespace: "eva-app"}},
	}, fakeRunner(map[string]CommandResult{
		"systemctl is-active docker": {Output: "active\n"},
		"systemctl is-active k3s":    {Output: "active\n"},
		"kubectl get nodes -o json":  {Output: `{"items":[{"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`},
		"kubectl get pods,deployments,statefulsets,daemonsets,services,ingresses -n eva-iam -o json": {Output: healthyResources("eva-iam", true)},
		"kubectl get pods,deployments,statefulsets,daemonsets,services,ingresses -n eva-app -o json": {Output: healthyResources("eva-app", true)},
	}))
	if !report.Healthy {
		t.Fatalf("report = %#v, want healthy", report)
	}
	if got := entry(report, "eva-iam").Detail; got != "workloads=1 ready=1 ingress=1" {
		t.Fatalf("IAM summary = %q", got)
	}
	if got := entry(report, "Operation").Detail; got != "succeeded" {
		t.Fatalf("operation summary = %q", got)
	}
}

func TestCheckReportsUnhealthyPodsAndVerboseDetails(t *testing.T) {
	report := Check(Input{
		RuntimeVersion: "1.0.1", OperationState: "failed",
		Components: []Component{{Name: "agent", Namespace: "eva-agent"}},
	}, fakeRunner(map[string]CommandResult{
		"systemctl is-active docker": {Output: "active\n"},
		"systemctl is-active k3s":    {Output: "active\n"},
		"kubectl get nodes -o json":  {Output: `{"items":[{"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`},
		"kubectl get pods,deployments,statefulsets,daemonsets,services,ingresses -n eva-agent -o json": {
			Output: `{"items":[
              {"kind":"Pod","metadata":{"name":"eva-agent-api-123"},"status":{"phase":"Running","containerStatuses":[{"name":"api","ready":false,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}},
              {"kind":"Deployment","metadata":{"name":"eva-agent-api"},"spec":{"replicas":1},"status":{"replicas":1,"readyReplicas":0}},
              {"kind":"Service","metadata":{"name":"eva-agent-api"}}
            ]}`,
		},
		"kubectl get events -n eva-agent --sort-by=.metadata.creationTimestamp -o json": {Output: `{"items":[{"message":"Back-off restarting failed container"}]}`},
	}))
	if report.Healthy {
		t.Fatalf("report = %#v, want unhealthy", report)
	}
	if got := entry(report, "eva-agent").Detail; got != "unhealthy_pods=1" {
		t.Fatalf("agent summary = %q", got)
	}
	if len(report.Details) != 1 || !strings.Contains(report.Details[0].UnhealthyPods[0], "CrashLoopBackOff") || report.Details[0].Events[0] != "Back-off restarting failed container" {
		t.Fatalf("verbose details = %#v", report.Details)
	}
}

func TestCheckReportsAcceleratorInfrastructureForAgent(t *testing.T) {
	report := Check(Input{
		RuntimeVersion: "1.0.1", OperationState: "succeeded",
		Components: []Component{{Name: "agent", Namespace: "eva-agent"}},
	}, fakeRunner(map[string]CommandResult{
		"systemctl is-active docker":        {Output: "active\n"},
		"systemctl is-active k3s":           {Output: "active\n"},
		"kubectl get nodes -o json":         {Output: `{"items":[{"status":{"allocatable":{"nvidia.com/mig-2g.24gb":"4"},"conditions":[{"type":"Ready","status":"True"}]}}]}`},
		"kubectl get daemonsets -A -o json": {Output: `{"items":[{"kind":"DaemonSet","metadata":{"name":"nvidia-device-plugin-daemonset"},"status":{"desiredNumberScheduled":1,"numberReady":1}}]}`},
		"kubectl get pods,deployments,statefulsets,daemonsets,services,ingresses -n eva-agent -o json": {Output: `{"items":[
          {"kind":"Pod","metadata":{"name":"eva-agent-vllm"},"spec":{"containers":[{"resources":{"limits":{"nvidia.com/mig-2g.24gb":"1"}}}]},"status":{"phase":"Running","containerStatuses":[{"name":"vllm","ready":true}]}},
          {"kind":"Deployment","metadata":{"name":"eva-agent-vllm"},"spec":{"replicas":1},"status":{"replicas":1,"readyReplicas":1}}
        ]}`},
	}))
	if !report.Healthy {
		t.Fatalf("report = %#v, want healthy", report)
	}
	if got := entry(report, "NVIDIA GPU").Detail; got != "allocatable=nvidia.com/mig-2g.24gb=4" {
		t.Fatalf("NVIDIA GPU summary = %q", got)
	}
	if got := entry(report, "NVIDIA Device Plugin").Detail; got != "ready=1/1" {
		t.Fatalf("NVIDIA Device Plugin summary = %q", got)
	}
}

func fakeRunner(results map[string]CommandResult) Runner {
	return func(name string, args ...string) CommandResult {
		key := strings.Join(append([]string{name}, args...), " ")
		result, ok := results[key]
		if !ok {
			return CommandResult{Err: fmt.Errorf("unexpected command: %s", key)}
		}
		return result
	}
}

func entry(report Report, name string) Entry {
	for _, entry := range report.Entries {
		if entry.Name == name {
			return entry
		}
	}
	return Entry{}
}

func healthyResources(namespace string, ingress bool) string {
	ingressResource := ""
	if ingress {
		ingressResource = fmt.Sprintf(`,{"kind":"Ingress","metadata":{"name":"%s"}}`, namespace)
	}
	return `{"items":[
      {"kind":"Pod","metadata":{"name":"` + namespace + `-api"},"status":{"phase":"Running","containerStatuses":[{"name":"api","ready":true}]}},
      {"kind":"Deployment","metadata":{"name":"` + namespace + `-api"},"spec":{"replicas":1},"status":{"replicas":1,"readyReplicas":1}}` + ingressResource + `
    ]}`
}
