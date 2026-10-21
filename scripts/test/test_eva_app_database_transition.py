"""Fast, offline tests of the production DB transition helper (no live cluster)."""

import base64
import copy
import csv
import importlib.util
import hashlib
import json
import os
from pathlib import Path
import shutil
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
READER_SPEC = importlib.util.spec_from_file_location("read_license_identity", HELPER.with_name("read_license_identity.py"))
READER = importlib.util.module_from_spec(READER_SPEC)
READER_SPEC.loader.exec_module(READER)


FEWSHOT_TRIGGER = ["trg_after_delete_fewshot", "DELETE", "fewshot", "AFTER", "ROW",
                   "INSERT INTO deleted_frame (frame_path, created_at)\n"
                   "        VALUES (OLD.frame_path, NOW())"]


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
        self.license = {"status": "absent", "product": "test-product"}
        self.identity_updates = []
        self.config_engine = "InnoDB"
        self.config_triggers = "0"
        self.config_references = "0"

    def cached_license(self, current_app, desired):
        return copy.deepcopy(self.license)

    def get(self, kind, name):
        return self.objects.get((kind, name))

    def run_command(self, args, **kwargs):
        self.calls.append(args)
        return subprocess.CompletedProcess(args, 0, stdout=json.dumps({"items": self.pods}).encode())

    def tables(self, db):
        return sorted(self.databases.get(db, {}).get("tables", {}))

    def sql(self, statement):
        self.calls.append(statement)
        if statement.startswith("START TRANSACTION; UPDATE"):
            db = statement.split("UPDATE `")[1].split("`")[0]
            new = statement.split("CONVERT(0x")[1].split(" ")[0]
            old = statement.split("HEX(value)='")[1].split("'")[0]
            if self.databases[db]["eva_id"].encode().hex().upper() != old:
                return "0"
            self.identity_updates.append((db, old, new))
            self.databases[db]["eva_id"] = bytes.fromhex(new).decode()
            return "1"
        if "information_schema.TRIGGERS WHERE EVENT_OBJECT_SCHEMA" in statement:
            return self.config_triggers
        if "information_schema.KEY_COLUMN_USAGE WHERE REFERENCED_TABLE_SCHEMA" in statement:
            return self.config_references
        if statement.startswith("SELECT ENGINE"):
            return self.config_engine
        if "information_schema.COLUMNS" in statement:
            db = statement.split("TABLE_SCHEMA='")[1].split("'")[0]
            return "\n".join(self.databases[db]["columns"])
        if statement.startswith("SELECT HEX(TRIGGER_NAME)"):
            db = statement.split("TRIGGER_SCHEMA='")[1].split("'")[0]
            return "\n".join("\t".join(value.encode().hex().upper() for value in trigger)
                             for trigger in self.databases[db].get("triggers", []))
        if "information_schema.TABLES" in statement:
            if "information_schema.TRIGGERS" in statement:
                db = statement.split("TABLE_SCHEMA='")[1].split("'")[0]
                return str(int(self.special_objects) + len(self.databases[db].get("triggers", [])))
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

    def test_populated_destination_retained_even_if_live_points_to_legacy(self):
        self.t.databases["eva_v3_0"] = copy.deepcopy(self.t.databases["eva"])
        self.t.databases["eva_v3_0"]["rows"] = [["42", "destination-data", "private-hash"]]
        before = copy.deepcopy(self.t.databases)
        result = self.t.run()
        self.assertFalse(result["copied"])
        self.assertTrue(result["existing_destination_retained"])
        self.assertEqual(result["source"], "eva_v3_0")
        self.assertEqual(before, self.t.databases)

    def test_populated_destination_different_legacy_id_no_longer_blocks(self):
        self.t = fixture(self.root, live="eva_v3_0")
        self.t.databases["eva_v3_0"] = copy.deepcopy(self.t.databases["eva"])
        self.t.databases["eva_v3_0"]["eva_id"] = "new-id"
        before = copy.deepcopy(self.t.databases)
        result = self.t.run()
        self.assertFalse(result["copied"])
        self.assertEqual(result["license_identity"], "no-local-license")
        self.assertEqual(self.t.databases, before)

    def license_evidence(self, identity="old-id"):
        cache = b"encrypted-private-test-cache"
        return {"status": "present", "product": "test-product", "eva_id_hex": identity.encode().hex().upper(),
                "cache_sha256": hashlib.sha256(cache).hexdigest(),
                "encrypted_cache_base64": base64.b64encode(cache).decode()}

    def populated_target(self):
        self.t = fixture(self.root, live="eva_v3_0")
        self.t.databases["eva_v3_0"] = copy.deepcopy(self.t.databases["eva"])
        self.t.databases["eva_v3_0"].update(eva_id="new-id", rows=[["42", "keep-target-user", "private-hash"]])
        self.t.databases["eva_v3_0"]["tables"]["camera"] = "7"

    def test_only_target_identity_changes_and_all_other_data_remains(self):
        self.populated_target()
        self.t.license = self.license_evidence()
        before = copy.deepcopy(self.t.databases)
        result = self.t.run()
        before["eva_v3_0"]["eva_id"] = "old-id"
        self.assertEqual(before, self.t.databases)
        self.assertFalse(result["copied"])
        self.assertEqual(result["license_identity"], "corrected")
        self.assertTrue(result["identity_updated"])
        self.assertEqual(len(self.t.identity_updates), 1)
        self.assertNotIn("old-id", json.dumps(result))
        directory = Path(result["backup_directory"])
        backup = json.loads((directory / "eva_v3_0-before-identity.sql").read_text())
        self.assertEqual(backup["eva_id"], "new-id")
        self.assertTrue((directory / "identity-complete.json").is_file())
        self.assertEqual((directory / "license-cache-before-identity.key").read_bytes(), b"encrypted-private-test-cache")

    def test_license_id_not_guessed_from_legacy_db(self):
        self.populated_target()
        self.t.license = self.license_evidence("license-is-authoritative")
        self.t.run()
        self.assertEqual(self.t.databases["eva"]["eva_id"], "old-id")
        self.assertEqual(self.t.databases["eva_v3_0"]["eva_id"], "license-is-authoritative")

    def test_matching_license_and_existing_target_need_no_identity_write(self):
        self.populated_target()
        self.t.license = self.license_evidence("new-id")
        self.t.databases["eva_v3_0"]["columns"] = ["seq", "keycloak_user_id"]
        result = self.t.run()
        self.assertEqual(result["license_identity"], "matched")
        self.assertFalse(result["changed"])
        self.assertEqual(self.t.identity_updates, [])

    def test_bad_cache_or_failed_backup_prevents_identity_write(self):
        self.populated_target()
        self.t.license = self.license_evidence()
        self.t.dump_fails = True
        with self.assertRaises(MODULE.UnsafeTransition):
            self.t.run()
        self.assertEqual(self.t.databases["eva_v3_0"]["eva_id"], "new-id")
        self.assertEqual(self.t.identity_updates, [])

    def test_config_triggers_and_nontransactional_engine_block_id_correction(self):
        for engine, triggers in (("MyISAM", "0"), ("InnoDB", "1")):
            self.populated_target()
            self.t.license = self.license_evidence()
            self.t.config_engine, self.t.config_triggers = engine, triggers
            with self.assertRaises(MODULE.UnsafeTransition):
                self.t.run()
            self.assertEqual(self.t.identity_updates, [])

    def test_invalid_license_evidence_does_not_change_identity(self):
        for key, value in (("eva_id_hex", "not-hex"), ("cache_sha256", "0" * 64)):
            self.populated_target()
            self.t.license = self.license_evidence()
            self.t.license[key] = value
            with self.assertRaises(MODULE.UnsafeTransition):
                self.t.run()
            self.assertEqual(self.t.identity_updates, [])

    def test_config_value_foreign_key_cascade_cannot_change_other_data(self):
        self.populated_target()
        self.t.license = self.license_evidence()
        self.t.config_references = "1"
        before = copy.deepcopy(self.t.databases)
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "references"):
            self.t.run()
        self.assertEqual(self.t.databases, before)
        self.assertEqual(self.t.identity_updates, [])

    def test_zero_row_compare_and_set_never_records_completion(self):
        self.populated_target()
        self.t.license = self.license_evidence()
        original_sql = self.t.sql

        def changed_concurrently(statement):
            return "0" if statement.startswith("START TRANSACTION; UPDATE") else original_sql(statement)

        with patch.object(self.t, "sql", side_effect=changed_concurrently):
            with self.assertRaisesRegex(MODULE.UnsafeTransition, "concurrently"):
                self.t.run()
        self.assertEqual(self.t.databases["eva_v3_0"]["eva_id"], "new-id")
        self.assertTrue(list(self.root.glob("*/identity-before.json")))
        self.assertEqual(list(self.root.glob("*/identity-complete.json")), [])

    def test_license_read_failure_stops_before_any_db_write(self):
        self.populated_target()
        with patch.object(self.t, "cached_license", side_effect=MODULE.UnsafeTransition("Unreadable local license")):
            with self.assertRaises(MODULE.UnsafeTransition):
                self.t.run()
        self.assertEqual(self.t.identity_updates, [])
        self.assertEqual(list(self.root.glob("*/identity-complete.json")), [])

    def test_reviewed_original_db_can_be_retained_without_overwriting_either_db(self):
        self.t = fixture(self.root, live="eva_v3_0", target="eva")
        self.t.databases["eva_v3_0"] = copy.deepcopy(self.t.databases["eva"])
        self.t.databases["eva_v3_0"]["eva_id"] = "unrelated-new-id"
        self.t.explicit_source = "eva"
        before = copy.deepcopy(self.t.databases)
        result = self.t.run()
        self.assertFalse(result["copied"])
        self.assertEqual(result["source"], "eva")
        self.assertEqual(result["destination"], "eva")
        self.assertEqual(self.t.databases, before)
        self.assertEqual(len(result["legacy_user_csv"]), 1)
        self.assertTrue((Path(result["backup_directory"]) / "eva.sql").is_file())

    def test_explicit_original_source_does_not_authorize_populated_target_overwrite(self):
        self.t = fixture(self.root, live="eva_v3_0")
        self.t.databases["eva_v3_0"] = copy.deepcopy(self.t.databases["eva"])
        self.t.databases["eva_v3_0"]["eva_id"] = "unrelated-new-id"
        self.t.explicit_source = "eva"
        before = copy.deepcopy(self.t.databases)
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "populated"):
            self.t.run()
        self.assertEqual(self.t.databases, before)

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

    def test_identifiable_but_unverified_restore_cannot_be_accepted_on_retry(self):
        self.t.corrupt_restore = True
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "verification failed"):
            self.t.run()
        self.t.corrupt_restore = False
        before = copy.deepcopy(self.t.databases)
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "incomplete"):
            self.t.run()
        self.assertEqual(self.t.databases, before)

    def test_verified_copy_receipt_allows_retry_with_existing_destination(self):
        first = self.t.run()
        self.assertTrue(first["copied"])
        before = copy.deepcopy(self.t.databases)
        second = self.t.run()
        self.assertFalse(second["copied"])
        self.assertNotEqual(first["backup_directory"], second["backup_directory"])
        self.assertEqual(self.t.databases, before)

    def test_schema_objects_require_manual_copy(self):
        self.t.special_objects = "1"
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "stored objects"):
            self.t.run()

    def fewshot_trigger(self, live="eva"):
        self.t = fixture(self.root, live=live)
        data = self.t.databases["eva"]
        data["tables"].update(fewshot="2", deleted_frame="0")
        data["triggers"] = [copy.deepcopy(FEWSHOT_TRIGGER)]
        return data

    def test_missing_target_copies_legacy_fewshot_trigger_without_changing_source(self):
        original = copy.deepcopy(self.fewshot_trigger(live="eva_v3_0"))
        result = self.t.run()
        self.assertTrue(result["copied"])
        self.assertEqual((result["source"], result["destination"]), ("eva", "eva_v3_0"))
        self.assertEqual(self.t.databases["eva"], original)
        self.assertEqual(self.t.databases["eva_v3_0"], original)
        self.assertTrue((Path(result["backup_directory"]) / "copy-complete.json").is_file())

    def test_fewshot_trigger_allows_only_harmless_sql_formatting(self):
        data = self.fewshot_trigger()
        data["triggers"][0][-1] = (" insert into `deleted_frame` (`frame_path`, `created_at`)\n"
                                   " values (old . `frame_path`, now ( )) ; ")
        self.assertTrue(self.t.run()["copied"])

    def test_unknown_or_modified_triggers_still_block_before_copy(self):
        variants = [
            (0, "other_trigger"), (1, "INSERT"), (2, "config"), (3, "BEFORE"), (4, "STATEMENT"),
            (5, "INSERT INTO eva.deleted_frame (frame_path, created_at) VALUES (OLD.frame_path, NOW())"),
            (5, "INSERT INTO `eva`.`deleted_frame` (frame_path, created_at) VALUES (OLD.frame_path, NOW())"),
            (5, "INSERT INTO eva_v3_0.deleted_frame (frame_path, created_at) VALUES (OLD.frame_path, NOW())"),
            (5, "INSERT INTO Deleted_Frame (frame_path, created_at) VALUES (OLD.frame_path, NOW())"),
            (5, "INSERT INTO deleted_frame (frame_path, created_at) VALUES (NEW.frame_path, NOW())"),
            (5, FEWSHOT_TRIGGER[-1] + "; DELETE FROM config"),
            (5, FEWSHOT_TRIGGER[-1] + " /* modified */"),
        ]
        for field, value in variants:
            with self.subTest(field=field, value=value):
                data = self.fewshot_trigger()
                data["triggers"][0][field] = value
                before = copy.deepcopy(self.t.databases)
                with self.assertRaisesRegex(MODULE.UnsafeTransition, "trigger"):
                    self.t.run()
                self.assertEqual(self.t.databases, before)
                self.assertFalse(list(self.root.glob("*/source.json")))

    def test_known_trigger_does_not_allow_an_additional_trigger_or_missing_table(self):
        for missing_table in (False, True):
            with self.subTest(missing_table=missing_table):
                data = self.fewshot_trigger()
                if missing_table:
                    del data["tables"]["deleted_frame"]
                else:
                    other = copy.deepcopy(FEWSHOT_TRIGGER)
                    other[0] = "other_trigger"
                    data["triggers"].append(other)
                before = copy.deepcopy(self.t.databases)
                with self.assertRaisesRegex(MODULE.UnsafeTransition, "trigger"):
                    self.t.run()
                self.assertEqual(self.t.databases, before)

    def test_malformed_trigger_metadata_fails_closed_without_disclosing_body(self):
        encoded = [value.encode().hex().upper() for value in FEWSHOT_TRIGGER]
        invalid = ["private-trigger-body", "\t".join(encoded[:-1]),
                   "\t".join(encoded[:-1] + ["FF"]), "\t".join(encoded[:-1] + ["0"])]
        for raw in invalid:
            with self.subTest(raw=raw):
                self.fewshot_trigger()
                original_sql = self.t.sql

                def malformed(statement):
                    return raw if statement.startswith("SELECT HEX(TRIGGER_NAME)") else original_sql(statement)

                with patch.object(self.t, "sql", side_effect=malformed):
                    with self.assertRaisesRegex(MODULE.UnsafeTransition, "unsupported trigger") as failure:
                        self.t.run()
                self.assertNotIn("private-trigger-body", str(failure.exception))
                self.assertNotIn("eva_v3_0", self.t.databases)

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
        render = next(task for task in yaml.safe_load((ROOT / "src/solution/roles/eva_app/tasks/prepare_database.yaml").read_text())
                  if task["name"] == "Render exact EVA App target manifest for DB preparation")
        self.assertTrue(render["no_log"])
        self.assertIn("eva_cli_helm_set", render["ansible.builtin.shell"])
        self.assertIn("eva_app_effective_values_dest", render["ansible.builtin.shell"])


class DatabaseTaskFailureTests(unittest.TestCase):
    """Execute the real registered task and rescue, without Helm or a cluster."""

    def test_failed_helper_preserves_original_error_and_restores_replicas(self):
        tasks = yaml.safe_load((ROOT / "src/solution/roles/eva_app/tasks/main.yaml").read_text())
        block = copy.deepcopy(next(task for task in tasks if task["name"] ==
                                   "Install EVA App and restore prior replicas if Helm fails"))
        task = next(task for task in block["block"] if task["name"] ==
                    "Preserve EVA App database identity and legacy local users before upgrade")
        with tempfile.TemporaryDirectory(prefix="eva-db-failure-test-") as temporary:
            root = Path(temporary)
            binary = root / "bin"
            binary.mkdir()
            helper = root / "fail-helper.py"
            message = "Legacy eva and destination have different eva_id values; refuse automatic overwrite."
            helper.write_text("import sys\nprint(" + repr(message)
                              + ", file=sys.stderr)\nsys.exit(1)\n")
            log = root / "kubectl-calls.log"
            kubectl = binary / "kubectl"
            kubectl.write_text("#!" + sys.executable + "\n"
                               "import sys\nfrom pathlib import Path\n"
                               "args = sys.argv[1:]\n"
                               "with Path(" + repr(str(log)) + ").open('a') as stream:\n"
                               "    stream.write(' '.join(args) + '\\n')\n"
                               "if args[:2] == ['get', 'deployment']:\n"
                               "    print('original-deployment-uid', end='')\n"
                               "elif args[:2] == ['scale', 'deployment']:\n"
                               "    print('scaled')\n"
                               "elif args[:2] == ['rollout', 'status']:\n"
                               "    print('successfully rolled out')\n"
                               "else:\n    sys.exit(99)\n")
            kubectl.chmod(0o700)
            task["ansible.builtin.command"]["argv"] = [sys.executable, str(helper)]
            block["block"] = [task, {"name": "Install sentinel", "ansible.builtin.debug":
                                      {"msg": "MUST-NOT-RUN-INSTALL"}}]
            playbook = root / "play.yaml"
            playbook.write_text(yaml.safe_dump([{
                "hosts": "localhost", "connection": "local", "gather_facts": False,
                "environment": {"PATH": str(binary) + os.pathsep + os.environ["PATH"]},
                "vars": {"ansible_python_interpreter": sys.executable,
                         "eva_app_name": "eva-app", "eva_app_namespace": "eva-app",
                         "eva_app_kubeconfig": str(root / "unused-kubeconfig"),
                         "eva_app_deployment_exists": {"rc": 0},
                         "eva_app_preinstall_deployment_uid": "original-deployment-uid",
                         "eva_app_preinstall_replicas": 2},
                "tasks": [block],
            }], sort_keys=False))
            ansible = shutil.which("ansible-playbook") or str(Path(sys.executable).parent / "ansible-playbook")
            result = subprocess.run([ansible, "-i", "localhost,", str(playbook)], text=True,
                                    stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=60)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(message, result.stdout)
            self.assertNotIn("from_json", result.stdout)
            self.assertNotIn("Expecting value", result.stdout)
            self.assertNotIn("MUST-NOT-RUN-INSTALL", result.stdout)
            self.assertIn('"changed": false', result.stdout)
            self.assertIn("replica restoration rc=0, rollout rc=0", result.stdout)
            calls = log.read_text().splitlines()
            self.assertEqual(len(calls), 3)
            self.assertIn("--replicas=2", calls[1])


class LicenseReaderTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.config = self.root / "config.py"
        # Test-only random encryption material, never production defaults.
        self.key, self.iv = os.urandom(32).hex(), os.urandom(16).hex()
        self.config.write_text("raise RuntimeError('product module must not be imported')\n"
                               "class Settings:\n    LICENSE_FILE_KEY: str = " + repr(self.key)
                               + "\n    LICENSE_FILE_IV: str = " + repr(self.iv) + "\n")
        self.cache = self.root / "test-product" / "License.key"
        self.cache.parent.mkdir()

    def cache_data(self, **overrides):
        from Crypto.Cipher import AES
        from Crypto.Util.Padding import pad
        data = {"product": "test-product", "hardware_id": "eva_id:fixture-license-id",
                "license_key": "private-license-key-never-log", **overrides}
        raw = base64.b64encode(AES.new(bytes.fromhex(self.key), AES.MODE_CBC,
                                      bytes.fromhex(self.iv)).encrypt(pad(json.dumps(data).encode(), 16)))
        self.cache.write_bytes(raw)
        return raw

    def read(self, env=None):
        return READER.read_identity(self.root, "test-product", self.config, env or {})

    def test_actual_aes_cache_read_without_app_import_or_cache_modification(self):
        raw = self.cache_data()
        before = self.cache.stat().st_mtime_ns
        result = self.read()
        self.assertEqual(result["eva_id_hex"], b"fixture-license-id".hex().upper())
        self.assertEqual(result["cache_sha256"], hashlib.sha256(raw).hexdigest())
        self.assertEqual(self.cache.read_bytes(), raw)
        self.assertEqual(self.cache.stat().st_mtime_ns, before)
        self.assertNotIn("private-license-key-never-log", json.dumps(result))

    def test_absent_cache_needs_no_app_import_or_decryption(self):
        self.config.unlink()
        self.assertEqual(self.read()["status"], "absent")

    def test_bad_crypto_and_wrong_product_or_non_eva_identity_rejected(self):
        for data in ({"product": "wrong-product"}, {"hardware_id": "not-an-eva-id"},
                     {"hardware_id": "eva_id:';DROP DATABASE eva;"}):
            self.cache_data(**data)
            with self.assertRaises(ValueError):
                self.read()
        self.cache_data()
        with self.assertRaises(ValueError):
            self.read({"LICENSE_FILE_KEY": os.urandom(32).hex()})
        self.cache.write_text("corrupt-not-base64")
        with self.assertRaises(ValueError):
            self.read()

    def test_license_file_symlink_is_refused(self):
        other = self.root / "untrusted"
        other.write_text("not-a-license")
        self.cache.symlink_to(other)
        with self.assertRaises(ValueError):
            self.read()

    def test_env_crypto_override_used_without_executing_config(self):
        self.cache_data()
        self.config.write_text("raise RuntimeError('must not execute')\n")
        result = self.read({"LICENSE_FILE_KEY": self.key, "LICENSE_FILE_IV": self.iv})
        self.assertEqual(result["status"], "present")


class LicenseProbeTests(unittest.TestCase):
    def test_probe_failure_still_attempts_cleanup_of_both_resources(self):
        for failure in ("exec", "wait", "pod-delete"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as directory:
                t = fixture(Path(directory) / "backups")
                app = t.objects[("deployment", "eva-app")]
                app["spec"]["template"]["spec"]["containers"][0]["image"] = "app-image:test"
                t.objects[("pod", "db-pod")] = {"spec": {"nodeName": "node-1"}}
                t.pod = "db-pod"
                environment = {"LICENSE_PRODUCT_CODE": "test-product"}
                deleted = []

                def command(args, **kwargs):
                    payload = {}
                    if "nodes" in args:
                        payload = {"items": [{"metadata": {"name": "node-1"}}]}
                    elif "delete" in args:
                        kind = args[args.index("delete") + 1]
                        deleted.append(kind)
                        if failure == "pod-delete" and kind == "pod":
                            raise MODULE.UnsafeTransition("Injected delete failure")
                    elif failure in args:
                        raise MODULE.UnsafeTransition("Injected " + failure + " failure")
                    elif "exec" in args:
                        payload = {"status": "absent", "product": "test-product"}
                    return subprocess.CompletedProcess(args, 0, stdout=json.dumps(payload).encode())

                with patch.object(t, "env", return_value=environment), patch.object(t, "run_command", side_effect=command):
                    with self.assertRaises(MODULE.UnsafeTransition):
                        MODULE.Transition.cached_license(t, app, environment)
                self.assertEqual(deleted, ["pod", "networkpolicy"])

    def test_multinode_hostpath_is_refused_before_creating_any_probe(self):
        with tempfile.TemporaryDirectory() as directory:
            t = fixture(Path(directory) / "backups")
            app = t.objects[("deployment", "eva-app")]
            t.objects[("pod", "db-pod")] = {"spec": {"nodeName": "node-1"}}
            t.pod = "db-pod"
            environment = {"LICENSE_PRODUCT_CODE": "test-product"}
            nodes = {"items": [{"metadata": {"name": node}} for node in ("node-1", "node-2")]}
            with patch.object(t, "env", return_value=environment), patch.object(t, "run_command",
                    return_value=subprocess.CompletedProcess([], 0, stdout=json.dumps(nodes).encode())) as command:
                with self.assertRaisesRegex(MODULE.UnsafeTransition, "single verified node"):
                    MODULE.Transition.cached_license(t, app, environment)
            self.assertEqual(command.call_count, 1)
            self.assertNotIn("create", command.call_args.args[0])

    def test_production_probe_is_readonly_isolated_and_always_cleaned_up(self):
        with tempfile.TemporaryDirectory() as directory:
            t = fixture(Path(directory) / "backups")
            app = t.objects[("deployment", "eva-app")]
            app["spec"]["template"]["spec"]["containers"][0]["image"] = "app-image:test"
            t.objects[("pod", "db-pod")] = {"spec": {"nodeName": "node-1"}}
            t.pod = "db-pod"
            current_env = t.env(t.app_container(app))
            current_env["LICENSE_PRODUCT_CODE"] = "test-product"
            created, deleted = [], []

            def command(args, **kwargs):
                if "nodes" in args:
                    payload = {"items": [{"metadata": {"name": "node-1"}}]}
                elif "create" in args:
                    created.append(json.loads(kwargs["input"]))
                    payload = {}
                elif "exec" in args:
                    self.assertIn("python", args)
                    self.assertIn(b"def read_identity", kwargs["input"])
                    payload = {"status": "absent", "product": "test-product"}
                elif "delete" in args:
                    deleted.append(args)
                    payload = {}
                else:
                    payload = {}
                return subprocess.CompletedProcess(args, 0, stdout=json.dumps(payload).encode())

            with patch.object(t, "env", return_value=current_env), patch.object(t, "run_command", side_effect=command):
                result = MODULE.Transition.cached_license(t, app, current_env)
            self.assertEqual(result["status"], "absent")
            self.assertEqual([resource["kind"] for resource in created], ["NetworkPolicy", "Pod"])
            policy, pod = created
            self.assertEqual(policy["spec"]["egress"], [])
            self.assertFalse(pod["spec"]["automountServiceAccountToken"])
            container = pod["spec"]["containers"][0]
            self.assertTrue(container["volumeMounts"][0]["readOnly"])
            self.assertEqual(container["volumeMounts"][0]["mountPath"], "/eva-license")
            self.assertTrue(container["securityContext"]["readOnlyRootFilesystem"])
            self.assertNotIn("MYSQL_PSWD", json.dumps(pod))
            self.assertNotIn("LICENSE_API_KEY", json.dumps(pod))
            self.assertEqual(len(deleted), 2)


if __name__ == "__main__":
    unittest.main(verbosity=2)
