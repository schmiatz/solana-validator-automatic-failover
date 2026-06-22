# Automatic Failover for Solana Validators

> **CRITICAL: MAIN NODE MUST NOT RESTART WITH STAKED IDENTITY**
>
> Before using this tool, you **MUST** ensure your **main/primary validator** is configured to not reboot with its staked identity keypair.
>
> **Why?** When failover occurs, this tool switches the main node's identity via SSH (STONITH fencing). If the main node restarts with the staked identity, it will immediately conflict with the spare node that just took over — potentially causing a failover loop.
>
> **Solution:** Configure your main validator's startup script/service to use a **different, unstaked identity keypair** at boot. Only switch to the staked identity after manual verification or via a separate identity-switch mechanism.
>
> **Failure to do this correctly can result in both nodes fighting over the staked identity and extended validator downtime.**

---

A tool to monitor Solana validator health and trigger automatic failover with STONITH fencing when issues are detected. Supports Agave and Frankendancer.

## Quick Start

```bash
# Build
go build -o bin/failover ./cmd/failover

# Run with TOML config (recommended)
./bin/failover --config /path/to/config.toml

# Run with CLI flags
./bin/failover --votepubkey <VOTE_PUBKEY> --identity-keypair <PATH> --ledger <PATH>
```

See [example-config.toml](example-config.toml) for all available options with descriptions.

## Configuration

Three-layer precedence: **defaults → TOML config → CLI flags**

CLI flags always win over TOML values. TOML values override built-in defaults.

### Parameters

| Parameter | Required | Default | Description |
|-----------|----------|---------|-------------|
| `--config` | No | - | Path to TOML configuration file |
| `--votepubkey` | **Yes** | - | Vote account public key to monitor |
| `--identity-keypair` | **Yes** | - | Path to staked identity keypair JSON file |
| `--ledger` | **Agave** | - | Path to validator ledger directory |
| `--fdctl-config` | **Frankendancer** | - | Path to fdctl config.toml |
| `--rpc` | No | `http://127.0.0.1:8899` | Local RPC endpoint |
| `--max-vote-latency` | No | `0` (delinquency only) | Trigger failover when this many slots behind |
| `--retry-count` | No | `3` | Consecutive latency/delinquency RPC checks before confirming failover (1s apart) |

#### SSH Fencing (STONITH)

Required for safe failover when the active validator is still in gossip. Without these, failover only proceeds when that identity has left gossip (confirmed over several polls).

| Parameter | Required | Default | Description |
|-----------|----------|---------|-------------|
| `--remote-ssh` | No | Auto-detected from gossip | SSH target for active node (`user@host`) |
| `--ssh-key` | No | System default | Path to SSH private key |
| `--ssh-port` | No | `22` | SSH port |
| `--remote-identity-keypair` | For fencing | - | Path to unstaked keypair on active node |
| `--remote-ledger` | For Agave fencing | - | Ledger path on active node |
| `--remote-fdctl-config` | For FD fencing | - | fdctl config path on active node |
| `--remote-fdctl-bin` | No | - | Full path to `fdctl` on active node if not on SSH PATH |
| `--remote-agave-bin` | No | - | Full path to `agave-validator` on active node if not on SSH PATH |
| `--ssh-timeout` | No | `5` | SSH connection timeout in seconds |
| `--ssh-retries` | No | `2` | Extra SSH attempts when active identity is still in gossip (5s pause + gossip re-poll between retries) |

Fencing is considered fully configured when `remote-ssh` + `remote-identity-keypair` + (`remote-ledger` or `remote-fdctl-config`) are all set.

Remote SSH commands use **`bash -i -l -c`** (interactive login) so **`~/.bashrc`** runs fully. Plain **`bash -l -c`** over SSH is non-interactive on Ubuntu, so `.bashrc` exits early and **`fdctl`** is missing from PATH even when an interactive SSH session has it. The tool also auto-probes **`~/firedancer/build/native/gcc/bin/fdctl`**. If discovery still fails, set **`remote-fdctl-bin`** to the full executable path.

#### Hooks

Bash commands executed via `bash -l -c` (login shell — sources `.profile`, gets full PATH under systemd).

| Parameter | Required | Default | Description |
|-----------|----------|---------|-------------|
| `--pre-failover-hook` | No | - | Runs before fencing. **Non-zero exit aborts the failover.** |
| `--post-failover-hook` | No | - | Runs after successful identity switch. Best-effort (failures logged). |

Environment variables available in hooks:

| Variable | Description |
|----------|-------------|
| `FAILOVER_REASON` | Why failover was triggered (e.g. "vote account is delinquent") |
| `FAILOVER_VOTE_PUBKEY` | Vote account public key |
| `FAILOVER_ACTIVE_NODE` | Identity pubkey of the active node |
| `FAILOVER_ACTIVE_IP` | IP address of the active node |
| `FAILOVER_LOCAL_IDENTITY` | Current local identity (before failover) |
| `FAILOVER_NEW_IDENTITY` | The staked identity being assumed |
| `FAILOVER_HOSTNAME` | Hostname of this machine |

#### Alerting

| Parameter | Required | Default | Description |
|-----------|----------|---------|-------------|
| `--pagerduty-key` | No | - | PagerDuty Events API v2 routing key |
| `--webhook-url` | No | - | Generic webhook URL (receives JSON POST) |
| `--webhook-body` | No | - | Custom webhook body template (see placeholders below) |

#### Logging

| Parameter | Required | Default | Description |
|-----------|----------|---------|-------------|
| `--log` | No | - | Log file path (logs to both stdout and file) |

### Example CLI

```bash
# Minimal Agave setup (no fencing)
./bin/failover \
  --votepubkey DvAmv1VbS2GNaZiSwQjyyjQqx1UUR283HMrgh3Txh1DA \
  --identity-keypair /home/sol/staked-identity.json \
  --ledger /home/sol/ledger

# Full production setup with STONITH fencing (Agave)
./bin/failover \
  --config /home/sol/failover.toml \
  --votepubkey DvAmv1VbS2GNaZiSwQjyyjQqx1UUR283HMrgh3Txh1DA \
  --identity-keypair /home/sol/staked-identity.json \
  --ledger /home/sol/ledger \
  --remote-ssh sol@10.0.0.1 \
  --remote-identity-keypair /home/sol/unstaked-identity.json \
  --remote-ledger /home/sol/ledger \
  --max-vote-latency 50 \
  --pagerduty-key YOUR_KEY \
  --log /home/sol/failover.log

# Frankendancer
./bin/failover \
  --votepubkey DvAmv1VbS2GNaZiSwQjyyjQqx1UUR283HMrgh3Txh1DA \
  --identity-keypair /home/sol/staked-identity.json \
  --fdctl-config /home/sol/fdctl-config.toml \
  --remote-ssh sol@10.0.0.1 \
  --remote-identity-keypair /home/sol/unstaked-identity.json \
  --remote-fdctl-config /home/sol/fdctl-config.toml
```

---

## How It Works

### Overview

The client runs on both **active** and **standby** validator nodes, automatically detecting which mode it's in.

1. **Startup**: Health check, client detection, gossip verification, identity validation
2. **Mode detection**: Compares local identity with vote account's `nodePubkey`
   - Match → **ACTIVE mode** (this node is currently validating)
   - No match → **STANDBY mode** (this node is a hot spare)
3. **Identity verification**: Validates `--identity-keypair` based on mode
   - **ACTIVE**: Keypair must be DIFFERENT from voting identity (unstaked keypair for stepping down)
   - **STANDBY**: Keypair must MATCH the vote account's validator (staked keypair to take over)
4. **Continuous monitoring**: Checks vote account status every second
   - Monitors for delinquency (always)
   - Monitors vote latency threshold (if `--max-vote-latency` is set)
   - Issue detected → retries `retry-count` times (1s apart) → triggers failover

At startup, the tool resolves the **active validator identity** from the vote account (`nodePubkey` in `getVoteAccounts`) and finds that pubkey in `getClusterNodes` (for SSH auto-detection and fencing). That identity is what Phase 1 gossip polls look for during failover — it is not refreshed on each trigger.

### Vote latency failover path

When `--max-vote-latency` is set and `slotsBehind` (current slot − last vote, from batched `getVoteAccounts` + `getSlot` at `confirmed`) exceeds the limit:

1. **Warning** on the first offending 1s tick, then **`checkLatencyWithRetries`**: up to `--retry-count` attempts (default 3), 1s apart.
   - If any successful read shows latency back within the limit → **no failover**, monitoring continues.
   - If all attempts are RPC errors → **no failover**.
   - If at least one successful read is still over the limit (and none recovered) → **`triggerFailover`**.

2. **Pre-failover hook** (optional): non-zero exit **aborts** (no latency re-check here).

3. **Phase 1 — Gossip + SSH fencing** (see below): not a repeat of the latency math.

4. **Phase 2**: local `set-identity`, then **`getIdentity`** RPC verification.

Delinquency on the same 1s tick is handled first and uses the same retry pattern before failover.

### Failover Process (Two-Phase with STONITH)

When failover is triggered:

1. **Pre-failover hook** (if configured): Runs custom command. Non-zero exit **aborts** the failover.

2. **Phase 1 — Fence the active node (STONITH)**:
   - Poll local RPC **`getClusterNodes`** **5 times**, **1 second apart**, for the vote account’s **`nodePubkey`** (staked validator identity saved at startup as `ActiveNodePubkey`)
   - **Still in gossip** if that pubkey appears on **any** successful poll → treat active as running → **SSH fencing required** (if SSH fencing is not configured → failover **aborted**)
   - **Left gossip** if absent on **every** successful poll → safe to proceed; best-effort SSH `set-identity` to unstaked key + optional tower copy if SSH is configured anyway
   - If **all** gossip polls fail (RPC errors) → failover **aborted** (cannot confirm it is safe)
   - When SSH is required: `1 + --ssh-retries` attempts (default 3 total); **5s** between retries, then **another 5×1s gossip poll** — if the identity left gossip during the wait, proceed without further SSH
   - On successful SSH: remote `set-identity` to unstaked keypair; copy Agave **tower file** when both ledger paths are set (copy failure is non-fatal)
   - If still in gossip and all SSH attempts fail → failover **aborted** (prevents double-signing)

   Liveness is based on **gossip visibility**, not a TCP probe to the TPU address. TPU/gossip contact addresses from startup are still used for SSH target auto-detection (`user@host` from gossip IP).

3. **Phase 2 — Switch local identity**:
   - Execute `set-identity` on the local node to assume the staked keypair
   - Verify identity switch via RPC query

4. **Post-failover hook** (if configured): Best-effort execution, failures are logged but don't affect outcome.

### Why use `--max-vote-latency`?

A validator becomes delinquent when it's >150 slots behind. If you set `--max-vote-latency X`, the failover can trigger before delinquency occurs — avoiding downtime entirely.

---

## Startup Checks

The tool performs these checks before starting monitoring:

| Check | What it verifies |
|-------|-----------------|
| **Health** | Local node RPC is responding and caught up |
| **Gossip** | Node appears in cluster gossip, TCP port reachable |
| **PATH** | Required CLI tool (`agave-validator` or `fdctl`) is in PATH |
| **Config/Ledger** | Configured path exists on disk |
| **Keypair** | Identity keypair readable; pubkey matches vote account `nodePubkey` |
| **Vote** | Vote account queryable via RPC |
| **Standby** | Local identity ≠ vote `nodePubkey` (tool must run on hot spare) |
| **Active** | Staked identity found in `getClusterNodes` (TPU/`tpuQuic` used for SSH IP hint) |
| **SSH** | SSH connectivity and remote binary available (if fencing configured) |
| **Tower** | `--ledger` and `--remote-ledger` set so `tower-1_9-<identity>.bin` can be SCP'd on failover (if fencing configured) |

All failed checks are printed before exit so you can fix everything in one pass.

---

## Example Output

### Successful Startup (STANDBY node with fencing)

```
2026/01/02 16:42:13 Starting Automatic Failover Manager:
╔══════════════════════════════════════════════════════════════════════════════╗
║                        Automatic Failover Manager                            ║
╠══════════════════════════════════════════════════════════════════════════════╣
║  Vote Account       DvAmv1VbS2GNaZiSwQjyyjQqx1UUR283HMrgh3Txh1DA             ║
║  Active Node        5rfxa1dGE3AysgHJLSPMBxgo2DUyhp8zQbapRS9spS1K             ║
║  Latency Limit      50 slots                                                 ║
║  Client             Agave 3.1.5                                              ║
║  Local Identity     CL6kvcozv6BDnXA3vQKnq8VjwrNx31zMo24Erpi6SNcE             ║
║  Failover Key       HH1d1t8xjY8ERpFPfKYdWzveEJYkRZE5b6ahewc2SKLL             ║
║  Fencing            SSH sol@10.0.0.1:22                                      ║
║  Alerting           PagerDuty                                                ║
║  Hooks              Pre + Post                                               ║
║  Logfile            /home/sol/failover.log                                   ║
║  Startup checks     Health ✓  Gossip ✓  PATH ✓  Ledger ✓                     ║
║                     Keypair ✓  Vote ✓  Standby ✓  Active ✓  SSH ✓  Tower ✓     ║
╚══════════════════════════════════════════════════════════════════════════════╝
```

### Monitoring Output

```
2026/01/02 16:42:13 Starting Votelatency Monitoring every 1s:
╔══════════════════════════════════════════════════════════════════════════════╗
║  Counts   Low[≤2]: 847    │   Medium[3-10]: 12     │   High[11+]: 0          ║
║  Status   Slot: 379082824 │   Last vote: 379082823   │   Latency: 1          ║
╚══════════════════════════════════════════════════════════════════════════════╝
```

Latency thresholds:
- **Low[≤2]**: Excellent — voting within ~800ms
- **Medium[3-10]**: Acceptable — 1-4 seconds behind
- **High[11+]**: Concerning — approaching warning zone

### Detailed Log File

When `--log` is set, the terminal shows the compact display while the log file gets per-check detail:

```
2026/01/02 16:04:18.155751 Slot: 379082648 | Last vote: 379082647 | Category: Low | Latency: 1
2026/01/02 16:05:11.157280 Slot: 379082785 | Last vote: 379082780 | Category: Medium | Latency: 5
2026/01/02 16:05:45.156892 Slot: 379082820 | Last vote: 379082805 | Category: High | Latency: 15
```

---

## Alerts

Alerts are sent on **both success and failure** of the failover process, as well as when fencing fails or a pre-hook aborts.

### PagerDuty

```bash
--pagerduty-key YOUR_PAGERDUTY_ROUTING_KEY
```

Summary format:
```
[validator-backup-01 STANDBY→ACTIVE] Failover SUCCESS for DvAmv1Vb...: vote account delinquent
```

### Generic Webhook

```bash
--webhook-url https://hooks.slack.com/services/...
```

Default payload (Slack-compatible):
```json
{"text": "[validator-backup-01 STANDBY→ACTIVE] Failover SUCCESS\nReason: vote account delinquent\nVote account: DvAmv1Vb...\nPrevious identity: 5rfxa1dG...\nNew identity: HH1d1t8x..."}
```

### Telegram

```bash
--webhook-url "https://api.telegram.org/bot<TOKEN>/sendMessage" \
--webhook-body '{"chat_id": "CHAT_ID", "text": "{transition} Failover {status}: {reason}"}'
```

### Custom Webhook Body

Supported placeholders: `{transition}`, `{reason}`, `{status}`, `{error}`, `{vote_account}`, `{previous_identity}`, `{new_identity}`, `{identity}`

---

## Requirements

- **Go 1.21+**
- **Validator CLI in PATH**:
  - Agave: `agave-validator`
  - Frankendancer: `fdctl`
- **SSH access** to active node (for STONITH fencing)

## Building

```bash
go build -o bin/failover ./cmd/failover
```

## Project Structure

```
automatic-failover/
├── cmd/failover/main.go      # Core logic, CLI parsing, failover engine
├── internal/
│   ├── rpc/client.go         # Solana JSON-RPC client
│   └── health/checker.go     # Health checking logic
├── example-config.toml       # Example TOML configuration with all options
└── bin/failover              # Compiled binary
```
