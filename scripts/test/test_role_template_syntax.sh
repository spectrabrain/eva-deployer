#!/usr/bin/env bash
set -euo pipefail

# Ansible templates every task argument with Jinja before running it. Shell
# syntax such as ${#array[@]} opens a Jinja comment ({#), so a script that is
# not wrapped in {% raw %} fails only when the task actually runs, which for
# the airgap paths happens first on a real target. Parse every templated
# string in the Ansible sources, and every .j2 template, up front.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
PYTHON="${PYTHON:-python3}"

"$PYTHON" - "$REPO_ROOT/src" <<'PY'
import pathlib
import sys

import jinja2
import yaml


class Loader(yaml.SafeLoader):
    pass


# Ansible-specific tags such as !unsafe and !vault are data, not templates.
Loader.add_multi_constructor("!", lambda loader, suffix, node: None)

environment = jinja2.Environment(extensions=["jinja2.ext.do", "jinja2.ext.loopcontrols"])
markers = ("{{", "{%", "{#")
failures = []


def strings(value, path):
    if isinstance(value, str):
        yield path, value
    elif isinstance(value, dict):
        for key, item in value.items():
            yield from strings(item, f"{path}.{key}")
    elif isinstance(value, list):
        for index, item in enumerate(value):
            yield from strings(item, f"{path}[{index}]")


def check(source, label, text):
    if not any(marker in text for marker in markers):
        return
    try:
        environment.parse(text)
    except jinja2.TemplateSyntaxError as error:
        failures.append(f"{source}: {label}: line {error.lineno}: {error.message}")


root = pathlib.Path(sys.argv[1])
checked = 0
for path in sorted(root.rglob("*.yaml")) + sorted(root.rglob("*.yml")):
    try:
        documents = list(yaml.load_all(path.read_text(), Loader=Loader))
    except yaml.YAMLError as error:
        failures.append(f"{path.relative_to(root.parent)}: invalid YAML: {error}")
        continue
    for document in documents:
        for label, text in strings(document, "$"):
            check(path.relative_to(root.parent), label, text)
    checked += 1
for path in sorted(root.rglob("*.j2")):
    check(path.relative_to(root.parent), "template", path.read_text())
    checked += 1

if failures:
    print("\n".join(failures), file=sys.stderr)
    sys.exit(1)
print(f"checked {checked} files")
PY

echo 'EVA Ansible template syntax tests passed.'
