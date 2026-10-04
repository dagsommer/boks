#!/bin/sh
# Discover model servers on opened host ports and write ~/.pi/agent/models.json.
#
# pi-local is started with --allow-host-port PORT for each local model server
# (llama-server, Ollama, LM Studio). The opened ports are given in BOKS_HOST_PORTS
# as a comma-separated list (e.g. "8080,11434").
#
# For each port, probe /v1/models (OpenAI-compatible API) to find the model, then
# write a models.json entry with the correct baseUrl and apiKey (if the port
# carries a BOKS_LOCAL_MODEL_API_KEY env var).
#
# The file is written to ~/.pi/agent/models.json, which is where pi reads custom
# model definitions from (see model-registry.ts, getAgentDir).
#
# A script that fails does NOT stop the sandbox: boks-prepare steps over failures
# and reports them. Still, this script is defensive — curl returns non-zero if
# the endpoint is unreachable, and the probe skips that port cleanly.

set -eu

# Nothing to do if no host ports were opened.
ports="${BOKS_HOST_PORTS:-}"
[ -n "$ports" ] || exit 0

# Where pi stores its custom model definitions.
agent_dir="${HOME}/.pi/agent"
mkdir -p "$agent_dir"

models_json="$agent_dir/models.json"

# API key from the local-model secret, if configured.
# pi reads apiKey from models.json; the proxy swaps the placeholder for the real key.
api_key="${BOKS_LOCAL_MODEL_API_KEY:-}"

# Discover models across all opened host ports, writing a single models.json.
#
# For each port in BOKS_HOST_PORTS, curl the /v1/models endpoint of the OpenAI-compatible
# server reachable as host.boks.internal:<port>/v1. On success, a provider block is
# emitted with the discovered model IDs.
#
# Written as a Python helper so we can parse JSON reliably without jq (which is on the
# base image but we want this to be a self-contained shell script).

python3 - "$ports" "$models_json" "$api_key" <<'PYEOF'
import json, subprocess, sys, os

ports_str = sys.argv[1]
models_json_path = sys.argv[2]
api_key = sys.argv[3] if len(sys.argv) > 3 else ""

providers = {}

for port in ports_str.split(","):
    port = port.strip()
    if not port:
        continue

    host = os.environ.get("BOKS_MODEL_HOST", "host.boks.internal")
    base_url = f"http://{host}:{port}/v1"
    models_url = f"{base_url}/models"

    try:
        result = subprocess.run(
            ["curl", "-fsSL", "--max-time", "5", models_url],
            capture_output=True, text=True
        )
        if result.returncode != 0:
            continue

        data = json.loads(result.stdout)
        model_ids = [m["id"] for m in data.get("data", []) if "id" in m]

        if not model_ids:
            continue

        provider = {
            "api": "openai-completions",
            "baseUrl": base_url,
            "models": model_ids,
        }

        if api_key:
            provider["apiKey"] = api_key

        providers[f"host:{port}"] = provider

    except Exception:
        # Port unreachable or malformed response — skip it.
        continue

output = {"providers": providers}

with open(models_json_path + ".tmp", "w") as f:
    json.dump(output, f, indent=2)
    f.write("\n")
os.replace(models_json_path + ".tmp", models_json_path)
PYEOF

echo "pi-local: wrote models.json with discovered providers" >&2
cat "$models_json" >&2
