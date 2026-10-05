<div align="center">

<img src="assets/icon.png" width="96" alt="mcp-lane">

# MCP Lane (mcp-lane)

**Let ChatGPT, Claude, Grok and Gemini on the web read and edit a project on your computer or your VPS. Every edit and command waits for your OK first.**

English | [简体中文](README.zh-CN.md)

[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.25-00ADD8)](go.mod)
[![MCP](https://img.shields.io/badge/protocol-MCP-6E56CF)](https://modelcontextprotocol.io)

</div>

<br>

## What it is

The AI in your browser cannot see the project on your computer. To get a file
changed, you paste code in and paste the answer back.

mcp-lane runs on your computer. Pick a folder, and the AI in your browser can
connect to it: read files, edit code, run tests.

The AI can only send requests. mcp-lane makes the actual change or runs the
command on your machine, and shows it to you in its window first. Nothing
happens until you approve.

mcp-lane is a fork of [leazoot/fylane](https://github.com/leazoot/fylane).
It keeps the same protocol, the same binary names (`fylane-companion`,
`fylane-relay`) and the Apache-2.0 license, and adds headless operation,
security hardening and a reworked approval lane — see
[What's new in this fork](#whats-new-in-this-fork).

![A write waiting for approval](assets/approval.png)

What you can do with it:

- Ask ChatGPT "where is this error thrown" and let it read the project. No pasting.
- Let Claude edit three files, check the changes in mcp-lane, then approve.
- Let Grok run `npm test` and read the results back.
- Works for a project on a VPS too, and you still approve on this computer.
  See [Remote machines](#remote-machines).
- Open a new chat tomorrow and carry on where you left off. See [Memory](#memory).
- Close your laptop and the AI can no longer reach it.

How it connects:

```
AI in the browser  ──►  public address (tunnel or relay)  ──►  mcp-lane on your computer  ──►  the folder you chose
                                                                              │
                                                                      approval happens here
```

Only mcp-lane on your computer touches your files. The public address in the
middle just passes messages along and stores nothing.

When the project is on a VPS, there is one more hop over ssh:

```
AI in the browser  ──►  public address  ──►  mcp-lane on your computer  ──ssh──►  mcp-lane on the VPS  ──►  the folder on the server
                                                          │
                                                  approval still happens here
```

The mcp-lane on the VPS is not open to the internet. Only your computer can reach
it, over ssh. Nothing changes on the AI platform: it connects to the same
address.

## What's new in this fork

### Headless CLI: no window needed

A server or an agent script can do the whole setup from the terminal —
register a folder, pair the device, list and decide approvals — through the
same approval path the desktop window uses, never a second semantics:

```sh
fylane-companion init --workspace ~/projects/my-app --non-interactive
fylane-companion pair -relay https://relay.example.com --non-interactive --ttl 10m
fylane-companion serve -data-dir ~/.fylane-data &
fylane-companion approvals --json
fylane-companion approve --json <change-set-id>
fylane-companion reject --json <change-set-id>
```

- `init` registers a folder (idempotent: reruns verify and exit 0).
  `--approval-mode safe|balanced` sets the policy for a fresh data dir only.
- `pair` registers the device and prints a pairing code plus one trailing
  machine-readable line (`PAIR_CODE=... EXPIRES_IN=... CONNECTOR_URL=...`).
  `-relay` is required; `--ttl` states the wanted code lifetime, the relay's
  own TTL still caps it.
- `approvals` lists what is waiting; `approve`/`reject` take one
  `<change-set-id>` and resolve it through the desktop's Resolve.
- `--json` contract: success exits 0 with exactly one JSON document on
  stdout; any human explanation goes to stderr. Without `--json`, human lines
  go to stdout.
- Exit codes: `0` success · `1` operational failure (relay refused, daemon
  not running, …) · `2` usage error (bad flags, wrong argument count, …) ·
  `3` target unknown (an id that is unknown or already decided).

Full sequence with a local relay, environment variables and the no-keychain
risk note: [docs/headless.md](docs/headless.md).

### Security hardening

- **Access tokens live 30 minutes** (was 2 hours upstream), bounding the
  replay window of a stolen token. A relay operator can tune it with
  `fylane-relay serve -access-ttl` (floor 5 minutes); platforms that never
  refresh see `401`s after expiry until they reconnect, and the rotating
  refresh token covers the ones that do.
- **Caddy access logs are desensitized.** Next to the existing query-string
  and `Authorization` redaction, the legacy capability token travelling in
  the path (`/mcp/<token>`) is redacted too — only the segment's existence is
  logged, never its value. See [`deploy/Caddyfile`](deploy/Caddyfile).
- **Tunnel concurrency is capped.** The companion serves at most 32 tunneled
  requests at once; past the cap a request is refused at once with a
  retryable 503 instead of queueing without bound behind handlers that may
  block on a human approval.

### Lane experience

- **Risk strip on every approval card**: each request is graded low, medium
  or high with one plain sentence saying why, before you decide.
- **Approval queue**: when several requests wait, the card shows its position
  (`2 of 5`), with previous/next stepping and an approve-all button.
- **Keyboard shortcuts**: `a` approves, `d` rejects, `e` opens the details.
  They fire only while the card is on screen — never from a text field, never
  with a modifier held, never under an open dialog.

## Install

Download from [Releases](https://github.com/dotpopo/mcp-lane/releases/latest):

| System | Download |
| --- | --- |
| macOS | `fylane-desktop-macos.dmg`, drag into Applications |
| Windows | `fylane-desktop-windows-amd64.zip`, unzip and run `Fylane.exe` |
| Linux / servers | command line only for now, see [Command line](#command-line) |

Binary names are unchanged from upstream (`fylane-companion`,
`fylane-relay`, `fylane-desktop-*`), so scripts written for Fylane keep
working.

The packages are not signed yet. On first open, macOS says the developer cannot
be verified: go to System Settings → Privacy & Security and click Open Anyway.
On Windows, when SmartScreen appears, click More info → Run anyway. To check
the files first, see `SHA256SUMS` on the release page.

## First run

Pick one of the two. They end in the same place.

### A. In the window (no terminal needed)

mcp-lane walks you through four steps in its window.

![Step 1: choose a folder](assets/first-run.png)

1. **Choose a folder.** The AI can see this folder and nothing above it. You
   can change it or take it back at any time.
2. **Try one write.** mcp-lane writes a sample file into the folder so you can
   see what approving looks like.
3. **Decide what needs asking.** By default mcp-lane asks once per folder before
   running commands, then ordinary commands just run. File writes and risky
   commands still ask. You can change this later in Settings.
4. **Connect an AI.** This happens on the AI platform, see the next section.

Then you reach the main screen. On the left is the lane, where requests waiting
for you appear. On the right are the current folder and the connected AIs.

![The lane](assets/lane.png)

### B. Headless (terminal only)

For a machine with no desktop, such as a VPS over ssh:

```sh
# 0. Pick private directories. serve binds loopback only.
export FYLANE_DATA_DIR=~/.mcp-lane/data
WS=~/projects/my-app
mkdir -p "$WS"

# 1. Register the folder (idempotent: reruns verify and exit 0).
fylane-companion init --workspace "$WS" --non-interactive

# 2. Pair with the relay (prints a code for the platform's connection
#    page, plus a PAIR_CODE=... line for scripts).
fylane-companion pair -relay https://relay.example.com \
  -data-dir "$FYLANE_DATA_DIR" --non-interactive --ttl 10m

# 3. Serve it (loopback only; keep it running in the background).
fylane-companion serve -data-dir "$FYLANE_DATA_DIR" &

# 4. List pending approvals (empty until the first write arrives).
fylane-companion approvals --json

# 5. Decide one (the same Resolve the desktop window uses).
fylane-companion approve --json <change-set-id>
# ... or refuse it:
fylane-companion reject --json <change-set-id>
```

Heads-up for keychain-less servers: without an OS keychain, `pair` and
`serve` need `FYLANE_DEVICE_CREDENTIALS_FILE` pointing at an owner-only
(0600) JSON file, which holds the device secret as **plaintext at rest**.
Prefer a real keychain (e.g. GNOME Keyring) when you can. Details and the
full loopback-verifiable example: [docs/headless.md](docs/headless.md).

## Connecting mcp-lane to an AI platform

Every platform takes three steps: copy the address from mcp-lane, paste it into
the platform's connector settings, then approve the connection in mcp-lane.

### Step 1: get the address from mcp-lane

Open **Settings → Connection**.

![Connection](assets/connection.png)

The first time, choose **Cloudflare quick tunnel** and click Set up. No account
or domain needed. After a few seconds an address like
`https://xxx.trycloudflare.com/mcp` appears. Click Copy.

This address changes every time mcp-lane restarts, so you will need to paste it
into the platform again. For an address that stays the same, see
[A fixed address](#a-fixed-address).

### Step 2: paste it into the platform

<details open>
<summary><b>ChatGPT</b></summary>

Needs a paid plan (Plus, Pro or Team).

1. Avatar → **Settings → Security and login** → turn on **Developer mode**.
   In older versions it is under Apps & Connectors → Advanced.

   ![Developer mode](assets/setup/chatgpt-developer-mode.png)

2. Go to **Plugins** (called Apps & Connectors in older versions) and click
   **Create**. Fill in:
   - Name: anything, for example `mcp-lane`
   - Connection: keep **Server URL** and paste the address
   - Authentication: **OAuth**. "No authentication" fails with
     `Error creating connector`.
   - Tick "I understand and want to continue".

   ![New Plugin form](assets/setup/chatgpt-create-connector.png)

3. Click Create. A browser page opens, see step 3.
4. In a chat, click **+** next to the input → **More**, and tick `mcp-lane`.

</details>

<details>
<summary><b>Claude</b></summary>

1. Avatar at the bottom left → **Settings → Connectors** → **Add custom
   connector**.
2. Name it `mcp-lane` and paste the address as the URL. **Leave Client ID and
   Client Secret under Advanced empty.** Filling them in causes an error.
3. Click **Continue**, then click **Connect** next to mcp-lane in the list. An
   authorization page opens, see step 3.
4. In a chat, open the **tools** button on the input and make sure mcp-lane is on.

![Add custom connector](assets/setup/claude-add-connector.png)

In Claude each tool can be set to "ask every time" or "always allow". That is
only Claude's setting. mcp-lane still asks what it needs to ask.

</details>

<details>
<summary><b>Grok</b></summary>

1. grok.com → **Settings → Connectors** → **New Connector** → **Custom
   Connector**.
2. Name it `mcp-lane`, paste the address as the Server URL, and click **Add
   Connector**.
3. Click connect. An authorization page opens, see step 3.

![Custom Connector](assets/setup/grok-add-connector.png)

A few things are different on Grok:

- **Grok does not confirm writes itself.** When using Grok, keep write approval
  on in mcp-lane.
- Grok has its own cloud sandbox, so "run the tests" may run them there. Say it
  plainly: "use mcp-lane's run_command to run the tests".
- Grok waits only 60 seconds per call. If you have not approved by then, it is
  told the request is pending. Approve, then ask it to try again.

</details>

<details>
<summary><b>Gemini</b></summary>

Needs Google AI Pro or Ultra, and Gemini Spark. Spark is not available in the
EEA, the UK, Switzerland or Nigeria.

1. gemini.google.com → switch to **Spark** → **Connected Apps** → under
   **Custom apps**, click **Add a custom app**.
2. Paste the address. Leave the fields under **Advanced features** empty:
   Gemini registers itself with mcp-lane.
3. Click **Next**. An authorization page opens, see step 3.

![Add a custom app](assets/setup/gemini-add-custom-app.png)

A few things are different on Gemini:

- Connected Apps needs **Gemini Activity** turned on. If the page says apps
  are unavailable, turn it on first.
- Custom apps can only be added in the web app. Once added, they also work in
  the mobile app.
- Custom apps only work inside Spark tasks, not in ordinary chats.

</details>

### Step 3: approve the connection in mcp-lane

The platform opens an authorization page with a short code. The mcp-lane window
shows the same code. Check they match and click **Approve connection**.

![Approve connection](assets/pairing.png)

If you are using a browser on another computer, the page asks for a pairing
code instead. In mcp-lane, go to **Settings → Connection**, click **Show a code**,
and type it in. A code lasts 10 minutes and works once.

### Try it

Back in the chat, type:

> List the files in the root of this project.

The AI lists the folder. Reading needs no approval. Then try:

> Create hello.txt in the project with the content "hello".

The mcp-lane window lights up and shows what the AI wants to write. Approve and
the file appears. Reject and the AI is told no.

### The tools the AI gets

| | Tools |
| --- | --- |
| Read | `list_directory` `read_file` `search_files` `git_query` |
| Write | `write_file` `edit_file` `apply_patch` `change_manage` |
| Run | `run_command` `task_status` `code_task` |
| Remember | `memory`, with `recall` `note` `plan` `step` `search` `read` `compact` |
| Navigate | `code_navigate`, finds definitions and references |
| Extend | `mcp_gateway`, passes calls to another MCP server on your computer |

Writes and commands that need approval return `pending_approval` until you
decide in mcp-lane. `change_manage` handles moves, deletes and undo.

### A fixed address

The Cloudflare quick tunnel gets a new address on every restart. For one that
stays the same, pick another option under **Settings → Connection**:

| Way | What it needs | Address |
| --- | --- | --- |
| Cloudflare quick tunnel | nothing | changes on restart |
| Tailscale Funnel | install Tailscale, sign in once (free) | fixed, `xxx.ts.net` |
| Cloudflare named tunnel | a domain hosted on Cloudflare | fixed, your own domain |
| ngrok | an ngrok account | free tier changes, paid stays |
| Your own relay | a server with a public domain | fixed, your own domain |

mcp-lane starts and manages the first four for you. Click Set up in the window.

Your own relay suits a team, or several computers sharing one address. It runs
on your server and stores no file content. See [`deploy/`](deploy/):

```bash
FYLANE_RELAY_HOST=relay.example.com docker compose -f deploy/docker-compose.yml up -d
```

## Remote machines

Your project is on a VPS, you are at your Mac, and you want the AI to edit and
test over there. Add that machine to mcp-lane. Nothing changes on the AI
platform.

**Before you start**: from a terminal on this computer, `ssh user@host` already
logs in without a password. mcp-lane uses your system ssh, so your keys and
`~/.ssh/config` work as usual. It never asks for a password.

1. On the lane, under **Machine**, click **Switch machine → Add a remote
   machine…** and type what you would type after `ssh`, such as an alias or
   `user@host`.
2. If mcp-lane is not on that machine yet, click **Install mcp-lane** in the rail.
   It installs the same version as this app.
3. When it says **Connected**, click **Choose a folder** under Workspace and
   pick a folder on that machine, or type a path such as `~/project`.

After that it works like a local folder. Reads, writes and commands happen on
the VPS, and you approve on your Mac. On the Tasks page, records from a remote
machine show the machine's name.

Good to know:

- **The mcp-lane on the VPS is not open to the internet.** The AI reaches it
  through your computer, so when your computer is off, that VPS is unavailable
  too.
- **You approve on this computer only.** Commands on a remote machine ask every
  time by default.
- **Switching machines only changes which machine the lane shows.** The Tasks
  page still shows all machines, and pending requests are never hidden.
- The remote machine keeps its data in `~/.fylane/` there. **Remove** only
  makes this computer forget the machine. Nothing on it is deleted.
- Remote settings cannot be changed from the window yet. The Windows app needs
  the built-in OpenSSH client.

No window on the VPS at all? Use the headless flow instead:
[First run → B](#b-headless-terminal-only).

## Memory

Start a new chat and carry on where you left off.

For each folder, mcp-lane remembers two things:

- **Where things stand**: what is done, what comes next, what is decided, what is still open.
- **What happened**: the work done and the important decisions along the way.

In a new chat there is no need to explain the background again. Just say
"continue where we left off".

- If it does not pick up, say "check this folder's memory first".
- When you finish a piece of work or make an important decision, say "note this down".

Memory does not keep growing. The current state is kept up to date, and old
notes can be folded into a summary, with the originals still there when you
need them.

On the Memory page you can view, edit, delete, export or clear all of it.
Everything is stored in mcp-lane, never in your project's files.

## Command line

On a machine without the desktop app, such as a Linux server, use
`fylane-companion`. It does the same job, with approvals in the terminal: press
`y` to approve, any other key to reject. Deleting a whole folder needs you to
type `yes`.

Install (macOS and Linux):

```bash
curl -fsSL https://raw.githubusercontent.com/dotpopo/mcp-lane/main/scripts/install.sh | sh
```

Then, inside a project:

```bash
cd ~/projects/my-app
fylane-companion share
```

It prints the address and a pairing code:

```
sharing my-app — starting a tunnel, this takes a few seconds

  Connector URL   https://swift-lane-9f2c.trycloudflare.com/mcp
  Pairing code    7K4M-2QB9   (valid for 10m0s)
```

Then it is the same as the desktop app: paste the address into the platform
and enter the pairing code on the authorization page. Press Ctrl-C to stop. On
a server over ssh, run it inside `tmux` or `screen` so it keeps running when
you disconnect.

With your own relay:

```bash
fylane-companion pair -relay wss://relay.example.com/tunnel -register
fylane-companion serve -workspace ~/projects/my-app
```

For scripted setup and approvals without a terminal watching
(`init` / `approvals` / `approve` / `reject`, `--json`, exit codes), see
[What's new in this fork](#headless-cli-no-window-needed) and
[docs/headless.md](docs/headless.md).

Building from source needs Go 1.25:

```bash
git clone https://github.com/dotpopo/mcp-lane
cd mcp-lane
go build -o bin/fylane-companion ./companion/cmd/companion
```

## Security

Only your approval on this machine counts. Confirmations on the AI platform are
just hints. The relay never stores file content, changes, folder listings or
sensitive file names.

The full trust model, what is in scope and how to report a vulnerability:
[SECURITY.md](SECURITY.md). Known limits, stated plainly:

- **The device-credential file fallback is unencrypted.** On a server with no
  OS keychain, `FYLANE_DEVICE_CREDENTIALS_FILE` holds the device secret as
  plaintext at rest. Keep it at 0600, never back it up to shared storage, and
  prefer a real keychain.
- **Verify the Caddy redaction yourself.** The shipped
  [`deploy/Caddyfile`](deploy/Caddyfile) redacts query strings, the
  `Authorization` header and the `/mcp/<token>` path segment, but nothing in
  CI checks your deployed Caddy actually applies it — read your own access
  logs once after deploying.
- **Relay-side backpressure is still open.** The companion refuses load past
  32 concurrent tunneled requests with a 503, but the relay itself holds each
  waiting caller for up to 15 minutes with no admission cap, so many stalled
  callers pile up relay handlers behind one slow companion.

## License

[Apache 2.0](LICENSE). mcp-lane is a fork of
[leazoot/fylane](https://github.com/leazoot/fylane), which is also Apache-2.0 —
its license and attributions travel with this repository unchanged.
