#!/usr/bin/env python3
"""Reconcile selected MIG GPUs without rewriting already prepared devices."""

import argparse
import json
import re
import subprocess
import sys
import time


class ConfigurationError(RuntimeError):
    pass


def command(argv, allow_empty=False):
    result = subprocess.run(argv, capture_output=True, text=True, check=False)
    output = result.stdout + result.stderr
    if result.returncode:
        if allow_empty and result.returncode == 6 and re.search(
            r"No (?:GPU|compute) instances found", output, re.IGNORECASE
        ):
            return ""
        raise ConfigurationError(f"{' '.join(argv)} failed ({result.returncode}): {output.strip()}")
    return result.stdout


def mig_mode(index):
    mode = command([
        "nvidia-smi", "-i", index,
        "--query-gpu=mig.mode.current", "--format=csv,noheader",
    ]).strip()
    if mode not in ("Enabled", "Disabled"):
        raise ConfigurationError(f"GPU {index}: unexpected MIG mode {mode!r}")
    return mode


def inventory(index, profile, count):
    state = {"index": index, "mode": mig_mode(index), "instances": [], "ready": False}
    if state["mode"] == "Disabled":
        return state

    text = command(["nvidia-smi", "mig", "-i", index, "-lgi"], allow_empty=True)
    rows = re.findall(
        r"^\|\s*(\d+)\s+MIG\s+\S+\s+(\d+)\s+(\d+)\s+\d+:\d+\s*\|",
        text, re.MULTILINE,
    )
    if text.strip() and not rows and not re.search(r"No GPU instances found", text, re.I):
        raise ConfigurationError(f"GPU {index}: cannot read GPU instance table")
    for gpu, found_profile, instance in rows:
        if gpu != index or found_profile != profile:
            raise ConfigurationError(
                f"GPU {index}: existing MIG layout differs from profile {profile}; "
                "stop GPU workloads and change the layout during maintenance"
            )
        state["instances"].append(instance)
    if len(state["instances"]) > count:
        raise ConfigurationError(f"GPU {index}: existing MIG instance count exceeds {count}")

    ci_text = ""
    if rows:
        ci_text = command(["nvidia-smi", "mig", "-i", index, "-lci"], allow_empty=True)
    ci_rows = re.findall(
        r"^\|\s*(\d+)\s+(\d+)\s+MIG\s+\S+\s+\d+\*?\s+(\d+)\s*\|",
        ci_text, re.MULTILINE,
    )
    if ci_text.strip() and not ci_rows and not re.search(r"No compute instances found", ci_text, re.I):
        raise ConfigurationError(f"GPU {index}: cannot read compute instance table")
    ci_by_gi = {instance: [] for instance in state["instances"]}
    for gpu, gi, ci in ci_rows:
        if gpu != index or gi not in ci_by_gi:
            raise ConfigurationError(f"GPU {index}: unexpected compute instance")
        ci_by_gi[gi].append(ci)
    state["incomplete_instances"] = [gi for gi, cis in ci_by_gi.items() if len(cis) != 1]
    devices = re.findall(r"\(UUID:\s*(MIG-[^)\s]+)\)", command(["nvidia-smi", "-i", index, "-L"]))
    state["devices"] = devices
    state["ready"] = (
        len(rows) == count
        and not state["incomplete_instances"]
        and len(devices) == count
        and len(set(devices)) == count
    )
    return state


def compute_mode(selector, index):
    text = command([selector, "--listgpumodes", "--index", index])
    modes = re.findall(r"^\s*GPU Mode:\s*([^\r\n]+)", text, re.MULTILINE)
    if len(modes) != 1:
        raise ConfigurationError(f"GPU {index}: cannot determine one display mode; refusing firmware write")
    mode = modes[0].strip().lower()
    if mode in ("compute", "physical display disabled"):
        return True
    if mode in ("graphics", "physical display enabled"):
        return False
    raise ConfigurationError(f"GPU {index}: unknown display mode {modes[0]!r}; refusing firmware write")


def stop_graphics():
    # Only called when a GPU change is needed. Prepared GPUs never reach here.
    for service in ("display-manager", "gdm", "gdm3", "lightdm", "sddm"):
        subprocess.run(["systemctl", "stop", service + ".service"], capture_output=True, check=False)
    for attempt in range(5):
        processes = command(["nvidia-smi"]).partition("Processes:")[2]
        if not re.search(r"Xorg|gnome-shell|gnome-control-center", processes):
            return
        if attempt < 4:
            time.sleep(2)
    raise ConfigurationError("Graphical GPU clients remain; stop them during maintenance before changing MIG")


def prepare(indices, selector, profile, count, phase):
    states = [inventory(index, profile, count) for index in indices]
    if phase == "verify":
        unready = [state["index"] for state in states if not state["ready"]]
        if unready:
            raise ConfigurationError(f"MIG devices are not ready on GPUs {unready}")
        return {"changed": False, "reboot_required": False, "gpus": states}
    changed = False
    disabled = [state["index"] for state in states if state["mode"] == "Disabled"]
    partition_needed = phase in ("partition", "reconcile") and any(not s["ready"] for s in states)
    if phase == "partition" and disabled:
        raise ConfigurationError(f"MIG is disabled on GPUs {disabled}; enable MIG before partitioning")
    if phase in ("partition", "reconcile"):
        for state in states:
            if state.get("incomplete_instances"):
                raise ConfigurationError(
                    f"GPU {state['index']}: existing GPU instances have missing or multiple "
                    "compute instances; preserving them for maintenance"
                )
            if state["mode"] == "Enabled" and not state["ready"] and len(state["instances"]) == count:
                raise ConfigurationError(f"GPU {state['index']}: instances exist but usable MIG devices are not ready")
    display_changes = [index for index in disabled if not compute_mode(selector, index)]
    if disabled or partition_needed:
        stop_graphics()
    if phase in ("enable", "reconcile"):
        for index in display_changes:
            command([selector, "--gpumode", "compute", "--index", index, "--auto"])
            changed = True
        for index in disabled:
            # Recheck: another process may have completed the enable meanwhile.
            if mig_mode(index) == "Disabled":
                command(["nvidia-smi", "-i", index, "-mig", "1"])
                changed = True
        states = [inventory(index, profile, count) for index in indices]
    reboot_required = any(state["mode"] == "Disabled" for state in states)
    if reboot_required and phase == "reconcile":
        raise ConfigurationError("MIG remains disabled after enable; reboot is required")
    if phase in ("partition", "reconcile"):
        for index in indices:
            for attempt in range(3):
                state = inventory(index, profile, count)
                if state["ready"]:
                    break
                if state.get("incomplete_instances"):
                    raise ConfigurationError(
                        f"GPU {index}: existing GPU instances have missing or multiple compute instances; "
                        "preserving them for maintenance"
                    )
                missing = count - len(state["instances"])
                if missing <= 0:
                    raise ConfigurationError(f"GPU {index}: instances exist but usable MIG devices are not ready")
                try:
                    command(["nvidia-smi", "mig", "-i", index, "-cgi", ",".join([profile] * missing), "-C"])
                    changed = True
                except ConfigurationError:
                    if attempt == 2:
                        raise
                    # A failed create can partially succeed. Recount before retrying.
                    time.sleep(2)
            if not inventory(index, profile, count)["ready"]:
                raise ConfigurationError(f"GPU {index}: expected MIG devices are not ready after partitioning")
        states = [inventory(index, profile, count) for index in indices]
    return {"changed": changed, "reboot_required": reboot_required, "gpus": states}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=("enable", "partition", "reconcile", "verify"))
    parser.add_argument("--gpu", action="append", required=True)
    parser.add_argument("--profile", required=True)
    parser.add_argument("--count", required=True, type=int)
    parser.add_argument("--selector", required=True)
    args = parser.parse_args()
    if args.count <= 0 or not args.profile.isdigit() or any(not x.isdigit() for x in args.gpu):
        parser.error("GPU indices and profile must be numeric, and count must be positive")
    indices = list(dict.fromkeys(args.gpu))
    try:
        result = prepare(indices, args.selector, args.profile, args.count, args.phase)
    except (ConfigurationError, OSError) as error:
        print(f"gpu-mig-configure: {error}", file=sys.stderr)
        return 1
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    sys.exit(main())
