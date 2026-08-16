#!/usr/bin/env bash
#
# verify-linux.sh — collect the evidence that Boks works on Linux, including inside WSL2.
#
#   scripts/verify-linux.sh [options]
#   scripts/verify-linux.sh --list          # what it checks, in order, without running anything
#   scripts/verify-linux.sh --print-probes  # the guest probes, for reading before you trust them
#
# WHY THIS EXISTS
#
# Boks' only end-to-end evidence is macOS on Apple silicon (docs/verification.md). Linux with
# KVM is the platform Boks is designed and built for, and no sandbox has ever booted on it.
# This script is the procedure in that document, mechanised, so the first person to run Boks
# on Linux produces a transcript that is worth something instead of an anecdote.
#
# WHAT IT IS CAREFUL ABOUT
#
# 1. It never reports success for a machine it did not test. Every result is pass, fail,
#    warn, indeterminate or skip, and only pass counts towards a verdict: "cannot determine"
#    and "with this caveat" are first-class outcomes and neither is a pass. VERIFIED also
#    requires every check in CHECK_LIST to have recorded a result, checked by name — a run
#    that recorded nothing at all used to satisfy "no failures" and print VERIFIED.
# 2. It stops at the first failure that makes the rest meaningless. No /dev/kvm means no VM,
#    so there is nothing to learn from twenty further checks that all fail for that one
#    reason.
# 3. It changes nothing on the machine: no installs, no configuration edits, no policy rules
#    written. See "WHAT IT DOES CHANGE" below for the two unavoidable exceptions, both of
#    which it announces before doing them.
# 4. The network check carries a positive control. A sandbox where *everything* is blocked
#    produces the same transcript as a working policy engine, so "denied host refused" on its
#    own is not evidence. The check requires an explicitly allowed destination to connect end
#    to end, with the origin's own certificate, in the same sandbox and the same process as
#    the refusal.
#
# WHAT IT DOES CHANGE
#
# - containerd's content store gains the base image, if it is not already there. Unavoidable:
#   there is no sandbox to observe without a rootfs to boot.
# - a temporary directory, by default under $TMPDIR, holds the workspace and the probe
#   scripts. It is removed on exit unless --keep.
#
# Boks' own state (policy log, CA, supervisor state) goes to a throwaway BOKS_STATE_DIR, so
# the run leaves the operator's real policy log untouched. Pass --host-state to use the real
# one instead, at the cost of writing decisions into it.
#
# WHAT IT CANNOT TEST
#
# - Whether the guest kernel is the one Boks shipped rather than one the host substituted.
#   The evidence here is that the guest's kernel identity is *its own*, not that it is any
#   particular kernel.
# - The credential-injection and TLS-interception paths (docs/verification.md covers those
#   against a real guest on macOS and against real origins on Linux without a hypervisor).
# - Anything about the persistent lifecycle beyond a single boot: stop/start snapshot
#   persistence, exec into a running sandbox, cp over vsock.
# - Whether a *stream*-link guest behaves like the datagram-link guest the macOS run used.
#   It exercises whatever link Boks asks for today; it cannot tell you the transport changed.
#
# See docs/verification.md for what each observation does and does not establish.

set -euo pipefail

readonly SCRIPT_NAME="verify-linux.sh"

# --- options ----------------------------------------------------------------------------

BOKS_BIN=""
WORKROOT=""
STATE_DIR=""
ALLOWED_HOST="example.com"
DENIED_HOST="www.google.com"
DENIED_IP="1.1.1.1"
CPUS_A=2
CPUS_B=1
MEMORY="2048m"
KEEP=0
HOST_STATE=0
SKIP_NETWORK=0
MODE="run"

usage() {
	cat <<'EOF'
verify-linux.sh — collect the evidence that Boks works on Linux (including WSL2).

Usage:
  scripts/verify-linux.sh [options]

Options:
  --boks PATH          the boks binary to test (default: boks on PATH, else ./bin/boks)
  --workdir DIR        where to create the temporary workspace (default: mktemp -d)
  --state-dir DIR      BOKS_STATE_DIR for the run (default: a throwaway dir)
  --host-state         use the operator's real Boks state dir instead of a throwaway one
  --allow-host HOST    the destination the policy permits      (default: example.com)
  --deny-host HOST     a destination the policy must refuse    (default: www.google.com)
  --deny-ip IP         an address the policy must refuse       (default: 1.1.1.1)
  --cpus N             vCPUs for the first boot                (default: 2)
  --memory SIZE        guest memory, boks units, e.g. 2048m    (default: 2048m)
  --skip-network       skip checks 7 and 8; the run is then INCOMPLETE, never VERIFIED
  --keep               leave sandboxes and the workspace behind for inspection
  --list               print the checks, in order, and exit without touching anything
  --print-probes       print the guest probe scripts and exit without touching anything
  -h, --help           this text

Exit status:
  0  VERIFIED    every declared check recorded a result and every result was a pass
  1  FAILED      a check failed; at least one thing Boks claims is not true here
  2  STOPPED or INCOMPLETE  a prerequisite is missing, something could not be determined,
                            or a declared check recorded nothing at all
EOF
}

die_usage() {
	printf '%s: %s\n\n' "$SCRIPT_NAME" "$1" >&2
	usage >&2
	exit 2
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--boks) BOKS_BIN="${2:?--boks needs a path}" && shift 2 ;;
	--workdir) WORKROOT="${2:?--workdir needs a directory}" && shift 2 ;;
	--state-dir) STATE_DIR="${2:?--state-dir needs a directory}" && shift 2 ;;
	--host-state) HOST_STATE=1 && shift ;;
	--allow-host) ALLOWED_HOST="${2:?--allow-host needs a hostname}" && shift 2 ;;
	--deny-host) DENIED_HOST="${2:?--deny-host needs a hostname}" && shift 2 ;;
	--deny-ip) DENIED_IP="${2:?--deny-ip needs an address}" && shift 2 ;;
	--cpus) CPUS_A="${2:?--cpus needs a number}" && shift 2 ;;
	--memory) MEMORY="${2:?--memory needs a size, e.g. 2048m}" && shift 2 ;;
	--skip-network) SKIP_NETWORK=1 && shift ;;
	--keep) KEEP=1 && shift ;;
	--list) MODE="list" && shift ;;
	--print-probes) MODE="probes" && shift ;;
	-h | --help)
		usage
		exit 0
		;;
	*) die_usage "unknown option: $1" ;;
	esac
done

[[ "$CPUS_A" =~ ^[0-9]+$ ]] || die_usage "--cpus must be a number, got: $CPUS_A"
[[ "$CPUS_A" -ge 1 ]] || die_usage "--cpus must be at least 1"
# The second boot deliberately asks for a different count from the first, because "nproc
# tracks --cpus" is only evidence if the two runs disagree with each other.
if [[ "$CPUS_A" -eq 1 ]]; then CPUS_B=2; fi

# --- reporting --------------------------------------------------------------------------
#
# No colour on purpose. This transcript is meant to be pasted into an issue or a document,
# and escape codes survive neither.

RESULTS=()
N_FAIL=0
N_WARN=0
N_INDET=0
N_SKIP=0

# Which of the declared checks have recorded at least one result. A verdict is only
# meaningful if every check in CHECK_LIST appears here, so this is what the verdict compares
# against — see "coverage" below. A space-delimited string rather than an associative array,
# because `declare -A` needs bash 4 and --list has to work wherever it is read.
COVERED_IDS=" "
CURRENT_CHECK="(none)"

hdr() {
	printf '\n=== %s\n' "$*"
}

# begin_check prints the section header AND names the check every result until the next one
# belongs to. Attribution is what lets the verdict tell "check 8 passed" from "check 8 never
# ran", which is the difference between a verdict and a guess.
begin_check() {
	CURRENT_CHECK="$1"
	shift
	hdr "$@"
}

say() {
	printf '  %s\n' "$*"
}

# evidence prints raw command output with a marker, so nothing in a transcript can be
# mistaken for something the script asserted rather than observed.
evidence() {
	if [[ $# -gt 0 ]]; then
		printf '%s\n' "$*" | sed 's/^/  | /'
		return
	fi
	sed 's/^/  | /'
}

record() {
	local status="$1" name="$2" note="${3:-}"
	RESULTS+=("$status|$name|$note")
	case "$COVERED_IDS" in
	*" $CURRENT_CHECK "*) : ;;
	*) COVERED_IDS="${COVERED_IDS}${CURRENT_CHECK} " ;;
	esac
	printf '  [ %-5s ] %s%s\n' "$status" "$name" "${note:+ — $note}"
}

pass() { record PASS "$1" "${2:-}"; }

# WARN is NOT a pass. It is the outcome for a check that ran, was not shown to be broken, and
# was not shown to work either — a caveat on the evidence. It was invisible to the verdict
# until 2026-08-16, which meant a --net none sandbox reporting an eth0 read as VERIFIED, so it
# is counted here and rolled into INCOMPLETE below alongside indeterminate and skip.
warn() {
	record WARN "$1" "${2:-}"
	N_WARN=$((N_WARN + 1))
}

fail() {
	record FAIL "$1" "${2:-}"
	N_FAIL=$((N_FAIL + 1))
}

# indeterminate is the outcome that keeps this script honest: the thing was not shown to
# work and was not shown to be broken. It is never rolled up into a pass.
indeterminate() {
	record "?????" "$1" "${2:-}"
	N_INDET=$((N_INDET + 1))
}

skip() {
	record SKIP "$1" "${2:-}"
	N_SKIP=$((N_SKIP + 1))
}

# stop ends the run because everything after it would fail for one reason. The remedy is
# printed in full: a verification kit that says "no" without saying "and here is how to get
# to yes" wastes the trip.
stop() {
	local name="$1" reason="$2" remedy="${3:-}"
	fail "$name" "$reason"
	printf '\n%s\n' "STOPPED: the remaining checks cannot mean anything until this is fixed."
	if [[ -n "$remedy" ]]; then
		printf '\n%s\n' "$remedy"
	fi
	summary
	exit 2
}

summary() {
	printf '\n=== Summary\n'
	local line status name note
	for line in "${RESULTS[@]}"; do
		status="${line%%|*}"
		name="${line#*|}"
		note="${name#*|}"
		name="${name%%|*}"
		printf '  [ %-5s ] %s%s\n' "$status" "$name" "${note:+ — $note}"
	done
	printf '\n'
}

# --- the checks, as data, so --list and the run cannot drift apart -----------------------
#
# Each entry is "<id>|<description>". The id is stable, is what begin_check names, and is
# what the verdict requires to have recorded a result. Before that requirement existed the
# run counted its own RESULTS array against itself — `TOTAL_CHECKS="${#RESULTS[@]}"` — which
# is a tautology: a harness that recorded nothing at all printed "checks recorded: 0" and
# then VERDICT: VERIFIED. These ten are the checks a verdict is about; nothing else counts.

CHECK_LIST=(
	"host-prereqs|0  host prerequisites          the tools this script itself needs                          GATE"
	"platform|1  platform and CPU            Linux, architecture, bare metal or WSL2, virt extensions    GATE"
	"kvm|2  KVM                         /dev/kvm exists and this user can open it                   GATE"
	"erofs|3  erofs                       the kernel filesystem and mkfs.erofs, whose version matters"
	"doctor|4  boks doctor                 Boks' own prerequisite report, and it must be ready         GATE"
	"boot|5  a sandbox boots as a VM     boot_id, uptime, vCPU count and virtio topology are its own GATE"
	"workspace|6  workspace                   shared at the exact host path; the parent is not"
	"network|7  network boundary            a denied destination refused AND an allowed one connected"
	"net-none|8  --net none                  no interface, nothing reachable"
	"cleanup|9  cleanup                     no container, task, shim, mount or stack left behind"
)

check_ids() {
	local entry
	for entry in "${CHECK_LIST[@]}"; do printf '%s\n' "${entry%%|*}"; done
}

if [[ "$MODE" == "list" ]]; then
	printf 'Checks, in order. A GATE stops the run when it fails.\n\n'
	printf '  %s\n' "${CHECK_LIST[@]#*|}"
	cat <<'EOF'

Check 7 is the one that decides whether Boks is a boundary at all, and it is the one most
easily faked into a pass. Two traps produce a false pass, both recorded in
docs/verification.md and both handled here:

  - the guest's proxy variables are LOWERCASE (http_proxy, https_proxy) as well as
    uppercase. A probe that unsets only HTTP_PROXY/HTTPS_PROXY still goes through the proxy
    and is correctly refused, which reads exactly like enforcement working. All eight
    variables are unset.
  - the base image has no wget, nslookup or nc, and its /bin/sh is dash, so /dev/tcp is
    unavailable. A probe using any of those fails with "command not found", which also reads
    as a pass. Every probe checks its tool exists first and reports a missing tool as
    indeterminate.

And the control that makes the whole check mean something: refusing everything would produce
the same transcript as judging each flow. Check 7 therefore requires an explicitly allowed
address to complete a TLS handshake and present the origin's own certificate, in the same
sandbox and the same process as the refusal of a denied address.
EOF
	exit 0
fi

# --- the guest probes --------------------------------------------------------------------
#
# Written into the workspace and executed from there by their absolute host path, which is
# also a small proof of the workspace sharing in its own right. They report through lines of
# the form "BOKSFACT <key> <value>", so a probe that dies halfway leaves a detectable gap
# rather than a plausible-looking partial answer.

# shellcheck disable=SC2016  # $ inside these is for the guest shell, not this one.
GUEST_BOOT_PROBE='
printf "BOKSFACT kernel %s\n" "$(uname -r)"
printf "BOKSFACT machine %s\n" "$(uname -m)"
printf "BOKSFACT boot_id %s\n" "$(cat /proc/sys/kernel/random/boot_id 2>/dev/null || echo MISSING)"
printf "BOKSFACT uptime %s\n" "$(cut -d" " -f1 /proc/uptime 2>/dev/null || echo MISSING)"
printf "BOKSFACT nproc %s\n" "$(nproc 2>/dev/null || echo MISSING)"
printf "BOKSFACT memtotal_kb %s\n" "$(grep MemTotal /proc/meminfo 2>/dev/null | tr -s " " | cut -d" " -f2)"
printf "BOKSFACT virtio %s\n" "$(ls /sys/bus/virtio/devices 2>/dev/null | tr "\n" "," || echo MISSING)"
printf "BOKSFACT proc_version %s\n" "$(cat /proc/version 2>/dev/null || echo MISSING)"
printf "BOKSFACT pid1 %s\n" "$(cat /proc/1/comm 2>/dev/null || echo MISSING)"
printf "BOKSFACT docker_sock %s\n" "$(test -e /var/run/docker.sock && echo present || echo absent)"
printf "BOKSFACT containerd_sock %s\n" "$(test -e /run/containerd/containerd.sock && echo present || echo absent)"
printf "BOKSFACT probe_complete boot\n"
'

# The parent path arrives as an environment variable rather than being interpolated into the
# command, so no quoting on this side can change what the guest lists.
# shellcheck disable=SC2016
GUEST_WORKSPACE_PROBE='
printf "BOKSFACT pwd %s\n" "$(pwd)"
printf "BOKSFACT host_marker %s\n" "$(cat host-wrote-this.txt 2>/dev/null || echo MISSING)"
if echo guest-wrote-this >guest-wrote-this.txt 2>/dev/null; then
	printf "BOKSFACT guest_write ok\n"
else
	printf "BOKSFACT guest_write refused\n"
fi
printf "BOKSFACT parent_listing %s\n" "$(ls -A "$BOKS_VERIFY_PARENT" 2>&1 | tr "\n" "," || echo MISSING)"
printf "BOKSFACT probe_complete workspace\n"
'

# shellcheck disable=SC2016
GUEST_NETNONE_PROBE='
printf "BOKSFACT interfaces %s\n" "$(ls /sys/class/net 2>/dev/null | tr "\n" "," || echo MISSING)"
if command -v curl >/dev/null 2>&1; then
	printf "BOKSFACT curl_allowed %s\n" "$(curl -s -o /dev/null -w "%{http_code}" --max-time 10 "https://$BOKS_VERIFY_ALLOWED/" 2>/dev/null || true)"
else
	printf "BOKSFACT curl_allowed NO_CURL\n"
fi
printf "BOKSFACT probe_complete netnone\n"
'

read -r -d '' GUEST_PROXY_PROBE <<'PROBE' || true
#!/bin/sh
# Check 7, the cooperating half: does the proxy allow what policy allows and refuse what it
# denies — and, the part that matters, does a guest that stops cooperating still get refused?
#
# The last question is the one with a trap in it. Boks sets SIX proxy variables in the guest
# (HTTP_PROXY, HTTPS_PROXY, NO_PROXY and all three lowercased). A probe that clears only the
# uppercase pair is still proxied, is still correctly refused, and reads exactly like
# enforcement working. Every one of them is cleared below, plus ALL_PROXY, which curl also
# honours.
say() { printf 'BOKSFACT %s %s\n' "$1" "$2"; }

if ! command -v curl >/dev/null 2>&1; then
	# A missing tool must never read as a block. Report it and let the caller call the
	# check indeterminate.
	say tooling NO_CURL
	exit 0
fi

http_code() {
	# Prints "<code> rc=<curl status>". curl writes 000 and exits non-zero when it never
	# got a response, so both halves are needed to tell a refusal from a 403.
	c=$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 "$1" 2>/dev/null)
	r=$?
	printf '%s rc=%s' "${c:-000}" "$r"
}

noproxy() {
	env -u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY -u NO_PROXY \
		-u http_proxy -u https_proxy -u all_proxy -u no_proxy "$@"
}

noproxy_http_code() {
	c=$(noproxy curl -s -o /dev/null -w '%{http_code}' --max-time 20 "$1" 2>/dev/null)
	r=$?
	printf '%s rc=%s' "${c:-000}" "$r"
}

# Recorded so the transcript proves the lowercase pair really was set, and really was the
# thing that had to be cleared.
say env_HTTP_PROXY "${HTTP_PROXY:-UNSET}"
say env_http_proxy "${http_proxy:-UNSET}"

say allowed_via_proxy "$(http_code "https://$BOKS_VERIFY_ALLOWED/")"
say denied_via_proxy "$(http_code "https://$BOKS_VERIFY_DENIED/")"
say denied_no_proxy "$(noproxy_http_code "https://$BOKS_VERIFY_DENIED/")"
say allowed_no_proxy "$(noproxy_http_code "https://$BOKS_VERIFY_ALLOWED/")"
say resolver "$(grep -s nameserver /etc/resolv.conf | tr '\n' ',')"
say probe_complete proxy
PROBE

read -r -d '' GUEST_RAW_PROBE <<'PROBE' || true
#!/usr/bin/env python3
"""Check 7, the decisive half: raw sockets, where the proxy cannot be involved at all.

A guest that ignores HTTP_PROXY and opens its own socket is the threat model. curl is not
used here because a socket carries no configuration a probe could forget to clear.

The positive control is the whole point of this file. A stack that refused every
non-proxied flow would produce exactly the transcript of a working policy engine, so
"denied address refused" proves nothing on its own. The allowed address must complete a
TLS handshake and present the ORIGIN's certificate — not Boks' CA, not the proxy's — in
this same process, moments away from the refusal.

Argument order: <allowed-ip> <allowed-host> <denied-ip> [<host-loopback-port>]
"""

import os
import socket
import ssl
import sys

PROXY_VARS = (
    "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
    "http_proxy", "https_proxy", "all_proxy", "no_proxy",
)


def fact(key, value):
    print("BOKSFACT %s %s" % (key, value), flush=True)


def issuer_of(cert):
    if not cert:
        return "none-presented"
    parts = []
    for rdn in cert.get("issuer", ()):
        for name, value in rdn:
            if name in ("organizationName", "commonName"):
                parts.append("%s=%s" % ("O" if name == "organizationName" else "CN", value))
    return ", ".join(parts) or "unnamed"


def tls(label, ip, port, sni):
    """Report the outcome as one of: refused, timeout, connected, or a TLS-level result.

    The exception TYPE is reported rather than swallowed, because a RST (refused before
    anything was dialled, which is what the policy forwarder produces) and a silent drop
    (timeout) are different behaviours and only one of them is judged.
    """
    try:
        sock = socket.create_connection((ip, port), 10)
    except Exception as exc:                                  # noqa: BLE001
        fact(label, "tcp_refused %s: %s" % (type(exc).__name__, exc))
        return
    try:
        ctx = ssl.create_default_context()
        if sni:
            wrapped = ctx.wrap_socket(sock, server_hostname=sni)
        else:
            ctx.check_hostname = False
            wrapped = ctx.wrap_socket(sock)
        fact(label, "connected issuer=%s" % issuer_of(wrapped.getpeercert()))
        wrapped.close()
    except ssl.SSLCertVerificationError as exc:
        # The TCP flow was carried — that is the containment-relevant fact — but the
        # certificate could not be checked, so it is not evidence about WHOSE endpoint
        # answered. Reported as its own outcome rather than folded into either verdict.
        fact(label, "tcp_connected_tls_unverified %s" % exc)
        sock.close()
    except Exception as exc:                                  # noqa: BLE001
        fact(label, "tcp_connected_tls_failed %s: %s" % (type(exc).__name__, exc))
        sock.close()


def tcp(label, ip, port):
    """A plain TCP connect and one HTTP GET, for the host-loopback probe.

    TLS would only add noise here: the listener on the host speaks plain HTTP, and the
    containment-relevant fact is whether the flow was carried at all.
    """
    try:
        sock = socket.create_connection((ip, port), 6)
    except Exception as exc:                                  # noqa: BLE001
        fact(label, "tcp_refused %s: %s" % (type(exc).__name__, exc))
        return
    try:
        sock.sendall(b"GET / HTTP/1.0\r\nHost: localhost\r\n\r\n")
        body = sock.recv(256).decode("latin-1").replace("\r", " ").replace("\n", " ")
        fact(label, "tcp_connected reply=%s" % body.strip()[:120])
    except Exception as exc:                                  # noqa: BLE001
        fact(label, "tcp_connected no_reply %s" % type(exc).__name__)
    finally:
        sock.close()


def udp(label, ip, port):
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.settimeout(6)
    # A minimal DNS query for example.com, so anything that answers had to be a resolver.
    query = (b"\xab\xcd\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00"
             b"\x07example\x03com\x00\x00\x01\x00\x01")
    try:
        sock.sendto(query, (ip, port))
        data, _ = sock.recvfrom(1024)
        fact(label, "answered %d bytes" % len(data))
    except Exception as exc:                                  # noqa: BLE001
        fact(label, "no_answer %s: %s" % (type(exc).__name__, exc))
    finally:
        sock.close()


def main():
    present = [name for name in PROXY_VARS if os.environ.get(name)]
    fact("proxy_vars_present", ",".join(present) or "none")
    for name in PROXY_VARS:
        os.environ.pop(name, None)
    # Python's socket and ssl modules never consult these anyway; clearing them is belt and
    # braces, and reporting which ones existed is the evidence that the lowercase trap was
    # real on this machine rather than assumed.
    fact("proxy_vars_cleared", ",".join(PROXY_VARS))

    allowed_ip, allowed_host, denied_ip = sys.argv[1], sys.argv[2], sys.argv[3]
    loopback_port = sys.argv[4] if len(sys.argv) > 4 else ""

    # POSITIVE CONTROL first, so a transcript that stops early is obviously incomplete
    # rather than looking like a clean sweep of refusals.
    tls("positive_control", allowed_ip, 443, allowed_host)
    tls("negative_control", denied_ip, 443, None)
    udp("udp_external_resolver", "8.8.8.8", 53)

    if loopback_port:
        tcp("host_loopback", "127.0.0.1", int(loopback_port))
    else:
        fact("host_loopback", "not_probed")

    try:
        fact("dns_resolves", socket.gethostbyname(allowed_host))
    except Exception as exc:                                  # noqa: BLE001
        fact("dns_resolves", "failed %s" % type(exc).__name__)

    fact("probe_complete", "raw")


main()
PROBE

read -r -d '' HOST_LOOPBACK_SERVER <<'SERVER' || true
#!/usr/bin/env python3
"""A host-side listener on 127.0.0.1, for the one probe that needs the host to be present.

Under libkrun's TSI — the runtime's default, and what Boks used before it wired its own
stack — a guest's 127.0.0.1 IS the host's, so a host loopback service answers the guest.
That is the failure this listener is here to detect. It binds port 0 and prints the port so
nothing has to guess a free one.
"""
import http.server
import socketserver


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):                                          # noqa: N802
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"host-loopback-reached\n")

    def log_message(self, *args):
        pass


server = socketserver.TCPServer(("127.0.0.1", 0), Handler)
print(server.server_address[1], flush=True)
server.serve_forever()
SERVER

if [[ "$MODE" == "probes" ]]; then
	printf '# --- guest: boot probe (sh -c) ---\n%s\n' "$GUEST_BOOT_PROBE"
	printf '# --- guest: workspace probe (sh -c) ---\n%s\n' "$GUEST_WORKSPACE_PROBE"
	printf '# --- guest: --net none probe (sh -c) ---\n%s\n' "$GUEST_NETNONE_PROBE"
	printf '# --- guest: proxy probe (sh) ---\n%s\n\n' "$GUEST_PROXY_PROBE"
	printf '# --- guest: raw-socket probe (python3) ---\n%s\n\n' "$GUEST_RAW_PROBE"
	printf '# --- host: loopback listener (python3) ---\n%s\n' "$HOST_LOOPBACK_SERVER"
	exit 0
fi

# --- fact extraction ----------------------------------------------------------------------
#
# A missing key returns the empty string, and every caller treats that as "cannot determine"
# rather than as a negative answer. That distinction is the whole reason the probes emit
# keyed lines instead of bare output.

fact_of() {
	local key="$1" text="$2"
	printf '%s\n' "$text" | sed -n "s/^BOKSFACT ${key} //p" | tail -n 1
}

probe_completed() {
	local expected="$1" text="$2"
	[[ "$(fact_of probe_complete "$text")" == "$expected" ]]
}

# --- run --------------------------------------------------------------------------------

CREATED_SANDBOXES=()
LOOPBACK_PID=""
TMPROOT=""

cleanup() {
	local rc=$?
	if [[ -n "$LOOPBACK_PID" ]]; then
		kill "$LOOPBACK_PID" 2>/dev/null || true
		wait "$LOOPBACK_PID" 2>/dev/null || true
	fi
	if [[ ${#CREATED_SANDBOXES[@]} -gt 0 && $KEEP -eq 0 && -n "$BOKS_BIN" ]]; then
		"$BOKS_BIN" rm -f "${CREATED_SANDBOXES[@]}" >/dev/null 2>&1 || true
	fi
	if [[ -n "$TMPROOT" && $KEEP -eq 0 ]]; then
		rm -rf "$TMPROOT"
	elif [[ -n "$TMPROOT" ]]; then
		printf '\nkept: %s\n' "$TMPROOT"
	fi
	exit $rc
}
trap cleanup EXIT

printf '%s\n' "$SCRIPT_NAME — evidence that Boks works on Linux"
printf 'started %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
cat <<'EOF'

This run changes two things on the machine and nothing else: containerd's content store
gains the Boks base image if it is not already there, and a temporary directory holds the
workspace. It installs nothing and edits no configuration. Sandboxes it creates are removed
before it exits.
EOF

# --- 0. host prerequisites (GATE) ---------------------------------------------------------

begin_check host-prereqs "0. Host prerequisites"

if [[ -z "$BOKS_BIN" ]]; then
	if command -v boks >/dev/null 2>&1; then
		BOKS_BIN="$(command -v boks)"
	elif [[ -x "./bin/boks" ]]; then
		BOKS_BIN="$(cd "$(dirname ./bin/boks)" && pwd)/boks"
	fi
fi

if [[ -z "$BOKS_BIN" || ! -x "$BOKS_BIN" ]]; then
	stop "boks binary" "not found" \
		"No boks binary. Build one from a checkout of this repository:

    make build            # writes ./bin/boks

then re-run this script, or point it at a binary with --boks PATH."
fi
say "boks: $BOKS_BIN"
"$BOKS_BIN" version 2>&1 | evidence || true
pass "boks binary" "$BOKS_BIN"

MISSING_TOOLS=()
for tool in python3 grep sed awk tr cut date mktemp; do
	command -v "$tool" >/dev/null 2>&1 || MISSING_TOOLS+=("$tool")
done
if [[ ${#MISSING_TOOLS[@]} -gt 0 ]]; then
	stop "host tools" "missing: ${MISSING_TOOLS[*]}" \
		"This script needs those on the host. python3 is the one most likely to be absent on
a minimal image, and it is not optional: it resolves the allowed destination and holds
the loopback listener that check 7 needs.

    sudo apt install python3       # or the equivalent for your distribution"
fi
pass "host tools" "python3 and coreutils present"

# --- 1. platform and CPU (GATE) ------------------------------------------------------------

begin_check platform "1. Platform and CPU"

KERNEL_NAME="$(uname -s)"
if [[ "$KERNEL_NAME" != "Linux" ]]; then
	stop "platform" "this is $KERNEL_NAME, not Linux" \
		"This script verifies the Linux path. On macOS follow the procedure in
docs/verification.md directly."
fi

ARCH="$(uname -m)"
HOST_KERNEL="$(uname -r)"
HOST_BOOT_ID="$(cat /proc/sys/kernel/random/boot_id 2>/dev/null || echo unavailable)"
HOST_UPTIME="$(cut -d' ' -f1 /proc/uptime 2>/dev/null || echo unavailable)"
HOST_NPROC="$(nproc 2>/dev/null || echo unavailable)"
CPU_VENDOR="$(sed -n 's/^vendor_id[[:space:]]*: //p' /proc/cpuinfo 2>/dev/null | head -n 1)"
[[ -n "$CPU_VENDOR" ]] || CPU_VENDOR="unknown (not an x86 cpuinfo)"

IN_WSL=0
if [[ -e /bin/wslinfo ]]; then
	IN_WSL=1
elif grep -qiE 'microsoft|wsl' /proc/sys/kernel/osrelease 2>/dev/null; then
	IN_WSL=1
fi

{
	printf 'kernel        %s\n' "$HOST_KERNEL"
	printf 'osrelease     %s\n' "$(cat /proc/sys/kernel/osrelease 2>/dev/null || echo unavailable)"
	printf 'arch          %s\n' "$ARCH"
	printf 'cpu vendor    %s\n' "$CPU_VENDOR"
	printf 'cpus          %s\n' "$HOST_NPROC"
	printf 'boot_id       %s\n' "$HOST_BOOT_ID"
	printf 'uptime (s)    %s\n' "$HOST_UPTIME"
} | evidence

if [[ $IN_WSL -eq 1 ]]; then
	say "detected: WSL2 (via /bin/wslinfo or the osrelease string)"
	if command -v wslinfo >/dev/null 2>&1; then
		# The LIVE mode, which is worth having because .wslconfig lies when mirrored mode
		# silently falls back to NAT.
		WSL_NET_MODE="$(wslinfo --networking-mode 2>/dev/null || true)"
		printf 'networking mode  %s\n' "${WSL_NET_MODE:-unavailable}" | evidence
	fi
	# WSL 2.5.1 introduced the modules image, without which KVM and erofs — both built as
	# modules — cannot be loaded at all. There is no way to read the WSL version from
	# inside the distribution, so this is a question for the operator, not a check.
	cat <<'EOF'

  This is WSL2, which means two things for the rest of this run:

    - WSL 2.5.1 is a hard floor. It introduced the modules image that makes any loadable
      module loadable, and both KVM and erofs are built as modules. Check on the Windows
      side with `wsl --version`; nothing inside the distribution can report it.
    - the sandbox is then a microVM inside the WSL2 utility VM. The boundary is still a
      hypervisor boundary, but the threat model now includes WSL2's own.

  See docs/troubleshooting.md#wsl2 and docs/windows.md.
EOF
	pass "platform" "Linux/$ARCH inside WSL2"
else
	say "detected: Linux, not WSL"
	pass "platform" "Linux/$ARCH, bare metal or an ordinary VM"
fi

# The vmx/svm discriminator is x86-only. On aarch64 the extension is EL2 and /proc/cpuinfo
# carries a "Features" line with no equivalent flag, so reporting "0" there would diagnose a
# problem that does not exist.
VIRT_FLAGS="unknown"
if [[ "$ARCH" == "x86_64" || "$ARCH" == "i686" ]]; then
	VIRT_FLAGS="$(grep -Ec '^flags.*\b(vmx|svm)\b' /proc/cpuinfo || true)"
	say "vmx/svm CPUs: $VIRT_FLAGS"
else
	say "vmx/svm discriminator: not applicable on $ARCH (the extension there is EL2)"
fi

# --- 2. KVM (GATE) --------------------------------------------------------------------------

begin_check kvm "2. KVM"

KVM_MISSING_REMEDY_BARE="Boks needs hardware virtualisation through KVM.

  - On bare metal: enable virtualisation (VT-x / AMD-V / EL2) in firmware and load the
    modules. Check what is loaded with:  lsmod | grep kvm
  - Inside a VM: enable nested virtualisation on the outer hypervisor. Some, including
    Apple Virtualization Framework guests, do not expose it at all."

KVM_MISSING_REMEDY_WSL_NOFLAG="This is WSL and the vCPU exposes no virtualisation extensions (no vmx/svm in
/proc/cpuinfo), so nested virtualisation is off at the Windows level. Nothing installed
inside the distribution can change that.

Note that nested virtualisation is already ON by default on Windows 11 x64, so adding it
to .wslconfig is usually NOT the fix. It is genuinely off only on Windows 10, on ARM64, on
a CPU predating Haswell or Zen, under safeMode=true, or under the AllowNestedVirtualization
enterprise policy.

If it really is disabled, in %UserProfile%\\.wslconfig on the Windows side — the section and
key are case-sensitive, and there is no equivalent in /etc/wsl.conf:

    [wsl2]
    nestedVirtualization=true

then 'wsl --shutdown' and reopen the distribution. A malformed .wslconfig is ignored
silently, so a typo'd stanza looks identical to not having set it."

KVM_MISSING_REMEDY_WSL_FLAG="This is WSL and the vCPU does expose virtualisation extensions, so nested virtualisation
is working — the KVM module is simply not loaded. WSL loads exactly three modules at boot
(tun, ip_tables, br_netfilter) and KVM is built as a module.

    sudo modprobe kvm_amd     # or kvm_intel on an Intel CPU
    sudo modprobe erofs       # Boks needs this one too, missing for the same reason

To persist, in %UserProfile%\\.wslconfig on the Windows side, then 'wsl --shutdown':

    [wsl2]
    loadKernelModules=kvm_amd,erofs

loadKernelModules is present in WSL's source but undocumented, so treat it as best-effort
and keep modprobe as the fallback. Loadable modules need WSL 2.5.1 or newer. 'nested=1' on
the KVM module is NOT needed."

KVM_PERM_REMEDY_WSL="This is WSL, where /dev/kvm exists but is root-owned and mode 0600: WSL runs no udev, so
the rule that would widen it on an ordinary distribution never runs.

    getent group kvm || sudo groupadd -r kvm   # it arrives with qemu/libvirt, not the base system
    sudo usermod -aG kvm \$USER

Then fix the node on every boot, in /etc/wsl.conf inside the distribution, and
'wsl --shutdown' on the Windows side:

    [boot]
    command = /bin/bash -c 'chown root:kvm /dev/kvm && chmod 660 /dev/kvm'

Do NOT use 'chmod 666 /dev/kvm', which many guides suggest: it lets every local account
create virtual machines on the host."

KVM_PERM_REMEDY_BARE="You do not have permission to open /dev/kvm.

    sudo usermod -aG kvm \$USER

then start a new login session. Do NOT 'chmod 666 /dev/kvm', which many guides suggest: it
lets every local account create virtual machines on the host."

if [[ ! -e /dev/kvm ]]; then
	say "/dev/kvm: absent"
	lsmod 2>/dev/null | grep -E '^kvm' | evidence || say "(no kvm modules loaded)"
	if [[ $IN_WSL -eq 1 && "$VIRT_FLAGS" == "0" ]]; then
		stop "kvm" "/dev/kvm missing, and no vmx/svm (WSL)" "$KVM_MISSING_REMEDY_WSL_NOFLAG"
	elif [[ $IN_WSL -eq 1 && "$VIRT_FLAGS" == "unknown" ]]; then
		# The discriminator that separates the two WSL causes is an x86 CPU flag, so on any
		# other architecture there is nothing to read and both remedies are printed rather
		# than one of them being asserted.
		stop "kvm" "/dev/kvm missing (WSL on $ARCH, where the vmx/svm discriminator does not apply)" \
			"This is WSL on $ARCH, so the /proc/cpuinfo flag that would say whether nested
virtualisation is exposed does not exist, and this script will not guess between the two
causes.

Note that WSL turns nested virtualisation OFF on ARM64 — its own default is
'not Arm64 and Windows 11 or above' — so on an ARM64 Windows device this is expected and
nothing inside the distribution can change it. Boks has no answer there today.

If this is neither ARM64 nor x86, the module is the likelier cause:

    sudo modprobe kvm_amd     # or kvm_intel on an Intel CPU
    sudo modprobe erofs

See docs/windows.md."
	elif [[ $IN_WSL -eq 1 ]]; then
		stop "kvm" "/dev/kvm missing, vmx/svm present (WSL)" "$KVM_MISSING_REMEDY_WSL_FLAG"
	else
		stop "kvm" "/dev/kvm missing" "$KVM_MISSING_REMEDY_BARE"
	fi
fi

# shellcheck disable=SC2012  # one fixed path with no surprising characters, and `ls -l
# /dev/kvm` is the line docs/troubleshooting.md tells people to run, so the transcript should
# contain the output they are told to look at.
ls -l /dev/kvm | evidence
lsmod 2>/dev/null | grep -E '^kvm' | evidence || say "(kvm not in lsmod; it may be built in)"

# Group membership is worth printing because the usual fix is a group the current login
# session predates, which looks identical to not having run usermod at all.
say "groups: $(id -nG)"

# The device is OPENED rather than tested with -r/-w, because the permission bits are not
# the whole answer: a node can be mode 0666 and still refuse with EPERM under an LSM or a
# seccomp filter — which was observed while writing this script — and that has a completely
# different remedy from a group membership problem. The errno is the diagnosis.
KVM_OPEN="$(python3 -c '
import errno
import os
try:
    fd = os.open("/dev/kvm", os.O_RDWR | os.O_CLOEXEC)
except OSError as exc:
    print("%s %s" % (errno.errorcode.get(exc.errno, exc.errno), exc.strerror))
else:
    os.close(fd)
    print("OK opened read-write")
' 2>&1)"
say "open(/dev/kvm, O_RDWR): $KVM_OPEN"

case "$KVM_OPEN" in
OK*) : ;;
EACCES*)
	if [[ $IN_WSL -eq 1 ]]; then
		stop "kvm" "/dev/kvm exists but this user cannot open it (WSL)" "$KVM_PERM_REMEDY_WSL"
	fi
	stop "kvm" "/dev/kvm exists but this user cannot open it" "$KVM_PERM_REMEDY_BARE"
	;;
EPERM*)
	stop "kvm" "/dev/kvm exists and opening it returned EPERM" \
		"The permission bits are not the obstacle — EPERM rather than EACCES means something
above the filesystem refused: an LSM (AppArmor, SELinux), a seccomp filter, or a
container/sandbox this shell is running inside. Group membership will not fix it.

If you are inside a container, run this on the host instead. Boks needs to open /dev/kvm
itself, so no amount of nesting makes this work."
	;;
ENODEV* | ENXIO*)
	stop "kvm" "/dev/kvm exists but the driver behind it does not ($KVM_OPEN)" \
		"The device node is there and nothing is answering it — usually a stale node, or a
node created by hand, with no kvm module loaded.

    lsmod | grep kvm
    sudo modprobe kvm_amd     # or kvm_intel on an Intel CPU"
	;;
*)
	stop "kvm" "/dev/kvm could not be opened: $KVM_OPEN" \
		"Opening the device failed in a way this script does not have specific advice for.
Report the errno above; 'boks doctor' will give its own diagnosis of the same failure."
	;;
esac

# What has NOT been established here is that the hypervisor works — only that the device
# opens. KVM_GET_API_VERSION is the test that settles that, and `boks doctor` already issues
# it (internal/doctor/virt_linux.go), so it is not repeated. Check 4 is where that answer
# comes from.
pass "kvm" "/dev/kvm opened read-write by $(id -un); doctor issues the version ioctl"

# --- 3. erofs -----------------------------------------------------------------------------

begin_check erofs "3. erofs"

# Two separate things, and doctor checks neither in this shape: the kernel filesystem the
# snapshotter's images are mounted through, and the userspace tool's VERSION. containerd's
# EROFS snapshotter needs mkfs.erofs 1.8 or later and Ubuntu 24.04 LTS ships 1.7.1, which
# passes doctor's existence check and then fails opaquely in the middle of an image unpack.
if grep -qw erofs /proc/filesystems 2>/dev/null; then
	pass "erofs kernel support" "erofs in /proc/filesystems"
elif modinfo erofs >/dev/null 2>&1; then
	warn "erofs kernel support" "module available but not loaded — 'sudo modprobe erofs'"
else
	warn "erofs kernel support" "not in /proc/filesystems and no module found; an unpack may fail"
fi

if command -v mkfs.erofs >/dev/null 2>&1; then
	EROFS_VERSION_LINE="$(mkfs.erofs -V 2>&1 | head -n 1 || true)"
	evidence "$EROFS_VERSION_LINE"
	EROFS_VERSION="$(printf '%s' "$EROFS_VERSION_LINE" | grep -oE '[0-9]+\.[0-9]+(\.[0-9]+)?' | head -n 1)"
	if [[ -z "$EROFS_VERSION" ]]; then
		warn "mkfs.erofs" "present, version unparseable from: $EROFS_VERSION_LINE"
	elif [[ "$(printf '1.8\n%s\n' "$EROFS_VERSION" | sort -V | head -n 1)" != "1.8" ]]; then
		warn "mkfs.erofs" "$EROFS_VERSION is older than 1.8; an image unpack is likely to fail"
	else
		pass "mkfs.erofs" "$EROFS_VERSION"
	fi
else
	warn "mkfs.erofs" "not on PATH — doctor will fail on this; apt install erofs-utils"
fi

# --- 4. boks doctor (GATE) ------------------------------------------------------------------

begin_check doctor "4. boks doctor"

if [[ -z "$STATE_DIR" && $HOST_STATE -eq 0 ]]; then
	TMPROOT="${WORKROOT:-$(mktemp -d "${TMPDIR:-/tmp}/boks-verify.XXXXXX")}"
	STATE_DIR="$TMPROOT/state"
	mkdir -p "$STATE_DIR"
elif [[ -z "$TMPROOT" ]]; then
	TMPROOT="${WORKROOT:-$(mktemp -d "${TMPDIR:-/tmp}/boks-verify.XXXXXX")}"
fi
if [[ $HOST_STATE -eq 0 ]]; then
	export BOKS_STATE_DIR="$STATE_DIR"
	say "BOKS_STATE_DIR=$BOKS_STATE_DIR (throwaway; the real policy log is untouched)"
else
	say "using the host's own Boks state dir (--host-state): decisions land in the real policy log"
fi

DOCTOR_RC=0
DOCTOR_OUT="$("$BOKS_BIN" doctor 2>&1)" || DOCTOR_RC=$?
printf '%s\n' "$DOCTOR_OUT" | evidence

if [[ $DOCTOR_RC -ne 0 ]]; then
	stop "boks doctor" "exited $DOCTOR_RC — the host is not ready" \
		"Every remedy doctor printed is above, in its own words. Fix them and re-run this
script. Nothing below check 4 can be observed until a sandbox can start."
fi
DOCTOR_WARNS="$(printf '%s\n' "$DOCTOR_OUT" | grep -c '[[:space:]]warn[[:space:]]*' || true)"
if [[ "$DOCTOR_WARNS" -gt 0 ]]; then
	pass "boks doctor" "ready, with $DOCTOR_WARNS warn(s) — see the transcript above"
else
	pass "boks doctor" "ready, no warnings"
fi

# --- workspace layout ----------------------------------------------------------------------
#
# The workspace lives one level down so that its PARENT can be listed from inside the guest.
# A sibling file in that parent is what makes "the parent is not exposed" a positive
# observation rather than an empty directory listing that proves nothing.

PARENT_DIR="$TMPROOT/parent"
WORKSPACE="$PARENT_DIR/workspace"
mkdir -p "$WORKSPACE"
printf 'written-on-the-host\n' >"$WORKSPACE/host-wrote-this.txt"
printf 'this file is outside the workspace and must not be visible in the guest\n' \
	>"$PARENT_DIR/sibling-must-not-be-visible.txt"
printf '%s' "$GUEST_PROXY_PROBE" >"$WORKSPACE/boks-proxy-probe.sh"
printf '%s' "$GUEST_RAW_PROBE" >"$WORKSPACE/boks-raw-probe.py"
chmod +x "$WORKSPACE/boks-proxy-probe.sh" "$WORKSPACE/boks-raw-probe.py"

if [[ $IN_WSL -eq 1 && "$WORKSPACE" == /mnt/* ]]; then
	# Attributed to check 6, whose evidence it qualifies — this runs between sections 4 and
	# 5, so without naming the check it would be filed under `doctor` and read as a caveat on
	# a report it has nothing to do with.
	CURRENT_CHECK=workspace
	warn "workspace location" "$WORKSPACE is under /mnt — WSL2 reaches it over 9p, and the guest then crosses 9p and virtiofs. Use --workdir under \$HOME"
	CURRENT_CHECK=doctor
fi

# --- 5. a sandbox boots, and it is a VM (GATE) ----------------------------------------------

begin_check boot "5. A sandbox boots, and what boots is a VM"

# The macOS run had it easy: the host was Darwin and the guest was Linux, so a shared kernel
# was not a possible explanation for anything. On Linux both are Linux, so kernel VERSION is
# weak evidence and boot_id is the load-bearing fact — a container shares the host's, a VM
# has its own — corroborated by an uptime bounded by the sandbox's age and a vCPU count that
# tracks --cpus across two runs that asked for different numbers.

# A sandbox left behind by an earlier --keep run, or by a crash, would be RE-ATTACHED to
# rather than created — and re-attaching ignores --cpus and --net, which are fixed at
# creation. That produces a transcript that looks like a measurement and is not one, so the
# names this script uses are cleared first. Nothing else is touched.
VERIFY_SANDBOX_NAMES=(
	boksverify-boot-a boksverify-boot-b boksverify-workspace
	boksverify-net-proxy boksverify-net-raw boksverify-net-none
)
STALE="$("$BOKS_BIN" ls -q 2>/dev/null | grep -x -F -f <(printf '%s\n' "${VERIFY_SANDBOX_NAMES[@]}") || true)"
if [[ -n "$STALE" ]]; then
	say "removing sandboxes left by an earlier run of this script: $(printf '%s' "$STALE" | tr '\n' ' ')"
	# shellcheck disable=SC2086  # deliberately word-split: these are validated names.
	"$BOKS_BIN" rm -f $STALE >/dev/null 2>&1 || true
fi

# The caller registers the sandbox, not this function: it is invoked in a command
# substitution, which is a subshell, and an array appended to in there does not survive.
run_boot_probe() {
	local name="$1" cpus="$2" out rc=0
	out="$("$BOKS_BIN" run shell "$WORKSPACE" --name "$name" --cpus "$cpus" -m "$MEMORY" \
		--net none -- sh -c "$GUEST_BOOT_PROBE" 2>&1)" || rc=$?
	printf '%s' "$out"
	return $rc
}

BOOT_RC=0
CREATED_SANDBOXES+=("boksverify-boot-a")
BOOT_A="$(run_boot_probe "boksverify-boot-a" "$CPUS_A")" || BOOT_RC=$?
printf '%s\n' "$BOOT_A" | evidence

if ! probe_completed boot "$BOOT_A"; then
	stop "sandbox boots" "the guest probe did not complete (boks exited $BOOT_RC)" \
		"The output above is everything the run produced. A sandbox that does not boot makes
every check below it unanswerable. Two Linux-specific causes worth eliminating first:

  - containerd's PATH is the daemon's, not your shell's, and it must contain the nerdbox
    _output directory — which supplies the shim AND the guest kernel and rootfs. The shim
    finds nerdbox-kernel-<arch> and nerdbox-rootfs.erofs by scanning PATH, not by looking
    next to itself.
  - mkfs.erofs older than 1.8 fails during the unpack rather than at the start.

See docs/troubleshooting.md."
fi

BOOT_RC=0
CREATED_SANDBOXES+=("boksverify-boot-b")
BOOT_B="$(run_boot_probe "boksverify-boot-b" "$CPUS_B")" || BOOT_RC=$?
printf '%s\n' "$BOOT_B" | evidence

GUEST_BOOT_ID_A="$(fact_of boot_id "$BOOT_A")"
GUEST_BOOT_ID_B="$(fact_of boot_id "$BOOT_B")"
GUEST_UPTIME_A="$(fact_of uptime "$BOOT_A")"
GUEST_NPROC_A="$(fact_of nproc "$BOOT_A")"
GUEST_NPROC_B="$(fact_of nproc "$BOOT_B")"
GUEST_VIRTIO="$(fact_of virtio "$BOOT_A")"
GUEST_KERNEL="$(fact_of kernel "$BOOT_A")"

{
	printf 'host  boot_id  %s\n' "$HOST_BOOT_ID"
	printf 'guest boot_id  %s   (run a, --cpus %s)\n' "${GUEST_BOOT_ID_A:-MISSING}" "$CPUS_A"
	printf 'guest boot_id  %s   (run b, --cpus %s)\n' "${GUEST_BOOT_ID_B:-MISSING}" "$CPUS_B"
	printf 'host  kernel   %s\n' "$HOST_KERNEL"
	printf 'guest kernel   %s\n' "${GUEST_KERNEL:-MISSING}"
	printf 'host  uptime   %s s\n' "$HOST_UPTIME"
	printf 'guest uptime   %s s\n' "${GUEST_UPTIME_A:-MISSING}"
	printf 'host  nproc    %s\n' "$HOST_NPROC"
	printf 'guest nproc    %s / %s   (asked for %s / %s)\n' \
		"${GUEST_NPROC_A:-MISSING}" "${GUEST_NPROC_B:-MISSING}" "$CPUS_A" "$CPUS_B"
} | evidence

if [[ -z "$GUEST_BOOT_ID_A" || "$GUEST_BOOT_ID_A" == "MISSING" ]]; then
	indeterminate "guest boot_id" "the guest reported no boot_id, so nothing can be concluded"
elif [[ "$GUEST_BOOT_ID_A" == "$HOST_BOOT_ID" ]]; then
	fail "guest boot_id" "identical to the host's — this is a shared kernel, not a VM"
elif [[ "$GUEST_BOOT_ID_A" == "$GUEST_BOOT_ID_B" ]]; then
	fail "guest boot_id" "the same across two runs — not a fresh kernel per sandbox"
else
	pass "guest boot_id" "differs from the host's and from the other run's"
fi

if [[ -z "$GUEST_UPTIME_A" || "$GUEST_UPTIME_A" == "MISSING" ]]; then
	indeterminate "guest uptime" "not reported"
elif awk -v g="$GUEST_UPTIME_A" -v h="$HOST_UPTIME" 'BEGIN{exit !(g < 120 && g < h)}'; then
	pass "guest uptime" "${GUEST_UPTIME_A}s, bounded by the sandbox's age rather than the host's"
else
	fail "guest uptime" "${GUEST_UPTIME_A}s — a container reports the host's uptime, and this looks like it"
fi

if [[ "$GUEST_NPROC_A" == "$CPUS_A" && "$GUEST_NPROC_B" == "$CPUS_B" ]]; then
	pass "vCPU topology" "nproc tracked --cpus across two runs ($CPUS_A then $CPUS_B)"
elif [[ -z "$GUEST_NPROC_A" || -z "$GUEST_NPROC_B" ]]; then
	indeterminate "vCPU topology" "nproc not reported"
else
	fail "vCPU topology" "asked for $CPUS_A/$CPUS_B, got ${GUEST_NPROC_A}/${GUEST_NPROC_B} — the request is advisory"
fi

if [[ -z "$GUEST_VIRTIO" || "$GUEST_VIRTIO" == "MISSING" ]]; then
	fail "virtio devices" "none present — nothing is being emulated for this guest"
else
	pass "virtio devices" "$GUEST_VIRTIO"
fi

if [[ "$(fact_of docker_sock "$BOOT_A")" == "absent" && "$(fact_of containerd_sock "$BOOT_A")" == "absent" ]]; then
	pass "no host runtime socket" "neither docker.sock nor containerd.sock in the guest"
else
	fail "no host runtime socket" "a host container-runtime socket is visible inside the guest"
fi

# --- 6. workspace --------------------------------------------------------------------------

begin_check workspace "6. Workspace"

WS_RC=0
CREATED_SANDBOXES+=("boksverify-workspace")
WS_OUT="$("$BOKS_BIN" run shell "$WORKSPACE" --name boksverify-workspace --net none \
	--env "BOKS_VERIFY_PARENT=$PARENT_DIR" -- sh -c "$GUEST_WORKSPACE_PROBE" 2>&1)" || WS_RC=$?
printf '%s\n' "$WS_OUT" | evidence

if ! probe_completed workspace "$WS_OUT"; then
	indeterminate "workspace" "the probe did not complete (boks exited $WS_RC)"
else
	if [[ "$(fact_of pwd "$WS_OUT")" == "$WORKSPACE" ]]; then
		pass "workspace path" "the guest's cwd is the host path $WORKSPACE, exactly"
	else
		fail "workspace path" "guest cwd is '$(fact_of pwd "$WS_OUT")', expected '$WORKSPACE'"
	fi

	if [[ "$(fact_of host_marker "$WS_OUT")" == "written-on-the-host" ]]; then
		pass "host to guest" "a file written on the host was read in the guest"
	else
		fail "host to guest" "the guest did not see the host's file"
	fi

	if [[ "$(fact_of guest_write "$WS_OUT")" == "ok" && -f "$WORKSPACE/guest-wrote-this.txt" ]]; then
		pass "guest to host" "a file written in the guest reached the host"
	else
		fail "guest to host" "the guest's write did not appear on the host"
	fi

	PARENT_LISTING="$(fact_of parent_listing "$WS_OUT")"
	if [[ "$PARENT_LISTING" == *"sibling-must-not-be-visible.txt"* ]]; then
		fail "parent not exposed" "the guest listed a sibling file outside the workspace: $PARENT_LISTING"
	elif [[ "$PARENT_LISTING" == *workspace* || "$PARENT_LISTING" == *"No such file"* ]]; then
		pass "parent not exposed" "the parent holds only the workspace: $PARENT_LISTING"
	else
		indeterminate "parent not exposed" "unrecognised listing: $PARENT_LISTING"
	fi
fi

# --- 7. the network boundary ------------------------------------------------------------------

begin_check network "7. The network boundary (verification.md check 6)"

if [[ $SKIP_NETWORK -eq 1 ]]; then
	skip "network boundary" "--skip-network was passed"
	# Attributed to check 8 rather than check 7, so the coverage requirement below sees both
	# checks answered. A SKIP recorded against the wrong id would leave check 8 looking as if
	# it had never run, which is a different — and worse — thing to report.
	CURRENT_CHECK=net-none
	skip "--net none" "--skip-network was passed"
	CURRENT_CHECK=network
else
	cat <<'EOF'
  This is the check that decides whether Boks is a boundary at all, and the one most easily
  faked into a pass. A stack that refused EVERYTHING would produce the same transcript as a
  policy engine that judges each flow, so the refusal of a denied destination is only
  evidence when an explicitly allowed destination connects end to end in the same sandbox.
EOF

	ALLOWED_IPS="$(python3 -c '
import socket, sys
try:
    addrs = sorted({i[4][0] for i in socket.getaddrinfo(sys.argv[1], 443, socket.AF_INET)})
except Exception as exc:
    print("RESOLVE_FAILED %s" % exc)
    raise SystemExit(0)
print(" ".join(addrs[:2]))
' "$ALLOWED_HOST" 2>&1)" || ALLOWED_IPS="RESOLVE_FAILED"

	if [[ "$ALLOWED_IPS" == RESOLVE_FAILED* || -z "$ALLOWED_IPS" ]]; then
		indeterminate "network boundary" "could not resolve $ALLOWED_HOST on the host: $ALLOWED_IPS"
		CURRENT_CHECK=net-none
		indeterminate "--net none" "not attempted without a reachable allowed host"
		CURRENT_CHECK=network
	else
		say "allowed host $ALLOWED_HOST resolves to: $ALLOWED_IPS"

		# --- 7a. the cooperating guest, through the proxy ---------------------------

		hdr "7a. Through the proxy, and then refusing to use it"
		PROXY_RC=0
		CREATED_SANDBOXES+=("boksverify-net-proxy")
		PROXY_OUT="$("$BOKS_BIN" run shell "$WORKSPACE" --name boksverify-net-proxy \
			--policy locked --allow "$ALLOWED_HOST" \
			--env "BOKS_VERIFY_ALLOWED=$ALLOWED_HOST" --env "BOKS_VERIFY_DENIED=$DENIED_HOST" \
			-- sh "$WORKSPACE/boks-proxy-probe.sh" 2>&1)" || PROXY_RC=$?
		printf '%s\n' "$PROXY_OUT" | evidence

		if [[ "$(fact_of tooling "$PROXY_OUT")" == "NO_CURL" ]]; then
			indeterminate "proxy path" "no curl in the guest image; a missing tool is not a block"
		elif ! probe_completed proxy "$PROXY_OUT"; then
			indeterminate "proxy path" "the probe did not complete (boks exited $PROXY_RC)"
		else
			ALLOWED_VIA_PROXY="$(fact_of allowed_via_proxy "$PROXY_OUT")"
			DENIED_VIA_PROXY="$(fact_of denied_via_proxy "$PROXY_OUT")"
			DENIED_NO_PROXY="$(fact_of denied_no_proxy "$PROXY_OUT")"

			if [[ "$(fact_of env_http_proxy "$PROXY_OUT")" == "UNSET" ]]; then
				warn "lowercase proxy variables" "http_proxy was not set in the guest; the trap this probe guards against did not apply here"
			else
				pass "lowercase proxy variables" "http_proxy was set, and the decisive probe cleared all eight"
			fi

			if [[ "$ALLOWED_VIA_PROXY" == 200* ]]; then
				pass "allowed via proxy" "$ALLOWED_HOST -> $ALLOWED_VIA_PROXY"
			else
				indeterminate "allowed via proxy" "$ALLOWED_HOST -> $ALLOWED_VIA_PROXY; without this working, a refusal proves nothing"
			fi

			if [[ "$DENIED_VIA_PROXY" == 200* ]]; then
				fail "denied via proxy" "$DENIED_HOST -> $DENIED_VIA_PROXY; the proxy served a denied destination"
			else
				pass "denied via proxy" "$DENIED_HOST -> $DENIED_VIA_PROXY"
			fi

			# The one that decides it on this half: a guest that stops cooperating.
			if [[ "$DENIED_NO_PROXY" == 200* ]]; then
				fail "denied with every proxy variable cleared" "$DENIED_HOST -> $DENIED_NO_PROXY; policy is ADVISORY, not enforced"
			else
				pass "denied with every proxy variable cleared" "$DENIED_HOST -> $DENIED_NO_PROXY"
			fi
		fi

		# --- 7b. raw sockets, with the positive control ------------------------------

		hdr "7b. Raw sockets: an allowed address must connect, a denied one must not"

		LOOPBACK_PORT=""
		printf '%s' "$HOST_LOOPBACK_SERVER" >"$TMPROOT/loopback-server.py"
		python3 "$TMPROOT/loopback-server.py" >"$TMPROOT/loopback.port" 2>/dev/null &
		LOOPBACK_PID=$!
		for _ in 1 2 3 4 5 6 7 8 9 10; do
			LOOPBACK_PORT="$(tr -d '[:space:]' <"$TMPROOT/loopback.port" 2>/dev/null || true)"
			[[ -n "$LOOPBACK_PORT" ]] && break
			sleep 0.3
		done
		if [[ -n "$LOOPBACK_PORT" ]]; then
			say "host loopback listener on 127.0.0.1:$LOOPBACK_PORT"
		else
			say "host loopback listener did not start; that sub-probe will report not_probed"
		fi

		ALLOW_ARGS=()
		for ip in $ALLOWED_IPS; do ALLOW_ARGS+=(--allow "$ip"); done
		FIRST_ALLOWED_IP="${ALLOWED_IPS%% *}"

		RAW_RC=0
		CREATED_SANDBOXES+=("boksverify-net-raw")
		RAW_OUT="$("$BOKS_BIN" run shell "$WORKSPACE" --name boksverify-net-raw \
			--policy locked --allow "$ALLOWED_HOST" "${ALLOW_ARGS[@]}" \
			-- python3 "$WORKSPACE/boks-raw-probe.py" \
			"$FIRST_ALLOWED_IP" "$ALLOWED_HOST" "$DENIED_IP" "$LOOPBACK_PORT" 2>&1)" || RAW_RC=$?
		printf '%s\n' "$RAW_OUT" | evidence

		if ! probe_completed raw "$RAW_OUT"; then
			indeterminate "raw socket boundary" "the probe did not complete (boks exited $RAW_RC); a probe that died is not a refusal"
		else
			POSITIVE="$(fact_of positive_control "$RAW_OUT")"
			NEGATIVE="$(fact_of negative_control "$RAW_OUT")"

			# 0 = the allowed flow was not carried at all, 1 = carried but the origin
			# was never confirmed, 2 = connected end to end with the origin's own
			# certificate, which is the control this check's header promises.
			POSITIVE_OK=0
			if [[ "$POSITIVE" == connected* ]]; then
				POSITIVE_OK=2
				pass "positive control" "$FIRST_ALLOWED_IP:443 $POSITIVE"
				if [[ "$POSITIVE" == *"O=Boks"* ]]; then
					warn "positive control certificate" "the issuer is Boks' own CA, so this flow was intercepted rather than carried to the origin"
				fi
			elif [[ "$POSITIVE" == tcp_connected* ]]; then
				# The flow was carried, which is the fact the negative control needs to
				# mean anything — but without a verified certificate this says nothing
				# about WHOSE endpoint answered, so it is not the control this check
				# claims to have run. It was a WARN until 2026-08-16, which the verdict
				# could not see, so an unverified positive control passed the negative
				# control through as a full PASS and the run reported VERIFIED.
				POSITIVE_OK=1
				indeterminate "positive control" "$FIRST_ALLOWED_IP:443 was carried but the certificate did not verify ($POSITIVE); the flow reached something, the origin is unconfirmed"
			else
				fail "positive control" "$FIRST_ALLOWED_IP:443 was allowed by policy and did not connect: $POSITIVE"
			fi

			if [[ "$NEGATIVE" == connected* || "$NEGATIVE" == tcp_connected* ]]; then
				fail "negative control" "$DENIED_IP:443 was reached from inside the sandbox: $NEGATIVE"
			elif [[ "$NEGATIVE" == tcp_refused* ]]; then
				if [[ $POSITIVE_OK -eq 2 ]]; then
					pass "negative control" "$DENIED_IP:443 $NEGATIVE, while the allowed address connected to the origin"
				elif [[ $POSITIVE_OK -eq 1 ]]; then
					indeterminate "negative control" "$DENIED_IP:443 was refused and the allowed address was carried, but its certificate never verified — the control this check requires did not run"
				else
					indeterminate "negative control" "$DENIED_IP:443 was refused, but so was the allowed address — indistinguishable from a stack that drops everything"
				fi
			else
				indeterminate "negative control" "unrecognised outcome: $NEGATIVE"
			fi

			UDP_OUT="$(fact_of udp_external_resolver "$RAW_OUT")"
			if [[ "$UDP_OUT" == answered* ]]; then
				fail "udp to an external resolver" "8.8.8.8:53 answered: $UDP_OUT"
			elif [[ -n "$UDP_OUT" ]]; then
				pass "udp to an external resolver" "8.8.8.8:53 $UDP_OUT"
			else
				indeterminate "udp to an external resolver" "not reported"
			fi

			LOOPBACK_OUT="$(fact_of host_loopback "$RAW_OUT")"
			if [[ "$LOOPBACK_OUT" == "not_probed" ]]; then
				skip "host loopback" "the host listener did not start"
			elif [[ "$LOOPBACK_OUT" == connected* || "$LOOPBACK_OUT" == tcp_connected* ]]; then
				fail "host loopback" "the guest reached a service on the HOST's 127.0.0.1: $LOOPBACK_OUT"
			else
				pass "host loopback" "127.0.0.1:$LOOPBACK_PORT $LOOPBACK_OUT"
			fi
		fi

		if [[ -n "$LOOPBACK_PID" ]]; then
			kill "$LOOPBACK_PID" 2>/dev/null || true
			wait "$LOOPBACK_PID" 2>/dev/null || true
			LOOPBACK_PID=""
		fi

		hdr "7c. The decisions, as Boks recorded them"
		# Every judgement above should appear here. A refusal that leaves no row never
		# reached the policy engine, which is precisely how check 6 failed on macOS on
		# 2026-08-12: the flow was carried by a forwarder that consulted nothing.
		"$BOKS_BIN" policy log 2>&1 | evidence || true

		# --- 8. --net none -------------------------------------------------------

		begin_check net-none "8. --net none"
		NONE_RC=0
		CREATED_SANDBOXES+=("boksverify-net-none")
		NONE_OUT="$("$BOKS_BIN" run shell "$WORKSPACE" --name boksverify-net-none --net none \
			--env "BOKS_VERIFY_ALLOWED=$ALLOWED_HOST" \
			-- sh -c "$GUEST_NETNONE_PROBE" 2>&1)" || NONE_RC=$?
		printf '%s\n' "$NONE_OUT" | evidence

		if ! probe_completed netnone "$NONE_OUT"; then
			indeterminate "--net none" "the probe did not complete (boks exited $NONE_RC)"
		else
			IFACES="$(fact_of interfaces "$NONE_OUT")"
			CURL_NONE="$(fact_of curl_allowed "$NONE_OUT")"
			if [[ "$CURL_NONE" == "NO_CURL" ]]; then
				indeterminate "--net none" "no curl in the guest; interfaces were: $IFACES"
			elif [[ "$IFACES" == "lo," && "$CURL_NONE" != "200" ]]; then
				pass "--net none" "only lo, and the allowed host gave $CURL_NONE"
			elif [[ "$CURL_NONE" == "200" ]]; then
				fail "--net none" "a sandbox with no network reached $ALLOWED_HOST"
			else
				# A sandbox that asked for no network and reports a NIC anyway is the
				# thing this check exists to find. curl not reaching the allowed host
				# says nothing about whether that interface can carry traffic
				# elsewhere, so this is "not determined", not "fine". It was a WARN
				# until 2026-08-16, and WARN was invisible to the verdict.
				indeterminate "--net none" "interfaces were '$IFACES' (expected 'lo,'), curl gave $CURL_NONE — an interface is present that should not be"
			fi
		fi
	fi
fi

# --- 9. cleanup ------------------------------------------------------------------------------

begin_check cleanup "9. Cleanup"

if [[ ${#CREATED_SANDBOXES[@]} -gt 0 ]]; then
	"$BOKS_BIN" rm -f "${CREATED_SANDBOXES[@]}" 2>&1 | evidence || true
fi

LEAKS=()
REMAINING="$("$BOKS_BIN" ls -q 2>/dev/null || true)"
for name in "${CREATED_SANDBOXES[@]}"; do
	if printf '%s\n' "$REMAINING" | grep -qx "$name"; then
		LEAKS+=("sandbox $name still listed by boks ls")
	fi
	if pgrep -af 'containerd-shim-nerdbox' 2>/dev/null | grep -q "$name"; then
		LEAKS+=("a nerdbox shim process for $name")
	fi
	if grep -q "$name" /proc/mounts 2>/dev/null; then
		LEAKS+=("a mount for $name in /proc/mounts")
	fi
done

NET_LS="$("$BOKS_BIN" net ls 2>&1 || true)"
printf '%s\n' "$NET_LS" | evidence
for name in "${CREATED_SANDBOXES[@]}"; do
	if printf '%s\n' "$NET_LS" | grep -q "$name"; then
		LEAKS+=("a network stack for $name")
	fi
done

# ctr is optional, and when it can reach containerd it sees what boks cannot round off. It is
# often unable to — containerd's socket is usually root-owned while Boks is run rootless — so
# a failure here is silence rather than noise, and the boks-level checks above stand alone.
if command -v ctr >/dev/null 2>&1; then
	if CTR_CONTAINERS="$(ctr -n boks containers ls 2>/dev/null)"; then
		printf 'ctr -n boks containers ls:\n%s\n' "$CTR_CONTAINERS" | evidence
		ctr -n boks tasks ls 2>/dev/null | evidence || true
		for name in "${CREATED_SANDBOXES[@]}"; do
			if printf '%s\n' "$CTR_CONTAINERS" | grep -q "$name"; then
				LEAKS+=("a containerd container for $name")
			fi
		done
	else
		say "ctr could not reach containerd in namespace boks; skipping that cross-check"
	fi
fi

if [[ ${#LEAKS[@]} -eq 0 ]]; then
	pass "cleanup" "no sandbox, shim, mount or network stack left behind"
else
	printf '%s\n' "${LEAKS[@]}" | evidence
	fail "cleanup" "${#LEAKS[@]} thing(s) left behind, listed above"
fi

# --- verdict ------------------------------------------------------------------------------

summary

# COVERAGE. Counting results against themselves — the old `TOTAL_CHECKS="${#RESULTS[@]}"` —
# answers "how many results are in the list of results", which is true of any list including
# the empty one. The question a verdict needs answered is whether each of the ten checks
# CHECK_LIST declares recorded anything at all, so that is what is asked here, and a check
# that did not is named. No verdict is printed before this runs.
UNANSWERED=()
DECLARED=0
while IFS= read -r check_id; do
	DECLARED=$((DECLARED + 1))
	case "$COVERED_IDS" in
	*" $check_id "*) : ;;
	*) UNANSWERED+=("$check_id") ;;
	esac
done < <(check_ids)
ANSWERED=$((DECLARED - ${#UNANSWERED[@]}))
if [[ $DECLARED -eq 0 ]]; then
	# Reachable only if CHECK_LIST or check_ids has been broken. A coverage requirement that
	# silently has nothing to require is the same tautology this replaced, so it says so and
	# refuses rather than falling through to a verdict.
	printf 'BROKEN: CHECK_LIST declared no checks, so nothing here can be a verdict.\n' >&2
	exit 2
fi

printf 'host: %s %s, %s, %s\n' "$KERNEL_NAME" "$HOST_KERNEL" "$ARCH" \
	"$([[ $IN_WSL -eq 1 ]] && echo 'inside WSL2' || echo 'not WSL')"
printf 'boks: %s\n' "$BOKS_BIN"
printf 'checks answered: %s of %s declared; %s results recorded (%s failed, %s warned, %s indeterminate, %s skipped)\n' \
	"$ANSWERED" "$DECLARED" "${#RESULTS[@]}" "$N_FAIL" "$N_WARN" "$N_INDET" "$N_SKIP"
if [[ ${#UNANSWERED[@]} -gt 0 ]]; then
	printf 'checks that recorded NOTHING: %s\n' "${UNANSWERED[*]}"
fi
printf '\n'

if [[ $N_FAIL -gt 0 ]]; then
	cat <<'EOF'
VERDICT: FAILED.

At least one thing Boks claims is not true on this machine. Paste this whole transcript
into the run's record; the failures above are the evidence, and docs/verification.md is
where they belong.
EOF
	exit 1
fi

if [[ ${#UNANSWERED[@]} -gt 0 || $N_WARN -gt 0 || $N_INDET -gt 0 || $N_SKIP -gt 0 ]]; then
	cat <<'EOF'
VERDICT: INCOMPLETE.

Nothing failed, and that is not the same as everything passing. The checks marked ?????,
WARN or SKIP were not answered — and any check listed above as having recorded NOTHING did
not run at all, which is the same thing said more loudly. This run does not establish that
Boks works here. Resolve them and run it again rather than reporting a pass.
EOF
	exit 2
fi

cat <<'EOF'
VERDICT: VERIFIED.

Every check ran and passed on this machine. What that establishes, precisely: a sandbox
booted behind KVM with a kernel identity, uptime and vCPU topology of its own; the workspace
was shared at its exact host path and its parent was not; a destination the policy denied
was refused on a raw socket while a destination it allowed connected end to end in the same
sandbox; and teardown left nothing behind.

What it does not establish is in the header of this script. Record the transcript in
docs/verification.md with the date and the hardware — an unrecorded pass is an anecdote.
EOF
