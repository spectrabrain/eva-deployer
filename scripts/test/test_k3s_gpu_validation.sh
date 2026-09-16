#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ANSIBLE_PLAYBOOK="${ANSIBLE_PLAYBOOK:-ansible-playbook}"
TMP_ROOT="$(mktemp -d)"
RESOURCE='nvidia.com/mig-1g.24gb'

cleanup() {
  rm -rf "$TMP_ROOT"
}
trap cleanup EXIT

if ! command -v "$ANSIBLE_PLAYBOOK" >/dev/null 2>&1 && [[ ! -x "$ANSIBLE_PLAYBOOK" ]]; then
  echo "ansible-playbook was not found: $ANSIBLE_PLAYBOOK" >&2
  exit 1
fi

run_case() {
  local name="$1"
  local allocatable="$2"
  local pods_json="$3"
  local pod_phase="$4"
  local expect_result="$5"
  local expect_validation="$6"
  local expected_allocated="$7"
  local expected_available="$8"
  local expected_ready_eva_pods="$9"
  local pod_reason="${10:-}"
  local root="$TMP_ROOT/$name"

  mkdir -p "$root/bin"
  printf '%s\n' "{\"items\":[{\"status\":{\"allocatable\":{\"$RESOURCE\":\"$allocatable\"}}}]}" >"$root/nodes.json"
  printf '%s\n' "$pods_json" >"$root/pods.json"
  : >"$root/kubectl.log"

  cat >"$root/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

args="$*"
printf '%s\n' "$args" >>"${KUBECTL_LOG:?}"

case "$args" in
  'get nodes -o yaml')
    printf '    nvidia.com/mig-1g.24gb: "4"\n'
    ;;
  'get nodes -o json')
    cat "${FIXTURE_ROOT:?}/nodes.json"
    ;;
  'get pods -A -o json')
    cat "${FIXTURE_ROOT:?}/pods.json"
    ;;
  'get pod gpu-pod -o jsonpath='*)
    printf '%s|%s|||\n' "${GPU_POD_PHASE:?}" "${GPU_POD_REASON:-}"
    ;;
  'get pod gpu-pod -o wide')
    printf 'gpu-pod %s\n' "${GPU_POD_PHASE:?}"
    ;;
  'describe pod gpu-pod')
    printf 'diagnostic pod description\n'
    ;;
  'logs gpu-pod --all-containers=true')
    printf 'diagnostic pod logs\n'
    ;;
  'logs gpu-pod')
    printf 'Test PASSED\n'
    ;;
  'get events --field-selector involvedObject.kind=Pod,involvedObject.name=gpu-pod --sort-by=.lastTimestamp')
    printf 'diagnostic event\n'
    ;;
  apply\ -f\ *)
    printf 'pod/gpu-pod created\n'
    ;;
esac
EOF
  chmod 0755 "$root/bin/kubectl"

  cat >"$root/playbook.yaml" <<EOF
---
- hosts: localhost
  gather_facts: false
  connection: local
  vars:
    eva_cache_root: $root/cache
    repository_mode: cloud_repository
    k3s_gpu_validation_pod_manifest_path: $root/k3s-gpu-pod.yaml
  tasks:
    - ansible.builtin.include_tasks: $REPO_ROOT/src/infra/roles/k3s/tasks/nvidia-device-plugin.yaml
    - ansible.builtin.assert:
        that:
          - k3s_gpu_validation_allocated | int == $expected_allocated
          - k3s_gpu_validation_available | int == $expected_available
          - k3s_gpu_validation_ready_eva_pods | length == $expected_ready_eva_pods
EOF

  local rc=0
  set +e
  PATH="$root/bin:$PATH" \
    KUBECTL_LOG="$root/kubectl.log" \
    FIXTURE_ROOT="$root" \
    GPU_POD_PHASE="$pod_phase" \
    GPU_POD_REASON="$pod_reason" \
    "$ANSIBLE_PLAYBOOK" "$root/playbook.yaml" >"$root/ansible.log" 2>&1
  rc=$?
  set -e

  if [[ "$expect_result" == 'success' ]]; then
    if (( rc != 0 )); then
      cat "$root/ansible.log" >&2
      exit 1
    fi
  else
    if (( rc == 0 )); then
      echo "$name unexpectedly succeeded" >&2
      cat "$root/ansible.log" >&2
      exit 1
    fi
  fi

  grep -Fq "allocatable=$allocatable" "$root/ansible.log"
  grep -Fq "allocated=$expected_allocated" "$root/ansible.log"
  grep -Fq "available=$expected_available" "$root/ansible.log"
  grep -Fq "ready_eva_pods=$expected_ready_eva_pods" "$root/ansible.log"

  if [[ "$expect_validation" == 'yes' ]]; then
    grep -Fq 'apply -f ' "$root/kubectl.log"
  else
    if grep -Fq 'apply -f ' "$root/kubectl.log"; then
      echo "$name created a validation pod unexpectedly" >&2
      exit 1
    fi
  fi

  case "$name" in
    upgrade-evidence)
      grep -Fq 'validation=existing-eva-workloads' "$root/ansible.log"
      ;;
    external-workloads|not-ready-eva)
      grep -Fq 'evidence_pods=none' "$root/ansible.log"
      ;;
    overallocated)
      grep -Fq 'NVIDIA resource capacity is invalid' "$root/ansible.log"
      ;;
    diagnostics-before-cleanup)
      grep -Fq 'GPU validation diagnostics collected before cleanup' "$root/ansible.log"
      local events_line cleanup_line
      events_line="$(grep -n -m1 'get events --field-selector' "$root/kubectl.log" | cut -d: -f1)"
      cleanup_line="$(grep -n -m2 'delete pod gpu-pod --ignore-not-found=true' "$root/kubectl.log" | tail -n1 | cut -d: -f1)"
      [[ -n "$events_line" && -n "$cleanup_line" ]]
      (( events_line < cleanup_line ))
      ;;
  esac
}

ready_eva_pods='{"items":[
  {"metadata":{"namespace":"eva-agent","name":"agent-1"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]},"spec":{"containers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"1"}}}]}},
  {"metadata":{"namespace":"eva-agent","name":"agent-2"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]},"spec":{"containers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"1"}}}]}},
  {"metadata":{"namespace":"eva-vision","name":"vision-1"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]},"spec":{"containers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"1"}}}]}},
  {"metadata":{"namespace":"eva-vision","name":"vision-2"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]},"spec":{"containers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"1"}}}]}}
]}'

external_pods='{"items":[
  {"metadata":{"namespace":"external","name":"workload-1"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]},"spec":{"containers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"1"}}}]}},
  {"metadata":{"namespace":"external","name":"workload-2"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]},"spec":{"containers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"1"}}}]}},
  {"metadata":{"namespace":"external","name":"workload-3"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]},"spec":{"containers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"1"}}}]}},
  {"metadata":{"namespace":"external","name":"workload-4"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]},"spec":{"containers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"1"}}}]}}
]}'

not_ready_eva_pods='{"items":[
  {"metadata":{"namespace":"eva-agent","name":"agent-not-ready-1"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}]},"spec":{"containers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"1"}}}]}},
  {"metadata":{"namespace":"eva-agent","name":"agent-not-ready-2"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}]},"spec":{"containers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"1"}}}]}},
  {"metadata":{"namespace":"eva-vision","name":"vision-not-ready-1"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}]},"spec":{"containers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"1"}}}]}},
  {"metadata":{"namespace":"eva-vision","name":"vision-not-ready-2"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}]},"spec":{"containers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"1"}}}]}}
]}'

available_one_with_eva='{"items":[{"metadata":{"namespace":"eva-agent","name":"agent-uses-three"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]},"spec":{"containers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"3"}}}]}}]}'

multi_container_and_init='{"items":[{"metadata":{"namespace":"external","name":"multi"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]},"spec":{"containers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"1"}}},{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"2"}}}],"initContainers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"1"}}}]}}]}'

multiple_init_containers='{"items":[{"metadata":{"namespace":"external","name":"multiple-init"},"status":{"phase":"Running"},"spec":{"containers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"1"}}}],"initContainers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"2"}}},{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"3"}}}]}}]}'

larger_init_container='{"items":[{"metadata":{"namespace":"external","name":"larger-init"},"status":{"phase":"Running"},"spec":{"containers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"1"}}},{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"2"}}}],"initContainers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"4"}}}]}}]}'

terminal_pods_excluded='{"items":[
  {"metadata":{"namespace":"external","name":"active"},"status":{"phase":"Running"},"spec":{"containers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"1"}}}]}},
  {"metadata":{"namespace":"external","name":"done"},"status":{"phase":"Succeeded"},"spec":{"containers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"4"}}}]}},
  {"metadata":{"namespace":"external","name":"failed"},"status":{"phase":"Failed"},"spec":{"initContainers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"4"}}}]}}
]}'

overallocated_pods='{"items":[{"metadata":{"namespace":"external","name":"too-many"},"status":{"phase":"Running"},"spec":{"containers":[{"resources":{"limits":{"nvidia.com/mig-1g.24gb":"4"}}}]}}]}'

run_case new-install 4 '{"items":[]}' Succeeded success yes 0 4 0
run_case upgrade-evidence 4 "$ready_eva_pods" Succeeded success no 4 0 4
run_case external-workloads 4 "$external_pods" Succeeded failure no 4 0 0
run_case not-ready-eva 4 "$not_ready_eva_pods" Succeeded failure no 4 0 0
run_case available-one-with-eva 4 "$available_one_with_eva" Succeeded success yes 3 1 1
run_case multi-container-and-init 5 "$multi_container_and_init" Succeeded success yes 3 2 0
run_case multiple-init-containers 5 "$multiple_init_containers" Succeeded success yes 3 2 0
run_case larger-init-container 5 "$larger_init_container" Succeeded success yes 4 1 0
run_case terminal-pods-excluded 4 "$terminal_pods_excluded" Succeeded success yes 1 3 0
run_case overallocated 3 "$overallocated_pods" Succeeded failure no 4 -1 0
run_case diagnostics-before-cleanup 1 '{"items":[]}' Pending failure yes 0 1 0 FailedScheduling

echo 'K3s GPU validation contract tests passed.'
