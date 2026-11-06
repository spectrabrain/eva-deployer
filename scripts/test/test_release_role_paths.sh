#!/usr/bin/env bash
set -euo pipefail

# A Release install runs playbooks from the eva-infra and eva-solution
# artifacts only; the repository's scripts/ tree is not there. Every path a
# role resolves from eva_repo_root, and every file a role runs with
# ansible.builtin.script, must therefore ship in those artifacts.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

fail() {
  echo "[FAIL] $*" >&2
  exit 1
}

# The members build_release.sh puts into eva-infra and eva-solution.
mapfile -t members < <(
  awk '/^make_archive "\$repo_root" / { for (i = 4; i <= NF; i++) print $i }' "$REPO_ROOT/scripts/release/build_release.sh"
)
((${#members[@]} > 0)) || fail "could not read Release artifact members from build_release.sh"
release="$TMP_ROOT/release"
mkdir -p "$release"
for member in "${members[@]}"; do
  mkdir -p "$release/$(dirname "$member")"
  cp -a "$REPO_ROOT/$member" "$release/$member"
done
[[ -d "$release/src/infra" && -d "$release/src/solution" ]] || fail "Release artifact members lack src/infra or src/solution"

# Paths resolved from eva_repo_root must exist in the Release, except the
# out/ tree that runs generate.
while IFS=: read -r file _ reference; do
  path="${reference#*\{\{ eva_repo_root \}\}/}"
  path="${path%%[[:space:]\"\']*}"
  [[ "$path" == out/* ]] && continue
  [[ -e "$release/$path" ]] || fail "${file#"$release"/} resolves eva_repo_root/$path, which is not in the Release"
done < <(grep -rn --include='*.yaml' --include='*.j2' '{{ eva_repo_root }}/' "$release/src" || true)

# ansible.builtin.script runs a file from the role's own files/ directory.
while IFS= read -r task_file; do
  role_root="${task_file%/tasks/*}"
  while IFS= read -r script; do
    [[ -f "$role_root/files/$script" ]] || fail "${task_file#"$release"/} runs $script, which is not in ${role_root#"$release"/}/files"
  done < <(awk '/ansible\.builtin\.script:/ { getline; if ($1 == "cmd:") print $2 }' "$task_file")
done < <(find "$release/src" -path '*/roles/*/tasks/*.yaml' -type f)

# The shipped validator works from the Release, and the repository wrapper
# still serves manual diagnosis.
command -v dpkg-deb >/dev/null 2>&1 || fail "dpkg-deb is required"
build_deb() {
  local name="$1" version="$2" root="$TMP_ROOT/deb-$1-$2"
  mkdir -p "$root/DEBIAN"
  printf 'Package: %s\nVersion: %s\nArchitecture: all\nMaintainer: EVA <eva@example.invalid>\nDescription: test\n' "$name" "$version" >"$root/DEBIAN/control"
  dpkg-deb --build --root-owner-group "$root" "$3/${name}_${version}_all.deb" >/dev/null
}
debs="$TMP_ROOT/debs"
mkdir -p "$debs"
build_deb eva-test-package 1.0 "$debs"
validator="$release/src/infra/roles/base/files/validate_offline_debs.sh"
bash "$validator" "$debs" >/dev/null || fail "shipped validator rejected a valid bundle"
bash "$REPO_ROOT/scripts/install/validate_offline_debs.sh" "$debs" >/dev/null || fail "repository wrapper rejected a valid bundle"
build_deb eva-test-package 2.0 "$debs"
if bash "$validator" "$debs" >/dev/null 2>&1; then
  fail "shipped validator accepted two versions of one package"
fi

# Ansible resolves the script from the Release role, not the repository.
ANSIBLE_PLAYBOOK="${ANSIBLE_PLAYBOOK:-ansible-playbook}"
if ! command -v "$ANSIBLE_PLAYBOOK" >/dev/null 2>&1 && [[ ! -x "$ANSIBLE_PLAYBOOK" ]]; then
  fail "ansible-playbook was not found: $ANSIBLE_PLAYBOOK"
fi
cache="$TMP_ROOT/cache with space"
mkdir -p "$cache/apt/debs"
build_deb eva-test-package 1.0 "$cache/apt/debs"
cat >"$TMP_ROOT/validate.yaml" <<'PLAYBOOK'
---
- hosts: localhost
  gather_facts: false
  connection: local
  tasks:
    - name: Run the base role offline package validation from the Release
      ansible.builtin.include_role:
        name: base
        tasks_from: validate_offline_debs.yaml
PLAYBOOK
run_role_validation() {
  ANSIBLE_ROLES_PATH="$release/src/infra/roles" "$ANSIBLE_PLAYBOOK" \
    -e "{\"eva_cache_root\": \"$cache\"}" "$TMP_ROOT/validate.yaml" >"$TMP_ROOT/ansible.log" 2>&1
}
run_role_validation || {
  cat "$TMP_ROOT/ansible.log" >&2
  fail "base role could not run its shipped validator from the Release"
}
build_deb eva-test-package 2.0 "$cache/apt/debs"
if run_role_validation; then
  fail "base role validation accepted two versions of one package"
fi

echo 'EVA Release role path contract tests passed.'
