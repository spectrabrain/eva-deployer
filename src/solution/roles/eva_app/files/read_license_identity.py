"""Read an encrypted local LicenseSpring cache, never initialize the EVA App.

Runs inside the existing App IMAGE with only its license volume mounted READ ONLY.
No license manager, database, network, activation, signature bypass or cache writes.
The output is private machine input; the caller must never log it.
"""

import ast
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import sys


def read_identity(root, product, config_path, environment):
    if not re.fullmatch(r"[A-Za-z0-9_-]+", product):
        raise ValueError("Unsupported license product code")
    root = Path(root)
    path = root / product / "License.key"
    for item in (root, root / product, path):
        if item.is_symlink():
            raise ValueError("License path must not contain symlinks")
    if not path.exists():
        return {"status": "absent", "product": product}
    if not path.is_file() or not 0 < path.stat().st_size <= 8 * 1024 * 1024:
        raise ValueError("Invalid license cache file")
    # Extract only literal defaults. Importing app.core.config would initialize
    # product settings/dependencies; importing License would access the DB.
    defaults = {}
    tree = ast.parse(Path(config_path).read_text())
    for node in ast.walk(tree):
        if isinstance(node, ast.AnnAssign) and isinstance(node.target, ast.Name):
            if node.target.id in ("LICENSE_FILE_KEY", "LICENSE_FILE_IV"):
                defaults[node.target.id] = ast.literal_eval(node.value)
    key, iv = (environment.get(name, defaults.get(name))
               for name in ("LICENSE_FILE_KEY", "LICENSE_FILE_IV"))
    if not isinstance(key, str) or not isinstance(iv, str):
        raise ValueError("License encryption configuration unavailable")
    # Same AES-CBC/PKCS7 format as SDK DefaultCryptoProvider. Import only the
    # crypto primitives, not SDK initialization or product code.
    from Crypto.Cipher import AES
    from Crypto.Util.Padding import unpad

    raw = path.read_bytes()
    encrypted = base64.b64decode(raw.strip(), validate=True)
    decrypted = unpad(AES.new(bytes.fromhex(key), AES.MODE_CBC, bytes.fromhex(iv)).decrypt(encrypted), 16)
    data = json.loads(decrypted)
    if not isinstance(data, dict) or data.get("product") != product:
        raise ValueError("License product does not match configured product")
    hardware = data.get("hardware_id")
    if not isinstance(hardware, str) or not hardware.startswith("eva_id:"):
        raise ValueError("Unsupported license hardware identity")
    eva_id = hardware[len("eva_id:"):]
    if not re.fullmatch(r"[A-Za-z0-9_-]{1,128}", eva_id):
        raise ValueError("Invalid EVA license identity")
    return {"status": "present", "product": product,
            "eva_id_hex": eva_id.encode().hex().upper(),
            "cache_sha256": hashlib.sha256(raw).hexdigest(),
            "encrypted_cache_base64": base64.b64encode(raw).decode()}


def main():
    try:
        # Defense in depth, independent of CNI NetworkPolicy enforcement.
        def deny_network(event, _args):
            if event in ("socket.connect", "socket.getaddrinfo", "socket.bind"):
                raise RuntimeError("Network access prohibited in local license reader")
        sys.addaudithook(deny_network)
        result = read_identity("/eva-license", os.environ.get("LICENSE_PRODUCT_CODE", ""),
                               "/apps/backend/app/app/core/config.py", os.environ)
        print(json.dumps(result))
    except Exception:
        print("Local license identity could not be read; no identity correction allowed.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
