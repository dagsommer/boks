# Agents

Boks is agent-first: `boks run claude` names a prepared environment, not a command. An agent
is data — a name, an image, a startup command, and the destinations its CLI cannot work
without — never a branch in the CLI.

The registry in [`internal/agent/agent.go`](https://github.com/dagsommer/boks/blob/main/internal/agent/agent.go)
is the source of truth for everything on this page.

## The ten agents

| Agent | What it runs | Image |
|---|---|---|
| `shell` | a plain shell in the Boks base image | `ghcr.io/dagsommer/boks/base` |
| `claude` | Claude Code | `ghcr.io/dagsommer/boks/claude` |
| `codex` | OpenAI Codex | `ghcr.io/dagsommer/boks/codex` |
| `copilot` | GitHub Copilot CLI | `ghcr.io/dagsommer/boks/copilot` |
| `cursor` | Cursor CLI | `ghcr.io/dagsommer/boks/cursor` |
| `docker-agent` | Docker Agent | `ghcr.io/dagsommer/boks/docker-agent` |
| `droid` | Factory Droid | `ghcr.io/dagsommer/boks/droid` |
| `gemini` | Google Gemini CLI | `ghcr.io/dagsommer/boks/gemini` |
| `kiro` | Kiro | **none — needs `--template`** |
| `opencode` | OpenCode | `ghcr.io/dagsommer/boks/opencode` |

`shell` is the default, and it is what makes the agent grammar cover the plain "run a command
in a sandbox" case rather than sitting beside it. Images are multi-arch, built from
[`images/`](https://github.com/dagsommer/boks/tree/main/images) as a shared Debian base plus
one thin layer per agent, and published to GHCR as public images.

**Only the base image has run in a microVM.** The agent layers were exercised with
`docker run`, which establishes that each CLI is installed and starts, and nothing at all
about isolation.

### The one with no image

`kiro` is registered by name and ships nothing to run it, so asking for it gives a real
answer instead of "unknown agent":

```bash
boks run kiro --template ghcr.io/example/kiro:latest
```

The reason is in the registry: Kiro's CLI is distributed as a roughly 500 MB archive per
architecture, which would about triple the size of an agent image, and its installer resolves
the download through a "latest" manifest with no documented version-pinned URL — so there is
no artifact to pin and checksum the way every other image here does. Both would have to
change before it becomes an image.

## What an agent is allowed to reach

Each agent carries the destinations its CLI cannot work without. They resolve as an allow
layer of their own, labelled with the agent's name in `boks policy ls --agent claude`, beside
the preset and the global scope:

```bash
boks policy ls --agent claude    # what running that agent would add, and why
```

This changes no precedence at all. **A deny in any scope still beats them**, so
`boks policy deny api.anthropic.com` denies it for the `claude` agent too, and
`--policy locked` drops the layer entirely, because "deny everything" has to keep meaning
that.

| Agent | Allowed by default | Why |
|---|---|---|
| `claude` | `api.anthropic.com:443` | the Claude API; the agent cannot work without it |
| | `platform.claude.com:443` | the OAuth token endpoint a subscription login exchanges and refreshes against |
| `codex` | `api.openai.com:443` | the OpenAI API (vendor's own devcontainer firewall) |
| | `auth.openai.com:443` | sign-in issuer for `codex login` |
| | `chatgpt.com:443` | model API on a ChatGPT plan; exact host, never `*.chatgpt.com` |
| `copilot` | `*.githubcopilot.com:443` | Copilot API (docs.github.com allowlist reference) |
| | `github.com:443` | the device-flow sign-in Copilot CLI uses |
| | `api.github.com:443` | Copilot user management |
| `cursor` | `api2.cursor.sh:443`, `api5.cursor.sh:443` | Cursor API and agent requests (cursor.com network configuration) |
| | `authentication.cursor.sh:443`, `prod.authentication.cursor.sh:443`, `authenticate.cursor.sh:443` | sign-in, token issuer, authorisation endpoint |
| `gemini` | `cloudcode-pa.googleapis.com:443` | Gemini Code Assist endpoint (Google's network-access page) |
| | `oauth2.googleapis.com:443` | Google sign-in and token refresh |
| | `generativelanguage.googleapis.com:443` | the Gemini API on the API-key path |
| `shell`, `docker-agent`, `droid`, `kiro`, `opencode` | **nothing** | see below |

### The rule for what goes in one

Quoted from the registry, because a default allowlist nobody can audit is worth very little:

> Each entry is a default allow in every sandbox running that agent, so the bar is evidence,
> not plausibility. Two things qualify:
>
> - a destination *observed* to be needed on a real run, or
> - a destination the vendor's own documentation names as required.
>
> Anything else is left out.

Three consequences of that rule, each of which is deliberate:

- **Five agents carry nothing.** `docker-agent`, `droid` and `opencode` have no vendor page
  naming the destinations their CLIs require, and none has been found. The empty list is the
  honest state; their users will see the denial in `boks policy log` and write one
  `boks policy allow`. A domain guessed into the registry would be a hole in every user's
  policy, and invisible in exactly the way the nuisance is not.
- **Telemetry is deliberately absent.** Analytics, feature-flag and error-reporting endpoints
  — Statsig, Sentry, Datadog, Segment — are not what an agent needs to do the work, and a run
  with Datadog's intake blocked was observed to break nothing and to draw no complaint from
  the agent. They stay denied by default; a user who wants them can allow them by name.
- **Ports are pinned to 443**, for the same reason the presets pin them: allowing port 80 to
  the same host adds a plaintext downgrade path nobody asked for.

Only one entry in the whole registry was confirmed by a run rather than by reading:
`api.anthropic.com:443` for `claude`, which a real `boks run claude` was refused on before it
existed. Everything else is vendor documentation, cited in the entry.

A test enforces the rule mechanically: every entry must parse, must carry a reason, must not
allow every port, must not be a catch-all, must not wildcard a multi-tenant domain, and must
not match a known telemetry host.

## Credentials, by service name

Separate from the allowlist, and worth not confusing with it: naming a host for a credential
says **where a value may go**, not what is reachable. Boks knows eleven services, of which
nine carry a rule.

| Service | Destination | Guest variable |
|---|---|---|
| `anthropic` | `api.anthropic.com` | `ANTHROPIC_API_KEY` |
| `github` | `api.github.com`, `github.com` | `GITHUB_TOKEN` |
| `google` | `generativelanguage.googleapis.com` | `GEMINI_API_KEY` |
| `groq` | `api.groq.com` | `GROQ_API_KEY` |
| `mistral` | `api.mistral.ai` | `MISTRAL_API_KEY` |
| `nebius` | `api.tokenfactory.nebius.com` | `NEBIUS_API_KEY` |
| `openai` | `api.openai.com` | `OPENAI_API_KEY` |
| `openrouter` | `openrouter.ai` | `OPENROUTER_API_KEY` |
| `xai` | `api.x.ai` | `XAI_API_KEY` |
| `cursor` | **no rule** | — |
| `droid` | **no rule** | — |

`boks secret services` prints this list with what Boks knows about each. The bar is the one
the allowlist sets — vendor documentation, cited in the entry, or nothing — because a guessed
header is worse than an absent one: it ships your placeholder to the real API instead of your
credential, and fails in a way you cannot diagnose.

Neither Cursor nor Factory documents the host their CLI sends its API key to.
`boks secret set cursor` and `boks secret set droid` therefore refuse and explain, and give
you the `--inject` form instead. See [Usage](usage.md#credentials-the-name-is-the-configuration).

Note that two of these do **not** use bearer tokens: Anthropic sends `x-api-key` and Google
sends `x-goog-api-key`. That is the kind of detail the service registry exists to hold so
that nobody has to know it.

## Defining your own

There is no way to define an agent in a file rather than in code yet — see the
[roadmap](roadmap.md). Until then, `--template` points any agent at another image:

```bash
boks run shell --template ghcr.io/example/my-toolchain:latest .
```

An image supplied with `--template` does not inherit the Boks images' init prefix, because
that prefix names paths only a Boks image has.
