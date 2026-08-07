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

excluded_packages = {
    "systemd",
    "systemd-dev",
    "systemd-sysv",
    "systemd-resolved",
    "systemd-timesyncd",
    "systemd-oomd",
    "udev",
    "libsystemd-shared",
    "libsystemd0",
    "libudev1",
    "libnss-systemd",
    "libpam-systemd",
    "dpkg",
    "libc6",
}
filtered_deb_files = []
for deb_file in deb_files:
    package_name = subprocess.check_output(
        ["dpkg-deb", "-f", deb_file, "Package"], text=True
    ).strip().split(":", 1)[0]
    if package_name not in excluded_packages:
        filtered_deb_files.append(deb_file)
deb_files = filtered_deb_files

bundle = {}
for deb_file in deb_files:
    package = subprocess.check_output(
        ["dpkg-deb", "-f", deb_file, "Package"], text=True
    ).strip().split(":", 1)[0]
    version = subprocess.check_output(
        ["dpkg-deb", "-f", deb_file, "Version"], text=True
    ).strip()
    bundle[package] = version

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

if problems:
    print("[ERROR] offline package bundle is not version-consistent with this host:", file=sys.stderr)
    for problem in sorted(set(problems)):
        print(f"        - {problem}", file=sys.stderr)
    print("        Install the missing dependency from the local bundle or target host package cache.", file=sys.stderr)
    sys.exit(1)

print(f"[ok] offline package bundle validated: {len(deb_files)} files")
PY
