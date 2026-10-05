# Headless setup and approvals (no GUI)

[中文版](headless.zh.md)

This page is for agents and scripts that must bring up a Companion on a
machine with no desktop window: register a folder, pair the device,
approve the first write — all from the CLI, all verifiable locally.

Full sequence (each step prints what the next one needs):

```sh
# 0. Pick private directories. serve binds loopback only.
export FYLANE_DATA_DIR=/tmp/fylane-headless-test/data
WS=/tmp/fylane-headless-test/ws
mkdir -p "$WS"

# 1. Register the folder (idempotent: reruns verify and exit 0).
fylane-companion init --workspace "$WS" --non-interactive

# 2. Pair with the relay (needs the relay reachable; prints a code for the
#    platform's connection page, plus a PAIR_CODE=... line for scripts).
fylane-companion pair -relay https://relay.example \
  -data-dir "$FYLANE_DATA_DIR" --non-interactive --ttl 10m

# 3. Serve it (loopback only; keep it running in the background).
fylane-companion serve -data-dir "$FYLANE_DATA_DIR" &
SERVE_PID=$!

# 4. List pending approvals (empty until the first write arrives).
fylane-companion approvals --json

# 5. Decide one (same Resolve the desktop window uses).
fylane-companion approve --json <change-set-id>
# ... or refuse it:
fylane-companion reject --json <change-set-id>

kill $SERVE_PID
```

`--json` contract (all new commands): success exits 0 with exactly one
JSON document on stdout; any human explanation goes to stderr. Without
`--json`, human lines go to stdout, and `pair` appends one trailing
machine-readable line:

```
PAIR_CODE=ABCD-1234 EXPIRES_IN=10m0s CONNECTOR_URL=https://relay.example/mcp
```

Secrets (pairing codes, tokens) appear only on those two surfaces —
never in logs. The `PAIR_CODE=` line carries the code because the script
must show it to the user; treat it like a password on the way through.

## Environment variables

| Variable | Used by | Meaning |
|---|---|---|
| `FYLANE_DATA_DIR` | every subcommand | Data directory override (same convention as the desktop shell). A `-data-dir` flag wins over it. |
| `FYLANE_TUNNEL_TOKEN` | `serve` (legacy shared-token mode) | Tunnel credential; takes precedence over device credentials, never stored. |
| `FYLANE_TUNNEL_PROVIDER_TOKEN` | tunnel providers | Provider credential for tunnels Fylane starts (direct mode). Lives in the OS keychain, never in config files. |
| `FYLANE_DEVICE_CREDENTIALS_FILE` | `pair`, `serve` | **Opt-in fallback** for device credentials when no OS keychain exists (headless servers). JSON object mapping `relay:<host>` to `{"device_id": ..., "device_secret": ...}`. The file must be owner-only (0600); wider permissions are refused. |

## When there is no keychain (risk note)

Device credentials live in the OS keychain by default and never in config
files or the database. A headless Linux server often has no keychain
(no D-Bus secret service). There is **no silent fallback**: without
`FYLANE_DEVICE_CREDENTIALS_FILE`, `pair` and `serve` fail loudly and the
error names the variable.

Setting the variable stores the device secret as **plaintext at rest**.
That is weaker than a keychain in exactly one way: anyone who can read the
file owns the device identity (can request pairing codes and hold the
tunnel open). Mitigations, in order:

1. Prefer a real keychain (e.g. run GNOME Keyring even headless).
2. Keep the file at 0600 on a disk only root/that user reads; never back
   it up to shared storage, never log it, never pass it on a command line.
3. Prefer `FYLANE_TUNNEL_TOKEN` legacy mode only if you understand it
   grants the same power with fewer moving parts — it does not reduce it.

Encryption of the fallback file at rest is **not implemented** (open gap,
see below); the mitigations above are the whole protection today.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Success (`init` also returns 0 when the workspace was already registered). |
| `1` | Operational failure: relay refused, daemon not running (no `control.json`), unknown flag values the relay/server rejected, ... |
| `1` | `pair --help` (and any subcommand `--help`: flag package help requested). |
| `2` | Usage error: missing `--workspace`, bad `-ttl`, wrong argument count, ... |
| `2` | Top-level `--help` (usage to stderr; no subcommand matched). |
| `3` | Target unknown: `approve`/`reject` for an id that is unknown or already decided. |

`--help` works on every subcommand (`-h` included, standard flag package).
Both help cases are native flag-package behavior: a help request makes
flag parsing return `ErrHelp`, which maps to exit 1 for subcommands
(the top-level dispatcher prints usage and exits 2).

## Worked end-to-end example (loopback only)

The whole flow is verifiable on one machine: run a local relay, pair
against it, serve, push a write through approval, decide it.

```sh
export FYLANE_DATA_DIR=/tmp/fylane-headless-test/data
export FYLANE_DEVICE_CREDENTIALS_FILE=/tmp/fylane-headless-test/creds.json
WS=/tmp/fylane-headless-test/ws
mkdir -p "$WS"
echo "v1" > "$WS/a.txt"

# Local relay (OAuth mode, in-memory store) on loopback.
fylane-relay serve -addr 127.0.0.1:19000 -issuer http://127.0.0.1:19000 &
RELAY_PID=$!

fylane-companion init --workspace "$WS" --non-interactive
# workspace ready: ws (ws_...) at /tmp/fylane-headless-test/ws
# next: pair this device, then serve it: ...

fylane-companion pair -relay http://127.0.0.1:19000 \
  --non-interactive --ttl 10m --data-dir "$FYLANE_DATA_DIR"
# PAIR_CODE=... EXPIRES_IN=10m0s CONNECTOR_URL=http://127.0.0.1:19000/mcp

fylane-companion serve -addr 127.0.0.1:18787 \
  -relay ws://127.0.0.1:19000/tunnel -data-dir "$FYLANE_DATA_DIR" &
SERVE_PID=$!
sleep 2  # wait for control.json

fylane-companion approvals --json
# {"approvals":[]}

# A write that needs approval: create b.txt via the control API
# (5s block budget, then pending_approval with a change_set_id).
TOKEN=$(python3 -c "import json;print(json.load(open('$FYLANE_DATA_DIR/control.json'))['token'])")
ADDR=$(python3 -c "import json;print(json.load(open('$FYLANE_DATA_DIR/control.json'))['addr'])")
curl -s -X POST http://$ADDR/v1/save \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"provider":"local","summary":"headless e2e","files":[{"path":"b.txt","content_base64":"aGVsbG8K"}]}'
# {"status":"pending_approval","change_set_id":"chg_...","pending":true,...}

fylane-companion approvals --json
# {"approvals":[{"change_set_id":"chg_...","kind":"write",...}]}

fylane-companion approve --json chg_...
# {"approved":true,"change_set_id":"chg_...","resolved":true}

# Retry the same change set: the recorded decision replays and the write lands.
curl -s -X POST http://$ADDR/v1/save \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"provider":"local","summary":"headless e2e","change_set_id":"chg_...","files":[{"path":"b.txt","content_base64":"aGVsbG8K"}]}'
# {"status":"applied",...}

cat "$WS/b.txt"   # hello
fylane-companion approvals --json
# {"approvals":[]}

kill $SERVE_PID $RELAY_PID
```

Notes on the example: `serve` needs no `-workspace` here because `init`
already selected the current one; the relay's 10-minute pairing-code TTL
caps `--ttl` (the flag states intent, the relay decides); the approve
→ retry shape is the normal platform loop (budget expiry, then retry with
the same `change_set_id`), not a test artifact.

## Known gaps

- `FYLANE_DEVICE_CREDENTIALS_FILE` is plaintext at rest; no encrypted
  fallback exists yet. Until one does, the mitigations above apply.
- `pair` still needs a relay (`-relay` is required). Relay-less machines
  use `init` + `serve` in direct mode and pair from the served surface.
- `init --approval-mode` is only written for a fresh data dir; changing
  policy later stays a Safety-page / `POST /v1/safety` action.
