#!/bin/sh
# Register the pi extensions the image ships in /opt/pi-extensions (see the Dockerfile) in
# pi's settings, beside whatever the agent installed itself with `pi install`.
#
# Re-run on every start, so a sandbox picks up a newer image's set: entries under
# /opt/pi-extensions that the image no longer has are dropped, and everything else in
# "packages" is left as it is. BOKS_NO_PI_EXTENSIONS=1 leaves pi's settings untouched.
set -eu

[ "${BOKS_NO_PI_EXTENSIONS:-}" = "1" ] && exit 0
root=/opt/pi-extensions
[ -r "$root/package.json" ] || exit 0

settings="${HOME}/.pi/agent/settings.json"
mkdir -p "$(dirname "$settings")"

python3 - "$root" "$settings" <<'PYEOF'
import json, os, sys

root, path = sys.argv[1], sys.argv[2]
shipped = [os.path.join(root, "node_modules", name) for name in json.load(open(os.path.join(root, "package.json")))["dependencies"]]
shipped = [p for p in shipped if os.path.isdir(p)]

try:
    with open(path) as f:
        settings = json.load(f)
except (OSError, ValueError):
    settings = {}

own = [p for p in settings.get("packages", []) if not (isinstance(p, str) and p.startswith(root + "/"))]
packages = own + shipped
if settings.get("packages") != packages:
    settings["packages"] = packages
    with open(path + ".tmp", "w") as f:
        json.dump(settings, f, indent=2)
    os.replace(path + ".tmp", path)
PYEOF
