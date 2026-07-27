#!/usr/bin/env bash
set -euo pipefail

PACKAGE_DIR="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/apt/debs}"

if [[ ! -d "$PACKAGE_DIR" ]]; then
  echo "[ERROR] offline package directory not found: $PACKAGE_DIR" >&2
  exit 1
fi

if ! command -v dpkg-deb >/dev/null 2>&1 || ! command -v dpkg-query >/dev/null 2>&1; then
  echo "[ERROR] dpkg tools are required to validate offline packages." >&2
  exit 1
fi

mapfile -t packages < <(find "$PACKAGE_DIR" -maxdepth 1 -type f -name '*.deb' -print | sort)
if [[ ${#packages[@]} -eq 0 ]]; then
  echo "[ERROR] no .deb files found in $PACKAGE_DIR" >&2
  exit 1
fi

declare -A bundle_versions=()
for package_file in "${packages[@]}"; do
  package_name="$(dpkg-deb -f "$package_file" Package)"
  package_version="$(dpkg-deb -f "$package_file" Version)"
  if [[ -n "${bundle_versions[$package_name]:-}" && "${bundle_versions[$package_name]}" != "$package_version" ]]; then
    echo "[ERROR] bundle contains multiple versions of $package_name:" >&2
    echo "        ${bundle_versions[$package_name]} and $package_version" >&2
    exit 1
  fi
  bundle_versions[$package_name]="$package_version"
done

python3 - "$PACKAGE_DIR" <<'PY'
import os
import re
import subprocess
import sys

package_dir = sys.argv[1]
deb_files = sorted(
    os.path.join(package_dir, name)
    for name in os.listdir(package_dir)
    if name.endswith(".deb")
)

bundle = {}
for deb_file in deb_files:
    package, version = subprocess.check_output(
        ["dpkg-deb", "-f", deb_file, "Package", "Version"], text=True
    ).splitlines()
    bundle[package.split(":", 1)[0]] = version

exact_dependency = re.compile(
    r"(?<![A-Za-z0-9.+-])([A-Za-z0-9.+-]+)(?::[A-Za-z0-9-]+)?\s*\(=\s*([^ )]+)\)"
)
problems = []

for deb_file in deb_files:
    package = subprocess.check_output(["dpkg-deb", "-f", deb_file, "Package"], text=True).strip()
    dependencies = subprocess.check_output(
        ["dpkg-deb", "-f", deb_file, "Pre-Depends", "Depends"], text=True
    )
    for dependency, required_version in exact_dependency.findall(dependencies):
        bundled_version = bundle.get(dependency)
        installed_version = subprocess.run(
            ["dpkg-query", "-W", "-f=${Version}", dependency],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
        ).stdout.strip()
        if bundled_version != required_version and installed_version != required_version:
            problems.append(
                f"{package} requires {dependency} (= {required_version}), "
                f"but the bundle has {bundled_version or 'no package'}"
            )

query = subprocess.check_output(
    ["dpkg-query", "-W", "-f=${binary:Package}\t${db:Status-Abbrev}\t${Pre-Depends}\t${Depends}\n"],
    text=True,
    stderr=subprocess.DEVNULL,
)
for line in query.splitlines():
    fields = line.split("\t", 3)
    if len(fields) != 4 or not fields[1].startswith("i"):
        continue
    installed_package, _, pre_depends, depends = fields
    for dependency, installed_required_version in exact_dependency.findall(
        f"{pre_depends}, {depends}"
    ):
        bundled_version = bundle.get(dependency)
        if bundled_version and bundled_version != installed_required_version:
            replacement_version = bundle.get(installed_package.split(":", 1)[0])
            if replacement_version != bundled_version:
                problems.append(
                    f"installed {installed_package} requires {dependency} (= {installed_required_version}), "
                    f"but the bundle upgrades {dependency} to {bundled_version} without a matching {installed_package} package"
                )

if problems:
    print("[ERROR] offline package bundle is not version-consistent with this host:", file=sys.stderr)
    for problem in sorted(set(problems)):
        print(f"        - {problem}", file=sys.stderr)
    print("        Rebuild install/apt/debs with ./install/download_offline_assets.sh and copy the complete directory.", file=sys.stderr)
    sys.exit(1)

print(f"[ok] offline package bundle validated: {len(deb_files)} files")
PY
