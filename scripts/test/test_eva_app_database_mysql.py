"""Opt-in real MySQL test. Creates/removes ONLY its own isolated container.

Set EVA_TEST_MYSQL_IMAGE to an already-cached MySQL 8 image. No pulls, published
ports, host mounts or live cluster access. Uses the production SQL/dump/CSV code.
"""

import csv
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import unittest
import uuid

from test_eva_app_database_transition import MODULE, fixture


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

    def test_no_name_change_still_archives_pre32_users(self):
        self.t.resources[2]["data"]["MYSQL_NAME"] = self.t.objects[("secret", "app-secret")]["data"]["MYSQL_NAME"]
        result = self.t.run()
        self.assertFalse(result["copied"])
        self.assertEqual(len(result["legacy_user_csv"]), 1)
        self.assertTrue((Path(result["backup_directory"]) / "eva.sql").exists())

    def test_populated_destination_is_unchanged(self):
        self.t.sql("CREATE DATABASE eva_v3_0; CREATE TABLE eva_v3_0.unrelated (id INT); INSERT INTO eva_v3_0.unrelated VALUES (42);")
        with self.assertRaises(MODULE.UnsafeTransition):
            self.t.run()
        self.assertEqual(self.t.sql("SELECT id FROM eva_v3_0.unrelated;"), "42")


class LiveTransition(MODULE.Transition):
    def get(self, kind, name):
        return self.objects.get((kind, name))

    def run_command(self, args, **kwargs):
        if args[0] == "kubectl":
            return subprocess.CompletedProcess(args, 0, stdout=json.dumps({"items": self.pods}).encode())
        return super().run_command(args, **kwargs)

    def mysql(self, args, **kwargs):
        shell = 'export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec "$@"'
        return self.run_command(["docker", "exec", "-i", self.test_container, "sh", "-c", shell, "sh", *args], **kwargs)


if __name__ == "__main__":
    unittest.main(verbosity=2)
