#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ANSIBLE_PLAYBOOK="${ANSIBLE_PLAYBOOK:-ansible-playbook}"
TMP_ROOT="$(mktemp -d)"

cleanup() {
  rm -rf "$TMP_ROOT"
}
trap cleanup EXIT

if ! command -v "$ANSIBLE_PLAYBOOK" >/dev/null 2>&1 && [[ ! -x "$ANSIBLE_PLAYBOOK" ]]; then
  echo "ansible-playbook was not found: $ANSIBLE_PLAYBOOK" >&2
  exit 1
fi

make_fixture() {
  local name="$1"
  local root="$TMP_ROOT/$name"

  mkdir -p "$root/bin" "$root/templates"
  cp "$REPO_ROOT/src/solution/roles/config/templates/eva.yaml.j2" "$root/templates/eva.yaml.j2"
  cat >"$root/bin/lspci" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

if [[ "${GPU_MODE:?}" == "nvidia" ]]; then
  echo '01:00.0 VGA compatible controller: NVIDIA Corporation Test GPU'
else
  echo '00:02.0 VGA compatible controller: Intel Corporation Test GPU'
fi
EOF
  cat >"$root/bin/nvidia-smi" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

if [[ "${GPU_MODE:?}" == "nvidia" ]]; then
  echo Disabled
  exit 0
fi

exit 1
EOF
  chmod 0755 "$root/bin/lspci" "$root/bin/nvidia-smi"

  printf '%s\n' '[local]' 'localhost ansible_connection=local ansible_become=false' >"$root/inventory.ini"
  cat >"$root/playbook.yaml" <<EOF
---
- hosts: all
  gather_facts: false
  vars:
    airgap_mode: true
    config_gpu_cdi_status: enabled
    config_gpu_cdi_nvidia_ctk_path: $root/bin/nvidia-ctk
    config_gpu_cdi_runtime_config_path: $root/nvidia-container-runtime-config.toml
    eva_site_config_root: $root/config/site
    ansible_facts:
      env:
        HOME: /tmp
      mounts:
        - device: /dev/sda
          fstype: ext4
          mount: /mnt/eva
          size_total: 100
  tasks:
    - name: Detect NVIDIA GPU presence for Config
      ansible.builtin.import_tasks: $REPO_ROOT/src/infra/roles/gpu/tasks/detect.yaml

    - name: Apply GPU CDI settings
      ansible.builtin.include_tasks: $REPO_ROOT/src/solution/roles/config/tasks/gpu_cdi.yaml
      when:
        - gpu_has_nvidia | default(false) | bool
        - (config_gpu_cdi_status | default('enabled') | lower) == 'enabled'

    - name: Write EVA summary file
      ansible.builtin.import_tasks: $REPO_ROOT/src/solution/roles/config/tasks/eva.yaml

    - name: Assert expected GPU detection result
      ansible.builtin.assert:
        that:
          - (gpu_has_nvidia | bool) == (expected_gpu_has_nvidia | bool)
EOF

  printf '%s\n' "$root"
}

grep -Fq 'roles/gpu/tasks/detect.yaml' "$REPO_ROOT/src/solution/roles/config/tasks/main.yaml"
grep -Fq 'gpu_has_nvidia | default(false) | bool' "$REPO_ROOT/src/solution/roles/config/tasks/main.yaml"
grep -Fq "config_gpu_cdi_status | default('enabled')" "$REPO_ROOT/src/solution/roles/config/tasks/main.yaml"

root="$(make_fixture cpu-only)"
if ! ANSIBLE_ROLES_PATH="$REPO_ROOT/src/infra/roles:$REPO_ROOT/src/solution/roles" \
  PATH="$root/bin:$PATH" \
  GPU_MODE=cpu \
  "$ANSIBLE_PLAYBOOK" -i "$root/inventory.ini" "$root/playbook.yaml" \
  -e expected_gpu_has_nvidia=false >"$root/ansible.log" 2>&1; then
  echo 'CPU-only Config Ansible execution failed' >&2
  cat "$root/ansible.log" >&2
  false
fi
if grep -Fq 'Check nvidia-ctk binary exists' "$root/ansible.log"; then
  echo 'CPU-only Config unexpectedly executed NVIDIA CDI tasks' >&2
  false
fi
grep -Fq 'gpu_count: 0' "$root/config/site/localhost/eva.yaml"
grep -Fq 'backendHost: "localhost"' "$root/config/site/localhost/eva.yaml"
grep -Fq 'workerNum: 16' "$root/config/site/localhost/eva.yaml"
grep -Fq 'workerNum: 2' "$root/config/site/localhost/eva.yaml"
if grep -Fq 'workerNum: 0' "$root/config/site/localhost/eva.yaml"; then
  echo 'CPU-only eva.yaml contains workerNum: 0' >&2
  false
fi

root="$(make_fixture gpu-without-ctk)"
if ANSIBLE_ROLES_PATH="$REPO_ROOT/src/infra/roles:$REPO_ROOT/src/solution/roles" \
  PATH="$root/bin:$PATH" \
  GPU_MODE=nvidia \
  "$ANSIBLE_PLAYBOOK" -i "$root/inventory.ini" "$root/playbook.yaml" \
  -e expected_gpu_has_nvidia=true >"$root/ansible.log" 2>&1; then
  echo 'GPU CDI accepted a missing nvidia-ctk binary' >&2
  exit 1
fi
grep -Fq "nvidia-ctk not found at $root/bin/nvidia-ctk" "$root/ansible.log"

root="$(make_fixture gpu-with-ctk)"
printf '%s\n' '# test runtime configuration' '[nvidia-container-runtime]' >"$root/nvidia-container-runtime-config.toml"
cat >"$root/bin/nvidia-ctk" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
EOF
chmod 0755 "$root/bin/nvidia-ctk"
if ! ANSIBLE_ROLES_PATH="$REPO_ROOT/src/infra/roles:$REPO_ROOT/src/solution/roles" \
  PATH="$root/bin:$PATH" \
  GPU_MODE=nvidia \
  "$ANSIBLE_PLAYBOOK" --check -i "$root/inventory.ini" "$root/playbook.yaml" \
  -e expected_gpu_has_nvidia=true >"$root/ansible.log" 2>&1; then
  echo 'GPU CDI check-mode Ansible execution failed' >&2
  cat "$root/ansible.log" >&2
  false
fi
grep -Fq 'Generate NVIDIA CDI spec' "$root/ansible.log"
grep -Fq 'Install NVIDIA CDI refresh systemd unit' "$root/ansible.log"

echo 'Config GPU CDI contract tests passed.'
