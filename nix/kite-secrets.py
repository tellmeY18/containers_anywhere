"""Generate installation-specific Kite keys once, before K3s imports manifests."""
import json
import os
from pathlib import Path
import secrets

root = Path("/var/lib/containers-anywhere")
keys = root / "kite-secrets.json"
if not keys.exists():
    data = {"JWT_SECRET": secrets.token_hex(32), "KITE_ENCRYPT_KEY": secrets.token_hex(32)}
    temporary = root / "kite-secrets.tmp"
    with temporary.open("w") as f:
        json.dump(data, f)
        f.flush()
        os.fsync(f.fileno())
    temporary.replace(keys)
data = json.loads(keys.read_text())
assert all(isinstance(data.get(k), str) and len(data[k]) == 64 for k in ("JWT_SECRET", "KITE_ENCRYPT_KEY"))
manifest = Path("/var/lib/rancher/k3s/server/manifests/00-kite-secrets.json")
manifest.parent.mkdir(parents=True, exist_ok=True)
temporary = manifest.with_suffix(".tmp")
with temporary.open("w") as f:
    json.dump({"apiVersion": "v1", "kind": "List", "items": [
        {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": "kite-system"}},
        {"apiVersion": "v1", "kind": "Secret", "metadata": {"name": "kite-secrets", "namespace": "kite-system"}, "stringData": data},
    ]}, f)
    f.flush()
    os.fsync(f.fileno())
temporary.replace(manifest)
