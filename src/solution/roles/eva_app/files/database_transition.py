"""Pre-start DB copy and legacy-user preservation for the internal App MySQL.

Only kubectl/standard MySQL clients already present in the DB Pod are used.
Never import the application or call LicenseSpring. Child diagnostics may contain
credentials or row data: expose only our fixed, actionable error messages.
"""

import argparse
import base64
import csv
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
from datetime import datetime, timezone

import yaml


class UnsafeTransition(Exception):
    pass


def require(condition, message):
    if not condition:
        raise UnsafeTransition(message)


def identifier(value):
    require(isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9_]+", value),
            "Unsupported SQL identifier; use letters, digits and underscores.")
    return "`" + value + "`"


def private_dir(path):
    path = Path(path)
    require(path.is_absolute(), "Backup root must be an absolute path.")
    for item in [*reversed(path.parents), path]:
        require(not item.is_symlink(), "Backup path contains a symlink.")
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    require(path.is_dir() and path.stat().st_uid == os.geteuid(),
            "Backup directory must belong to the executing account.")
    path.chmod(0o700)
    return path


def write_json(path, value):
    # Unique run directories: never overwrite a prior successful backup.
    with path.open("x", encoding="utf-8") as stream:
        json.dump(value, stream, indent=2)
        stream.write("\n")


def sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


class Transition:
    def __init__(self, namespace, release, resources, backup_root, source="", auto=True):
        self.namespace = namespace
        self.release = release
        self.resources = resources
        self.backup_root = Path(backup_root)
        self.explicit_source = source
        self.auto = auto
        self.pod = None
        self.container = None
        self.directory = None
        self.csv_files = []

    def run_command(self, args, **kwargs):
        try:
            result = subprocess.run(args, stderr=subprocess.PIPE, timeout=3600,
                                    check=False, **kwargs)
        except (OSError, subprocess.TimeoutExpired) as exc:
            raise UnsafeTransition("DB preparation command unavailable or timed out; inspect private backups.") from exc
        require(result.returncode == 0,
                "DB preparation command failed; check Kubernetes/MySQL access and private backups. Child output suppressed.")
        return result

    def kubectl(self, *args):
        return ["kubectl", "-n", self.namespace, "--request-timeout=30s", *args]

    def get(self, kind, name):
        raw = self.run_command(self.kubectl("get", kind, name, "--ignore-not-found=true", "-o", "json"),
                               stdout=subprocess.PIPE).stdout
        return json.loads(raw) if raw.strip() else None

    def resource(self, kind, name):
        matches = [r for r in self.resources if r.get("kind") == kind
                   and r.get("metadata", {}).get("name") == name
                   and r.get("metadata", {}).get("namespace", self.namespace) == self.namespace]
        require(len(matches) == 1, "Rendered Chart resource identity is ambiguous or unsupported.")
        return matches[0]

    def env(self, container, rendered=False):
        values = {}

        def config(kind, name):
            obj = self.resource(kind, name) if rendered else self.get(kind.lower(), name)
            require(obj is not None, "Referenced DB environment resource is missing.")
            data = dict(obj.get("data", {}))
            if kind == "Secret":
                data = {k: base64.b64decode(v, validate=True).decode("utf-8") for k, v in data.items()}
                data.update(obj.get("stringData", {}))
            return data

        for ref in container.get("envFrom", []):
            require(not ref.get("prefix"), "Prefixed App environment is unsupported for automatic DB preparation.")
            for key, kind in (("configMapRef", "ConfigMap"), ("secretRef", "Secret")):
                if key in ref:
                    values.update(config(kind, ref[key]["name"]))
        for item in container.get("env", []):
            if "value" in item:
                values[item["name"]] = str(item["value"])
            elif "valueFrom" in item:
                for key, kind in (("configMapKeyRef", "ConfigMap"), ("secretKeyRef", "Secret")):
                    if key in item["valueFrom"]:
                        ref = item["valueFrom"][key]
                        values[item["name"]] = config(kind, ref["name"])[ref["key"]]
        return values

    @staticmethod
    def app_container(deployment):
        containers = deployment["spec"]["template"]["spec"]["containers"]
        matches = [c for c in containers if c.get("name") == "eva-app"]
        require(len(matches) == 1, "Automatic DB preparation requires exactly one eva-app container.")
        return matches[0]

    @staticmethod
    def storage(deployment, container, mount_path):
        spec = deployment["spec"]["template"]["spec"]
        mounts = [m for m in container.get("volumeMounts", []) if m.get("mountPath") == mount_path]
        require(len(mounts) == 1, "Required persistent DB/license mount is missing or ambiguous.")
        mount = mounts[0]
        volumes = [v for v in spec.get("volumes", []) if v["name"] == mount["name"]]
        require(len(volumes) == 1, "Persistent volume is missing or ambiguous.")
        volume = volumes[0]
        if "hostPath" in volume:
            backing = ("hostPath", "/" + os.path.normpath(volume["hostPath"]["path"]).lstrip("/"))
        elif "persistentVolumeClaim" in volume:
            backing = ("pvc", volume["persistentVolumeClaim"]["claimName"])
        else:
            raise UnsafeTransition("Automatic DB preparation supports only persistent hostPath/PVC storage.")
        return backing + (mount.get("subPath", ""), mount.get("subPathExpr", ""))

    def mysql(self, args, **kwargs):
        # Password remains inside the DB container; never pass it via argv,
        # controller env, shell history, or backup metadata.
        shell = ('test -n "$MYSQL_ROOT_PASSWORD" || exit 40; '
                 'export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec "$@"')
        return self.run_command(self.kubectl("exec", "-i", self.pod, "-c", self.container,
                                            "--", "sh", "-c", shell, "sh", *args), **kwargs)

    def sql(self, statement):
        return self.mysql(["mysql", "--user=root", "--default-character-set=utf8mb4",
                           "--batch", "--raw", "--skip-column-names"],
                          input=statement.encode("utf-8"), stdout=subprocess.PIPE).stdout.decode("utf-8").rstrip("\n")

    def tables(self, db):
        identifier(db)
        text = self.sql("SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA='"
                        + db + "' ORDER BY TABLE_NAME;")
        return text.splitlines() if text else []

    def eva_id(self, db):
        value = self.sql("SELECT HEX(value) FROM " + identifier(db) + ".config WHERE `key`='eva_id';")
        require(bool(re.fullmatch(r"[0-9A-Fa-f]+", value)) and len(value) % 2 == 0,
                "Source/destination config.eva_id is missing or ambiguous; no identity will be generated.")
        return value

    def backup_directory(self):
        if self.directory is None:
            self.directory = Path(tempfile.mkdtemp(
                prefix=datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ-"), dir=self.backup_root))
            self.directory.chmod(0o700)
        return self.directory

    def dump(self, db, path):
        with path.open("xb") as stream:
            self.mysql(["mysqldump", "--user=root", "--default-character-set=utf8mb4",
                        "--single-transaction", "--quick", "--hex-blob", "--no-tablespaces",
                        "--set-gtid-purged=OFF", "--skip-comments", "--skip-dump-date",
                        "--order-by-primary", "--skip-add-locks", db], stdout=stream)
            stream.flush()
            os.fsync(stream.fileno())
        require(path.stat().st_size > 0, "Empty DB dump; migration stopped.")

    def snapshot(self, db):
        # Quiesced App + InnoDB transaction dump. Never trust estimated TABLE_ROWS.
        tables = self.tables(db)
        counts = {t: self.sql("SELECT COUNT(*) FROM " + identifier(db) + "." + identifier(t) + ";")
                  for t in tables}
        revisions = self.sql("SELECT version_num FROM " + identifier(db) + ".alembic_version ORDER BY version_num;")
        return {"tables": counts, "eva_id": self.eva_id(db), "alembic": revisions}

    def copy_database(self, source, target):
        source_tables = self.tables(source)
        require("config" in source_tables and "alembic_version" in source_tables,
                "Source is not an identifiable EVA App DB; migration stopped.")
        self.eva_id(source)
        require(self.auto, "DB name change requires migration; enable eva_app_db_auto_migrate or migrate manually.")
        require(not self.tables(target),
                "Destination DB is populated: refusing overwrite. Back up and reconcile both DBs manually; source retained.")
        objects = self.sql(
            "SELECT (SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='" + source
            + "' AND (TABLE_TYPE!='BASE TABLE' OR ENGINE!='InnoDB'))"
            + "+(SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA='" + source + "')"
            + "+(SELECT COUNT(*) FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA='" + source + "')"
            + "+(SELECT COUNT(*) FROM information_schema.EVENTS WHERE EVENT_SCHEMA='" + source + "')"
            + "+(SELECT COUNT(*) FROM information_schema.KEY_COLUMN_USAGE WHERE TABLE_SCHEMA='" + source
            + "' AND REFERENCED_TABLE_SCHEMA IS NOT NULL AND REFERENCED_TABLE_SCHEMA!='" + source + "');")
        require(objects == "0", "Source has non-InnoDB tables, stored objects or cross-DB references; manual migration required.")
        before = self.snapshot(source)
        directory = self.backup_directory()
        dump = directory / (source + ".sql")
        self.dump(source, dump)
        write_json(directory / "source.json", {"source": source, "destination": target,
                                               "snapshot": before, "dump_sha256": sha256(dump)})
        charset = self.sql("SELECT DEFAULT_CHARACTER_SET_NAME,DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='"
                           + source + "';").split("\t")
        require(len(charset) == 2, "Cannot determine source DB charset/collation.")
        self.sql("CREATE DATABASE IF NOT EXISTS " + identifier(target) + " CHARACTER SET "
                 + identifier(charset[0]) + " COLLATE " + identifier(charset[1]) + ";")
        # A failed restore is NOT cleaned up automatically. Retain dump and partial
        # destination; a retry fails closed instead of destroying either DB.
        with dump.open("rb") as stream:
            self.mysql(["mysql", "--user=root", "--default-character-set=utf8mb4", target],
                       stdin=stream, stdout=subprocess.DEVNULL)
        require(self.snapshot(source) == before and self.snapshot(target) == before,
                "Restored DB verification failed (tables/counts/Alembic/eva_id); source retained, inspect private backup.")
        # Compare canonical dumps too: row counts alone cannot prove row fidelity.
        verify = directory / (target + "-verified.sql")
        self.dump(target, verify)
        require(sha256(dump) == sha256(verify),
                "Restored DB content dump differs; source retained, manual review required.")
        write_json(directory / "copy-complete.json", {"source": source, "destination": target,
                                                      "verified": True, "source_retained": True})

    def run(self):
        proposed = self.resource("Deployment", self.release)
        desired = self.env(self.app_container(proposed), rendered=True)
        target = desired.get("MYSQL_NAME")
        identifier(target)
        mysql_name = (self.release + "-mysql")[:63].rstrip("-")
        current_mysql = self.get("deployment", mysql_name)
        current_app = self.get("deployment", self.release)
        if not current_mysql:
            require(not current_app, "Existing/external DB requires a verified manual dump and legacy-user export before upgrade.")
            require(any(r.get("kind") == "Deployment" and r.get("metadata", {}).get("name") == mysql_name
                        for r in self.resources),
                    "External DB requires manual identity validation and pre-upgrade legacy-user export.")
            return {"changed": False, "status": "new-install-no-existing-mysql"}
        require(current_app is not None, "Existing MySQL without App deployment: specify and validate recovery manually.")
        require(current_app["spec"].get("replicas", 1) == 0,
            "App Deployment must be scaled to zero before DB preparation.")
        mysql_target = self.resource("Deployment", mysql_name)
        current_env = self.env(self.app_container(current_app))
        require(all(current_env.get(k) == desired.get(k) for k in ("MYSQL_HOST", "MYSQL_PORT", "MYSQL_USER", "MYSQL_PSWD")),
                "DB endpoint/credentials change is not a DB-name migration; preserve current connection settings.")
        require(current_env.get("MYSQL_USER") == "root", "Automatic internal DB copy currently requires the existing root App user.")
        old_mysql_containers = current_mysql["spec"]["template"]["spec"]["containers"]
        new_mysql_containers = mysql_target["spec"]["template"]["spec"]["containers"]
        require(len(old_mysql_containers) == len(new_mysql_containers) == 1, "Ambiguous internal MySQL container.")
        require(self.storage(current_mysql, old_mysql_containers[0], "/var/lib/mysql")
                == self.storage(mysql_target, new_mysql_containers[0], "/var/lib/mysql"),
                "MySQL storage change requires manual migration; refusing DB copy into different storage.")
        for mount in ("/apps/backend/app/data", "/apps/backend/app/.license/LicenseSpring"):
            require(self.storage(current_app, self.app_container(current_app), mount)
                    == self.storage(proposed, self.app_container(proposed), mount),
                    "App data/license storage must stay unchanged during DB preparation.")
        selector = "app.kubernetes.io/instance=" + self.release
        pods_raw = self.run_command(self.kubectl("get", "pods", "-l", selector, "-o", "json"), stdout=subprocess.PIPE).stdout
        pods = json.loads(pods_raw)["items"]
        require(not any(p.get("metadata", {}).get("labels", {}).get("component") == "eva-app" for p in pods),
                "App Pods must be fully terminated before DB backup/copy.")
        db_pods = [p for p in pods if p.get("metadata", {}).get("labels", {}).get("component") == "eva-app-mysql"
                   and not p["metadata"].get("deletionTimestamp")]
        require(len(db_pods) == 1, "Expected exactly one existing internal MySQL Pod.")
        pod = db_pods[0]
        owners = [o for o in pod["metadata"].get("ownerReferences", []) if o.get("controller")]
        require(len(owners) == 1 and owners[0]["kind"] == "ReplicaSet", "MySQL Pod controller cannot be verified.")
        rs = self.get("replicaset", owners[0]["name"])
        require(rs is not None and rs["metadata"]["uid"] == owners[0]["uid"]
                and any(o.get("uid") == current_mysql["metadata"]["uid"] and o.get("controller")
                        for o in rs["metadata"].get("ownerReferences", [])), "MySQL Pod ownership mismatch.")
        self.pod = pod["metadata"]["name"]
        self.container = old_mysql_containers[0]["name"]
        require(any(s.get("name") == self.container and s.get("ready") for s in pod.get("status", {}).get("containerStatuses", [])),
                "MySQL Pod is not ready.")
        pod_containers = [c for c in pod["spec"]["containers"] if c["name"] == self.container]
        require(len(pod_containers) == 1 and self.env(pod_containers[0]).get("MYSQL_ROOT_PASSWORD") == current_env["MYSQL_PSWD"],
                "MySQL Pod credentials cannot be verified; no DB changes made.")
        pod_deployment = {"spec": {"template": {"spec": pod["spec"]}}}
        require(self.storage(pod_deployment, pod_containers[0], "/var/lib/mysql")
            == self.storage(current_mysql, old_mysql_containers[0], "/var/lib/mysql"),
            "Running MySQL Pod and Deployment storage differ; finish the existing rollout first.")
        live = current_env.get("MYSQL_NAME")
        identifier(live)
        source = self.explicit_source or live
        identifier(source)
        # Recover the known default-name regression even when a previous failed
        # upgrade already switched the Deployment to the new DB. Never overwrite
        # that populated target: differing identity must be reconciled manually.
        if not self.explicit_source and target != "eva" and self.tables("eva"):
            require(not self.tables(target) or self.eva_id("eva") == self.eva_id(target),
                    "Legacy eva and destination have different eva_id values; refuse automatic overwrite. Back up/reconcile both DBs.")
            if not self.tables(target) or live == "eva":
                source = "eva"
        require(bool(self.tables(source)), "Selected existing DB is missing/empty; refusing a silent fresh identity.")
        self.eva_id(source)
        sessions = self.sql("SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE ID!=CONNECTION_ID() AND DB IN ('"
                    + source + "','" + target + "');")
        require(sessions == "0", "Other DB sessions are active; stop all writers before backup/copy.")
        self.backup_root = private_dir(self.backup_root)
        with (self.backup_root / ".lock").open("a") as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as exc:
                raise UnsafeTransition("Another App DB preparation is running.") from exc
            self.export_legacy_users(source)
            if source != target:
                self.copy_database(source, target)
            elif self.csv_files:
                self.dump(source, self.backup_directory() / (source + ".sql"))
            result = {"changed": bool(self.directory), "source": source, "destination": target,
                      "copied": source != target, "source_retained": True,
                      "backup_directory": str(self.directory) if self.directory else None,
                      "legacy_user_csv": self.csv_files}
            if self.directory:
                write_json(self.directory / "result.json", result)
            return result

    def export_legacy_users(self, db):
        # Implemented below independently of whether a DB rename is necessary.
        columns = self.sql("SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='"
                           + db + "' AND TABLE_NAME='user' ORDER BY ORDINAL_POSITION;").splitlines()
        if not columns:
            return
        if "user_password" not in columns:
            require("keycloak_user_id" in columns,
                    "Unknown user schema; export/review manually before upgrading.")
            return
        directory = self.backup_directory()
        path = directory / "user_accounts_all.csv"
        # HEX avoids mysql batch escaping and embedded tabs/newlines. N is SQL
        # NULL, H<hex> is a non-NULL string, including an empty string. csv.writer
        # correctly quotes commas, double quotes and embedded line breaks.
        # HEX(integer) encodes its numeric value, not the text shown by SELECT
        # (e.g. HEX(1)='1'). Cast every value to UTF-8 text before hex encoding.
        expressions = ["IF(" + identifier(c) + " IS NULL,'N',CONCAT('H',HEX(CAST(" + identifier(c)
                   + " AS CHAR CHARACTER SET utf8mb4))))" for c in columns]
        raw = self.sql("SELECT " + ",".join(expressions) + " FROM " + identifier(db) + ".`user`;")
        with path.open("x", encoding="utf-8", newline="") as stream:
            writer = csv.writer(stream)
            writer.writerow(columns)
            for line in raw.splitlines():
                cells = line.split("\t")
                require(len(cells) == len(columns), "Legacy CSV row width mismatch; upgrade stopped.")
                row = []
                for cell in cells:
                    require(cell == "N" or re.fullmatch(r"H(?:[0-9A-Fa-f]{2})*", cell),
                            "Legacy CSV cell encoding invalid; upgrade stopped.")
                    row.append("\\N" if cell == "N" else bytes.fromhex(cell[1:]).decode("utf-8"))
                writer.writerow(row)
            stream.flush()
            os.fsync(stream.fileno())
        write_json(directory / "user_accounts_all.metadata.json", {
            "database": db, "columns": columns, "rows": len(raw.splitlines()),
            "sha256": sha256(path), "sql_null": "\\N", "contains_password_hashes": True,
            "purpose": "Private archive; manually re-register approved users in EVA IAM. Not an IAM import file.",
        })
        self.csv_files.append(str(path))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--namespace", required=True)
    parser.add_argument("--release", required=True)
    parser.add_argument("--manifest", required=True)
    parser.add_argument("--backup-root", required=True)
    parser.add_argument("--source", default="")
    parser.add_argument("--auto-migrate", choices=("true", "false"), default="true")
    args = parser.parse_args()
    os.umask(0o077)
    try:
        resources = [r for r in yaml.safe_load_all(Path(args.manifest).read_text()) if isinstance(r, dict)]
        result = Transition(args.namespace, args.release, resources, args.backup_root,
                            args.source, args.auto_migrate == "true").run()
        print(json.dumps(result))
    except UnsafeTransition as exc:
        print("[ERROR] " + str(exc), file=sys.stderr)
        return 1
    except Exception:
        print("[ERROR] DB preparation failed; review private backup directory and DB schema. Details suppressed to protect credentials/rows.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
