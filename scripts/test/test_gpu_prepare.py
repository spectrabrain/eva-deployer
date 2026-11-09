"""GPU preparation contract tests; no hardware/system commands are executed."""
import importlib.util
import json
import pathlib
import subprocess
import unittest
from unittest.mock import patch

ROOT = pathlib.Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("gpu_prepare", ROOT / "src/infra/roles/gpu_prepare/files/prepare_gpu.py")
gpu = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(gpu)
MODEL_MAP = {"NVIDIA RTX PRO 6000 Blackwell": {"gpu_mig_mig_profile_id": "67", "gpu_mig_mig_instance_count": 4},
             "NVIDIA RTX PRO 5000 Blackwell": {"gpu_mig_mig_profile_id": "73", "gpu_mig_mig_instance_count": 2}}


class Device:
    def __init__(self, mode="Enabled", profiles=(), ci=(), names=None):
        self.mode = mode
        self.profiles = list(profiles)
        self.ci = list(ci)
        self.names = names or ["NVIDIA RTX PRO 6000 Blackwell"]
        self.busy = False
        self.pending_reboot = False
        self.display = "Disabled"
        self.calls = []
        self.mutations = []

    def run(self, *args, allow_failure=False):
        self.calls.append(args)
        stdout = ""
        if args[0] == "nvidia-smi":
            if "--query-gpu=index,uuid,name,mig.mode.current,display_mode" in args:
                stdout = "\n".join(f"{index}, GPU-{index}, {name}, {self.mode}, {self.display}" for index, name in enumerate(self.names))
            elif "-lgi" in args:
                stdout = "\n".join(f"| 0 MIG 2g.24gb {profile} {index} {index * 2}:2 |" for index, profile in enumerate(self.profiles))
            elif "-lci" in args:
                stdout = "\n".join(f"| 0 {index} MIG 2g.24gb 0 0 0:2 |" for index in self.ci)
            elif "--query-compute-apps=gpu_uuid,pid" in args:
                stdout = "GPU-0, 4321" if self.busy else ""
            elif "-mig" in args:
                self.mutations.append(args)
                if not self.pending_reboot:
                    self.mode = "Enabled"
            elif "-cgi" in args:
                self.mutations.append(args)
                self.profiles = args[args.index("-cgi") + 1].split(",")
                self.ci = list(range(len(self.profiles)))
            elif "-cci" in args:
                self.mutations.append(args)
                self.ci.append(int(args[args.index("-gi") + 1]))
            else:
                raise AssertionError("Unexpected device command: " + str(args))
        elif args[0] == "/selector":
            self.mutations.append(args)
            self.mode = "Disabled"
            self.display = "Disabled"
        elif args[:3] == ("systemctl", "stop", "display-manager.service"):
            self.mutations.append(args)
        else:
            raise AssertionError("Unexpected command: " + str(args))
        return subprocess.CompletedProcess(args, 0, stdout, "")


class GPUPreparationTests(unittest.TestCase):
    def apply(self, device, **kwargs):
        with patch.object(gpu, "run", device.run):
            return gpu.apply(MODEL_MAP, **kwargs)

    def test_ready_layout_is_reused_without_device_writes(self):
        device = Device(profiles=["67"] * 4, ci=range(4))
        device.busy = True
        result = self.apply(device, agent=True)
        self.assertTrue(result["ready"])
        self.assertFalse(result["changed"])
        self.assertEqual(device.mutations, [])

    def test_fresh_gpu_enables_mig_and_creates_four_slices(self):
        device = Device(mode="Disabled")
        result = self.apply(device, agent=True)
        self.assertTrue(result["changed"])
        self.assertEqual(len(device.mutations), 2)
        self.assertIn("67,67,67,67", device.mutations[1])
        self.assertEqual(device.ci, [0, 1, 2, 3])
        self.assertFalse(any("-dgi" in call or "-dci" in call for call in device.calls))

    def test_empty_enabled_gpu_creates_instances(self):
        device = Device()
        self.apply(device)
        self.assertEqual(len(device.mutations), 1)
        self.assertIn("-cgi", device.mutations[0])

    def test_missing_compute_instances_are_filled_without_resetting_gi(self):
        device = Device(profiles=["67"] * 4, ci=[0, 2])
        self.apply(device)
        self.assertEqual(len(device.mutations), 2)
        self.assertTrue(all("-cci" in call for call in device.mutations))
        self.assertEqual(sorted(device.ci), list(range(4)))

    def test_conflicting_or_partial_gpu_partition_is_preserved(self):
        for profiles in (["67"], ["73"] * 4, ["67", "67", "73", "67"]):
            device = Device(profiles=profiles)
            with self.assertRaisesRegex(gpu.PreparationError, "Existing MIG layout"):
                self.apply(device)
            self.assertEqual(device.mutations, [])

    def test_invalid_compute_layout_is_preserved(self):
        device = Device(profiles=["67"] * 4, ci=[0, 0])
        with self.assertRaisesRegex(gpu.PreparationError, "compute instances"):
            self.apply(device)
        self.assertEqual(device.mutations, [])

    def test_workloads_are_not_killed_to_prepare_gpu(self):
        device = Device(mode="Disabled")
        device.busy = True
        with self.assertRaisesRegex(gpu.PreparationError, "workloads are running"):
            self.apply(device)
        self.assertEqual(device.mutations, [])

    def test_missing_selector_fails_before_device_writes(self):
        device = Device(mode="[N/A]")
        with self.assertRaisesRegex(gpu.PreparationError, "Display Mode Selector"):
            self.apply(device)
        self.assertEqual(device.mutations, [])

    def test_compute_mode_conversion_is_scoped_to_one_gpu(self):
        device = Device(mode="N/A")
        with patch.object(gpu.pathlib.Path, "is_file", return_value=True):
            self.apply(device, selector="/selector")
        self.assertEqual(device.mutations[0][0], "/selector")
        self.assertEqual(len(device.mutations), 3)

    def test_reboot_pending_stops_before_partitioning(self):
        device = Device(mode="Disabled")
        device.pending_reboot = True
        with self.assertRaisesRegex(gpu.PreparationError, "EVA_GPU_REBOOT_REQUIRED"):
            self.apply(device)
        self.assertEqual(len(device.mutations), 1)
        self.assertNotIn("-cgi", device.mutations[0])

    def test_graphics_mode_requires_selector_even_when_mig_reports_disabled(self):
        device = Device(mode="Disabled")
        device.display = "Enabled"
        with patch.object(gpu.pathlib.Path, "is_file", return_value=True):
            self.apply(device, selector="/selector")
        self.assertEqual(device.mutations[0], ("systemctl", "stop", "display-manager.service"))
        self.assertEqual(device.mutations[1][0], "/selector")

    def test_whole_gpu_profiles_never_enable_mig(self):
        for names in (["NVIDIA RTX A6000"], ["NVIDIA L40S"], ["NVIDIA RTX PRO 5000 Blackwell"] * 3):
            device = Device(mode="Disabled", names=names)
            self.assertFalse(self.apply(device, agent=True)["needs_mig"])
            self.assertEqual(device.mutations, [])

    def test_pro6000_mig_opt_out_preserves_busy_whole_gpu_and_display_mode(self):
        for mode in ("Disabled", "N/A", "[N/A]", "[Not Supported]"):
            device = Device(mode=mode)
            device.busy = True
            device.display = "Enabled"
            state = self.apply(device, agent=True, mig_enabled=False)
            self.assertEqual(state["status"], "whole_gpu_ready")
            self.assertFalse(state["needs_mig"])
            self.assertEqual(device.mutations, [])
            self.assertEqual(len(device.calls), 1)

    def test_pro6000_explicit_mig_enable_still_prepares_four_slices(self):
        device = Device(mode="Disabled")
        state = self.apply(device, agent=True, mig_enabled=True)
        self.assertTrue(state["needs_mig"])
        self.assertEqual(device.profiles, ["67"] * 4)

    def test_pro6000_opt_out_preserves_existing_mig_and_unknown_modes(self):
        for mode in ("Enabled", "Unknown", ""):
            device = Device(mode=mode, profiles=["67"] * 4, ci=range(4))
            with self.assertRaises(gpu.PreparationError):
                self.apply(device, mig_enabled=False)
            self.assertEqual(device.mutations, [])

    def test_boot_restoration_honors_saved_mig_opt_out(self):
        device = Device(mode="Disabled")
        device.busy = True
        model_map = dict(MODEL_MAP, _eva_mig_enabled=False)
        with patch.object(gpu, "run", device.run):
            state = gpu.apply(model_map, expected_uuid="GPU-0")
        self.assertFalse(state["needs_mig"])
        self.assertEqual(device.mutations, [])

    def test_explicit_mig_selection_requires_the_selected_hardware(self):
        for names in (["NVIDIA RTX A6000"], ["NVIDIA L40S"], ["NVIDIA RTX PRO 6000 Blackwell"] * 2):
            for enabled in (False, True):
                device = Device(mode="Disabled", names=names)
                with self.assertRaisesRegex(gpu.PreparationError, "requires one supported"):
                    self.apply(device, mig_enabled=enabled)
                self.assertEqual(device.mutations, [])

    def test_pro5000_mig_model_map_does_not_trigger_agent_mig(self):
        device = Device(mode="Disabled", names=["NVIDIA RTX PRO 5000 Blackwell"])
        with self.assertRaisesRegex(gpu.PreparationError, "Unsupported Agent"):
            self.apply(device, agent=True)
        self.assertEqual(device.mutations, [])

    def test_saved_configuration_cannot_change_a_replacement_gpu(self):
        device = Device(mode="Disabled")
        with self.assertRaisesRegex(gpu.PreparationError, "UUID changed"):
            self.apply(device, expected_uuid="GPU-old")
        self.assertEqual(device.mutations, [])


class K3sPreparationTests(unittest.TestCase):
    def test_ready_local_node_needs_no_plugin_restart(self):
        calls = []
        def run(*args, **kwargs):
            calls.append(args)
            self.assertIn("nodes", args)
            value = {"items": [{"metadata": {"name": "local-gpu"}, "status": {"nodeInfo": {"machineID": "machine-local"}, "allocatable": {"nvidia.com/mig-2g.24gb": "4"}}}]}
            return subprocess.CompletedProcess(args, 0, json.dumps(value), "")
        with patch.object(gpu, "run", run), patch.object(gpu.pathlib.Path, "is_file", return_value=True), patch.object(gpu.pathlib.Path, "read_text", return_value="machine-local"):
            self.assertFalse(gpu.reconcile_k3s(required=True)["changed"])
        self.assertEqual(len(calls), 1)

    def test_reconcile_uses_local_node_and_restarts_only_its_device_plugin(self):
        def node(name, machine, count):
            return {"metadata": {"name": name}, "status": {"nodeInfo": {"machineID": machine}, "allocatable": {"nvidia.com/mig-2g.24gb": str(count)}}}
        local = node("local-gpu", "machine-local", 0)
        other = node("other-gpu", "machine-other", 4)
        writes = []
        calls = []
        ds = {"metadata": {"uid": "plugin-uid"}, "spec": {"template": {"spec": {"containers": [{"env": [{"name": "MIG_STRATEGY", "value": "mixed"}, {"name": "DEVICE_DISCOVERY_STRATEGY", "value": "nvml"}]}]}}}}
        def run(*args, **kwargs):
            calls.append(args)
            if "nodes" in args:
                value = {"items": [other, local]}
            elif "ds" in args:
                value = ds
            elif "pods" in args:
                self.assertIn("spec.nodeName=local-gpu", args)
                value = {"items": [{"metadata": {"name": "plugin-local", "ownerReferences": [{"uid": "plugin-uid"}]}},
                                   {"metadata": {"name": "vision-workload", "ownerReferences": [{"uid": "other-owner"}]}}]}
            elif "delete" in args:
                writes.append(args)
                value = {}
            elif "node" in args:
                self.assertIn("local-gpu", args)
                value = node("local-gpu", "machine-local", 4)
            else:
                raise AssertionError(args)
            return subprocess.CompletedProcess(args, 0, json.dumps(value), "")
        with patch.object(gpu, "run", run), patch.object(gpu.pathlib.Path, "is_file", return_value=True), patch.object(gpu.pathlib.Path, "read_text", return_value="machine-local"):
            result = gpu.reconcile_k3s(required=True)
        self.assertTrue(result["changed"])
        self.assertEqual(len(writes), 1)
        self.assertIn("plugin-local", writes[0])
        self.assertNotIn("vision-workload", writes[0])

    def test_before_infra_only_explicit_prepare_can_defer_k3s(self):
        with patch.object(gpu.pathlib.Path, "is_file", return_value=False):
            self.assertEqual(gpu.reconcile_k3s()["status"], "hardware_ready_k3s_pending")
            with self.assertRaisesRegex(gpu.PreparationError, "k3s is not ready"):
                gpu.reconcile_k3s(required=True)


if __name__ == "__main__":
    unittest.main(verbosity=2)
