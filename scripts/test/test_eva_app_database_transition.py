"""Fast, offline tests of the production DB transition helper (no live cluster)."""

import base64
import copy
import csv
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import yaml


ROOT = Path(__file__).resolve().parents[2]
sys.dont_write_bytecode = True  # Do not add cache files to packaged role sources.
HELPER = ROOT / "src/solution/roles/eva_app/files/database_transition.py"
SPEC = importlib.util.spec_from_file_location("database_transition", HELPER)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


def deployment(name, component, containers, volumes):
    return {"kind": "Deployment", "metadata": {"name": name, "uid": name + "-uid"},
            "spec": {"replicas": 0, "template": {"metadata": {"labels": {"component": component}},
                                  "spec": {"containers": containers, "volumes": volumes}}}}


def fixture(root, live="eva", target="eva_v3_0"):
    env = {"MYSQL_NAME": live, "MYSQL_USER": "root", "MYSQL_PSWD": "private-test-password",
           "MYSQL_HOST": "eva-app-mysql-internal.eva-app", "MYSQL_PORT": "3306"}
    mounts = [{"name": "data", "mountPath": "/apps/backend/app/data"},
              {"name": "license", "mountPath": "/apps/backend/app/.license/LicenseSpring"}]
    app = deployment("eva-app", "eva-app", [{"name": "eva-app", "envFrom": [{"secretRef": {"name": "app-secret"}}],
                                             "volumeMounts": mounts}],
                     [{"name": "data", "hostPath": {"path": "/eva-app/data"}},
                      {"name": "license", "hostPath": {"path": "//eva-app/.license/LicenseSpring"}}])
    mysql = deployment("eva-app-mysql", "eva-app-mysql",
                       [{"name": "eva-app-mysql", "env": [{"name": "MYSQL_ROOT_PASSWORD", "value": env["MYSQL_PSWD"]}],
                         "volumeMounts": [{"name": "mysql", "mountPath": "/var/lib/mysql"}]}],
                       [{"name": "mysql", "hostPath": {"path": "//eva-app/mysql"}}])
    secret = {"kind": "Secret", "metadata": {"name": "app-secret"},
              "data": {k: base64.b64encode(v.encode()).decode() for k, v in env.items()}}
    resources = copy.deepcopy([app, mysql, secret])
    resources[2]["data"]["MYSQL_NAME"] = base64.b64encode(target.encode()).decode()
    transition = FakeTransition("eva-app", "eva-app", resources, root)
    transition.objects = {("deployment", "eva-app"): app, ("deployment", "eva-app-mysql"): mysql,
                          ("secret", "app-secret"): secret,
                          ("replicaset", "db-rs"): {"metadata": {"uid": "rs-uid", "ownerReferences": [
                              {"controller": True, "uid": "eva-app-mysql-uid"}]}}}
    transition.pods = [{"metadata": {"name": "db-pod", "labels": {"component": "eva-app-mysql"},
                                    "ownerReferences": [{"controller": True, "kind": "ReplicaSet", "name": "db-rs", "uid": "rs-uid"}]},
                        "spec": copy.deepcopy(mysql["spec"]["template"]["spec"]),
                        "status": {"containerStatuses": [{"name": "eva-app-mysql", "ready": True}]}}]
    return transition


class FakeTransition(MODULE.Transition):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.objects = {}
        self.pods = []
        self.databases = {"eva": {"eva_id": "old-id", "tables": {"alembic_version": "1", "config": "2", "user": "2"},
                                  "alembic": "pre-iam", "columns": ["seq", "user_name", "user_password"],
                                  "rows": [["1", '홍길동, "관리자"\n두번째 줄\t탭', "$2b$private"], ["2", "", None]]}}
        self.calls = []
        self.fail_restore = False
        self.corrupt_restore = False
        self.special_objects = "0"
        self.dump_fails = False

    def get(self, kind, name):
        return self.objects.get((kind, name))

    def run_command(self, args, **kwargs):
        self.calls.append(args)
        return subprocess.CompletedProcess(args, 0, stdout=json.dumps({"items": self.pods}).encode())

    def tables(self, db):
        return sorted(self.databases.get(db, {}).get("tables", {}))

    def sql(self, statement):
        self.calls.append(statement)
        if "information_schema.COLUMNS" in statement:
            db = statement.split("TABLE_SCHEMA='")[1].split("'")[0]
            return "\n".join(self.databases[db]["columns"])
        if "information_schema.TABLES" in statement:
            return self.special_objects
        if "information_schema.PROCESSLIST" in statement:
            return "0"
        if "information_schema.SCHEMATA" in statement:
            return "utf8mb4\tutf8mb4_0900_ai_ci"
        if statement.startswith("CREATE DATABASE"):
            return ""
        db = statement.split("FROM `")[1].split("`")[0]
        data = self.databases[db]
        if "SELECT HEX(value)" in statement:
            return data["eva_id"].encode().hex().upper()
        if "SELECT COUNT(*)" in statement:
            table = statement.split("`.`")[1].split("`")[0]
            return data["tables"][table]
        if "SELECT version_num" in statement:
            return data["alembic"]
        if statement.startswith("SELECT IF("):
            return "\n".join("\t".join("N" if cell is None else "H" + cell.encode().hex().upper() for cell in row)
                             for row in data["rows"])
        raise AssertionError("Unexpected SQL: " + statement)

    def dump(self, db, path):
        if self.dump_fails:
            raise MODULE.UnsafeTransition("Injected dump failure")
        with path.open("x") as stream:
            stream.write(json.dumps(self.databases[db], sort_keys=True))

    def mysql(self, args, **kwargs):
        self.calls.append(args)
        if self.fail_restore:
            self.databases[args[-1]] = {"tables": {"partial": "0"}}
            raise MODULE.UnsafeTransition("Injected restore failure")
        self.databases[args[-1]] = json.load(kwargs["stdin"])
        if self.corrupt_restore:
            self.databases[args[-1]]["eva_id"] = "wrong-id"


class DatabaseTransitionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name) / "backups"
        self.old_umask = os.umask(0o077)
        self.t = fixture(self.root)

    def tearDown(self):
        os.umask(self.old_umask)
        self.temp.cleanup()

    def test_copy_preserves_original_identity_and_all_data(self):
        original = copy.deepcopy(self.t.databases["eva"])
        result = self.t.run()
        self.assertTrue(result["copied"])
        self.assertEqual(original, self.t.databases["eva"])
        self.assertEqual(original, self.t.databases["eva_v3_0"])
        directory = Path(result["backup_directory"])
        self.assertTrue((directory / "copy-complete.json").is_file())
        for p in directory.iterdir():
            self.assertEqual(p.stat().st_mode & 0o777, 0o600)
        self.assertEqual(directory.stat().st_mode & 0o777, 0o700)
        self.assertNotIn("DROP DATABASE", str(self.t.calls))
        self.assertNotIn("private-test-password", json.dumps(result))

    def test_full_csv_round_trips_unicode_quotes_commas_and_newlines(self):
        result = self.t.run()
        with Path(result["legacy_user_csv"][0]).open(newline="") as stream:
            rows = list(csv.reader(stream))
        source = self.t.databases["eva"]
        self.assertEqual(rows[0], source["columns"])
        self.assertEqual(rows[1], source["rows"][0])
        self.assertEqual(rows[2], ["2", "", "\\N"])
        metadata = json.loads((Path(result["backup_directory"]) / "user_accounts_all.metadata.json").read_text())
        self.assertEqual(metadata["rows"], 2)
        self.assertTrue(metadata["contains_password_hashes"])

    def test_pre32_export_even_when_name_already_canonical(self):
        self.t = fixture(self.root, live="eva_v3_0")
        self.t.databases = {"eva_v3_0": self.t.databases["eva"]}
        result = self.t.run()
        self.assertFalse(result["copied"])
        self.assertEqual(len(result["legacy_user_csv"]), 1)
        self.assertTrue((Path(result["backup_directory"]) / "eva_v3_0.sql").is_file())

    def test_pre32_custom_name_export_without_rename(self):
        self.t = fixture(self.root, live="customer", target="customer")
        self.t.databases = {"customer": self.t.databases["eva"]}
        self.assertEqual(len(self.t.run()["legacy_user_csv"]), 1)

    def test_post32_same_name_no_unnecessary_backup(self):
        self.t = fixture(self.root, live="eva", target="eva")
        self.t.databases["eva"]["columns"] = ["seq", "keycloak_user_id"]
        self.assertFalse(self.t.run()["changed"])

    def test_populated_destination_never_overwritten(self):
        self.t.databases["eva_v3_0"] = copy.deepcopy(self.t.databases["eva"])
        before = copy.deepcopy(self.t.databases)
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "populated"):
            self.t.run()
        self.assertEqual(before, self.t.databases)

    def test_previous_failed_switch_with_different_id_refused(self):
        self.t = fixture(self.root, live="eva_v3_0")
        self.t.databases["eva_v3_0"] = copy.deepcopy(self.t.databases["eva"])
        self.t.databases["eva_v3_0"]["eva_id"] = "new-id"
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "different eva_id"):
            self.t.run()

    def test_manual_source_allows_intentional_current_identity(self):
        self.t = fixture(self.root, live="eva_v3_0")
        self.t.databases["eva_v3_0"] = copy.deepcopy(self.t.databases["eva"])
        self.t.databases["eva_v3_0"]["eva_id"] = "intentional-id"
        self.t.explicit_source = "eva_v3_0"
        self.assertFalse(self.t.run()["copied"])

    def test_missing_id_stops_before_copy(self):
        self.t.databases["eva"]["eva_id"] = ""
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "eva_id"):
            self.t.run()
        self.assertNotIn("eva_v3_0", self.t.databases)

    def test_disabled_copy_does_not_disable_csv(self):
        self.t.auto = False
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "enable eva_app_db_auto_migrate"):
            self.t.run()
        self.assertTrue(list(self.root.glob("*/user_accounts_all.csv")))
        self.assertNotIn("eva_v3_0", self.t.databases)

    def test_dump_failure_never_creates_destination(self):
        self.t.dump_fails = True
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "dump failure"):
            self.t.run()
        self.assertNotIn("eva_v3_0", self.t.databases)

    def test_restore_failure_retains_source_and_retry_refuses_partial(self):
        self.t.fail_restore = True
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "restore failure"):
            self.t.run()
        self.assertEqual(self.t.databases["eva"]["eva_id"], "old-id")
        self.assertTrue(list(self.root.glob("*/eva.sql")))
        self.assertFalse(list(self.root.glob("*/copy-complete.json")))
        self.t.fail_restore = False
        with self.assertRaises((MODULE.UnsafeTransition, KeyError)):
            self.t.run()

    def test_identity_verification_failure_blocks_upgrade(self):
        self.t.corrupt_restore = True
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "verification failed"):
            self.t.run()

    def test_schema_objects_require_manual_copy(self):
        self.t.special_objects = "1"
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "stored objects"):
            self.t.run()

    def test_active_app_or_foreign_mysql_owner_refused(self):
        self.t.pods.append({"metadata": {"labels": {"component": "eva-app"}}})
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "terminated"):
            self.t.run()
        self.t.pods.pop()
        self.t.objects[("replicaset", "db-rs")]["metadata"]["ownerReferences"] = []
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "ownership"):
            self.t.run()

    def test_storage_and_credential_changes_refused(self):
        self.t.resources[2]["data"]["MYSQL_PSWD"] = base64.b64encode(b"new-password").decode()
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "credentials"):
            self.t.run()
        self.t = fixture(self.root)
        self.t.resources[0]["spec"]["template"]["spec"]["volumes"][1]["hostPath"]["path"] = "/new/license"
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "license storage"):
            self.t.run()

    def test_new_install_and_external_existing_db(self):
        self.t.objects = {}
        self.assertFalse(self.t.run()["changed"])
        self.t = fixture(self.root)
        del self.t.objects[("deployment", "eva-app-mysql")]
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "external DB"):
            self.t.run()
        self.t.objects = {}
        self.t.resources = [r for r in self.t.resources if r.get("metadata", {}).get("name") != "eva-app-mysql"]
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "External DB"):
            self.t.run()

    def test_scaled_deployment_and_running_db_storage_required(self):
        self.t.objects[("deployment", "eva-app")]["spec"]["replicas"] = 1
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "scaled to zero"):
            self.t.run()
        self.t.objects[("deployment", "eva-app")]["spec"]["replicas"] = 0
        self.t.pods[0]["spec"]["volumes"][0]["hostPath"]["path"] = "/wrong/mysql"
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "storage differ"):
            self.t.run()

    def test_other_writers_and_concurrent_preparation_refused(self):
        original_sql = self.t.sql

        def busy(statement):
            return "1" if "information_schema.PROCESSLIST" in statement else original_sql(statement)

        with patch.object(self.t, "sql", side_effect=busy):
            with self.assertRaisesRegex(MODULE.UnsafeTransition, "Other DB sessions"):
                self.t.run()
        with patch.object(MODULE.fcntl, "flock", side_effect=BlockingIOError):
            with self.assertRaisesRegex(MODULE.UnsafeTransition, "Another App DB"):
                self.t.run()

    def test_unknown_user_schema_stops_upgrade(self):
        self.t.databases["eva"]["columns"] = ["seq", "unknown_id"]
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "Unknown user schema"):
            self.t.run()

    def test_sql_identifier_and_backup_symlinks_rejected(self):
        with self.assertRaises(MODULE.UnsafeTransition):
            MODULE.identifier("eva';DROP DATABASE eva;--")
        link = Path(self.temp.name) / "link"
        link.symlink_to(Path(self.temp.name), target_is_directory=True)
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "symlink"):
            MODULE.private_dir(link / "child")

    def test_password_remains_in_container_and_diagnostics_hidden(self):
        t = MODULE.Transition("eva-app", "eva-app", [], self.root)
        t.pod, t.container = "db-pod", "db"
        with patch.object(t, "run_command", return_value=None) as command:
            t.mysql(["mysql", "--user=root"], stdout=subprocess.PIPE)
        args = command.call_args.args[0]
        self.assertIn('export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"', " ".join(args))
        self.assertNotIn("private-test-password", " ".join(args))
        with patch.object(MODULE.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, stderr=b"private-password-and-row")):
            with self.assertRaises(MODULE.UnsafeTransition) as raised:
                t.run_command(["kubectl"], stdout=subprocess.PIPE)
        self.assertNotIn("private-password-and-row", str(raised.exception))

    def test_task_order_and_exact_cli_render(self):
        tasks = yaml.safe_load((ROOT / "src/solution/roles/eva_app/tasks/main.yaml").read_text())
        names = [t["name"] for t in tasks]
        block = next(t for t in tasks if t["name"] == "Install EVA App and restore prior replicas if Helm fails")
        inner = [t["name"] for t in block["block"]]
        self.assertLess(names.index("Wait until EVA App pods are terminated before reinstall"), names.index(block["name"]))
        self.assertLess(inner.index("Preserve EVA App database identity and legacy local users before upgrade"), inner.index("Install EVA App from online Helm repo"))
        preparation = block["block"][1]
        self.assertEqual(preparation["when"], "not ansible_check_mode")
        render = yaml.safe_load((ROOT / "src/solution/roles/eva_app/tasks/prepare_database.yaml").read_text())[1]
        self.assertTrue(render["no_log"])
        self.assertIn("eva_cli_helm_set", render["ansible.builtin.shell"])
        self.assertIn("eva_app_effective_values_dest", render["ansible.builtin.shell"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
