"""Exercise GPU configuration decisions without touching a GPU or systemd."""

import importlib.util
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location(
    "gpu_mig_configure", ROOT / "roles/gpu_mig/files/gpu-mig-configure.py"
)
configure = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(configure)


class FakeGPU:
    def __init__(self, mode="Enabled", instances=4, display="Compute", profile="67"):
        self.mode = mode
        self.display = display
        self.profile = profile
        self.instances = list(range(instances))
        self.compute_instances = list(self.instances)
        self.calls = []
        self.query_failure = False
        self.partial_failure = False

    def run(self, argv, **kwargs):
        self.calls.append(argv)
        output, error, rc = "", "", 0
        if argv[0] == "systemctl":
            pass
        elif argv[0] == "selector":
            if "--listgpumodes" in argv:
                output = f"GPU ID: 0\nGPU Mode: {self.display}\n"
            else:
                self.display = "Compute"
        elif "--query-gpu=mig.mode.current" in argv:
            if self.query_failure:
                rc, error = 1, "Driver unavailable"
            else:
                output = self.mode + "\n"
        elif "-mig" in argv:
            self.mode = "Enabled"
        elif "-lgi" in argv:
            if not self.instances:
                rc, output = 6, "No GPU instances found.\n"
            else:
                output = "| GPU instances: |\n" + "".join(
                    f"| 0 MIG 1g.24gb {self.profile} {gi} {gi}:1 |\n"
                    for gi in self.instances
                )
        elif "-lci" in argv:
            if not self.compute_instances:
                rc, output = 6, "No compute instances found.\n"
            else:
                output = "| Compute instances: |\n" + "".join(
                    f"| 0 {gi} MIG 1g.24gb 0 0 |\n" for gi in self.compute_instances
                )
        elif "-L" in argv:
            output = "GPU 0: Mock GPU (UUID: GPU-0)\n" + "".join(
                f"  MIG 1g.24gb Device {gi}: (UUID: MIG-0-{gi})\n"
                for gi in self.compute_instances
            )
        elif "-cgi" in argv:
            count = len(argv[argv.index("-cgi") + 1].split(","))
            if self.partial_failure:
                count = 1
                self.partial_failure = False
                rc, error = 1, "Partially created instances"
            start = len(self.instances)
            self.instances.extend(range(start, start + count))
            self.compute_instances.extend(range(start, start + count))
        elif argv == ["nvidia-smi"]:
            output = "Processes:\nNo running processes found\n"
        else:
            raise AssertionError(f"Unexpected command: {argv}")
        return subprocess.CompletedProcess(argv, rc, output, error)

    def mutations(self):
        return [c for c in self.calls if c[0] == "systemctl" or "-mig" in c or "-cgi" in c
                or (c[0] == "selector" and "--gpumode" in c)]


class ConfigureTests(unittest.TestCase):
    def execute(self, gpu, phase="reconcile"):
        with patch.object(configure.subprocess, "run", side_effect=gpu.run), patch.object(configure.time, "sleep"):
            return configure.prepare(["0"], "selector", "67", 4, phase)

    def test_running_ready_gpu_is_untouched_on_repeated_execution(self):
        gpu = FakeGPU()
        self.assertFalse(self.execute(gpu)["changed"])
        self.assertFalse(self.execute(gpu)["changed"])
        self.assertEqual(gpu.mutations(), [])
        self.assertFalse(any(c[0] == "selector" for c in gpu.calls))

    def test_reboot_recreates_instances_without_firmware_or_mig_enable(self):
        gpu = FakeGPU(instances=0)
        self.assertTrue(self.execute(gpu)["changed"])
        self.assertEqual(len(gpu.instances), 4)
        self.assertFalse(any(c[0] == "selector" or "-mig" in c for c in gpu.calls))

    def test_compute_gpu_with_disabled_mig_only_enables_mig(self):
        gpu = FakeGPU(mode="Disabled", instances=0)
        self.execute(gpu)
        self.assertFalse(any("--gpumode" in c for c in gpu.calls))
        self.assertEqual(sum("-mig" in c for c in gpu.calls), 1)

    def test_graphics_gpu_changes_mode_before_enabling_mig(self):
        gpu = FakeGPU(mode="Disabled", instances=0, display="Graphics")
        self.execute(gpu)
        display = next(i for i, c in enumerate(gpu.calls) if "--gpumode" in c)
        enable = next(i for i, c in enumerate(gpu.calls) if "-mig" in c)
        self.assertLess(display, enable)
        self.assertIn("--index", gpu.calls[display])

    def test_unknown_display_mode_fails_without_changing_gpu(self):
        gpu = FakeGPU(mode="Disabled", instances=0, display="Unrecognized")
        with self.assertRaises(configure.ConfigurationError):
            self.execute(gpu)
        self.assertEqual(gpu.mutations(), [])

    def test_failed_inventory_does_not_change_gpu(self):
        gpu = FakeGPU()
        gpu.query_failure = True
        with self.assertRaises(configure.ConfigurationError):
            self.execute(gpu)
        self.assertEqual(gpu.mutations(), [])

    def test_existing_different_profile_is_preserved(self):
        gpu = FakeGPU(profile="68")
        with self.assertRaises(configure.ConfigurationError):
            self.execute(gpu)
        self.assertEqual(gpu.mutations(), [])

    def test_missing_compute_instance_does_not_recreate_gpu_instances(self):
        gpu = FakeGPU()
        gpu.compute_instances.pop()
        with self.assertRaises(configure.ConfigurationError):
            self.execute(gpu)
        self.assertFalse(any("-cgi" in c for c in gpu.calls))
        self.assertEqual(gpu.instances, [0, 1, 2, 3])
        self.assertEqual(gpu.mutations(), [])

    def test_partial_create_recounts_before_retry(self):
        gpu = FakeGPU(instances=2)
        gpu.partial_failure = True
        self.execute(gpu)
        creates = [c[c.index("-cgi") + 1] for c in gpu.calls if "-cgi" in c]
        self.assertEqual(creates, ["67,67", "67"])
        self.assertEqual(len(gpu.instances), 4)

    def test_enable_phase_does_not_partition(self):
        gpu = FakeGPU(mode="Disabled", instances=0)
        self.execute(gpu, "enable")
        self.assertFalse(any("-cgi" in c for c in gpu.calls))

    def test_partition_phase_does_not_enable_disabled_mig(self):
        gpu = FakeGPU(mode="Disabled", instances=0)
        with self.assertRaises(configure.ConfigurationError):
            self.execute(gpu, "partition")
        self.assertEqual(gpu.mutations(), [])


if __name__ == "__main__":
    unittest.main()
