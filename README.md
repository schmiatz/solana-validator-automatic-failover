# Automatic Failover for Solana Validators

> 🚨 **CRITICAL: MAIN NODE MUST NOT RESTART WITH STAKED IDENTITY** 🚨
>
> Before using this tool, you **MUST** ensure your **main/primary validator** is configured to not reboot with its staked identity keypair.
>
> **Why?** When failover occurs, this tool executes a `set-identity` command which may kill/restart the main node. If the main node restarts with the staked identity, it will immediately conflict with the spare node that just took over — potentially killing the spare and causing a failover loop.
>
> **Solution:** Configure your main validator's startup script/service to use a **different, unstaked identity keypair** at boot. Only switch to the staked identity after manual verification or via a separate identity-switch mechanism.
>
> **Failure to do this correctly can result in both nodes fighting over the staked identity and extended validator downtime.**

---

A tool to monitor Solana validator health and trigger automatic failover when issues are detected. Designed to run on a **hot spare validator** that waits to take over if the primary fails.

## Usage

```bash
./bin/failover --votepubkey <VOTE_PUBKEY> --identity-keypair <PATH> [options]
```

### Parameters

| Parameter | Required | Default | Description |
|-----------|----------|---------|-------------|
| `--votepubkey` | **Yes** | - | Vote account public key to monitor |
| `--identity-keypair` | **Yes** | - | Path to identity keypair JSON file (for set-identity command) |
| `--config` | **Frankendancer only** | - | Path to config.toml (required for Frankendancer nodes) |
| `--ledger` | **Agave only** | - | Path to validator ledger directory (required for Agave nodes) |
| `--rpc` | No | `http://127.0.0.1:8899` | Local RPC endpoint to query |
| `--max-vote-latency` | No | delinquency (~150) | Trigger failover when this many slots behind (default: only on delinquency) |
| `--retry-count` | No | `3` | Number of retries before triggering failover |
| `--pagerduty-key` | No | - | PagerDuty routing key for alerts on failover |
| `--webhook-url` | No | - | Generic webhook URL to POST on failover |
| `--webhook-body` | No | - | Custom webhook body (supports `{reason}`, `{identity}` placeholders) |
| `--log` | No | - | Path to log file (logs to stdout and file if set) |

### Example

```bash
# Frankendancer node
./bin/failover \
  --votepubkey DvAmv1VbS2GNaZiSwQjyyjQqx1UUR283HMrgh3Txh1DA \
  --identity-keypair /home/solana/identity.json \
  --config /home/solana/config.toml

# Agave node
./bin/failover \
  --votepubkey DvAmv1VbS2GNaZiSwQjyyjQqx1UUR283HMrgh3Txh1DA \
  --identity-keypair /home/solana/identity.json \
  --ledger /home/solana/validator-ledger

# With vote latency threshold (triggers failover if >50 slots behind)
./bin/failover \
  --votepubkey DvAmv1VbS2GNaZiSwQjyyjQqx1UUR283HMrgh3Txh1DA \
  --identity-keypair /home/solana/identity.json \
  --config /home/solana/config.toml \
  --max-vote-latency 50

# Full production setup with logging
./bin/failover \
  --votepubkey DvAmv1VbS2GNaZiSwQjyyjQqx1UUR283HMrgh3Txh1DA \
  --identity-keypair /home/solana/identity.json \
  --config /home/solana/config.toml \
  --max-vote-latency 50 \
  --log /home/solana/failover.log
```

---

## How It Works

### Overview

1. **Startup**: Check if local node (hot spare) is healthy
2. **Identity check**: Verify the provided `--identity-keypair` is NOT currently active on this node
   - If keypair matches node's identity → exit (this is the primary, not a spare)
   - If keypair doesn't match → continue (node is in standby mode)
3. **Initial delinquency check**: Check if monitored vote account is already delinquent
   - If delinquent → retry 2x (1s apart) → trigger failover
4. **Continuous monitoring**: Check vote account status every second
   - Always monitors for delinquency
   - If `--max-vote-latency` is set, also triggers failover when latency exceeds threshold
   - Any issue detected → retry 2x (1s apart) → trigger failover

### Why use `--max-vote-latency`?

A validator becomes delinquent when it's >150 slots behind. If you set `--max-vote-latency 50`, the failover triggers long before delinquency would occur — giving you faster response to issues.

---

## Health Check Process

The tool performs the following checks on the **local node** to verify it's ready to take over if needed:

### Step 1: Check Local Node Health (`getHealth`)

**RPC Method:** `getHealth`

**What it checks:**
- Is the RPC endpoint responding?
- Is the node caught up with the network?
- Is the node in a healthy state?

**Behavior:**
- If healthy → Proceed to detailed checks
- If unhealthy → Retry every 3 seconds until healthy

```
2025/12/24 12:52:50.981885 Checking local node health...
2025/12/24 12:52:52.202651 Local node is healthy
```

---

### Step 2: Detect Node Type (`getVersion`)

**RPC Method:** `getVersion`

**What it checks:**
- What software is the node running (Agave/Jito, Frankendancer)?
- What version is installed?

```
2025/12/24 12:52:52.227780 Client: Agave
2025/12/24 12:52:52.227818 Version: 3.1.4
```

---

### Step 3: Get Node Identity (`getIdentity`)

**RPC Method:** `getIdentity`

**What it returns:**
- The node's identity public key (used for gossip lookup)

---

### Step 4: Check Gossip Status (`getClusterNodes`)

**RPC Method:** `getClusterNodes`

**What it checks:**
- Does the node's identity appear in the cluster gossip?
- What is the advertised gossip address?

| Field | Description |
|-------|-------------|
| `In gossip` | Whether the node appears in the cluster's gossip list |
| `Gossip address` | The IP:port where the node advertises its gossip service |

---

### Step 5: Probe Gossip Port (TCP Connect)

**Method:** TCP dial to gossip address with 2 second timeout

**What it checks:**
- Can we actually reach the node's gossip port?
- Is the node online and accepting connections?

| Field | Description |
|-------|-------------|
| `TCP reachable` | `true` if TCP connection succeeded, `false` if unreachable |

---

## Example Output

### Healthy Node

```
2025/12/24 12:52:50.981144 Starting automatic failover manager...
2025/12/24 12:52:50.981717 Local RPC: http://localhost:8899
2025/12/24 12:52:50.981885 Checking local node health...
2025/12/24 12:52:52.202651 Local node is healthy
2025/12/24 12:52:52.227780 Client: Agave
2025/12/24 12:52:52.227818 Version: 3.1.4
2025/12/24 12:52:52.227824 Performing detailed health check...
2025/12/24 12:52:52.496518 Health check result:
2025/12/24 12:52:52.496602   Identity: 9iEjL9jaEx1FNTqJHGjarbjoLoqNVbFtRfGDfX2txyQn
2025/12/24 12:52:52.496613   Healthy: true (from getHealth RPC)
2025/12/24 12:52:52.496617   Gossip status:
2025/12/24 12:52:52.496620     In gossip: true
2025/12/24 12:52:52.496622     Gossip address: 64.130.42.168:8001
2025/12/24 12:52:52.496624     TCP reachable: true
```

**Interpretation:** Hot spare is ready — node is healthy, visible in gossip, and gossip port is reachable.

---

### Unhealthy Node (Waiting)

```
2025/12/24 12:44:59.202593 Starting automatic failover manager...
2025/12/24 12:44:59.202719 Local RPC: http://127.0.0.1:8899
2025/12/24 12:44:59.202944 Checking local node health...
2025/12/24 12:45:02.205974 Node not healthy yet: failed to make request: Post "http://127.0.0.1:8899": dial tcp 127.0.0.1:8899: connect: connection refused
2025/12/24 12:45:05.206356 Node not healthy yet: failed to make request: Post "http://127.0.0.1:8899": dial tcp 127.0.0.1:8899: connect: connection refused
2025/12/24 12:45:08.206463 Node not healthy yet: failed to make request: Post "http://127.0.0.1:8899": dial tcp 127.0.0.1:8899: connect: connection refused
```

**Interpretation:** Local node is not responding. The tool will retry every 3 seconds until the node becomes healthy.

---

## Failover Triggers

The following conditions trigger a failover:

1. **Delinquent:** Vote account is marked delinquent at startup (checked 3 times, 1s apart)
2. **Vote latency exceeded:** Vote account is behind by more than `--max-vote-latency` slots (checked 3 times, 1s apart)

### Failover Commands

When triggered, the tool executes the appropriate set-identity command:

**Frankendancer:**
```bash
fdctl set-identity --config <path/to/config.toml> <path/to/keypair.json>
```

**Agave:**
```bash
agave-validator --ledger <path/to/validator-ledger> set-identity <path/to/keypair.json>
```

### Verification

After executing the set-identity command, the tool verifies the identity switch was successful:

1. Queries `getIdentity` from the local RPC
2. Compares the returned identity with the pubkey from `--identity-keypair`
3. If they match → logs success and exits
4. If they don't match → logs error and exits with code 1

### Alerts

Alerts are sent on **both success and failure** of the failover process. You'll be notified if:
- ✅ Failover succeeded and identity switch was verified
- ❌ The set-identity command failed
- ❌ Identity verification failed (mismatch)

**PagerDuty** (simple — just provide routing key):
```bash
--pagerduty-key YOUR_PAGERDUTY_ROUTING_KEY
```

**Generic Webhook** (works with Slack, Discord, etc.):
```bash
--webhook-url https://hooks.slack.com/services/...
```

The default payload is Slack-compatible:
```json
{"text": "Validator failover success: vote latency exceeded threshold (50 slots)\nNew identity: DvAmv1VbS..."}
```

**Telegram** (requires custom body with chat_id):
```bash
--webhook-url "https://api.telegram.org/bot<YOUR_BOT_TOKEN>/sendMessage" \
--webhook-body '{"chat_id": "YOUR_CHAT_ID", "text": "Validator failover {status}: {reason}\nNew identity: {identity}"}'
```

**Custom Webhook Body** (optional, with placeholders):
```bash
--webhook-url https://api.example.com/alert \
--webhook-body '{"message": "Failover {status}: {reason}", "node": "{identity}"}'
```

Supported placeholders: `{reason}`, `{identity}`, `{status}`, `{error}`

**Example alert messages:**
```
✅ Validator failover SUCCESS: vote latency exceeded threshold (50 slots) (now active: DvAmv1VbS2GNaZiSwQjyyjQqx1UUR283HMrgh3Txh1DA)

❌ Validator failover FAILED: vote account is delinquent - set-identity command failed

❌ Validator failover FAILED: vote latency exceeded threshold (50 slots) - identity mismatch: expected ABC..., got XYZ...
```

---

## Requirements

- **Go 1.21+** (tested with Go 1.21.6)
- **Validator CLI in PATH** (required for failover command):
  - **Agave nodes**: `agave-validator` must be in PATH
  - **Frankendancer nodes**: `fdctl` must be in PATH

The client automatically detects the node type and checks for the appropriate CLI tool.

## Building

```bash
go build -o bin/failover ./cmd/failover
```

## Project Structure

```
automatic-failover/
├── cmd/failover/main.go      # Entry point, CLI parsing
├── internal/
│   ├── rpc/client.go         # Solana JSON-RPC client
│   └── health/checker.go     # Health checking logic
└── bin/failover              # Compiled binary
```
