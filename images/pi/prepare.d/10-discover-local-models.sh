#!/bin/sh
# Discover model servers on opened host ports and write ~/.pi/agent/models.json.
#
# pi-local is started with --allow-host-port PORT for each local model server
# (llama-server, Ollama, LM Studio). The opened ports are given in BOKS_HOST_PORTS
# as a comma-separated list (e.g. "8080,11434").
#
# For each port, probe /v1/models (OpenAI-compatible API) to find the model, then
# write a models.json entry with the correct baseUrl and a placeholder apiKey.
#
# The apiKey is ALWAYS a placeholder. pi refuses a custom provider without one ("apiKey is
# required when defining custom models" — the v0.1.29 failure), and the real key must never
# be in the sandbox: store it on the host with `boks secret set local-model` and the proxy
# replaces the Authorization header on every request to host.boks.internal. A server started
# without --api-key ignores the placeholder.
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


# Discover models across all opened host ports, writing a single models.json.
#
# For each port in BOKS_HOST_PORTS, curl the /v1/models endpoint of the OpenAI-compatible
# server reachable as host.boks.internal:<port>/v1. On success, a provider block is
# emitted with the discovered model IDs.
#
# Written as a Python helper so we can parse JSON reliably without jq (which is on the
# base image but we want this to be a self-contained shell script).

python3 - "$ports" "$models_json" "$agent_dir/settings.json" "$agent_dir/.boks-llama-router" <<'PYEOF'
import json, subprocess, sys, os

ports_str = sys.argv[1]
models_json_path = sys.argv[2]
settings_path = sys.argv[3]
router_path = sys.argv[4]

providers = {}
router_url = None       # the first llama.cpp router found; see boks-pi-local
router_default = None   # a model loaded on it, if any

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
        entries = [m for m in data.get("data", []) if "id" in m]
        if not entries:
            continue

        # llama-server in router mode lists every model it can serve, with
        # status.value "loaded" or "unloaded" and the arguments it starts each with. A
        # loaded one goes first, so the default below is a model that answers rather than
        # "400 model is not loaded"; and --ctx-size becomes pi's context window, so pi does
        # not plan for more context than the server gives. Servers without these fields
        # (Ollama, LM Studio, a single-model llama-server) keep their order and pi's defaults.
        def loaded(m):
            status = m.get("status")
            return isinstance(status, dict) and status.get("value") == "loaded"

        def ctx_size(m):
            status = m.get("status")
            args = status.get("args", []) if isinstance(status, dict) else []
            for flag in ("--ctx-size", "-c"):
                if flag in args:
                    i = args.index(flag)
                    if i + 1 < len(args) and args[i + 1].isdigit():
                        return int(args[i + 1])
            return None

        # A llama.cpp router is left to pi's built-in llama.cpp provider, which loads and
        # unloads models on it through /llama: listing it here too would show every model
        # twice, and as fixed names that answer "model is not loaded" until something loads
        # them. Its status field is how a router identifies itself.
        if any(isinstance(m.get("status"), dict) for m in entries):
            if router_url is None:
                router_url = f"http://{host}:{port}"
                router_default = next((m["id"] for m in entries if loaded(m)), None)
            continue

        entries.sort(key=lambda m: not loaded(m))
        model_objs = []
        for m in entries:
            obj = {"id": m["id"]}
            ctx = ctx_size(m)
            if ctx:
                obj["contextWindow"] = ctx
            model_objs.append(obj)
        provider = {
            "api": "openai-completions",
            "baseUrl": base_url,
            # A placeholder: see the comment at the top of this file.
            "apiKey": "boks-managed",
            # llama-server and most local servers do not know the developer role or
            # reasoning_effort; pi's own docs say to turn both off for them.
            "compat": {"supportsDeveloperRole": False, "supportsReasoningEffort": False},
            "models": model_objs,
        }

        # Not "host:PORT": pi reads ":<thinking>" off the end of a model reference, and
        # local model IDs already carry colons (":Q4_K_M").
        providers[f"local-{port}"] = provider

    except Exception:
        # Port unreachable or malformed response — skip it.
        continue

output = {"providers": providers}

def write(path, data):
    with open(path + ".tmp", "w") as f:
        json.dump(data, f, indent=2)
        f.write("\n")
    os.replace(path + ".tmp", path)

write(models_json_path, output)

# Recorded for boks-pi-local, which exports it as LLAMA_BASE_URL. Removed when no router
# was found this time, so a stale one is never used.
if router_url:
    with open(router_path, "w") as f:
        f.write(router_url + "\n")
elif os.path.exists(router_path):
    os.remove(router_path)

# Start on a model that answers unless the user has chosen one: without a default, pi starts
# on a cloud provider pi-local cannot reach. A router's loaded model comes first; a router
# with nothing loaded gets no default, and /llama is where one is loaded.
try:
    with open(settings_path) as f:
        settings = json.load(f)
except Exception:
    settings = {}
chosen = settings.get("defaultProvider")
# A "local-PORT" default is one this script wrote; once that provider is gone (the port is a
# router now, or closed) it is stale, not the user's choice, and is replaced.
if chosen and chosen.startswith("local-") and chosen not in providers:
    chosen = None
if not chosen and router_default:
    settings["defaultProvider"] = "llama.cpp"
    settings["defaultModel"] = router_default
    write(settings_path, settings)
elif not chosen and providers:
    first = next(iter(providers))
    settings["defaultProvider"] = first
    settings["defaultModel"] = providers[first]["models"][0]["id"]
    write(settings_path, settings)
PYEOF

if [ -r "$agent_dir/.boks-llama-router" ]; then
	echo "pi-local: llama.cpp router at $(head -n 1 "$agent_dir/.boks-llama-router"); /llama loads models, /model picks one" >&2
fi
