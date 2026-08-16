# Verifying the VM boundary

A sandbox is only worth the name if the command really ran behind a hypervisor. "It printed
`running in sandbox`" proves nothing. This document defines what counts as evidence, and the
procedure for collecting it.

> Flags throughout are written the way the CLI spells them today — `--cpus`, `--net none`,
> `--allow`. Runs recorded before the command line moved to cobra used the single-dash
> spellings (`-cpus`, `-net none`); only the spelling changed, and every observation below
> is reported exactly as it was made.

## Current status

**Verified on 2026-08-11.** `boks run` boots a genuine microVM through
`containerd-shim-nerdbox-v1` and libkrun, and the guest's kernel identity, uptime and
hardware topology are its own.

The host was macOS, which makes the result unusually clear-cut: the guest runs **Linux**
while the host runs **Darwin**, so a shared-kernel container is not a possible explanation
for any of it.

**Linux with KVM has never booted a sandbox**, which is a larger hole than it sounds: it is
the platform Boks is designed for, built for and shipped for, and the one most users would
reach first. Everything below is macOS. The kit for closing that gap is
[`scripts/verify-linux.sh`](../scripts/verify-linux.sh) and
[Verifying on Linux](verify-linux-prompt.md); on Linux the evidence is harder to read than it
was here, because host and guest are both Linux and `boot_id` rather than kernel version has
to carry the argument.

### Environment

| | |
|---|---|
| Host | Apple M5 Pro (T6050), 18 cores, 48 GiB |
| Host OS | macOS 26.5.2 (build 25F84), Darwin 25.5.0, `xnu-12377.121.10~1`, arm64 |
| containerd | 2.3.3 (Homebrew), running rootless as uid 502 |
| nerdbox | `cd2c23f`, shim codesigned with `com.apple.security.hypervisor` |
| libkrun / libkrunfw | 1.19.4 / 5.5.0 |
| erofs-utils / e2fsprogs | 1.9.3 / 1.47.4 |

### Host versus guest

Collected from the same machine, minutes apart. Guest values are from
`boks run shell . -- …` with `--cpus 2 --memory 2048`. (Those were the defaults at the time;
the defaults are now all host CPUs and half the host's memory, so a repeat run reports the
values you pass rather than these.)

| Fact | Host | Guest |
|---|---|---|
| `uname` | `Darwin 25.5.0 … arm64` | `Linux 6.12.44 #1 SMP aarch64` |
| kernel build | `xnu-12377.121.10~1/RELEASE_ARM64_T6050` | `gcc (Debian 12.2.0) … #1 SMP Tue Aug 11 17:34:15 UTC 2026` |
| `boot_id` | no procfs; macOS has none | `39e1d653-fa1d-420c-945f-97467b87c3b8`, **new on every run** |
| uptime | 28 days, 7:21 | `0.03` seconds |
| CPUs | 18 | 2 (tracks `--cpus`) |
| memory | 48 GiB | `MemTotal: 2044888 kB` (tracks `--memory`) |
| virtio devices | — | `virtio0`…`virtio7` (fs, block ×3, console, rng, balloon, vsock) |
| PID 1 | `launchd` | the sandboxed process itself |

Resource requests reach the VMM rather than being advisory:

| Flags | guest `nproc` | guest `MemTotal` |
|---|---|---|
| `--cpus 1 --memory 512` | 1 | 500532 kB |
| `--cpus 4 --memory 4096` | 4 | 4041812 kB |
| `--cpus 8 --memory 1024` | 8 | 1013764 kB |

A full boot, command and teardown takes **~0.23 s**.

### Behaviour verified behind that boundary

| Behaviour | Result |
|---|---|
| containerd connect, image pull, unpack (erofs) | pass |
| container + task create, start, wait, delete | pass |
| stdout/stderr streaming | pass |
| exit code propagation (0, 7, 42) | pass |
| workspace visible at its exact host path | pass |
| intermediate mount-point directories auto-created in the guest | pass |
| writes reaching the host promptly | pass |
| read-only workspace rejecting writes | pass |
| parent directories not exposed | pass |
| symlink out of the workspace does **not** escape to host files | pass |
| no host Docker or containerd socket in the guest | pass |
| no host processes or host filesystem visible | pass |
| cleanup: no leaked containers, tasks, shims or VM processes | pass |
| cleanup after SIGINT | pass |
| `BOKS_INTEGRATION=1` suite against `io.containerd.nerdbox.v1` | 7/7 pass |

### Not yet verified behind that boundary

The persistent sandbox lifecycle was built and tested against a real containerd on a host
with **no hypervisor**, using the runc dev runtime
(`--runtime io.containerd.runc.v2 --snapshotter native --i-know-this-is-not-isolated`). That
proved the containerd orchestration and nothing about the VM boundary.

`stop`, `start`, `exec`, `rm` and snapshot persistence have since been confirmed behind a
real hypervisor — see *Lifecycle behind a real VM* below. What still needs confirming:

- that an `exec`'d process runs inside the *same* VM as the sandbox's own process (compare
  `boot_id`, and check the exec'd process sees the sandbox's PID namespace);
- that `cp` works when the guest is reached through vsock rather than a local FIFO.

Run the suite unmodified — it defaults to the isolating runtime:

```bash
BOKS_INTEGRATION=1 go test ./internal/sandbox/ -run Integration -v
```

### What was *not* contained, in that configuration

Network. The guest had no virtio-net device — only `lo` — yet it reached the internet, and it
reached **the host's own loopback services**. nerdbox's default is libkrun's TSI, which
rewrites guest `AF_INET` sockets and performs the connection on the host, so guest
`127.0.0.1` is the *host's* `127.0.0.1`. Observed: DNS resolves, outbound HTTP and HTTPS
succeed, raw TCP connects, ICMP fails (`Network unreachable`), and a host service on
`127.0.0.1:11434` answered the guest. See [security-model.md](security-model.md).

`boks run` now wires every sandbox it creates to a host-side stack instead, which is what the
next section is for: that wiring has never been watched from inside a real guest.

## The network enforcement, answered on a host with a hypervisor

**Run 2026-08-12**, macOS 26.5.2 / Apple M5 Pro, containerd 2.3.3, nerdbox `cd2c23f`,
libkrun 1.19.4, against `io.containerd.nerdbox.v1` and the published
`ghcr.io/dagsommer/boks/base:0.1.0`. Eleven of the twelve checks passed; **check 6 failed**,
and it is the one that decides whether any of this is a boundary.

**All twelve now pass.** Check 6 was re-run on 2026-08-13 against a real guest, after the
stack was rebuilt around a policy-aware forwarder, and a real VM is now refused. The
caution below records the failure it replaced, because the fix is only meaningful next to
what it fixed.

> [!CAUTION]
> **Check 6 failed: policy in `--net nat` was advisory.** A guest that unsets the proxy
> variables reached denied destinations. Under `--policy locked --allow example.com`:
>
> ```
> === denied host, unsetting ALL FOUR proxy vars ===
>   http=200
> === raw TCP to denied 1.1.1.1:443, real TLS handshake via python ===
>   TLS to 1.1.1.1 SUCCEEDED, peer cert issuer: {'countryName': 'US', ...,
>     'commonName': 'SSL.com SSL Intermediate CA ECC R2'}
> ```
>
> The certificate is the origin's own, so this is a real end-to-end connection, not the
> proxy. Neither attempt appears in `boks policy log`: they never reached the policy engine.
>
> **Fixed, and re-verified against a real guest on 2026-08-13.** The host-side stack was
> gvisor-tap-vsock's `virtualnetwork.New`, whose TCP forwarder dials whatever address the
> guest puts in a SYN — no policy, and no hook in its public API to add one. Boks now
> assembles the stack itself and installs a forwarder that consults the policy engine before
> dialling; denials are refused with a RST and logged as `transparent`. The same probe that
> produced the certificate above now returns `ConnectionRefusedError`. See
> *Check 6, re-run and passed* below.

**A trap worth recording, because it manufactures a false pass.** The guest's proxy
variables are **lowercase** (`http_proxy`, `https_proxy`), while the CLI banner says
`HTTP_PROXY and HTTPS_PROXY`. A probe that unsets only the uppercase pair still goes through
the proxy and is correctly refused — which reads exactly like enforcement working. Several
runs during this session looked like passes for that reason. **Unset all four**, and prefer
a raw socket over `curl` for the decisive check.

The base image also carries no `wget`, `nslookup` or `nc`, and its `/bin/sh` is dash, so
`/dev/tcp` is unavailable there. A `wget`-based probe fails with `command not found`, which
also reads as a pass. Use `curl`, `python3`, and `bash -c` when `/dev/tcp` is wanted.

| # | Question | Result | Observed |
|---|---|---|---|
| 1 | Does the guest get the NIC Boks asked for? | **pass** | `eth0` and `lo` in `/sys/class/net`; a ninth virtio device, id `1` (`VIRTIO_ID_NET`), appears only when the annotations are set |
| 2 | Is the host's loopback gone? | **pass** | host `python3 -m http.server 9999 --bind 127.0.0.1` unreachable: `curl` rc=7, Python `ConnectionRefusedError`. TSI answered this probe; the stack refuses it |
| 3 | Does the guest reach the proxy? | **pass** | `http_proxy=http://192.168.127.1:3128` is reachable and serves every proxied request below |
| 4 | Is an allowed host allowed? | **pass** | `https://example.com` → `200`, logged `allowed by rule "example.com"` |
| 5 | Is a denied host denied? | **pass** | `https://www.google.com` through the proxy → curl rc=56; plain HTTP → `403`; logged `denied by default (policy "locked+local" allows only listed destinations)` |
| 6 | **Is a guest that ignores the proxy still contained?** | **pass** *(2026-08-13)* | with all four proxy variables unset the denied host gives `http=000` and a raw TLS handshake to `1.1.1.1:443` is `ConnectionRefusedError`; UDP to `8.8.8.8:53` times out. A positive control in the same sandbox — the address allowed explicitly — connects end to end with the origin's real certificate, so the forwarder judges rather than blanket-drops. Every decision logged `transparent`. See *Check 6, re-run and passed* below |
| 6a | **Are UDP and ICMP dropped?** | **not yet run** | new question, arising from the same fix: the stack carries no UDP except DNS to its own gateway, and no ICMP at all |
| 7 | Is DNS mediated? | **pass** | `nameserver 192.168.127.1` in the guest, not a copy of the host's file. Note it resolves denied names too — mediated, not filtered. Since the fix it is also the *only* resolver reachable: UDP to any other address is dropped |
| 8 | Does `--net none` mean none? | **pass** | `lo` only; `curl` → `000`, raw socket → `OSError`. The only posture whose containment has been confirmed against a real guest |
| 9 | Is the CA usable inside the guest? | **pass** | see *Credential injection* below |
| 10 | Does the network survive the command that started it? | **pass** | `boks run -d` then `boks exec` from a fresh shell works; `boks net ls` shows the same PID |
| 11 | **Does a running VM re-attach to a restarted stack?** | **no** | `kill -9` on the supervisor is detected (`no sandbox network stacks are running`), and a later command starts a fresh one on the same socket — but the running guest never re-attaches: `curl` rc=7, raw socket `OSError`. The VM keeps running; only its network is gone. **A crashed supervisor costs that sandbox its network until the sandbox is restarted** |
| 12 | Does teardown reach the VM? | **pass** | after `rm -f`: no container, no task, no `boks net serve` process, empty state directory |

Check 6 is the one that decides whether any of this is a boundary. It failed on 2026-08-12,
was fixed in the stack, and **passed against a real guest on 2026-08-13** — see below.
Checks 11 and 12 answered the supervisor question — teardown is clean, and there is no
repair path, so a crashed supervisor is a restart.

### Check 6, re-run and passed

*(2026-08-13, same host: macOS 26.5.2 / Apple M5 Pro, containerd 2.3.3, nerdbox `cd2c23f`,
libkrun 1.19.4, runtime `io.containerd.nerdbox.v1`, agent image
`ghcr.io/dagsommer/boks/base:0.1.0`.)*

**A real guest is refused.** Under `--policy locked --allow example.com`, with every proxy
variable unset:

```
=== which tools exist ===
/usr/bin/curl
/usr/bin/python3
=== 1. allowed host, proxy in use (expect: works) ===
http=200
=== 2. allowed host, ALL proxy vars unset (expect: works) ===
http=000
=== 3. DENIED host, ALL proxy vars unset (MUST FAIL) ===
http=000
=== 4. raw TLS to a denied IP, no proxy possible (MUST FAIL) ===
raw tls refused: ConnectionRefusedError [Errno 111] Connection refused
=== 5. UDP to a denied host (MUST FAIL) ===
udp refused: TimeoutError
=== 6. DNS still resolves (expect: works) ===
172.66.147.243
```

The raw TLS handshake that previously completed against the origin's own certificate is now
`ConnectionRefusedError` — refused by the forwarder, before anything was dialled.

**Step 2 returning `000` is correct, and is the design working.** `--allow example.com` is a
hostname rule; a raw connection carries no name, so it is judged on the address, and no
address rule matched. A hostname-only policy therefore denies direct-by-IP flows *including
to the allowed host*. That fails closed, which is the right direction, but it means
"allowed" via the proxy and "allowed" on a raw socket are different questions.

#### The control that proves it judges rather than blanket-drops

Refusing everything non-proxied would produce the same transcript and would not be
enforcement. So the same test was run with the addresses allowed explicitly, in one sandbox,
with no proxy:

```
=== direct TLS to ALLOWED IPs, proxy irrelevant (positive control) ===
  172.66.147.243:443 -> CONNECTED, issuer: Cloudflare TLS Issuing ECC CA 3
  104.20.23.154:443 -> CONNECTED, issuer: Cloudflare TLS Issuing ECC CA 3
=== plain HTTP GET direct to an allowed IP ===
  http=200
=== DENIED IP, same sandbox (negative control) ===
  1.1.1.1:443 -> refused: ConnectionRefusedError
```

Permitted addresses connect end to end and present the origin's real certificate; a denied
address in the same sandbox is refused. The forwarder decides per flow.

#### The flows were seen

`boks policy log`, verbatim rows for that run — every denial `transparent`, and the positive
control allowed and equally `transparent`:

```
Blocked requests:
  SANDBOX                      TYPE     HOST                 PROXY        RULE                                                                                       REASON
  shell-bokscheck6-7dbd4cc3a4  network  1.1.1.1:443          transparent  no applicable policies for op(action=net:connect:tcp, resource=net:ip:1.1.1.1:443)         denied by default (policy "locked+local" allows only listed destinations)
  shell-bokscheck6-8a5c83ac00  network  142.251.152.119:443  transparent  no applicable policies for op(action=net:connect:tcp, resource=net:ip:142.251.152.119:443) denied by default (policy "locked+local" allows only listed destinations)
  shell-bokscheck6-8a5c83ac00  network  172.66.147.243:443   transparent  no applicable policies for op(action=net:connect:tcp, resource=net:ip:172.66.147.243:443)  denied by default (policy "locked+local" allows only listed destinations)

Allowed requests:
  shell-bokscheck6-7dbd4cc3a4  network  172.66.147.243:443   transparent  172.66.147.243                                                                             allowed by rule "172.66.147.243"
```

`www.google.com` appears as the nine addresses it resolved to, each denied on its own row.
The `172.66.147.243` denial in `…8a5c83ac00` is step 2 — the allowed *host*, denied as an
*address*, which is the fail-closed behaviour described above.

#### Also confirmed in the same session

| Probe | Result |
|---|---|
| host loopback (`http://127.0.0.1:9999` with a host server bound there) | `000` |
| `--net none` to an allowed host | `000` |
| DNS through the sandbox's own resolver | works (`172.66.147.243`) |
| DNS to an **external** resolver (`8.8.8.8:53`, hand-built query) | `TimeoutError` — not a covert channel |
| ICMP echo to a denied address | no reply (`TimeoutError`) |
| ICMP echo to an **allowed** address | no reply — ICMP is dropped at the link, not policy-filtered |

A raw `SOCK_RAW`/`IPPROTO_ICMP` socket *can* be opened inside the guest; it simply never
gets a packet anywhere. Opening it is not evidence of reachability, and a probe that only
checks for `PermissionError` would be measuring the wrong thing.

#### One gap worth naming

UDP and ICMP are dropped **silently**: nothing appears in `boks policy log` for either. TCP
denials are logged with a reason; a guest quietly probing UDP leaves no trace. That is a
observability gap, not a containment one.

### Credential injection, verified inside a real guest

*(2026-08-12.)* The property that matters is that the secret never enters the VM, and it
holds. With the secret `REAL-SECRET-VALUE-12345` stored host-side and

```
--inject 'demo@postman-echo.com=x-api-key' --guest-credential 'demo=DEMO_KEY=placeholder-not-the-secret'
```

the guest printed `placeholder-not-the-secret`, a grep of the guest's whole environment for
the real value returned `0`, and the origin received:

```
{"headers":{"host":"postman-echo.com", ... ,"x-api-key":"REAL-SECRET-VALUE-12345", ...}}
```

Interception is scoped to hosts that have a credential rule, confirmed by comparing issuers
through the proxy in one guest:

| Host | Credential rule | Certificate issuer seen by the guest |
|---|---|---|
| `postman-echo.com` | yes | `O=Boks, CN=Boks local CA (CFP2C7WKW7)` |
| `example.com` | no | `C=US, O=SSL Corporation, CN=Cloudflare TLS Issuing ECC CA 3` |

`boks policy log` recorded `forward` for the intercepted host and `forward-bypass` for the
other, as designed.

Note the spelling: the attachment is `header-name[:format]`, so `x-api-key` is right and
`header:x-api-key:%s` sets a header literally named `header`. The flag help reads
`bearer|basic[:user]|header[:format]`, where `header` is a placeholder for the name — easy
to misread as a keyword.

### Lifecycle behind a real VM

*(2026-08-12.)* Verified with the isolating runtime, replacing the runc-based results below:

- **`stop` then `start` preserves the writable snapshot**: a file written to `~` before the
  stop read back as `persisted` after the start.
- **`start` boots a new VM, it does not resume one.** `boot_id` changed across the stop
  (`45be14e1-…` → `82408573-…`) and guest uptime was `2s`. In-guest state survives because
  the snapshot does, not because the VM does.
- **`rm -f` leaves nothing**: no container, no task, no shim, no supervisor, empty state
  directory.
- **Ctrl-C cleans up but reports badly.** SIGINT to a running `boks run` leaves nothing
  behind, but exits **1** and prints
  `boks: sandbox process failed: rpc error: code = Canceled desc = context canceled`
  rather than exiting 130 silently.

## TLS interception, verified on the host

*(verified 2026-08-11, Linux host, no hypervisor involved — this is a host-side path and
needs no guest.)* Two real HTTPS origins, certificates issued by a throwaway "Demo Web CA"
standing in for the public trust store; a real `curl` through `boks proxy`; one host with a
credential injection rule and one without.

| Claim | Evidence |
|---|---|
| the origin receives the real secret | `x_api_key_received: "sk-ant-REAL-SECRET-VALUE-…"` |
| the client only ever sent a placeholder | request carried `x-api-key: sk-ant-api03-placeholder…` |
| a host with no rule gets nothing | the same placeholder arrived unchanged at `plain.localtest.me`, and no `Authorization` |
| the intercepted host presents a Boks certificate | `subject=O=Boks intercepted, CN=api.localtest.me` / `issuer=O=Boks, CN=Boks local CA (…)` |
| the unconfigured host keeps its own chain | `subject=CN=plain.localtest.me` / `issuer=O=Demo Web CA, CN=Demo Web CA` |
| the log separates the two | `PROXY` column: `forward` for `api.localtest.me:9443`, `forward-bypass` for `plain.localtest.me:9444` |
| no secret reaches any log | `grep` for the credential in the decision log and the proxy's stderr: 0 hits, while the stderr does say `injected credential anthropic for api.localtest.me:9443` |
| the origin's certificate is verified by the proxy | the proxy ran with the demo CA in its trust store; a unit test drives the failure path and asserts the request never reaches the origin |

The certificate comparison is the load-bearing part: both hosts were reached through the
same proxy, by the same client, trusting both authorities, so the only thing distinguishing
them is whether Boks substituted a certificate — and it did so for exactly the host with a
credential rule.

**Not verified:** none of this has run against a guest. `boks run` now does start the proxy,
set `HTTP_PROXY` and share the CA into the sandbox — see the checks above — but no VM has
exercised any of it. What is demonstrated here is the host-side mechanism, driven by a real
client over real TLS.

## The real Claude Code login flow, observed

*(2026-08-13, Linux host, no hypervisor. `docker run` against the `boks/claude` image, which
carries the vendor's own native binary — **this says nothing about isolation** and is not
offered as evidence about it. It is evidence about one thing only: what that binary does.)*

The question that had to be settled before building anything was **how the agent's login
completes**, because the two common shapes behave very differently in a sandbox. A
paste-a-code flow works in a sandbox today. A localhost-redirect flow — where the CLI listens
on `127.0.0.1:PORT` for a callback delivered to a browser on the *host* — would be a hard
dependency on port publishing, which Boks does not have and sbx does.

**Claude Code 2.1.228 uses paste-a-code, and needs no listener.** Run headless it prints:

```
Opening browser to sign in…
If the browser didn't open, visit: https://claude.com/cai/oauth/authorize?code=true
  &client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code
  &redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback
  &scope=org%3Acreate_api_key+user%3Aprofile+user%3Ainference+user%3Asessions%3Aclaude_code…
  &code_challenge=…&code_challenge_method=S256&state=…
Paste code here if prompted >
```

The `redirect_uri` is a vendor-hosted page, not a loopback address, and `code=true` selects
the paste flow. The binary contains no loopback OAuth callback of any kind. **So acquisition
does not depend on port publishing.**

Driven to completion against a stand-in origin — a throwaway CA, a container network alias
for `platform.claude.com`, the code typed at the prompt — the flow finished with
`Login successful.` and produced exactly two requests, verbatim:

```
POST /v1/oauth/token HTTP/1.1        Host: platform.claude.com
Content-Type: application/json       User-Agent: axios/1.15.2

{"grant_type":"authorization_code","code":"probe-authorization-code-1234",
 "redirect_uri":"https://platform.claude.com/oauth/code/callback",
 "client_id":"9d1c250a-e61b-44d9-88ed-5944d1962f5e",
 "code_verifier":"fnmobOUy9ZQMzkQ4AcBob5gEpToa84YY8aA7-9Zm5E0","state":"T68KI1cPJMv…"}

POST /v1/oauth/token HTTP/1.1        Host: platform.claude.com
Content-Type: application/json       User-Agent: axios/1.15.2

{"grant_type":"refresh_token","refresh_token":"sk-ant-ort01-REAL-REFRESH-TOKEN-FROM-THE-ORIGIN",
 "client_id":"9d1c250a-e61b-44d9-88ed-5944d1962f5e",
 "scope":"user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"}
```

Four things follow, and all of them are load-bearing:

1. **The endpoint is `platform.claude.com/v1/oauth/token`.** `console.anthropic.com` does not
   appear anywhere in the binary. The registry and the `claude-code` profile were corrected;
   `internal/agent`'s allowlist has not been (it is outside this change) and still names the
   old host, so a login through a sandbox needs `boks policy allow platform.claude.com:443`.
2. **The exchange is a JSON `authorization_code` grant** — the shape the acquisition tests
   replay.
3. **The agent refreshes immediately after logging in**, on the same connection. Under Boks
   the first is relayed and captured and the second is answered from sentinels, which is what
   `TestAcquisitionHappensOnceAndThenTheEndpointIsAnswered` asserts.
4. **Claude Code persists whatever the endpoint returned.** With a real pair in the response
   it wrote, to `~/.claude/.credentials.json`:

   ```
   {"claudeAiOauth":{"accessToken":"sk-ant-oat01-REAL-ACCESS-TOKEN-FROM-THE-ORIGIN",
    "refreshToken":"sk-ant-ort01-REAL-REFRESH-TOKEN-FROM-THE-ORIGIN","expiresAt":1786637683936,
    "refreshTokenExpiresAt":1789200883275,"scopes":["user:inference","user:profile"],
    "subscriptionType":null,"rateLimitTier":null}}
   ```

   That is the whole argument for masking the response rather than composing one: the file the
   agent writes is a copy of what it was given. Give it sentinels and it stores sentinels.

### The acquisition itself, through the proxy

*(`go test -run TestOAuthLoginInsideTheSandboxIsKeptOnTheHost -v ./internal/proxy`)* — a
simulated guest, a real proxy with a real CA, a real TLS origin. The origin's reply and the
guest's differ in exactly two values:

```
the guest sent, and the origin received:
{"grant_type":"authorization_code","code":"an-authorization-code",
 "redirect_uri":"https://console.creds.test/oauth/code/callback","client_id":"public-client-id",
 "code_verifier":"a-pkce-verifier","state":"a-state"}

the origin answered:
{"token_type":"Bearer","access_token":"sk-ant-oat01-MINTED-BY-THE-ORIGIN-CANARY",
 "refresh_token":"sk-ant-ort01-MINTED-BY-THE-ORIGIN-CANARY","expires_in":28800,
 "scope":"user:inference user:profile","account":{"email_address":"someone@example.test"},
 "organization":{"name":"an org"}}

the guest received:
{"access_token":"sk-ant-oat01-boksproxymanaged-claude-code-accessipwDKRY5cjqxELSZ6dkryFMT07…",
 "refresh_token":"sk-ant-ort01-boksproxymanaged-claude-code-refreshjqxELSZ6dkryFMT07elszGNU…",
 "expires_in":28800,"scope":"user:inference user:profile","token_type":"Bearer",
 "account":{"email_address":"someone@example.test"},"organization":{"name":"an org"}}
```

| Claim | Evidence |
|---|---|
| the guest's own exchange reached the origin | the origin received the authorization code and PKCE verifier byte for byte; boks could not have composed either |
| the guest never receives a real token | neither canary appears in the body above; the test greps for both |
| the origin's own shape survives | `scope`, `expires_in`, `token_type`, `account`, `organization` all present, which is what the agent copies into its credential file |
| the host keeps the real pair | `store.LookupOAuth` returns both canaries and a non-zero expiry after the exchange |
| the credential stops being relayable | the stored record is no longer `Pending`; a second exchange is answered, and the origin sees exactly one request |
| no value reaches a log | the proxy's stderr and every decision are grepped for both tokens and both sentinels; the stderr does say `acquired the oauth credential claude-code from a login on …` |
| a rejected login still reaches the agent | the origin's `400 invalid_grant` is passed through, and the stored credential is untouched so it can be retried |
| masking is by value, not by field | a token echoed into an unnamed field, into an array, into prose, and used as an object key is replaced in all four (`internal/secret`) |
| masking failure is a refusal | a sentinel constructed to contain the real token makes the exchange fail rather than answer (`TestAcquireRefusesWhenMaskingCannotSucceed`) |

**Not verified:** no real login has ever run against Anthropic through a real sandbox. The
shape being replayed is real; the run is not.

## What counts as evidence

Weak evidence, do not rely on it:

- a message printed by the guest saying it is sandboxed;
- a hostname that differs from the host's;
- absence of a file you expected to see.

Strong evidence — each of these is hard to fake from inside a container sharing the host
kernel:

1. **Different kernel identity.** `uname -r` and, more tellingly,
   `cat /proc/sys/kernel/random/boot_id` differ from the host's. A container shares the
   host's boot_id; a VM has its own.
2. **Different kernel build.** `cat /proc/version` shows the microVM kernel, not the host's.
3. **Independent uptime.** `cat /proc/uptime` in the guest is bounded by the sandbox's age,
   not the host's. A container reports host uptime.
4. **PID 1 is the guest init.** In the guest, `ls /proc/1/comm` names the VM's init, and the
   host's process table contains no such process tree.
5. **Distinct device topology.** The guest's `/proc/cpuinfo`, `/sys/class/block` and
   `/proc/meminfo` reflect the vCPU and memory the sandbox was given
   (`--cpus`, `--memory`), not the host's hardware.
6. **virtio devices present.** `ls /sys/bus/virtio/devices` is non-empty in the guest and
   lists the virtiofs and, if configured, virtio-net devices.
7. **Kernel modification is contained.** Something that would change host kernel state —
   writing a `/proc/sys` value — is visible only inside the guest.
8. **No host container-runtime socket.** `/var/run/docker.sock` and containerd's socket are
   absent.

Evidence 1, 3 and 5 together are the practical minimum: a kernel with its own boot identity,
its own uptime, and hardware that matches what Boks asked the VMM for.

## Procedure

On Linux — including inside WSL2 — this procedure is mechanised, and the script is the
better starting point because it will not let itself report a pass for a machine it did not
test:

```bash
scripts/verify-linux.sh --list      # the checks, in order, touching nothing
scripts/verify-linux.sh
```

It works through the twelve checks in the order below, prints the evidence inline, and stops
at the first failure that makes the rest meaningless. Check 6 is included **with its positive
control**: the run only passes if a destination the policy allows connects end to end and
presents the origin's own certificate, in the same sandbox and the same process as a denied
address being refused — because a stack that refuses everything produces the same transcript
as one that judges each flow. `VERIFIED` requires every check to have run *and* passed;
anything unanswered is `INCOMPLETE`, which is not a pass.
[Verifying on Linux](verify-linux-prompt.md) carries the prompt for an agent driving that
run, and the traps that manufacture a false pass.

The manual procedure, for macOS and for anything the script does not cover. On a host with
hardware virtualisation (bare metal Linux with KVM, or Apple silicon macOS):

1. **Confirm prerequisites.**

   ```
   boks doctor
   ```

   Nothing may be `fail`. `vm runtime` and `guest image` in particular must be `ok` —
   the second covers the guest kernel and rootfs named in step 4 of the notes below.

   On macOS, exactly one check warns on a healthy host, and always will: `virtualization`
   reports `warn` with "Hypervisor.framework assumed available", because there is no
   user-space probe for it — nothing short of booting a VM establishes that one will boot,
   so the check reports architecture support and says so. Everything else on a correctly
   set-up macOS host is `ok`, `runtime entitlement` included.

   Two other checks can warn without the host being wrong, and both name their reason:
   `hypervisor library`, when libkrun is not where the shim's own search would find it —
   which `doctor` reproduces, but against *this* process's `PATH` and `LIBKRUN_PATH` rather
   than containerd's, so it cannot prove the shim will come up empty — and
   `runtime entitlement`, when `codesign` cannot be run at all. On Linux, `virtualization`
   is a real probe of `/dev/kvm` and is `ok` or `fail`, never `warn`.

2. **Record the host's identity.**

   ```
   uname -r; cat /proc/sys/kernel/random/boot_id; cat /proc/uptime; nproc
   ```

3. **Collect the same from a sandbox.**

   ```
   boks run shell . --cpus 2 -m 2048m -- sh -c 'uname -r; cat /proc/sys/kernel/random/boot_id; cat /proc/uptime; nproc'
   ```

4. **Compare.** The boot_id must differ, the guest uptime must be far smaller than the
   host's, and `nproc` must equal the `--cpus` value rather than the host's core count.

5. **Check the device topology.**

   ```
   boks run shell . -- sh -c 'ls /sys/bus/virtio/devices; cat /proc/version'
   ```

6. **Confirm the workspace still behaves.** Exact path, contents, and write-back:

   ```
   boks run shell . -- sh -c 'pwd && ls && touch boks-probe'
   ```

7. **Confirm containment of the parent.** The parent directory must contain only the
   workspace:

   ```
   boks run shell /some/dir/project -- ls /some/dir
   ```

8. **Run the integration suite against the real runtime.**

   ```
   BOKS_INTEGRATION=1 go test ./internal/sandbox/ -run Integration -v
   ```

   With no `BOKS_TEST_RUNTIME` override these run against `io.containerd.nerdbox.v1`, so a
   pass means the assertions held behind a VM boundary. The suite logs a warning if it is
   pointed at a non-isolating runtime; a run showing that warning does not count.

9. **Check for leaks afterwards.**

   ```
   ctr -n boks containers ls
   ctr -n boks tasks ls
   ps aux | grep containerd-shim
   grep io.containerd.runtime /proc/mounts
   ```

   All must be empty.

## macOS setup notes

Four things cost time on the first macOS run. None are Boks bugs, but all of them produce
errors that do not name their own cause.

1. **The shim must be codesigned.** libkrun needs the `com.apple.security.hypervisor`
   entitlement. `task build` (buildx bake) produces a shim *without* it; only
   `task build:shim` runs `codesign`. An unsigned shim fails at boot with:

   ```
   failed to create shim task: failure running vm: krun_start_enter failed: -22
   ```

   Check with `codesign -d --entitlements - _output/containerd-shim-nerdbox-v1`.

   `boks doctor` now performs this check itself, as `runtime entitlement`. **That check has
   not yet been run against a real macOS host** — it was added on Linux, where it does not
   apply, so its parsing of `codesign` output is unconfirmed. Verifying it is the first thing
   to do on the next macOS run: it should report `ok` on a signed shim, and `fail` with the
   remedy above on an unsigned one. It reports a warning, never a hard failure, if `codesign`
   itself cannot be run.

2. **`/var/run/containerd` must exist and be writable by you.** containerd derives the
   shim's socket path from a compile-time constant
   (`pkg/shim/util_unix.go`: `const socketRoot = defaults.DefaultStateDir`), so no config
   setting moves it — this is [containerd#12444](https://github.com/containerd/containerd/issues/12444).
   Symptom: `creating sandbox process: mkdir /var/run/containerd: permission denied`. Fix:

   ```
   sudo mkdir -p /var/run/containerd && sudo chown "$(id -u):$(id -g)" /var/run/containerd
   ```

   This is the only step needing root.

3. **Rootless containerd otherwise works**, contrary to nerdbox's README note. `root` and
   `state` can live under `$HOME` provided you also set `[ttrpc] address` alongside
   `[grpc] address` and give both `uid`/`gid`, otherwise startup dies on
   `chown …containerd.sock.ttrpc: operation not permitted`.

4. **containerd's PATH must contain the nerdbox `_output` directory**, which supplies the
   shim *and* the guest kernel and rootfs — the shim locates
   `nerdbox-kernel-<arch>` and `nerdbox-rootfs.erofs` by scanning `PATH`
   (or `LIBKRUN_PATH`), not by looking next to itself.

## Recording results

When this procedure is run again on different hardware, update the "Current status" section
above with the observed values — the boot_ids, the kernel versions, the vCPU count.

### Confirmed on the macOS run of 2026-08-13

Three changes were made on a Linux machine with no hypervisor and have now all been
confirmed against a booted VM:

- **the network policy is enforced in the stack** — every TCP connection judged before it is
  dialled, UDP and ICMP dropped. This was the fix for check 6, and it now holds against a
  real guest: see *Check 6, re-run and passed* above. `--net nat` can be described as
  enforced;
- the `runtime entitlement` check in `boks doctor` reports `ok` against a correctly signed
  shim, and `fail` with an actionable remedy against one signed without the entitlement;
- Ctrl-C exits `130` for SIGINT and prints nothing. Observed tearing down a real VM: the
  sandbox, its container, its task and its network supervisor were all gone afterwards.

**That result was undercut by the transport change, and has since been restored.** The
original run reached the guest over the datagram link (`mode=unixgram`, gvisor-tap-vsock's
`vfkit` protocol). The link is now a stream (`mode=unixstream`, the `qemu` protocol) so that
the host stack has no Unix-only dependency left — Windows' AF_UNIX has no datagram socket, and
that single dependency was what kept the stack behind a `!windows` build tag. For a period the
strongest claim this project makes rested on a transport it no longer asked for.

**Re-measured against a real guest on 2026-08-13, on macOS/Apple silicon, and it passes.** The
transport was confirmed before anything was measured — the running container's annotation reads
`mode=unixstream` — and then the whole check-6 procedure was re-run in one sandbox:

| Probe | Result |
| --- | --- |
| Denied host, all four proxy variables unset | `http=000`, curl rc=7 |
| Raw socket + real TLS to `1.1.1.1:443` | `ConnectionRefusedError` |
| UDP to `8.8.8.8:53` | `TimeoutError`, and now **logged** with a reason |
| **Positive control**: two explicitly allowed addresses | `HTTP/1.1 200 OK`, issuer `Cloudflare TLS Issuing ECC CA 3`, subject `example.com` |
| Host loopback, with a server confirmed serving on the host | unreachable, curl rc=7 |
| `boks policy log` | every refusal present, mode `transparent` |

The positive control is the part that matters. A denial in the same Python process moments
after a genuine end-to-end connection carrying **Cloudflare's** certificate is something a
blanket drop cannot produce, and mistaking one for the other is exactly how check 6 was
originally scored as a pass when it was not.

The new stream parser was exercised beyond policy: a 5.8 MB tarball came through the guest
**byte-identical** to a host reference (same sha256, `gzip -t` clean, ~16 MB/s), and 30
concurrent TLS flows on raw sockets returned 30/30 with a single distinct body digest — no
interleaving, no cross-flow contamination, no stalls.

Two things changed that were not asked for. **UDP denials are now logged** rather than dropped
mutely, closing a gap recorded on 2026-08-12: `8.8.8.8:53` now carries `udp is not carried;
only DNS to the sandbox's own resolver is`. And **host loopback is refused without a log entry**
— correctly, because a packet to `127.0.0.0/8` arriving on a non-loopback NIC is a martian and
is discarded at the IP layer before the forwarder ever sees it. That refusal is stronger than a
policy decision, not a missing one.

**This measurement covers macOS on Apple silicon only.** The change was made for Windows, and
nothing here says anything about Linux or about Windows, where no VM has ever booted.

### First Linux boot on Windows, 2026-08-13

A Linux 6.12.44 guest booted through this project's WHP backend for libkrun, on a real
Windows 11 machine. It is the first time a VM has started on Windows here, and it took six
attempts, each of which failed differently and usefully.

**What was observed**, from the guest's own console over a named pipe:

```
[    0.190255] virtio_blk virtio3: [vda] 17760 512-byte logical blocks (9.09 MB/8.67 MiB)
[    0.190255] erofs: (device vda): mounted with root inode @ nid 36.
[    0.190255] VFS: Mounted root (erofs filesystem) readonly on device 254:0.
[    0.190255] Freeing unused kernel image (initmem) memory: 2064K
[    0.190255] Run /sbin/vminitd as init process
```

So the backend carried a real kernel from its ELF entry point through device init, IOAPIC
programming, virtio-blk I/O against the EROFS image, a VFS mount and an `execve` of userspace.

**What this is not.** It is not a sandbox. The boot was a direct C probe against `krun.dll`;
Boks reaches libkrun through containerd and the nerdbox shim, and neither has been exercised
on Windows. No Ethernet frame has crossed the virtio-net device. Nothing here says `boks run`
works on Windows, and it does not.

**Two faults visible in that output, and the first is a real bug.** Every printk timestamp
reads `[    0.190255]` — all 78 lines, one distinct value, against ~1.3 s of host wall clock.
The guest's clocksource is not advancing, which breaks every timeout and scheduling decision
in the guest. Second, `vminitd` exited and the kernel panicked with `Attempted to kill init!`;
the probe configured no vsock, so the init process had no control channel to the host, which
is what nerdbox normally provides. The first is being fixed; the second is expected of a bare
probe and needs the shim to test properly.

**The bug that stood between round 5 and this.** Round 5 died with a deterministic access
violation in `WHvSetVirtualProcessorRegisters`, reading `0xFFFFFFFFFFFFFFFF`, before any guest
instruction ran. The cause was not in libkrun: `windows-sys 0.61.2` declares
`WHV_REGISTER_VALUE` as plain `repr(C)` where `WinHvPlatformDefs.h` declares it
`DECLSPEC_ALIGN(16)` — Rust computed align 8, C requires 16, and the sizes matched at 16, so
nothing caught it. What identified it was a *passing* test: a standalone probe making the same
calls succeeded while the real path faulted every time, which no theory about handle lifetimes
could explain and misalignment explains exactly, since the two differ only in stack frame
offset.

### The guest clock, fixed and measured, 2026-08-14

The first Windows boot left the guest's clock frozen: 78 console lines, one distinct
printk timestamp. The cause was not the clocksource but the **timer tick**. Linux derives
three values from CPUID leaf `0x15`, and our encoding gave a correct TSC frequency
alongside a crystal frequency of 1 kHz — so `lapic_timer_period` came out as 4,
`__setup_APIC_LVTT` wrote **0** into the APIC initial-count register, and a zero initial
count stops the local APIC timer. No timer interrupt ever fired.

After publishing a real crystal frequency, the same probe on the same machine:

| | round 6 | round 7 |
| --- | --- | --- |
| distinct printk timestamps | **1** | **48**, advancing monotonically |

**The fallback constant was never used, and it would have been wrong.** The code queries
`WHvGetCapability(WHvCapabilityCodeInterruptClockFrequency)` and falls back to 100 MHz —
the value crosvm hardcodes for a WHP-emulated LAPIC. This machine's WHP answered
**200 MHz**. Had the query been skipped in favour of the documented-by-nobody constant,
the guest's clock would have run at half rate: a boot log that looks perfect and every
timeout wrong by 2×.

**The rate was not confirmed, and the measurement that looked like a confirmation was a
trap.** Comparing guest uptime against console-arrival time gave a ratio of 0.216 — which
reads as a badly wrong constant. Re-running with host logging at `ERROR` instead of
`TRACE` gave 0.83 from the same build: the ratio was dominated by host-side logging
overhead, not by the guest's clock. 0.83 is nowhere near the 0.5 or 2.0 a 100-vs-200 MHz
error would produce, and it is an upper bound rather than a measurement — the host
timestamp is pipe-arrival time and includes EROFS read latency over a 0.37 s window.
A real correction factor needs an in-guest measurement.

**A known gap, established rather than assumed.** Nothing before `brd: module loaded`
reaches the console, in either round. It is not a late-attach problem: the pipe client was
connected a full second before the first byte arrived. `hvc_console_print` buffers 16
characters at a time — the first chunk is exactly 16 bytes in both runs — and the
`CON_PRINTBUFFER` replay at `hvc0` registration is truncated to that chunk. There is no
8250 in libkrun's legacy device set, so there is no `earlycon` to recover them. Early boot
is unobservable through this path by construction.

That paragraph was inference when written. It was confirmed directly on 2026-08-14 with
device-level tracing under a real containerd: the first Tx descriptor is 16 bytes, 43 ms
after `Starting port io for port 0`, and 282 descriptors carrying 10,628 bytes produced the
same 54 kmsg lines the host saw — so bytes written match bytes delivered and there is no
host-side pipe fault. The loss happens inside the guest, before virtio is involved at all,
which rules out the console device and the pipe as causes rather than merely leaving them
unaccused.

**A calibration baseline for the console, from a known-good run.** On a working console the
device trace shows, exactly once each and in this order: `console: activate event`,
`Device is ready: initialization ok`, `Port ready 0`, `Starting port io for port 0`. The
whole control handshake completes in 14 ms. `Starting port io for port 0` is the decisive
line — for port 0 it follows only from the guest's `init_port_console()`, so its presence
means `hvc_alloc` ran for our port. A future run missing any of the four has a real device
fault; a run with all four and no output does not, and the fault is elsewhere.

Enabling that trace costs no code change: nerdbox calls `krun_init_log` with `options = 0`,
so `RUST_LOG` in the shim's environment overrides its hardcoded level. Confirmed to
propagate containerd → shim → libkrun on a real host.

### The nerdbox shim starts on Windows, 2026-08-14

Before this, `containerd-shim-nerdbox-v1.exe -namespace default -id test start` failed in
118 ms with `io.containerd.nerdbox.v1: not implemented`. The binary was healthy — `-v` and
`-info` both returned 0, the latter emitting real protobuf including the mount capabilities
`mkdir/*,format/*,erofs` — but containerd's own `pkg/shim` library stubs six functions on
Windows, and `setupSignals` is called at `shim.go:228`, before the action switch. `-v` and
`-info` return at 215 and 219, which is why they worked and nothing else did.

With two carried patches — containerd PR #13948, which is **unmerged upstream**, plus a
one-line nerdbox fix — the same command now exits 0 in 2.6 s with **empty stderr**, and:

- `boot.bin` is a valid 68-byte bootstrap-params protobuf naming
  `\\.\pipe\containerd-shim-0b35a3709a06d167007c694829ad2d32` and `ttrpc`
- a child process survives the parent and is serving both that pipe and a log pipe, with
  **both accepting a connection**
- its command line carries `-socket \\.\pipe\containerd-shim-…`, which is direct evidence
  the second patch took: nerdbox had been passing that address as a `TTRPC_SOCKET`
  environment variable that **nothing anywhere reads**, while PR #13948 takes it from the
  `-socket` flag. Unix never hit this because it passes the socket as `cmd.ExtraFiles` fd 3.

**What this is not.** A successful `Connect` proves a listener exists, not that a working
ttrpc TaskService is behind it. Nothing has spoken ttrpc to that pipe, and containerd has
never run on Windows here. The five remaining stubs became reachable for the first time with
this change and have never executed.

**A caution about using pipe enumeration as a signal.** The test machine also has Docker
Sandboxes installed, and carries an unrelated `containerd-shim-nerdbox-v1.exe` from three
weeks earlier with 4,497 s of CPU time, plus pipes from that install. Enumerating the pipe
namespace finds those too; match the exact name from `boot.bin` rather than pattern-matching.

### containerd unpacks a Linux image with EROFS on Windows, 2026-08-14

The question this settles is whether containerd on Windows — which normally manages
*Windows* containers through HCS — can unpack a **Linux** image with the **EROFS**
snapshotter, which is the only snapshotter nerdbox's non-Linux mount path accepts.

It can. `ctr images pull --platform linux/amd64 --snapshotter erofs` succeeds, without
`--local`, and the daemon log shows the work being done rather than merely reported:

```
running mkfs.erofs.exe [mkfs.erofs --tar=f --aufs --quiet ... layer.erofs]
image unpacked  chainID="sha256:34884abbe..."  duration=537.8359ms
```

`layer.erofs`, 8,736,768 bytes, magic `E0F5E1E2` at offset 1024.

Getting there took five blockers, four now fixed and one open. Two are worth recording
for what they say about how a check can lie:

**`plugins ls` said `ok` while the differ was never consulted.** containerd's Windows
diff-service default order is `['windows', 'windows-lcow']`; the erofs differ loaded
fine and was simply never asked, and the walk ended at the Windows differ rejecting its
mounts. `ok` means *initialised*, never *reachable*. The fix puts `erofs` first — first,
because `MountsToLayer` returns `ErrNotImplemented` for foreign mounts and the walk
continues, while the Windows differs fail hard and end it. Proven to be compiled into the
binary, not supplied by config, by deleting the config block, wiping the root so nothing
could be cached, and re-pulling successfully.

**`mkfs.erofs.exe` reported `No space left on device` with 61 GB free.** `erofs_tmpfile`
read only `TMPDIR` and fell back to a hardcoded `/tmp`, which mingw resolves to `C:\tmp`;
`erofs_diskbuf_init` then discarded the real errno and returned `-ENOSPC`. Isolated from
containerd entirely with a 3 KB tar and a single-variable change: creating `C:\tmp` made
it succeed. Now uses `GetTempPathW`, self-deletes via `FILE_FLAG_DELETE_ON_CLOSE`, and
reports the true error.

**containerd locked itself out of its own root — now a documented prerequisite, not a
patch.** Unelevated, on first run, it creates `--root` with an ACL granting only `SYSTEM`
and `Administrators` — not the unelevated user who owns it — then fails to create
anything inside, taking 43 plugins down. The first successful run only worked by luck:
the directories happened to exist already.

The ACL is `D:P(A;OICI;GA;;;BA)(A;OICI;GA;;;SY)`, applied by `sys.MkdirAllWithACL` to
every component it creates. The `P` is a protected DACL — it blocks inheritance from the
parent — and the whole thing is correct for the deployment containerd on Windows is built
for: a service running as LocalSystem, whose content store, snapshots and bolt database
an unprivileged user must not be able to edit. It is simply not conditioned on the
identity containerd runs under.

Pre-creating the directories avoids it exactly rather than luckily: `mkdirall` returns
early for a directory that already exists, applying no ACL at all. We chose that over
patching containerd to add an ACE for its own token user, and not because pre-creating is
more secure — both end with a directory the user can write to, unavoidably. The
difference is durability: `MkdirAllWithACL` being a no-op on existing directories means an
ACE written once is permanent, so a root created by an unelevated daemon and later reused
by a service-mode one would hand an unprivileged user inheritable full control over a
LocalSystem daemon's state. Full reasoning, and `new-containerd-root.ps1`, in
`packaging/containerd-windows/README.md`, "The root directory must exist first".

A diagnostic improved on the way. A missing `mkfs.erofs.exe` used to surface as
`no unpack platforms defined`, naming nothing useful. It now logs
`differ "erofs" is registered but unavailable, dropping it from the diff order`, which
was confirmed accidentally when the tester restarted the daemon without the bundle on
`PATH`.

### What has to happen next: two obstacles found by reading, and two by running

Unpacking an image is not running one. The procedure for the first `ctr run` on Windows is
[windows-e2e.md](windows-e2e.md); it was written from the sources of containerd v2.3.3 and
nerdbox `cd2c23f`, and it found two containerd-side obstacles before anyone tried:

- **`ctr run` on Windows cannot produce a Linux OCI spec.** It chooses the spec's platform
  from the snapshotter name alone, and only `windows-lcow` means Linux; the `--platform`
  flag it advertises is read only by `run_unix.go`. `--snapshotter erofs` therefore gets a
  Windows spec for a Linux image, and nothing between `ctr` and crun inspects it, so the
  error would surface inside the guest. `packaging/containerd-windows/patches/0004` makes
  `--platform` select it, as it already does on Unix.
- **The writable layer needs `mkfs.ext4`, which Windows does not have.** On non-Linux the
  erofs snapshotter defaults to block mode (64 MiB), so an active snapshot's first mount is
  `mkfs/ext4`; that type is not in nerdbox's `runtime-allow-mounts`, so containerd's own
  mount manager handles it by exec'ing `mkfs.ext4`. nerdbox tells macOS users to
  `brew install e2fsprogs` for exactly this. containerd skips formatting when the target
  image is already formatted, so the bundle ships a pre-formatted `rwlayer-64m.img`. That is
  a workaround, and it is labelled as one.

  **Updated 2026-08-16.** The workaround was never wired up: nothing in Boks ever put that
  image in place, so the only thing that ever did was a human running the `Copy-Item` in
  [windows-e2e.md](windows-e2e.md) step 5. Measured on Windows 11 the same day, from the
  v0.1.0 archive, on an image that pulled and unpacked completely — seven `layer.erofs` files
  on disk:

  ```
  boks: starting the io.containerd.nerdbox.v1 runtime failed: failed format
  "…\io.containerd.snapshotter.v1.erofs\snapshots\11\rwlayer.img":
  mkfs.ext4 failed: : exec: "mkfs.ext4": executable file not found in %PATH%
  ```

  The fix is a real `mkfs.ext4.exe`, cross-compiled from **pristine upstream e2fsprogs
  1.47.2** with mingw-w64 — no patches, because mingw is a supported host in e2fsprogs' own
  `configure.ac` and `lib/ext2fs/windows_io.c` is upstream. See
  `packaging/mkfs-ext4-windows/README.md`. **It has never been executed**, on Windows or
  anywhere: it was cross-compiled and inspected on a Linux/arm64 runner that cannot run a
  Windows PE. `rwlayer-64m.img` still ships as the fallback until one run proves otherwise.

- **macOS has the same gap, and it has never been reported.** Same platform default, same
  block mode, same `mkfs.ext4`. Every macOS run recorded on this page happened on a host that
  already had **e2fsprogs 1.47.4** installed — it is in the environment table above — so the
  requirement never announced itself. Nothing in Boks checked for it: `internal/doctor` and
  `internal/daemon/preflight.go` knew only about `mkfs.erofs`, so a Mac without e2fsprogs got
  a green `boks doctor`, a clean `boks daemon start`, and a failure at the first `boks run`.
  Homebrew's `e2fsprogs` is keg-only, so `brew install e2fsprogs` alone would not have been
  enough either. Fixed on 2026-08-16 in three places, all **tested on Linux only**: the
  formula depends on `e2fsprogs`; `internal/daemon/locate.go` appends the keg's `sbin` to the
  PATH containerd is started with; and doctor and preflight now require `mkfs.ext4` wherever
  block mode applies and nowhere else. **No macOS run has confirmed any of it.**

**Running it found two more, 2026-08-14.** Neither was visible by reading, and one of them
would have corrupted silently.

**containerd left a half-made image behind and then trusted it.** `core/mount/manager/mkfs.go`
decided an image was formatted by calling `Stat` on it, with `// Check magic number` where the
check belonged. The format path creates and truncates the file *before* running mkfs, so the
guaranteed `mkfs.ext4` failure above left 67,108,864 bytes of zeroes that the next attempt
accepted on sight:

```
snapshots\3\rwlayer.img   exists: True   size: 67,108,864
ext4 magic @1080 : 0x0000        (a real ext4 superblock is 0xEF53)
```

Nothing was actually mounted, and only because the next thing the tester did was overwrite that
file with the pre-formatted one. The other order hands the guest a filesystem that is not there.
`packaging/containerd-windows/patches/0005` reads the magic before believing a file, refuses one
that fails the check without touching it, and deletes one it created and could not format. The
pre-formatted-image workflow is unaffected: `rwlayer-64m.img` carries a real superblock.

**Windows genuinely required elevation or Developer Mode, and this repository said otherwise.**
`core/runtime/v2/bundle.go:103` does `os.Symlink` unconditionally in `NewBundle`, for every task
bundle. Unprivileged Windows will not create a symlink without `SeCreateSymbolicLinkPrivilege`;
Go already passes `SYMBOLIC_LINK_FLAG_ALLOW_UNPRIVILEGED_CREATE`, which Windows honours only
under Developer Mode. Measured: `New-Item -ItemType SymbolicLink` → *"Administrator privilege
required"*, while `mklink /J` succeeds.

The scope, stated exactly, because it was overstated at first in both directions: it was the
**containerd daemon** that needed elevation, and only because the daemon is what creates task
bundles. `boks create`, `boks ls`, `boks inspect` and `boks rm` never touched it. Unpacking an
image did not either.

**It is now patched, and the patch has not been run on Windows.**
`packaging/containerd-windows/patches/0006` creates that link as a **junction** — the thing
`mklink /J` makes, and the thing the measurement above says an ordinary user can create — with
`os.Symlink` kept as a fallback for targets a junction cannot express, and with `!windows`
untouched. What was verified, on Linux, on 2026-08-15: the reparse buffer matches the layout in
[MS-FSCC] 2.1.2.2, against a golden written out by hand before the encoder was run; the
junction path is linked into both Windows binaries and reachable from `main`, read out of the
pclntab name table; the same assertion fails on a pristine v2.3.3 built from the same tree; and
`os.Readlink` reads `IO_REPARSE_TAG_MOUNT_POINT`, which is what `Bundle.Delete` needs to keep
removing working directories, read out of the Go 1.26.3 source and covered by Go's own
`os.TestReadlink`. What was **not** verified: any of it on Windows. No unelevated daemon has
created a task bundle.

An earlier version of this entry said the junction workaround "does not transfer — the link
must land inside `b.Path`, and `b.Path` must not pre-exist". That is true of *pre-creating* the
junction from outside containerd, which is how the root-ACL blocker is worked around. It was
never an argument against containerd creating the junction itself, and reading it as one is
what left this open for a day.

If containerd is run elevated anyway, its `--root` must be created by the elevated daemon
itself: pointing it at a root an unprivileged user pre-created hands that user full control
over a privileged daemon's content store, which is the escalation the protected DACL above
exists to prevent, reached from the other side. `new-containerd-root.ps1` is therefore for the
unelevated case only. None of that changes.

**And the two "known unrelated" plugin failures were neither.** `io.containerd.internal.v1.opt`
and `cri` failed unelevated and **disappear elevated** — they wanted directories under
`%ProgramData%` and `C:\Program Files`, the same root cause as the `--root` problem above.
Neither cascades, so no advice changes; what changes is that "unrelated" was wrong, and it is
the kind of wrong that stops you looking.

### A microVM boots under containerd on Windows, 2026-08-14

The full stack ran on real Windows 11 hardware: containerd started the nerdbox shim over
ttrpc, the shim loaded `krun.dll`, configured the VM and booted it, and the guest reached
userspace and mounted the container's own root filesystem.

```
kmsg "erofs: (device vda): mounted with root inode @ nid 36."
kmsg "Run /sbin/vminitd as init process"
msg="VM started" t_boot=1.7006629s
component=vminitd "initialized vminitd"
kmsg "EXT4-fs (vdb): mounted filesystem ... r/w with ordered data mode."
kmsg "overlayfs: \"xino\" feature enabled using 2 upper inode bits."
```

**`vminitd` survived**, which was the open question. Earlier bare-probe boots ended in
`Attempted to kill init!` because the probe configured no vsock and init had nothing to
dial back to; through the shim both ports exist and it came up, registered its plugins and
served them. 54 kmsg lines with 54 distinct timestamps, so the clock runs under the shim
too, not only under the probe.

**The predicted failure did not happen.** A console audit had warned the guest might boot
silently if the console device did not enumerate as `hvc0`. It does. `hvcN` numbering comes
from `hvc_alloc` taking the first free `vtermnos` slot rather than from MMIO order, nothing
else constructs a virtio-console on the Windows path, and the `console=hvc0` binding
survives its initial setup failure because `try_enable_preferred_console` sets the index
before setup and does not reset it on failure.

**Where it stopped:** `crun` refused the spec with `Required field 'layerFolders' not
present`. The spec carried `"windows":{"layerFolders":null}` — a Linux spec with a Windows
section, because spec generation adds one on a Windows *host* regardless of the target
platform, and libocispec makes `layerFolders` required whenever `windows` is present. So an
"empty and harmless" object is fatal. `docs/windows-e2e.md` had flagged this exact hazard
as unverified; it is now verified.

The section comes from one `if` in containerd's own spec generator
(`pkg/oci/spec.go`, `generateDefaultSpecWithPlatform`), which adds it to every non-Windows
spec when `runtime.GOOS == "windows"` — for LCOW, whose runtime fills it in. `omitempty`
does not drop it because `specs.Spec.Windows` is a *pointer*, and a non-nil pointer to a
zero struct is not empty to `encoding/json`. Both callers Boks has were affected, because
both call the same library function: `ctr` (fixed in
`packaging/containerd-windows/patches/0004`) and Boks itself (fixed in `internal/sandbox`,
with tests that build the Windows host's spec by hand rather than depending on the host).
The section is removed rather than filled in — a Linux guest has no layer folders — and it
must be removed *last* on the `ctr` path, because containerd reads `s.Windows != nil` as
"this is LCOW" to decide not to mount the image's rootfs on the host.

**Fixed by reading, not by running.** No Windows machine has executed either fix. What is
established is the mechanism and that the removal behaves on Linux under test; whether the
container then starts is unknown.

Also observed: after the create failure the run stalled a fixed 30.0 s at 0-3% CPU before
reporting `io shutdown: context deadline exceeded` — a separate defect on the error path.
That wait is nerdbox's IO teardown draining streams whose guest end was never attached to
anything, so it can only ever end at its own ceiling; the five teardowns reachable only by
a process that never started now get a second instead
(`packaging/nerdbox-windows/patches/0004`). Also unexecuted.

Every failure mode ranked ahead of this one is now cleared on that machine. Elevation was
still required at the time, for the task-bundle symlink; `containerd-windows/patches/0006`
has since replaced that symlink with a junction, unverified on Windows.

### A container runs in a microVM on Windows, 2026-08-14

`ctr tasks start` on real Windows 11 hardware, through the whole stack — containerd, the
nerdbox shim over ttrpc, `krun.dll`, the Windows Hypervisor Platform:

```
HELLO-FROM-THE-VM
Linux (none) 6.12.44 #1 SMP Thu Aug 13 14:58:57 UTC 2026 x86_64 Linux
1.22 0.56
```

Three lines, and each is load-bearing:

- **`HELLO-FROM-THE-VM`** — the container's own process ran and its stdout reached the host.
- **`uname -a`** reports **6.12.44**, the guest kernel this project builds, not the Windows
  host. A shared-kernel container cannot produce that line.
- **`/proc/uptime` = `1.22 0.56`** — non-zero and self-consistent, so the clock advances
  under the shim and not only under a bare probe. That is the CPUID crystal fix holding in
  production.

`t_boot=1.90127s`, `t_create=121.648ms`, boot to exited container in about 2.2 s.

The spec that reached the guest has no `windows` key anywhere, and containerd's stored spec
reports `windows: ABSENT` — the `layerFolders` fix confirmed from both ends.

The console baseline held exactly: `activate event`, `Device is ready`, `Port ready 0`,
`Starting port io for port 0`, once each in order, handshake in 11 ms.

**What this does not mean.** This is `ctr`, not `boks run`. No Ethernet frame has crossed the
virtio-net device on Windows, so nothing here says anything about the network enforcement,
which is the thing Boks is for. This run used an elevated containerd, for the task-bundle
symlink at `bundle.go:103`; `containerd-windows/patches/0006` makes that a junction so an
unelevated daemon should do, and no run has tested that. So: the stack underneath Boks works
on Windows; Boks on Windows does not yet.

> **Since this run, `boks run` no longer refuses on Windows.** The gate in
> `internal/network/vmm_windows.go` that stopped it before anything was bound has been
> removed, so the socket is bound, the stack is assembled and the sandbox is attempted. That
> is a change to what is *attempted*, not to what is known: still no frame, still nothing
> observed on the link, and nobody has run `boks run` on Windows at all. What the change adds
> is a bounded wait and a legible failure — if nothing connects to the link socket within 30 s
> of the task starting, the supervisor exits and says what did not happen. Anyone running it
> should expect that failure and record what happened *before* it, which is the evidence this
> section is still missing.

**Still open, measured in the same run.** The delete path stalls a fixed 30.009 s —
`reaped child process` at 12:47:08.080, then `failed to shutdown io after delete: io
shutdown: context deadline exceeded` at 12:47:38.089. Output is already complete by then, so
it is cosmetic, but every container takes thirty seconds to reap. The create-failure path
was fixed; this is a second call site.

A fix has since been written — `packaging/nerdbox-windows/patches/0005` — and **not executed**.
The mechanism it names is a Windows-only one: the shim's stdin copy reads a named-pipe
connection that nothing ever closes, because containerd's own client-side stdio leaves the
accepted connection out of its closers and its Windows `cio` has no cancel function, so the
shim's `stdinEOF` had nothing to end the read with. On Unix the equivalent close does happen,
which is why only Windows stalls. The patch makes `stdinEOF` disconnect the client, and — for
the case where that reading is wrong — makes the shim log *which* of its two waits expired,
`output drain` or `stdin drain`, so one line of the next run settles it. Until that run, the
30 s is measured and the cause is inferred.

### Boks runs a container on Windows, on its own stack, with policy enforced, 2026-08-14

`boks run --net nat shell <workspace> -- uname -a`, on real Windows 11 hardware, exit 0 in
12.2 s:

```
Linux (none) 6.12.44 #1 SMP Thu Aug 13 14:58:57 UTC 2026 x86_64 GNU/Linux
```

That is Boks itself — not `ctr` — running a container inside a microVM on Windows. The
guest enumerated `virtio0`…`virtio14`, fifteen devices against the nineteen-line budget,
and **said nothing at all about interrupts**: no `IRQ`, no `MP-BIOS`, no "not connected".
The IOAPIC pin fix is confirmed by silence, which is the strongest form of that result.

**The guest attached to Boks' own link socket** —

```
network: the guest attached to the link socket …\boks\net\shell-boks\net.sock
```

— the first Ethernet frame path contact on Windows, ending a claim this document carried
for a week.

**And the policy engine judged real traffic.** Not an attach; decisions:

```
Allowed: shell-boks network github.com:443    allowed by rule "github.com:443"
         shell-boks network 140.82.121.3:443  no deny rule matched the resolved address
Blocked: shell-boks network example.com:443   forward-bypass
         denied by default (policy "standard" allows only listed destinations)
```

Three `policy-log.jsonl` entries with `stage:connect` and `stage:dial`. The allowed
destination completed a TCP connection to a resolved GitHub address; the denied one was
refused at CONNECT.

**A new defect, found in the same run: the guest's wall clock is 1999.**

```
uptime  139.62 1110.64
date -u Tue Nov 30 00:02:19 UTC 1999      (host: 2026-08-14T14:58:07Z)
```

The monotonic clock was fixed on 2026-08-13 by publishing a real CPUID crystal; the
**RTC** is a separate thing and was unset in this run. The consequence is not cosmetic: the
allowed request above failed with `curl (60) SSL certificate problem: certificate is not
yet valid`. **Every TLS handshake in a Windows guest fails this way**, so the policy
result above is stronger than it looks — the denial was enforced, and the allowance was
carried far enough to fail on the guest's own date rather than on policy.

**Diagnosed and fixed in the libkrun series on 2026-08-14; not yet re-measured on
hardware.** 1999-11-30T00:00:00Z is 943,920,000, which is `mktime64(2000, 0, 0, 0, 0, 0)`
— exactly what `mach_get_cmos_time()` (arch/x86/kernel/rtc.c) computes from an all-zero
CMOS register file, since it adds `CMOS_YEARS_OFFS` to a year of 0 and passes month 0 and
day 0 through unvalidated. 00:02:19 is that constant plus the 139.62 s of uptime, to the
second. The guest read its clock correctly; libkrun's CMOS device had never had a clock in
it — `Cmos::new()` zeroed 128 bytes, filled in the two memory-size fields, and left every
time register at zero.

The device was therefore wrong on every host, not only Windows, and it stayed invisible
because no guest read it. Linux/KVM guests are built `CONFIG_KVM_GUEST=y`, so
`kvm_get_wallclock()` takes over `x86_platform.get_wallclock` before `timekeeping_init()`
and the wall clock comes from the kvmclock MSR page; macOS/aarch64 guests read the PL031,
which has always been seeded from the host clock. Under WHP there is no kvmclock, and no
PIT, HPET or PM timer either, so CMOS is the only wall clock the partition has. Patch 37
of the Windows series publishes the host's clock there in BCD. Because the x86_64 guest
image is built `# CONFIG_RTC_CLASS is not set`, the guest reads that clock exactly once,
in `timekeeping_init()` — inside `start_kernel()`, so a container that lives two seconds
still gets a valid date — and thereafter free-runs on the TSC. Nothing re-syncs it: there
is no `/dev/rtc`, no `hwclock` and no NTP client in the guest rootfs, and a host
suspend/resume will not be caught.

Unverified: no Windows host was available to boot it. What was tested, on the host, is
that the register file decodes back to the host's own clock through a verbatim
transcription of the kernel's `mktime64()`, for every date from 2000 to 2100 and for the
live clock through the same `BusDevice` entry point the vCPU's port-I/O exit calls.

Scope: `--net nat`, one sandbox, one machine. `boks rm` could not run because containerd
was already down, so teardown is unverified in this run.

### The Windows guest's clock is right, and TLS completes, 2026-08-15

The CMOS fix, measured against the host on both sides of the run:

```
HOST UTC BEFORE = 2026-08-15 06:40:56.490 UTC   epoch=1786776056
  guest: Sat Aug 15 06:41:02 UTC 2026   epoch=1786776062   uptime 1.58
HOST UTC AFTER  = 2026-08-15 06:41:12.588 UTC   epoch=1786776072
```

The guest's epoch falls inside the host window, six seconds after the earlier reading,
with the guest itself only 1.58 s old — the remainder is create and boot latency. Correct
to the second, against `1999-11-30 00:02:19` the day before.

**And it mattered.** The same probe that failed round 12 with `curl (60) SSL certificate
problem: certificate is not yet valid` now returns **HTTP 200 from github.com**, fetched by
a Linux container in a microVM on Windows through Boks' own gvisor stack. The denied host
still fails at `CONNECT tunnel failed, response 403` — the policy refusing it, not a
certificate. Three fresh policy-log records, host timestamps now consistent with the
guest's own clock.

The guest says almost nothing about the clock: no `rtc`, `cmos`, `hctosys` or `Time:`
line. That is expected rather than disappointing — this kernel reads CMOS through
`x86_platform.get_wallclock` during `timekeeping_init` instead of binding an `rtc-cmos`
driver, so a successful read is silent. The correct `date -u` is the only positive signal
available.

**Teardown, verified for the first time on Windows.** `boks rm` on a running sandbox
refuses and names the remedy; `boks stop` then `boks rm` leave nothing: the active
snapshot released with only the seven committed image layers remaining, the state
directory gone, the network directory including its socket, lock and log gone, and no shim
processes. One leftover, recorded without a verdict: `notices/<sandbox>.json` survives
`rm`, which may well be deliberate since it marks a warning as already seen.

### Workspace, persistence and SMP on Windows, 2026-08-15

**Workspace write-through, both directions.** `pwd` inside the guest is the derived guest
path; a file written there appears at the exact host path, byte-identical, LF not CRLF —
no line-ending translation through virtiofs — and a file written on the host is readable
in the guest. The exact-path promise holds on Windows, not only on macOS.

**Persistence.** A marker written to `/root` survives `boks stop` and a subsequent run,
with the new guest reporting 1.05 s of uptime — a fresh VM over the same writable
snapshot, as on macOS.

**SMP works, and had already been working.** This is a correction to something recorded
here as an untested risk. `boks create` with no `--cpus` takes the host's CPU count, and
that machine has eight — so **every Windows round from the sixth onward has booted an
eight-vCPU guest.** The AP startup path was never the untested code it was described as;
it had been carrying every result. Explicit runs at 2, 4 and 8 all boot, `nproc` agrees,
and no `VcpuInitSipiTrapLoop` ever fired:

```
smp: Bringing up secondary CPUs ...
smpboot: x86: Booting SMP configuration:
.... node  #0, CPUs:      #1 #2 #3 #4 #5 #6 #7
smp: Brought up 1 node, 8 CPUs
```

Seven APs in 24 ms. The clock does not misbehave with more than one vCPU either — idle
time scales 2.00×, 3.97× and 7.95× against a 1.01 s wall interval — and a two-thread busy
loop moved both per-CPU counters in `/proc/stat`, so the vCPUs genuinely execute rather
than merely existing.

**`/proc/interrupts` validates the IOAPIC fix directly**, which no host-side check could:

```
  5:  0 15  0  0  0  0  0  0  IO-APIC   5-edge   virtio0
 16:  0  0  0 12  0  0  0  0  IO-APIC  16-edge   virtio11
 19:  0  0  0  0  0 27  0  0  IO-APIC  19-edge   virtio14
ERR: 0     MIS: 0     SPU: 0
```

Fifteen devices on pins 5-19. Pins 16-19 — the ones the MPTable never described before,
which the guest logged as "not connected" and which would have failed at `request_irq` had
only `IRQ_MAX` been raised — are all delivering. Non-zero `RES`, `CAL` and `TLB` counters
mean real IPIs cross between vCPUs.

**One hypothesis refuted, and the question it left has an answer now.** sbx needs no elevation
and Boks did, and the guess recorded here was that sbx drives the shim directly and never runs
containerd's bundle code. Its MSI ships no `containerd.exe`, but a string scan of `sbx.exe`
finds `core/runtime/v2`, `NewBundle`, `bundle.go` and `io.containerd.runtime.v2.task` —
matching our own `containerd.exe` on all four, while `sailor.dll` matches none and both shims
match only `bundle.go`. sbx embeds containerd's runtime-v2 machinery in-process. So the
elevation requirement could not be explained by our having chosen containerd, and *how* sbx
avoids the symlink was left open.

It is still open as a fact about sbx — nothing here has read its `NewBundle` — but it stopped
being the interesting question, because the same code can be made not to need the privilege:
`containerd-windows/patches/0006` links with a junction, which `mklink /J` shows an ordinary
user can create. Whether sbx does that, patches the link out, or carries its own bundle
layout, is unexamined and no longer blocking.

### Windows needs no elevation, 2026-08-15

The last structural blocker is gone. Every step — creating the containerd root, running
the daemon, `boks create`, `boks run` — from an ordinary shell reporting
`elevated = False`, with no UAC prompt anywhere in the round:

```
elevated: False
Linux (none) 6.12.44 #1 SMP Thu Aug 13 14:58:57 UTC 2026 x86_64 GNU/Linux
RUN EXIT = 0   ELAPSED = 3.15s
```

Faster than any elevated round, and `A required privilege is not held` appears zero times
in 565 KB of debug log.

**Developer Mode was proven off first**, which is what makes the result mean anything:
creating a symlink failed with "Administrator privilege required" while creating a
junction succeeded, and `AppModelUnlock` has no `AllowDevelopmentWithoutDevLicense` value.
That is exactly the asymmetry the patch relies on.

**The link is a junction and the fallback never fired:**

```
Name       : work
Attributes : Directory, ReparsePoint
LinkType   : Junction
Reparse Tag Value : 0xa0000003     Microsoft / Name Surrogate / Mount Point
Substitute Name:  \??\C:\bokstest15\root\io.containerd.runtime.v2.task\boks\shell-boks
```

`symlink` appears zero times in the log. Sibling `rootfs` and `vm` are plain directories,
so only `work` is a reparse point.

**And teardown does not leak**, which is the check that would have failed silently:
after `boks stop` and `boks rm`, both the state bundle and the work target under `root`
are gone, with nothing orphaned on either side. A junction Windows creates but
`os.Readlink` cannot resolve would have left work directories accumulating with nothing
visibly wrong.

The create-time-fixed flags are hard errors now, each naming the requested value and the
actual one — `--cpus 2` against an 8-vCPU sandbox, `--net none` against one wired for
`nat`, `--memory 4g` against 2048 MiB.

### Boks runs and enforces policy on Linux, 2026-08-15

The platform Boks is designed for, verified end to end for the first time — in WSL2 on
Ubuntu 26.04, which also settles the fallback route the documentation had recommended
for a year without anyone running it. 25 of 26 checks pass.

**It is a VM, and the evidence is the part that carries the argument on Linux.** Both
host and guest are Linux, so a kernel version proves little:

```
host  boot_id  a7c70d34-…    guest boot_id  366323cf-…  (run a, --cpus 2)
                             guest boot_id  03b3b119-…  (run b, --cpus 1)
host  uptime   2344.89 s     guest uptime   1.31 s
host  nproc    8             guest nproc    2 then 1, tracking --cpus
```

Three distinct boot ids, a guest older than nothing, and vCPU counts following the flag
*downward* on an eight-core host. Neither `docker.sock` nor `containerd.sock` is visible
inside.

**The network boundary passes with its positive control.** With all eight proxy variables
cleared, an explicitly allowed address completed TLS and returned **the origin's own
Cloudflare certificate** — tunnelled, not intercepted — while `1.1.1.1:443` was refused in
the same sandbox on the same cleared environment. UDP to an external resolver timed out,
and the host's loopback listener was unreachable. Every decision is in `boks policy log`,
with `example.com` appearing as `forward-bypass` on the name and `transparent` on the
resolved address.

**Three host-configuration blockers, none of them a Boks bug, and one contradicts a
documented floor.**

1. **containerd will not start unprivileged with a default config** — it chowns its ttrpc
   socket to uid 0. `[ttrpc] uid/gid` set to the invoking user makes the chown a no-op.
2. **The Linux diff-service default is `['walking']`**, and the walking differ untars into
   a writable host mount, so a stacked erofs snapshot forces the template path and fails
   with `lowerdir={{ mount 0 }}` unresolved. `default = ['erofs', 'walking']` unpacks all
   seven layers. This is the exact twin of a bug already patched for Windows.
3. **The shim needs containerd ≥ 2.3, not the documented 2.2.** It emits version-3
   bootstrap parameters; a 2.2 daemon cannot decode them, falls back to treating the whole
   protobuf reply as an address, and fails with `unsupported protocol: Yunix` — the three
   leading control bytes rendering as letters. `go version -m` settles it: the shim links
   containerd v2.3.3, Ubuntu ships 2.2.2. The floor in the docs was wrong and is corrected.

**Still needing more privilege than Windows.** After the unpack succeeded as root,
sandbox creation failed for the client as an ordinary user:

```
failed to mount /run/user/1000/containerd-mount…: mount source: "overlay", err: operation not permitted
```

That is **boks itself** host-mounting the image overlay to read the image config — the
Linux twin of the Windows `invalid windows mount type: erofs`, which we answered there by
substituting a metadata-only image-config path. Whether that substitution transfers to
Linux is under investigation. Until it does, Linux needs privileges Windows no longer
does, which is the wrong way round.

**The single failure was the tester's own and was proved so.** `guest to host` write-through
failed because the client ran as root, leaving the workspace `root:root 755` while the
guest runs as uid 1000 and virtiofs passes uids through unmapped. Changing only the
ownership — same binaries, same daemon — produced `WRITE OK` and the file on the host. Not
a defect, and no source was changed to establish it.

### What was proven on the machine with no hypervisor

So that the two are never confused, this is the whole of what the fix for check 6 has been
shown to do, and it was shown against a **simulated** guest: a second gvisor stack on the far
end of the same link socket a VM would use — since the transport change, an `AF_UNIX`
`SOCK_STREAM` socket it connects to exactly as libkrun would — with its own address in the
sandbox's subnet and a default route through the gateway, speaking real Ethernet, ARP and TCP.

Driven through the real CLI — `boks net serve` holding the stack, `boks policy log` reading
the decisions — under `--policy locked` with one allowed destination:

```
raw TCP to 172.17.0.9:8099:     CONNECTED after 1.006s
raw TCP to 1.1.1.1:443:         FAILED: connect tcp 1.1.1.1:443: connection was refused
raw TCP to 169.254.169.254:80:  FAILED: connect tcp 169.254.169.254:80: connection was refused

$ boks policy log
Blocked requests:
  SANDBOX  TYPE     HOST         PROXY        REASON
  rawdemo  network  1.1.1.1:443  transparent  denied by default (policy "locked+local" …)

Allowed requests:
  SANDBOX  TYPE     HOST             PROXY        REASON
  rawdemo  network  172.17.0.9:8099  transparent  allowed by rule "172.17.0.9:8099"

$ cat …/net/rawdemo/stack.log
network: dropped tcp to link-local 169.254.169.254:80, which includes the instance metadata endpoint
```

No proxy was configured in that guest at all. Three things to read out of it: a denied
address is refused *and recorded*, an allowed one is carried and recorded, and the metadata
endpoint is refused before the policy is consulted — which is why it appears in the stack's
own log rather than as a decision.

This is the same shape of probe that returned `http=200` on the Mac — and it is *not*
evidence about a VM, because nothing here crossed a hypervisor.

### The update check, measured rather than reasoned about, 2026-08-15

Boks promises no telemetry. `internal/update` argues in its package comment why an update
check is not that promise broken — nothing about you or what you ran is sent, the comparison
happens locally, and the request is disclosed before the first one is made. What follows is
what was measured rather than what was intended.

**The endpoint answers differently than assumed.** `HEAD https://github.com/dagsommer/boks/releases/latest`
against the real repository returned `302` to `https://github.com/dagsommer/boks/releases` —
the releases index, not a `404`. A repository with no releases does not error, it redirects to
a page naming no version. Code that pattern-matched `/tag/` and reported a parse failure would
have sent a user hunting a bug in Boks on every check until the first release exists, which is
every check today. That case is now `ErrNoReleases` and prints "no release has been published
yet", exit 0.

**The once-a-day bound did not hold, and the test could not see it.** The refresh runs in a
goroutine that is abandoned when the process exits. With the timestamp written on completion,
a `boks run` against an absent containerd — which fails in about a tenth of a second — started
a request, died before the answer arrived, wrote nothing, and started another on the next run.
Measured directly: after two such runs the record still read
`{"v":1,"disclosed":true,"checked":"0001-01-01T00:00:00Z"}`. There is no back-off in that.

The test asserting the back-off passed throughout, because it waited on the completion channel
and so gave the goroutine a chance production never gives it. The invariant is now written as
"the attempt is on disk by the time `Notify` returns", which is synchronous and race-free, and
the same two-run sequence now records
`{"v":1,"disclosed":true,"checked":"2026-08-15T11:19:18Z"}`.

**Negative controls.** Four mutations were applied and each was required to fail: making the
request before disclosing (4 tests fail), recording the timestamp on completion instead of at
the start (fails 5 runs out of 5), omitting the timestamp on a failed check, and returning a
version from a redirect that names no tag. Two earlier assertions in this project passed
against code that did not work; these were checked against code that does not.

**What is not proven.** No release exists, so the path where a newer version is actually
reported has been exercised only against a local `httptest` server and a fake clock, never
against GitHub returning a real tag. The install-method detection is checked against literal
paths; no binary installed by Homebrew, winget, apt or dnf has been asked to identify itself.

### `boks doctor` was reading the wrong PATH, 2026-08-15

Found while packaging the Linux runtime, on a `.deb` that was correctly built and correctly
installed. `boks doctor` reported `vm runtime fail`, `hypervisor library warn` and `guest image
fail` — while, on the same run, `runtime skew ok` named the version of the very shim the first
line said was missing.

The cause is whose PATH each check consulted. A packaged install puts `containerd`, the shim
and `libkrun.so` in `/usr/libexec/boks/`, deliberately not on anyone's PATH: a `containerd` in
`/usr/bin` would collide with the distribution's own package. `boks daemon` bridges that by
prepending those directories to the PATH it starts containerd with, so containerd finds the
shim and the shim finds libkrun. `runtime skew` went through `daemon.FindShim`, which knows
this. The other three called `exec.LookPath` and `os.Getenv("PATH")`, which is the invoking
shell's — a question none of these checks is asking.

The remedy text on the `vm runtime` check has read "Note that containerd's PATH is the
daemon's, not your shell's" since long before this. The code said it and did not do it.

**Measured, on the layout a package installs** — a `boks` in `bin/` with the runtime in
`../libexec/boks/` and a PATH of `/usr/bin:/bin` that excludes it:

| | `vm runtime` | `hypervisor library` |
|---|---|---|
| before | `fail   containerd-shim-nerdbox-v1 not found on PATH` | `warn   libkrun.so not found where the shim looks` |
| after | `ok     …/libexec/boks/containerd-shim-nerdbox-v1` | `ok     …/libexec/boks/libkrun.so` |

Both binaries were rebuilt between the two runs. The first attempt at this comparison reverted
the source and did *not* rebuild, so it re-ran the fixed binary and reported "before" results
identical to "after"; the numbers above are from the corrected run.

**Negative control.** With the same binary and the runtime directory emptied, the checks return
to `fail` and `warn`, so they are not reporting `ok` unconditionally. **Mutation.** Reverting
`shimGetenv` to `os.Getenv` fails both new tests, one of which asserts up front that the shell
PATH does *not* already contain the staged directory — without that, the test would pass
against any implementation.

**Not proven.** No sandbox was started; this host has no `/dev/kvm`. The staged shim and
`libkrun.so` were a shell script and an empty file — the checks assert presence and name, which
is all they claim to, but nothing here shows the shim can actually `dlopen` a vendored libkrun.
That needs a machine with a hypervisor and a real package installed on it.

### The arm64 guest kernel was named so the shim could never find it, 2026-08-15

Found by reading, while wiring the guest images into the Linux packages. No hardware needed:
four files disagree and the disagreement is decidable from their text.

`guest-image.yml` built the 64-bit ARM guest with `KERNEL_ARCH=aarch64`. Every other component
in the chain spells that architecture `arm64`:

- nerdbox's `Dockerfile` names its output `nerdbox-kernel-${KERNEL_ARCH}`, so the file would
  have been `nerdbox-kernel-aarch64`;
- nerdbox's shim looks the kernel up at boot as `nerdbox-kernel-<kernelArch()>`, and
  `kernelArch()` is Go's `GOARCH` with only `amd64` rewritten to `x86_64` — so on 64-bit ARM
  it asks for `nerdbox-kernel-arm64`;
- Boks' own `guestArch()` in `internal/doctor/checks.go` is a faithful transcription of that
  and agrees;
- `scripts/package-linux.sh` looks for `nerdbox-kernel-arm64`.

So a guest built as `aarch64` is one the shim never finds. It would not have got that far in
any case: nerdbox ships `kernel/config-6.12.44-arm64` and `kernel/config-6.12.44-x86_64`, and
the Dockerfile does `COPY kernel/config-${KERNEL_VERSION}-${KERNEL_ARCH}`, so an `aarch64`
build fails at that COPY naming a config that does not exist — an error several layers from the
word that caused it. `KERNEL_ARCH` is also compared against the literal `arm64` twice in that
Dockerfile and passed to Linux's kbuild, which spells it `arm64` too.

**Why nothing caught it.** The matrix that builds both architectures lives in `release.yml`,
which has never run: `runtime-guest` was added on 2026-08-15 and no tag has been cut. A
standalone `guest-image.yml` run builds its default, `x86_64`, alone — and the most recent run,
31882491001, shows exactly one job. The `aarch64` leg had never executed.

`linux-runtime.yml` documents this same trap in a comment and avoids it deliberately: "plain
GOARCH otherwise, so `arm64` and NOT `aarch64` on 64-bit ARM ... avoids the aarch64/arm64 trap
entirely". The project knew, in one workflow, and fell into it in another.

**The guard.** An architecture nerdbox ships no kernel config for now stops the build with a
message naming the ones it does ship and the arm64/aarch64 distinction. The first version of
that guard was wrong in a way worth recording: it parsed the kernel version out of
`docker-bake.hcl` with `grep -oP`, which is line-based, so it never matched the multi-line HCL
block and rejected *every* architecture for want of `config--x86_64`. Run against nerdbox's
real filenames it accepts `x86_64` and `arm64` and rejects `aarch64`.

**Not proven.** No arm64 guest has been built. This says the naming can now agree, not that the
build succeeds — the first `aarch64` build never ran, and the first `arm64` one has not either.

### `boks daemon start` gave a Win32 error instead of an answer, 2026-08-15

The Windows CI leg failed `TestServeRefusesASecondDaemon`. Starting a daemon while one was
already running reported

```
the lock C:\...\containerd\daemon.lock is held: The process cannot access the file
because another process has locked a portion of the file.
```

instead of `a boks-managed containerd is already running; 'boks daemon status' shows it`.

`internal/proclock` exists to separate "I could not take the lock" from "somebody else has
it", and `serve.go` asks that question with `errors.Is(err, proclock.ErrHeld)`. The Unix
implementation wraps `ErrHeld` on the one branch that means contention. The Windows
implementation wrapped the raw Win32 error and nothing else, so the question always answered
false and every caller fell through to printing the operating system's sentence.

**This is the second time the same divergence has shipped, in opposite directions.**
`ErrHeld`'s own doc comment records the first: `boks net serve` used to answer *every* failure
of `Acquire` with "sandbox already has a network supervisor", which was false on Windows where
`Acquire` refused outright. The type was introduced to fix that, and the Windows half never
joined in. A shared error value only helps if both implementations produce it.

Windows now wraps `ErrHeld` for `ERROR_LOCK_VIOLATION` and `ERROR_SHARING_VIOLATION`, and for
nothing else — the same rule the Unix side follows.

**Not proven.** The fix compiles and vets for `windows/amd64`; it has not been run on Windows.
The next CI run on that leg is the test.

### A test fixture, not a limit, broke the Windows suite

`TestUnexercisedWarningIsNotSaidWhenThereIsNoLinkToDial` failed on Windows with "the link
socket path is 128 characters, over the 104-byte limit for UNIX sockets". Nothing was wrong
with the limit: `sun_path` is 104 bytes on macOS and Boks enforces that floor everywhere, which
is conservative on Windows and Linux, where it is 108.

What was wrong was the fixture. `t.TempDir()` embeds the test function's name in the path, this
one is 52 characters, and the runner's temp root is long as well — together 128 bytes, so a
test about network *modes* failed on a path length and advised using a shorter sandbox name.
The daemon package had already met this and answered it with a `shortStateDir` helper;
`internal/cli` now has the same one. The path it produces measures 73 bytes against the same
runner layout.

### The first release, built and installed, 2026-08-15

`release.yml` ran for the first time, dispatched rather than tagged so it produced a draft.
Run 31884221317, green end to end: twenty jobs, fifteen assets.

**The assumption the design rested on is now measured.** `docs/distribution.md` recorded that a
called workflow's artifacts being visible to a later job in the caller was "a conclusion from
two documented facts rather than a documented fact", and named it the first place to look if
`assemble` could not find an artifact. `assemble` found all of them. Reusable workflows do
share the caller's run and its artifact scope.

**The first arm64 guest ever built.** `boks-guest_0.1.0_arm64.tar.gz` contains
`nerdbox-kernel-arm64` and `nerdbox-rootfs.erofs` — the names the shim looks for. The naming
fix earlier the same day was correct, and the leg that had never executed now does.

**Installed from the `.deb`, on arm64 Linux.** `sudo dpkg -i boks_0.1.0_arm64.deb`, then:

```
vm runtime           ok     /usr/libexec/boks/containerd-shim-nerdbox-v1
hypervisor library   ok     /usr/libexec/boks/libkrun.so
```

which is the doctor PATH fix holding on a real package rather than a synthetic layout — both
files are in a directory on no `PATH`.

#### `boks daemon start` ran the wrong containerd

The first run of the installed package reported `containerd v2.2.6` from `/usr/bin`. The
package had just installed 2.3.3 into `/usr/libexec/boks`, and 2.2.6 is *below* the measured
floor — it is the version that fails at task start with `unsupported protocol: Yunix`. Every
part of the stack was correct and the daemon ran the distribution's binary anyway, which is
precisely the failure vendoring exists to prevent.

`RuntimeDirs()` searched the executable's own directory before the bundle. For a tarball those
are the same directory and the order never mattered; for a package the executable's directory
is `/usr/bin`, which on any real machine holds the distribution's containerd. The bundle is now
searched first.

Measured on the same installed package, before and after:

| | `boks daemon status` |
|---|---|
| before | `binary /usr/bin/containerd`, `containerd v2.2.6` |
| after | `binary /usr/libexec/boks/containerd`, `containerd v2.3.3` |

and `runtime skew` moved from unchecked to `ok (containerd v2.3.3, shim built against
v2.3.3)`. With the guest archive unpacked into the same directory, every check passes except
`virtualization`, which fails because this host has no `/dev/kvm`.

**Not proven, and it is the important one.** No sandbox was started. This host has no KVM, so
nothing here shows the packaged stack boots a VM — only that every component is present, the
right version, and found. The release is a draft; `boks version --check` against it correctly
reports "no release has been published yet", because GitHub's releases-latest excludes drafts.

### An image's `USER name` never reached anything that could read it, 2026-08-16

An OCI image may name its user rather than number it — `USER node` rather than `USER 1000`.
Boks records such a name in the OCI spec's `Process.User.Username` on Windows and macOS,
following containerd, which does the same on macOS and for LCOW. **Nothing downstream reads
that field**, so the name is discarded and the container runs as uid 0.

This was found by reading, not by running, and both halves were checked at the revisions this
project pins rather than assumed from documentation.

| Component | Revision | What it does with `Process.User.Username` |
| --- | --- | --- |
| `vminitd` | nerdbox `cd2c23f` (`packaging/nerdbox/NERDBOX_REV`) | Nothing. `grep -rn Username --include=*.go .` outside `vendor/` is **zero hits** in nerdbox's own code. Its only read of the spec is `ShouldKillAllOnExit` (`internal/vminit/runc/util.go:33`), which examines `Linux.Namespaces` and nothing else. |
| `crun` | `3425c83` | Nothing. Every read of the user struct in `src/libcrun/` is one of `user->uid` (6 sites), `user->gid` (4), `user->umask{,_present}` (4), `user->additional_gids{,_len}` (2). The only `username` matches in the tree are `libcrun_set_usernamespace` — user *namespace*, unrelated. |
| runtime-spec schema | `d64c1d9` | `username` **is** a valid property of `process.user` in `schema/config-schema.json`. |

The third row is what makes this silent rather than loud. An *unknown* field would be rejected
by libocispec and the container would fail to start — which is exactly what happened with
`layerFolders` on 2026-08-14. A *known-but-ignored* field parses cleanly and is then dropped.

#### What that meant per platform, before this change

| Host | Spec path | `USER 1000` | `USER 1000:1000` | `USER node` |
| --- | --- | --- | --- | --- |
| Linux | `oci.WithImageConfig` — mounts the image on the host | correct | correct | correct |
| macOS | metadata-only (containerd's own `darwin` branch) | **uid 0** | **uid 0** | **uid 0** |
| Windows | metadata-only (`withImageConfigFromMetadata`) | **uid 0** | **uid 0** | **uid 0** |

Linux is correct and expensive: the host-side mount is the reason Boks on Linux needs
`CAP_SYS_ADMIN`. Rootless containerd does not rescue it — `unshare -Umr mount -t erofs` is
`EPERM`, because erofs carries no `FS_USERNS_MOUNT`.

#### What was fixed, and what was not

The numeric forms are fixed on the host, on every platform: they need no `/etc/passwd`,
because they are already numbers. `USER 1000:1000` is now exactly right, and `USER 1000` takes
gid 0 — what containerd's own `WithUserID` settles on when passwd holds no matching entry.

`USER node` still runs as uid 0 on Windows and macOS. It cannot be resolved without reading
the image's filesystem, and those hosts cannot. The fix for it is guest-side and is carried,
**unapplied**, in `packaging/nerdbox/patches/`.

#### Verified by running

- The nerdbox patch applies cleanly to a pristine checkout of the pinned revision, and
  `go test ./internal/... ./pkg/...` passes there (`linux/arm64`, 2026-08-16).
- Three mutations of the guest resolver each fail a named test: making it a no-op (the
  pre-patch behaviour), not skipping the user's own group, and letting an unresolvable name
  fall through to uid 0.
- On the Boks side, `go build/vet/test ./...`, `gofmt`, `make docs` (no diff), and build+vet
  for `GOOS=windows` and `GOOS=darwin`. Four mutations each fail a named test, including
  opening the capability gate early — the one that would ship the regression.

#### Not verified, and it is the whole runtime claim

**No microVM has booted with any of this.** This machine has no `/dev/kvm`. What is proven is
that the resolver produces the right spec from the right inputs; what is *not* proven is that a
guest carrying it starts a container as the resolved uid, that the rewritten `config.json` is
the file crun actually reads, or that the rewrite happens early enough in `NewContainer`. The
last two are read from nerdbox's source and are the first things to check on a machine with a
hypervisor.

The check that would let Linux drop its `CAP_SYS_ADMIN` requirement — `ShimResolvesUsernames`
in `internal/daemon/compat.go` — has also never returned true, because no nerdbox revision
resolves the field yet. It is asserted false rather than exercised true, so the *closed* path
is tested and the *open* one is not.

Its *input* half, though, is measured rather than assumed. A `containerd-shim-nerdbox-v1`
built from a real nerdbox git checkout on 2026-08-16 carries

```
mod    github.com/containerd/nerdbox  v0.2.4-0.20260816080949-5f0e4a44d293
build  vcs.revision=5f0e4a44d29341a9391d7112756ff374726c7629
build  vcs.modified=false
```

and `ShimNerdbox` on that binary returns the revision string exactly. So the detection reads
what it claims to read; what is untested is only the branch taken when a revision is
recognised. `ShimResolvesUsernames` on that same binary is `false` — correctly, since a local
branch SHA is not an upstream revision anyone has verified a guest for, which is the
allowlist behaving as intended rather than a limitation.

### SmartScreen, measured precisely — and the first report of it was wrong, 2026-08-16

An earlier entry here recorded "Defender blocks the unsigned release binary" from a first-hand
report. **That framing was wrong and is retracted.** The full run, with Defender's own records
read back, says something more precise and more useful.

**Defender's antivirus engine never blocked anything.** `Get-MpThreat` and
`Get-MpThreatDetection` are both empty; there are zero 1116/1117 events in
`Microsoft-Windows-Windows Defender/Operational` in 24 hours, only configuration-change and
signature-update entries. All sixteen archive files were present and nothing was quarantined.
The nine `CodeIntegrity` events are boot-time policy activation.

What fired was **SmartScreen's application-reputation check** — a different component with a
different trigger — and it fired only under a condition the tester constructed deliberately:

| Launch path | Mark of the Web | SmartScreen |
|---|---|---|
| `.\boks.exe version` from PowerShell | none | nothing |
| ShellExecute, i.e. an Explorer double-click | `ZoneId=3`, written by hand | the dialog |

```
Windows protected your PC
Microsoft Defender SmartScreen prevented an unrecognized app from starting.
App:        boks-motw.exe
Publisher:  Unknown publisher
```

"Run anyway" was offered and worked. Two independent reasons the ordinary path is silent, both
verified: `gh release download` writes no `Zone.Identifier` stream, and console `CreateProcess`
does not consult the reputation check at all.

So the honest statement is narrower than the prediction the docs carried: a user who downloads
the archive **in a browser** and **double-clicks** `boks.exe` will meet the dialog; a user who
downloads with a tool and runs it from a shell will not. `boks.exe` was the binary; it was not
`containerd.exe`, `ctr.exe` or `mkfs.erofs.exe`.

**Not measured:** whether a `winget install` trips it. Nothing here tested a winget delivery.

### The erofs PATH diagnosis, confirmed — and the defect behind it, 2026-08-16

**Confirmed on Windows.** With `$env:PATH = "$PWD;$env:PATH"` before `boks daemon start` and no
other change:

- `daemon start` no longer prints "mkfs.erofs: not on PATH", and the self-documenting comment
  block it writes into the config is gone;
- the generated diff order went `['windows', 'windows-lcow']` → `['erofs', 'windows', 'windows-lcow']`;
- the image **pulled and unpacked completely**: eight snapshot directories and seven
  `layer.erofs` files, one per layer of the base image, each converted by the erofs differ;
- runtime went 2.59 s → 20.15 s, consistent with a real pull rather than an early failure.

One line on `PATH` turned total failure into a full unpack. That is the diagnosis exactly, and
the fix already in `main` — `HasEROFS` asking the PATH containerd is actually started with —
addresses it at the source rather than by asking users to edit their environment.

#### The next defect, which has never worked unattended

The run then died:

```
failed format "...\io.containerd.snapshotter.v1.erofs\snapshots\11\rwlayer.img":
mkfs.ext4 failed: : exec: "mkfs.ext4": executable file not found in %PATH%
```

Snapshot 11 is the writable layer; snapshots 4–10 are the seven read-only erofs layers. It held
`fs\` and a zero-byte `.erofslayer`, and no `rwlayer.img` — it died at exactly that step.

**This is not a regression, and it is worth being precise about why.** Windows has no
`mkfs.ext4` in any packaged form. `packaging/containerd-windows/patches/0005` exists because of
that: it makes containerd verify the ext4 superblock magic rather than trusting that the file
exists, and it names **placing a pre-formatted image** as the supported route where no mkfs
exists. The bundle ships `rwlayer-64m.img`, 67,108,864 bytes, made by `mkfs.ext4` on a CI
runner, for precisely this.

What put that file in place, every previous time, was **a human running `Copy-Item`** — step 5
of `docs/windows-e2e.md`. Nothing in Boks does it. So the round-15 result that booted a sandbox
unelevated was obtained with a manual step that no user of the release archive performs, and
every `boks run` from the archive fails here.

The tester established this rather than assuming it, and stopped rather than probing further:
the archive ships `rwlayer-64m.img` and no `mkfs.ext4`; the round-15 bundle that did boot also
had no `mkfs.ext4`; no literal `mkfs.ext4` string exists in `boks.exe`, `containerd.exe`, the
shim or `krun.dll` — the name is built as `mkfs.` plus a filesystem name at runtime; and
neither the shipped nor the generated `config.toml` carries any rwlayer or ext4 setting, so
configuration is not the difference. containerd's own log records none of this: it ends at
`containerd successfully booted`, and the error surfaced only through `boks`.

**Also established on this run.** No elevation at any point. `boks ls` and `daemon stop` clean,
no leftover `containerd`/`boks`/shim/`gvproxy` processes.

**Still unrun on Windows:** `uname -a`, the boot id, and both network-policy results. No
sandbox has started from a release archive.

### Boks boots a sandbox on Windows from an installable artifact, 2026-08-16

Every earlier Windows result was obtained by driving containerd by hand, from CI artifacts, in
a tree somebody had built — and, as the previous entry records, with a manual `Copy-Item` that
no user performs. This one was obtained from the release archive, unattended, on Windows 11
Enterprise 10.0.26200 on an Intel Xeon Platinum 8370C.

**The archive was confirmed to be the rebuilt one** before anything else: 58,407,809 bytes,
`da4c3cb0a991a45d…`, against the previous 58,141,552 / `64ee4365…`, with the old extraction
deleted first.

**`mkfs.ext4.exe` ran, on its first execution anywhere.** It had been cross-compiled and
inspected on a Linux/arm64 machine that cannot run a Windows PE, so this was the first time the
binary had started on any host:

```
mke2fs 1.47.2 (1-Jan-2025)  Using EXT2FS Library version 1.47.2   exit=0
mkfs.ext4.exe -q rw.img                                            exit=0
superblock magic at offset 1080:  53 ef
```

**No PATH edit, and no manual copy.** `boks daemon start` printed no mkfs note and wrote
`default = ['erofs', 'windows', 'windows-lcow']` unprompted. Both of the defects found earlier
the same day are closed from the artifact.

**The boundary, measured cold.** The tester caught and corrected a confound rather than
reporting it: a first run took 2.8 s because erofs snapshots from the earlier PATH-edited run
were still in `%LOCALAPPDATA%\boks\containerd\root`, which would have been a warm result
reported as a cold one. `root` and `state` were wiped and it was re-run from empty:

```
boks run shell . -- uname -a
Linux (none) 6.12.44 #1 SMP Sun Aug 16 15:12:27 UTC 2026 x86_64 GNU/Linux
exit=0   elapsed=19.77s
```

A Linux guest, on a Windows host, in 19.77 s from an empty root: pull, erofs unpack and VM boot.
Two sandboxes reported distinct boot ids (`29e8ed76-…`, `5e364b05-…`), while repeated `boks run`
into one running sandbox returned the same id in 0.11–0.19 s — an exec into the live VM rather
than a new boot, which is the correct distinction and worth having both halves of.

The step's own command stated the boundary a second way: `boks run shell . -- cmd /c ver` gave
`exec: cmd: not found`, exit 127 — the Linux guest refusing a Windows command.

**Network policy, enforced outside the guest.**

```
https://github.com    -> 200
https://example.com   -> curl: (56) CONNECT tunnel failed, response 403
```

and `boks policy log` recorded all three decisions, including
`Blocked: example.com:443  denied by default` and
`Allowed: 140.82.121.3:443  no deny rule matched the resolved address`.

**No elevation anywhere.** `Elevated: False` at every step, no UAC prompt, ordinary PowerShell
from start to finish including the cold pull and both boots.

#### The first policy attempt was indeterminate, and is reported as such

Both requests failed identically at first — `Failed to connect to 192.168.127.1 port 3128` — so
the positive control failed and the "must fail" result proved nothing. That is the trap a
negative control exists to catch, and it was caught rather than read as a pass. The cause is the
defect below; the sandbox was recreated and the measurement above is from that run.

#### New defect: the network stack dies while the sandbox keeps running

After the step-5 task exited, the sandbox remained `running` while the process serving its
network was gone. Boks detected this precisely and said so, and the warning is accurate:

> sandbox … is running, but the process serving its network is gone. A running guest does NOT
> re-attach to a new link socket — measured on 2026-08-12 — so this sandbox has no network
> until it is restarted.

**Observed:** a new supervisor (pid 17296) was alive and `stack.json` pointed at it, while the
running guest was still bound to the dead first stack (pid 15264); nothing listens on host 3128,
correctly, since the proxy is inside the stack behind `net.sock`.

**Inferred, not established:** the stack's lifetime appears tied to the first *task* rather than
to the sandbox. Step 5's task exited 127 and the VM outlived it; whether a task exiting 0 does
the same was not determined.

The consequence is worth stating plainly: **a user who runs one failing command silently loses
network for that sandbox's whole life**, with stop/start the only remedy the warning offers.

#### Smaller notes

- `rwlayer-64m.img` (67,108,864 bytes) still ships although the template route is gone — dead
  weight in the archive unless it still has a consumer.
- `doctor` is honest now: `platform ok`, and `virtualization warn — Windows Hypervisor Platform
  assumed available` with text saying it cannot probe without booting. `snapshotter tools` names
  both `mkfs.erofs.exe` and `mkfs.ext4.exe`.
- The `C:\ProgramData\containerd\state` warning is still printed and is still wrong on this
  machine: that path does not exist, and sandboxes started fine.

### What actually ended the network stack, established on a host with no hypervisor, 2026-08-16

The entry above reported a defect and an inference: the sandbox's network stack died while the
sandbox kept running, and the stack's lifetime *appeared* to be tied to the first task. The
inference was wrong in a way that matters, and the real rule was established by reading the two
code paths that can end a stack and then running the one that can be run on a Linux host with
no `/dev/kvm`.

**The rule, as it was.** A stack ended when any one of three things happened:

1. the sandbox's containerd **task** left `Running`/`Paused`/`Created`, which the supervisor
   sees for itself (`sandbox.WatchTask`, polled every 2 s) — correct, and the design;
2. `boks stop`, `boks rm` or `boks net stop` asked for it — correct;
3. **the `boks run` invocation that started it returned any error at all.** That is the defect.
   `boks run` reports the guest command's exit status by returning an `ExitError`, so *a
   non-zero exit inside a perfectly healthy sandbox was indistinguishable, to the cleanup path,
   from a run that never got a sandbox up.* An interrupted run — Ctrl-C — reached it the same
   way.

So the tester's "tied to the first task" was close but not the mechanism: a task exiting **0**
kept its stack, and a task exiting **127** did not. It was tied to the *exit status of the
first invocation*. On the Windows run, step 5's command exited 127 (`cmd: not found`, the Linux
guest refusing a Windows command) and that is what killed pid 15264 while the guest it served
stayed up. The orphan warning the next command printed was accurate, and it was describing
damage boks had done to itself.

This is exactly the failure `internal/enforce/supervisor.go` was written to prevent — its
opening paragraph says a stack in the CLI invocation would mean "pressing Ctrl-C would silently
disconnect a background build running inside the sandbox". The supervisor's own lifetime was
right; the CLI took it away afterwards.

**The fix.** The cleanup no longer asks whether the *command* failed. It asks containerd
whether the *sandbox* is still running, and a sandbox that is running keeps its stack however
the command ended. An ephemeral (`--rm`) run still takes its stack with it, because the sandbox
goes too. When the answer cannot be obtained — containerd unreachable, for instance — the stack
is kept and the reason is printed: keeping one costs an idle process that reaps itself within a
poll interval, and taking one costs a live guest its network permanently, because a running VM
does not re-attach to a new link socket (measured 2026-08-12).

**Measured on Linux, with no hypervisor.** The supervisor is a host-side process, so its
lifetime is observable without a VM. `internal/cli/stack_test.go` starts a **real** supervisor —
a child process holding a real link socket, a real lock and a real gvisor stack, spawned the way
`enforce.Ensure` spawns one — and then runs the cleanup path against it:

| after a run that… | the sandbox is | the stack |
|---|---|---|
| exited non-zero | running | **stays up** (same pid, socket still there) |
| was interrupted | running | **stays up** |
| never started one | not running | is stopped, socket removed |
| `--rm` | either | is stopped |

Each assertion was confirmed to be capable of failing: with the old condition restored, the
first two fail and the last two pass, which is the shape the defect had. The "cannot tell"
branch was exercised against a containerd address that does not exist.

**What this does not establish.** No sandbox was booted here — there is no `/dev/kvm` on this
host — so the containerd query itself (`sandbox.Running`) has been run only against a
containerd with no such sandbox and against an absent socket, never against a live VM. **A
Windows or macOS machine is still needed** for the end-to-end statement: run a command that
exits non-zero in a persistent sandbox, then confirm from the same sandbox that the network
still works — `boks net ls` still naming the stack, and a request from inside the guest
reaching the proxy and being judged.

#### The `C:\ProgramData\containerd\state` warning, fixed

It was testing a Unix mechanism on Windows. containerd puts each shim's socket under
`defaults.DefaultStateDir`, a path compiled into `pkg/shim/util_unix.go` — **unix only**. On
Windows a shim is reached over a named pipe: `pkg/shim/util_windows.go` (containerd v2.3.3) has
no `socketRoot`, no `SocketAddress` and no `writeSocketDir`, and its `RemoveSocket` is a no-op.
Nothing on that host needed the directory, which is why sandboxes started fine without it. The
note is now Unix-only, and its remedy no longer offers "run the daemon elevated" on the one
platform where Boks has been verified end to end with no elevation at all. The check still
fires where it means something: on this Linux host, `/run/containerd` can be neither created
nor written by the test user, and the note is produced with the `sudo mkdir` remedy.

#### `rwlayer-64m.img`: it had a consumer, and it was not the release archive

`docs/windows-e2e.md`'s by-hand procedure still copies it into place, and that procedure
collects its files from the containerd bundle — `boks-runtime_<v>_windows_amd64.zip` — not from
the archive a user installs. Nothing in `boks_<v>_windows_amd64.zip` reads it: `mkfs.ext4.exe`
formats the layer there, which is what the run above measured. So it stops being copied into
the all-in-one archive (64 MiB, ~8% of the download) and stays in the runtime zip, where it is
still that document's fallback for a machine where the formatter does not run. `release.yml`
asserts its absence from the all-in-one archive, because "we stopped shipping it" is a claim
about a file that is not there, and that is the kind of claim that goes wrong silently.

### The first Homebrew install could not run a command, and the cause was a stream Boks named but never made, 2026-08-16

`boks v0.1.0`, installed from the tap on macOS/arm64. `boks doctor` entirely green, the network
stack up, and then `boks run .` failed:

```
boks: running "/usr/bin/tini" in sandbox "shell-bokstest": containerd-shim: opening file
"/var/run/containerd/fifo/2028300063/boks-exec-af1c2eee9d36ee8a-stderr" failed:
open /var/run/containerd/fifo/2028300063/boks-exec-af1c2eee9d36ee8a-stderr: no such file or directory
```

**It is not a path, a permission or a symlink problem, and the error's own shape rules those
out.** The shim opens the streams it is handed in a fixed order — stdout, then stderr
(nerdbox `internal/shim/task/io_copystreams_unix.go`, the loop at `copyStreams`). It got past
stdout and failed on stderr. So `/var/run/containerd/fifo/2028300063/` existed, was reachable by
the shim under whatever `/private/var` resolves to, and contained an openable stdout FIFO.
Exactly one file was missing. That eliminates the `/var/run` symlink, the directory-ownership
caveat, and the containerd#12444 family of compile-time paths in one stroke: the path was fine,
the file was never created.

**Who was supposed to create it.** On unix, `cio.NewFIFOSetInDir` fills in all three paths
unconditionally (`containerd/v2/pkg/cio/io_unix.go`), and `copyIO` then creates the FIFOs — but
skips stderr whenever a terminal is in play:

```go
if !fifos.Terminal && fifos.Stderr != "" {   // io_unix.go, openFifos
```

`cio.NewCreator` blanks the stderr *path* only when the caller passed no stderr writer
(`if streams.Stderr == nil { fifos.Stderr = "" }`). Boks passed one, alongside
`cio.WithTerminal`. So `task.Exec` sent `ExecProcessRequest.Stderr` naming a FIFO that cio had
deliberately declined to make. `ctr` avoids this by passing `cio.WithStreams(con, con, nil)`
with `cio.WithTerminal` — the nil is load-bearing, not tidiness.

**Why it hit macOS and not the other two.**

| | what happens | why |
|---|---|---|
| Windows | **cannot happen** | `cio`'s Windows `NewFIFOSetInDir` sets `Stderr: ""` itself when `terminal` is set, so no uncreated stream is ever named |
| Linux | **same defect, not yet hit** | nerdbox's `copyStreams` is `//go:build !windows` — it has no terminal case and opens whatever it is given, on Linux exactly as on macOS. Every Linux and Windows run recorded on this page captured its output, so `cfg.TTY` was false (`run.go` requires *both* stdin and stdout to be terminals) and all three FIFOs were created. The Mac was the first interactive run. |
| macOS | fails | first run with a real terminal on either end |

So "Windows and Linux work" was true and misleading: one is safe by construction, the other had
been exercised only in the configuration that avoids the bug.

**The fix** is `ioOpts` in `internal/sandbox/sandbox.go`: with a TTY, stderr is not passed to
cio. Nothing is lost — a pty *is* one stream, which `boks run` already told the user ("with a
pty the guest's output would come back … no distinct stderr"). It covers `boks run` and
`boks exec` together, because both go through the same helper.

#### What was verified by running, on Linux, and what was not

`internal/sandbox/io_test.go` asserts the invariant the shim actually depends on: **every path
Boks names in the request exists on disk by the time the request is sent.** It drives the real
`cio` machinery into a temp FIFO directory and stats what comes back. With the fix reverted it
fails, on this Linux host, with the macOS failure's exact shape — a `-stderr` path in a random
numeric subdirectory, missing, while stdin and stdout are there:

```
stderr is announced to the shim as ".../2690299100/boks-exec-test-stderr" but does not exist
```

That is the client half of the mechanism reproduced on hardware, not argued. **The other half —
that the shim then opens it and the whole thing now succeeds — has not been run.** There is no
`/dev/kvm` on this host and no Mac. The shim's behaviour is established by reading nerdbox at
the pinned `cd2c23f` and containerd v2.2.6, not by execution.

**The command a Mac user should run to settle it**, from a real terminal (both ends must be a
tty or the bug is not exercised):

```
boks run shell . -- sh -c 'echo out; echo err >&2'
```

Both lines should appear — on the console, since a pty carries them together — and the exit
status should be 0. The v0.1.0 build fails before running anything, naming a `-stderr` FIFO.
The negative control is the same command piped, `… | cat`: that takes the non-tty path and
succeeds on both builds, which is the whole reason the defect survived to a release.

### macOS, installed the way a user installs it, 2026-08-16

The first Boks install performed by an ordinary route rather than by building the tree:
`brew tap dagsommer/boks`, `brew trust`, `brew install boks`, on macOS arm64.

**`boks doctor` was green on the first try**, every check, including the one nothing else can
substitute for:

```
containerd            ok  2.3.3 at /var/run/containerd/containerd.sock
snapshotter tools     ok  /opt/homebrew/bin/mkfs.erofs,
                          /opt/homebrew/opt/e2fsprogs/sbin/mkfs.ext4 (erofs-utils 1.9.3)
vm runtime            ok  /opt/homebrew/bin/containerd-shim-nerdbox-v1
runtime skew          ok  containerd 2.3.3, shim built against v2.3.3
hypervisor library    ok  /opt/homebrew/lib/libkrun.dylib
guest image           ok  /opt/homebrew/lib/nerdbox-kernel-arm64, nerdbox-rootfs.erofs
runtime entitlement   ok  com.apple.security.hypervisor
```

`mkfs.ext4` resolving to `/opt/homebrew/opt/e2fsprogs/sbin` is the keg-only path working as
designed: Homebrew links that directory onto no PATH, and Boks appends it to the one it starts
containerd with. Installing the formula is enough, with no PATH edit — which is what the
`depends_on "e2fsprogs"` added earlier the same day was for.

**v0.1.0 then failed to run anything**, and v0.1.1 fixes it. On v0.1.1, from a real terminal:

```
$ boks run shell . -- sh -c 'echo out; echo err >&2'
network: nat · policy standard · 11 allow, 0 deny · no TLS interception
         unchanged since this sandbox last ran.
out
err
```

Both streams from a tty run, which is the configuration that broke: `cio` fills in all three
FIFO paths but does not create the stderr one under a terminal, and Boks announced it anyway.
Every previous Linux and Windows run had captured its output, so `cfg.TTY` was false and all
three FIFOs existed. **This is the first genuinely interactive run in the project's history**,
and it is why "verified on three platforms" did not catch a defect present on two of them.

Note also the second line of that output: the policy summary is the short form, because this
sandbox had been described before. That is `internal/cli/notice.go` behaving as intended
against a real repeat run rather than against a test.

**What this establishes.** All three platforms now run a sandbox from an artifact an ordinary
user can obtain: Windows from the release archive, Linux from the `.deb`, macOS from Homebrew.
Until today every result in this file came from a tree somebody had built.

**Not established here.** This run did not exercise the network from inside the guest, so the
macOS policy evidence remains the 2026-08-12 run rather than this one. `--rm` and `-d` were not
used. And the machine is Apple silicon; Intel Macs are still not shipped and `boks doctor` still
refuses them.

### winget delivery, tested locally before any submission, 2026-08-16

`winget install --manifest` against the rendered v0.1.1 manifests, on Windows 11. The zip's
SHA-256 matched the release digest, and winget reported `Successfully verified installer hash`.

**Installed and ran, unelevated.** `winget validate` ok; install in 8.68 s; `boks --version` →
`boks 0.1.1`; `boks doctor` green; `boks daemon start` serving `containerd 2.3.3+boks-erofs`;
and `boks run shell . -- uname -a` → `Linux (none) 6.12.44 … x86_64`, exit 0, 22.84 s cold.
Only one step raised UAC: `winget settings --enable LocalManifestFiles`, which is a one-time
setting for local manifests and is not part of a normal install.

**Every runtime path resolved inside the winget package directory** — `mkfs.erofs.exe`,
`mkfs.ext4.exe`, `containerd-shim-nerdbox-v1.exe`, `krun.dll`, both guest files, and
containerd itself. Nothing resolved to a containerd elsewhere on a machine that has other
tooling installed.

#### The question this was meant to answer was not answered, and that is the finding

There is **no symlink**. `WinGet\Links` was empty and not on `PATH`; winget put the package's
own directory on the User `PATH` instead. The cause was observed rather than guessed:
Developer Mode is off and the user cannot create symlinks —
`SYMLINK REFUSED: Administrator privilege required`. Three other winget packages on the same
machine are on `PATH` the same way, so this is winget's unprivileged fallback in general.

So `os.Executable()` + `EvalSymlinks` was never asked to traverse a link, and **the symlink
indirection remains untested**. Reproducing it needs elevation to create the link. This round
must not be read as clearing that risk.

#### SmartScreen does not fire on a winget install

Measured: no dialog, no prompt. winget verifies the installer hash itself and never
`ShellExecute`s the binary, so nothing consults the reputation check. A browser download of the
same archive **does** fire it, measured the same day. The manifest's note and `docs/install.md`
said this was unmeasured; both now say which route raises it.

#### Two defects, neither blocking

- **`winget uninstall dagsommer.boks` fails**: `No installed package found matching input
  criteria`. The package registers under an ARP id
  (`ARP\User\X64\dagsommer.boks__DefaultSource`), and uninstalling by that id works and cleans
  up completely — package directory gone, off `PATH`, `winget list boks` empty. Inferred, not
  measured: an artefact of `--manifest` installs having no source to resolve the identifier
  against. Worth one check after the package is in winget-pkgs rather than assuming.
- **`%LOCALAPPDATA%\boks` survives uninstall: 59 files, 1,768.8 MB.** That is Boks' own state —
  containerd root with unpacked images, `net`, `policy-log.jsonl`, `update.json`. winget does
  not own it, so leaving it is arguably correct, but 1.7 GB is not a rounding error and there
  is no `boks` command that removes it.

Also observed: the two streams arrive `err` then `out`. They are independently buffered, so
this is not an ordering guarantee and was not treated as one.
