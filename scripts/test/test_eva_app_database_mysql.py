"""Opt-in real MySQL test. Creates/removes ONLY its own isolated container.

Set EVA_TEST_MYSQL_IMAGE to an already-cached MySQL 8 image. No pulls, published
ports, host mounts or live cluster access. Uses the production SQL/dump/CSV code.
"""

import base64
import csv
import hashlib
import json
import os
from pathlib import Path
import select
import subprocess
import tempfile
import time
import unittest
import uuid
from unittest.mock import patch

from test_eva_app_database_transition import FEWSHOT_TRIGGER, MODULE, fixture


@unittest.skipUnless(os.environ.get("EVA_TEST_MYSQL_IMAGE"), "set EVA_TEST_MYSQL_IMAGE to a locally cached MySQL 8 image")
class RealMySQLTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.container = "eva-db-transition-test-" + uuid.uuid4().hex[:12]
        cls.docker(["run", "--detach", "--pull=never", "--name", cls.container,
                    "--network=none", "--tmpfs", "/var/lib/mysql:rw,size=512m",
                    "--env", "MYSQL_ROOT_PASSWORD=private-test-password",
                    os.environ["EVA_TEST_MYSQL_IMAGE"]])
        cls.addClassCleanup(lambda: cls.docker(["rm", "--force", cls.container]))
        for _ in range(90):
            result = cls.docker(["exec", cls.container, "sh", "-c",
                                'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot -N -e "SELECT @@global.skip_networking"'], check=False)
            if result.returncode == 0 and result.stdout.strip() == b"0":
                break
            time.sleep(1)
        else:
            raise AssertionError("Isolated MySQL did not become ready")

    @classmethod
    def docker(cls, args, **kwargs):
        return subprocess.run(["docker", *args], capture_output=True, check=kwargs.pop("check", True), timeout=120, **kwargs)

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        old_umask = os.umask(0o077)
        self.addCleanup(lambda: os.umask(old_umask))
        setup = fixture(Path(self.temp.name) / "backups")
        self.t = LiveTransition(setup.namespace, setup.release, setup.resources, setup.backup_root)
        self.t.objects, self.t.pods = setup.objects, setup.pods
        self.t.test_container = self.container
        self.t.pod, self.t.container = "db-pod", "eva-app-mysql"
        # Destructive SQL is limited to this test's own ephemeral container.
        self.t.sql("DROP DATABASE IF EXISTS eva; DROP DATABASE IF EXISTS eva_v3_0; CREATE DATABASE eva CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;")
        self.t.sql("""CREATE TABLE eva.config (`key` VARCHAR(32) PRIMARY KEY, value VARCHAR(256)) ENGINE=InnoDB;
        INSERT INTO eva.config VALUES ('eva_id','preserved-test-id'),('installed','true');
        CREATE TABLE eva.alembic_version (version_num VARCHAR(32) PRIMARY KEY) ENGINE=InnoDB;
        INSERT INTO eva.alembic_version VALUES ('pre32');
        CREATE TABLE eva.user (seq INT PRIMARY KEY,user_name TEXT,user_password VARCHAR(60),notes TEXT) ENGINE=InnoDB;
        INSERT INTO eva.user VALUES (1,CONVERT(0xED998DEAB8B8EB8F992C2022EAB480EBA6AC22 USING utf8mb4),'$2b$test',CONCAT('line1',CHAR(10),'line2',CHAR(9),'tab')),(2,'',NULL,NULL);
        CREATE TABLE eva.payload (seq INT PRIMARY KEY,content LONGBLOB) ENGINE=InnoDB;
        INSERT INTO eva.payload VALUES (1,0x000AFF5C2722);
        """)

    def test_copy_csv_identity_data_and_repeat_install(self):
        result = self.t.run()
        self.assertTrue(result["copied"])
        self.assertEqual(self.t.eva_id("eva"), self.t.eva_id("eva_v3_0"))
        self.assertEqual(self.t.sql("SELECT HEX(content) FROM eva_v3_0.payload;"), "000AFF5C2722")
        with Path(result["legacy_user_csv"][0]).open(newline="") as stream:
            rows = list(csv.reader(stream))
        self.assertEqual(rows[1][1], '홍길동, "관리"')
        self.assertEqual(rows[1][3], "line1\nline2\ttab")
        self.assertEqual(rows[2][2:], ["\\N", "\\N"])
        # Simulate Helm switching only its connection name. Retained source does
        # not cause a second copy over the now-active destination.
        secret = self.t.objects[("secret", "app-secret")]
        secret["data"]["MYSQL_NAME"] = self.t.resources[2]["data"]["MYSQL_NAME"]
        again = LiveTransition(self.t.namespace, self.t.release, self.t.resources, self.t.backup_root)
        again.objects, again.pods = self.t.objects, self.t.pods
        again.test_container = self.container
        self.assertFalse(again.run()["copied"])

    def mysql_session(self, database="eva"):
        # Only this test's isolated container; keep a real client connected like
        # DBeaver. No host ports, installed Python DB driver or live credentials.
        args = ["docker", "exec", "-i", self.container, "sh", "-c",
                'export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec "$@"', "sh",
                "mysql", "--user=root", "--batch", "--raw", "--skip-column-names", "--unbuffered"]
        if database:
            args.append(database)
        client = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)

        def close():
            try:
                client.communicate(input=b"ROLLBACK; UNLOCK TABLES;\n", timeout=10)
            except subprocess.TimeoutExpired:
                client.kill()
                client.communicate()

        self.addCleanup(close)
        return client

    def client_sql(self, client, statement):
        marker = ("complete_" + uuid.uuid4().hex).encode()
        client.stdin.write(statement.encode() + b"\nSELECT '" + marker + b"';\n")
        client.stdin.flush()
        data = b""
        deadline = time.monotonic() + 15
        while not data.endswith(marker + b"\n"):
            remaining = deadline - time.monotonic()
            if remaining <= 0 or not select.select([client.stdout], [], [], remaining)[0]:
                self.fail("Isolated persistent MySQL client timed out")
            chunk = os.read(client.stdout.fileno(), 65536)
            if not chunk:
                self.fail("Isolated persistent MySQL client exited unexpectedly")
            data += chunk
        return data[:-len(marker) - 1].decode().rstrip("\n")

    def test_idle_database_viewer_allows_copy_and_stays_connected(self):
        client = self.mysql_session()
        connection = self.client_sql(client, "SELECT CONNECTION_ID();")
        self.client_sql(client, "SELECT COUNT(*) FROM eva.config;")
        self.assertEqual(self.t.sql("SELECT COMMAND FROM information_schema.PROCESSLIST WHERE ID=" + connection + ";"), "Sleep")
        self.assertEqual(self.t.sql("SELECT COUNT(*) FROM information_schema.INNODB_TRX WHERE trx_mysql_thread_id=" + connection + ";"), "0")
        self.assertTrue(self.t.run()["copied"])
        self.assertEqual(self.client_sql(client, "SELECT CONNECTION_ID();"), connection)
        self.assertEqual(self.t.eva_id("eva"), self.t.eva_id("eva_v3_0"))

    def test_sleeping_viewer_transaction_still_blocks_copy(self):
        client = self.mysql_session()
        connection = self.client_sql(client, "SELECT CONNECTION_ID();")
        self.client_sql(client, "START TRANSACTION; SELECT COUNT(*) FROM eva.config;")
        self.assertEqual(self.t.sql("SELECT COMMAND FROM information_schema.PROCESSLIST WHERE ID=" + connection + ";"), "Sleep")
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "transactions/locks"):
            self.t.run()
        self.assertEqual(self.t.tables("eva_v3_0"), [])
        self.client_sql(client, "ROLLBACK;")
        self.assertTrue(self.t.run()["copied"])
        self.assertEqual(self.client_sql(client, "SELECT CONNECTION_ID();"), connection)

    def test_sleeping_viewer_table_lock_still_blocks_copy(self):
        client = self.mysql_session()
        self.client_sql(client, "LOCK TABLES eva.config READ;")
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "transactions/locks"):
            self.t.run()
        self.assertEqual(self.t.tables("eva_v3_0"), [])
        self.client_sql(client, "UNLOCK TABLES;")
        self.assertTrue(self.t.run()["copied"])

    def test_idle_target_viewer_allows_identity_alignment(self):
        self.conflicting_databases()
        client = self.mysql_session("eva_v3_0")
        connection = self.client_sql(client, "SELECT CONNECTION_ID();")
        cache = b"encrypted-private-fixture"
        self.t.license_evidence = {"status": "present", "product": "test-product",
                                   "eva_id_hex": b"preserved-test-id".hex().upper(),
                                   "cache_sha256": hashlib.sha256(cache).hexdigest(),
                                   "encrypted_cache_base64": base64.b64encode(cache).decode()}
        self.assertEqual(self.t.run()["license_identity"], "corrected")
        self.assertEqual(self.client_sql(client, "SELECT CONNECTION_ID();"), connection)

    def test_schema_qualified_open_transaction_is_not_hidden_by_default_database(self):
        for database in ("", "mysql"):
            with self.subTest(database=database):
                client = self.mysql_session(database)
                self.client_sql(client, "START TRANSACTION; SELECT COUNT(*) FROM eva.config;")
                with self.assertRaisesRegex(MODULE.UnsafeTransition, "transactions/locks"):
                    self.t.require_quiescent_databases("eva", "eva_v3_0")
                self.client_sql(client, "ROLLBACK;")
                self.t.require_quiescent_databases("eva", "eva_v3_0")

    def test_active_query_is_blocked_and_short_refresh_can_finish(self):
        # A controllable waiting SELECT instead of a timing-sensitive SLEEP.
        holder = self.mysql_session()
        viewer = self.mysql_session()
        name = "viewer_refresh_" + uuid.uuid4().hex
        self.assertEqual(self.client_sql(holder, "SELECT GET_LOCK('" + name + "',0);"), "1")
        connection = self.client_sql(viewer, "SELECT CONNECTION_ID();")
        viewer.stdin.write(("SELECT GET_LOCK('" + name + "',30);\n").encode())
        viewer.stdin.flush()
        for _ in range(30):
            if self.t.sql("SELECT COMMAND FROM information_schema.PROCESSLIST WHERE ID=" + connection + ";") == "Query":
                break
            time.sleep(0.1)
        else:
            self.fail("Isolated viewer query did not start")
        try:
            with patch.object(MODULE.time, "sleep"):
                with self.assertRaisesRegex(MODULE.UnsafeTransition, "active=1"):
                    self.t.require_quiescent_databases("eva")
            # Finish the metadata-like query during the guard's bounded retry.
            with patch.object(MODULE.time, "sleep", side_effect=lambda _: self.client_sql(holder, "SELECT RELEASE_LOCK('" + name + "');")):
                self.t.require_quiescent_databases("eva")
        finally:
            self.client_sql(holder, "SELECT RELEASE_LOCK('" + name + "');")
        self.client_sql(viewer, "SELECT RELEASE_LOCK('" + name + "');")

    def test_no_name_change_still_archives_pre32_users(self):
        self.t.resources[2]["data"]["MYSQL_NAME"] = self.t.objects[("secret", "app-secret")]["data"]["MYSQL_NAME"]
        result = self.t.run()
        self.assertFalse(result["copied"])
        self.assertEqual(len(result["legacy_user_csv"]), 1)
        self.assertTrue((Path(result["backup_directory"]) / "eva.sql").exists())

    def create_fewshot_tables(self):
        self.t.sql("""CREATE TABLE eva.fewshot (seq INT PRIMARY KEY,frame_path VARCHAR(256)) ENGINE=InnoDB;
        CREATE TABLE eva.deleted_frame (seq INT AUTO_INCREMENT PRIMARY KEY,frame_path VARCHAR(256),created_at DATETIME) ENGINE=InnoDB;
        INSERT INTO eva.fewshot VALUES (1,'fixture-frame'),(2,'keep-frame');""")

    def test_missing_target_preserves_fewshot_trigger_and_routes_deletes_to_target_only(self):
        self.create_fewshot_tables()
        self.t.sql("CREATE TRIGGER eva.trg_after_delete_fewshot AFTER DELETE ON eva.fewshot FOR EACH ROW "
                   + FEWSHOT_TRIGGER[-1] + ";")
        # Existing App already points at the canonical DB that was deleted.
        secret = self.t.objects[("secret", "app-secret")]
        secret["data"]["MYSQL_NAME"] = self.t.resources[2]["data"]["MYSQL_NAME"]
        original = self.t.snapshot("eva")
        result = self.t.run()
        self.assertTrue(result["copied"])
        self.assertEqual((result["source"], result["destination"]), ("eva", "eva_v3_0"))
        self.assertEqual(self.t.snapshot("eva"), original)
        self.assertEqual(self.t.snapshot("eva_v3_0"), original)
        self.assertEqual(self.t.sql("SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA='eva_v3_0';"), "1")
        self.t.sql("DELETE FROM eva_v3_0.fewshot WHERE seq=1;")
        self.assertEqual(self.t.sql("SELECT frame_path FROM eva_v3_0.deleted_frame;"), "fixture-frame")
        self.assertEqual(self.t.sql("SELECT COUNT(*) FROM eva.deleted_frame;"), "0")
        self.assertEqual(self.t.snapshot("eva"), original)
        # Reinstall keeps the destination and its trigger instead of copying again.
        self.assertFalse(self.t.run()["copied"])

    def test_unknown_and_schema_qualified_fewshot_triggers_remain_blocked(self):
        self.create_fewshot_tables()
        variants = [
            ("other_trigger", FEWSHOT_TRIGGER[-1]),
            ("trg_after_delete_fewshot", "INSERT INTO eva.deleted_frame (frame_path,created_at) VALUES (OLD.frame_path,NOW())"),
            ("trg_after_delete_fewshot", "INSERT INTO deleted_frame (frame_path,created_at) VALUES ('not-the-old-frame',NOW())"),
        ]
        for name, body in variants:
            with self.subTest(name=name, body=body):
                self.t.sql("CREATE TRIGGER eva." + name + " AFTER DELETE ON eva.fewshot FOR EACH ROW " + body + ";")
                try:
                    before = self.t.snapshot("eva")
                    with self.assertRaisesRegex(MODULE.UnsafeTransition, "trigger"):
                        self.t.run()
                    self.assertEqual(self.t.snapshot("eva"), before)
                    self.assertEqual(self.t.tables("eva_v3_0"), [])
                    self.assertFalse(list(self.t.backup_root.glob("*/source.json")))
                finally:
                    self.t.sql("DROP TRIGGER eva." + name + ";")

    def test_known_trigger_does_not_bypass_other_unsafe_schema_checks(self):
        self.create_fewshot_tables()
        self.t.sql("CREATE TRIGGER eva.trg_after_delete_fewshot AFTER DELETE ON eva.fewshot FOR EACH ROW "
                   + FEWSHOT_TRIGGER[-1] + ";")
        # Everything here exists ONLY in this test's own ephemeral container.
        self.t.sql("CREATE DATABASE isolated_reference; CREATE TABLE isolated_reference.parent (id INT PRIMARY KEY) ENGINE=InnoDB;")
        variants = [
            ("CREATE TABLE eva.unsafe_table (id INT) ENGINE=MyISAM;", "DROP TABLE eva.unsafe_table;"),
            ("CREATE VIEW eva.unsafe_view AS SELECT seq FROM eva.fewshot;", "DROP VIEW eva.unsafe_view;"),
            ("CREATE PROCEDURE eva.unsafe_proc() SELECT 1;", "DROP PROCEDURE eva.unsafe_proc;"),
            ("CREATE EVENT eva.unsafe_event ON SCHEDULE AT CURRENT_TIMESTAMP + INTERVAL 1 DAY DO SELECT 1;", "DROP EVENT eva.unsafe_event;"),
            ("CREATE TABLE eva.unsafe_fk (id INT, FOREIGN KEY (id) REFERENCES isolated_reference.parent(id)) ENGINE=InnoDB;",
             "DROP TABLE eva.unsafe_fk;"),
        ]
        for create, remove in variants:
            with self.subTest(create=create):
                self.t.sql(create)
                try:
                    before = self.t.snapshot("eva")
                    with self.assertRaisesRegex(MODULE.UnsafeTransition, "stored objects"):
                        self.t.run()
                    self.assertEqual(self.t.snapshot("eva"), before)
                    self.assertEqual(self.t.tables("eva_v3_0"), [])
                    self.assertFalse(list(self.t.backup_root.glob("*/source.json")))
                finally:
                    self.t.sql(remove)

    def test_populated_destination_is_unchanged(self):
        self.t.sql("CREATE DATABASE eva_v3_0; CREATE TABLE eva_v3_0.unrelated (id INT); INSERT INTO eva_v3_0.unrelated VALUES (42);")
        with self.assertRaises(MODULE.UnsafeTransition):
            self.t.run()
        self.assertEqual(self.t.sql("SELECT id FROM eva_v3_0.unrelated;"), "42")

    def conflicting_databases(self):
        self.t.sql("""CREATE DATABASE eva_v3_0;
        CREATE TABLE eva_v3_0.config LIKE eva.config;
        INSERT INTO eva_v3_0.config VALUES ('eva_id','unrelated-test-id');
        CREATE TABLE eva_v3_0.alembic_version LIKE eva.alembic_version;
        INSERT INTO eva_v3_0.alembic_version VALUES ('different-schema');""")
        # Model a previous install switching the App connection to the new DB.
        secret = self.t.objects[("secret", "app-secret")]
        secret["data"]["MYSQL_NAME"] = self.t.resources[2]["data"]["MYSQL_NAME"]

    def test_populated_destination_retained_despite_different_legacy_id(self):
        self.conflicting_databases()
        before = {name: self.t.snapshot(name) for name in ("eva", "eva_v3_0")}
        result = self.t.run()
        self.assertFalse(result["copied"])
        self.assertTrue(result["existing_destination_retained"])
        self.assertEqual({name: self.t.snapshot(name) for name in before}, before)

    def test_license_identity_correction_changes_only_one_config_value(self):
        self.conflicting_databases()
        self.t.sql("""INSERT INTO eva_v3_0.config VALUES ('target-setting','do-not-touch');
        CREATE TABLE eva_v3_0.camera (seq INT PRIMARY KEY, name VARCHAR(80));
        INSERT INTO eva_v3_0.camera VALUES (42,'existing-target-camera');
        CREATE TABLE eva_v3_0.payload (seq INT PRIMARY KEY,content LONGBLOB);
        INSERT INTO eva_v3_0.payload VALUES (99,0x00FFAABB);""")
        original_legacy = self.t.snapshot("eva")
        cache = b"encrypted-private-fixture"
        self.t.license_evidence = {"status": "present", "product": "test-product",
                                   "eva_id_hex": b"preserved-test-id".hex().upper(),
                                   "cache_sha256": hashlib.sha256(cache).hexdigest(),
                                   "encrypted_cache_base64": base64.b64encode(cache).decode()}
        result = self.t.run()
        self.assertEqual(result["license_identity"], "corrected")
        self.assertFalse(result["copied"])
        self.assertEqual(self.t.eva_id("eva_v3_0"), b"preserved-test-id".hex().upper())
        self.assertEqual(self.t.snapshot("eva"), original_legacy)
        self.assertEqual(self.t.sql("SELECT value FROM eva_v3_0.config WHERE `key`='target-setting';"), "do-not-touch")
        self.assertEqual(self.t.sql("SELECT name FROM eva_v3_0.camera WHERE seq=42;"), "existing-target-camera")
        self.assertEqual(self.t.sql("SELECT HEX(content) FROM eva_v3_0.payload;"), "00FFAABB")
        directory = Path(result["backup_directory"])
        self.assertTrue((directory / "identity-complete.json").is_file())
        self.assertIn("unrelated-test-id", (directory / "eva_v3_0-before-identity.sql").read_text())
        again = self.t.run()
        self.assertEqual(again["license_identity"], "matched")
        self.assertFalse(again["identity_updated"])

    def test_reviewed_original_db_retained_with_full_legacy_backup(self):
        self.conflicting_databases()
        before = {name: self.t.snapshot(name) for name in ("eva", "eva_v3_0")}
        self.t.explicit_source = "eva"
        self.t.resources[2]["data"]["MYSQL_NAME"] = base64.b64encode(b"eva").decode()
        result = self.t.run()
        self.assertFalse(result["copied"])
        self.assertEqual(result["source"], "eva")
        self.assertEqual(result["destination"], "eva")
        self.assertEqual({name: self.t.snapshot(name) for name in before}, before)
        self.assertEqual(len(result["legacy_user_csv"]), 1)
        self.assertTrue((Path(result["backup_directory"]) / "eva.sql").is_file())

    def test_identity_correction_refuses_real_foreign_key_cascade(self):
        self.conflicting_databases()
        self.t.sql("""ALTER TABLE eva_v3_0.config ADD INDEX config_value (value);
        CREATE TABLE eva_v3_0.linked_identity (
            id INT PRIMARY KEY, identity_value VARCHAR(256),
            FOREIGN KEY (identity_value) REFERENCES eva_v3_0.config(value) ON UPDATE CASCADE
        ) ENGINE=InnoDB;
        INSERT INTO eva_v3_0.linked_identity VALUES (42,'unrelated-test-id');""")
        cache = b"encrypted-private-fixture"
        self.t.license_evidence = {"status": "present", "product": "test-product",
                                   "eva_id_hex": b"preserved-test-id".hex().upper(),
                                   "cache_sha256": hashlib.sha256(cache).hexdigest(),
                                   "encrypted_cache_base64": base64.b64encode(cache).decode()}
        with self.assertRaisesRegex(MODULE.UnsafeTransition, "references"):
            self.t.run()
        self.assertEqual(self.t.eva_id("eva_v3_0"), b"unrelated-test-id".hex().upper())
        self.assertEqual(self.t.sql("SELECT identity_value FROM eva_v3_0.linked_identity WHERE id=42;"), "unrelated-test-id")


class LiveTransition(MODULE.Transition):
    def cached_license(self, current_app, desired):
        return getattr(self, "license_evidence", {"status": "absent", "product": "test-product"})

    def get(self, kind, name):
        return self.objects.get((kind, name))

    def run_command(self, args, **kwargs):
        if args[0] == "kubectl":
            return subprocess.CompletedProcess(args, 0, stdout=json.dumps({"items": self.pods}).encode())
        return super().run_command(args, **kwargs)

    def mysql(self, args, **kwargs):
        shell = 'export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec "$@"'
        return self.run_command(["docker", "exec", "-i", self.test_container, "sh", "-c", shell, "sh", *args], **kwargs)


@unittest.skipUnless(os.environ.get("EVA_TEST_APP_IMAGE"), "set EVA_TEST_APP_IMAGE to a cached App image")
class AppImageLicenseReaderTests(unittest.TestCase):
    def test_actual_sdk_cache_format_with_readonly_image_and_no_network(self):
        reader = Path(__file__).resolve().parents[2] / "src/solution/roles/eva_app/files/read_license_identity.py"
        code = '''
import ast, importlib.util, json, tempfile
from pathlib import Path
from licensespring.licensefile.default_crypto import DefaultCryptoProvider
config = Path('/apps/backend/app/app/core/config.py')
defaults = {}
for node in ast.walk(ast.parse(config.read_text())):
    if isinstance(node, ast.AnnAssign) and isinstance(node.target, ast.Name):
        if node.target.id in ('LICENSE_FILE_KEY', 'LICENSE_FILE_IV'):
            defaults[node.target.id] = ast.literal_eval(node.value)
spec = importlib.util.spec_from_file_location('reader', '/tool/read_license_identity.py')
reader = importlib.util.module_from_spec(spec)
spec.loader.exec_module(reader)
with tempfile.TemporaryDirectory() as directory:
    root = Path(directory)
    (root / 'test-product').mkdir()
    crypto = DefaultCryptoProvider(defaults['LICENSE_FILE_KEY'], defaults['LICENSE_FILE_IV'])
    cache = root / 'test-product/License.key'
    cache.write_text(crypto.encrypt(json.dumps({'product': 'test-product', 'hardware_id': 'eva_id:fixture-only-id'})))
    before = cache.read_bytes()
    result = reader.read_identity(root, 'test-product', config, {})
    assert result['eva_id_hex'] == b'fixture-only-id'.hex().upper()
    assert before == cache.read_bytes()
print('SDK_READER_FORMAT_OK')
'''
        result = subprocess.run(["docker", "run", "--rm", "-i", "--pull=never", "--network=none", "--read-only",
                                 "--tmpfs", "/tmp:rw,size=32m", "--cap-drop=ALL", "--security-opt=no-new-privileges",
                                 "--mount", "type=bind,src=" + str(reader) + ",dst=/tool/read_license_identity.py,readonly",
                                 "--entrypoint", "python", os.environ["EVA_TEST_APP_IMAGE"], "-B", "-"],
                                input=code.encode(), capture_output=True, timeout=60)
        self.assertEqual(result.returncode, 0, result.stderr.decode())
        self.assertIn(b"SDK_READER_FORMAT_OK", result.stdout)


if __name__ == "__main__":
    unittest.main(verbosity=2)
