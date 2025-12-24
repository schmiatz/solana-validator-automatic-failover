# Automatic Failover for Solana Validators

> ⚠️ **WARNING: WORK IN PROGRESS** ⚠️
>
> This project is under active development and is **not ready for production use**.
> Do not use this on mainnet validators. The failover logic is incomplete and
> could cause unintended behavior. Use at your own risk.

---

A tool to monitor Solana validator health and trigger automatic failover when issues are detected. Designed to run on a **hot spare validator** that waits to take over if the primary fails.

## Usage

```bash
./bin/failover [--rpc <RPC_URL>]
```

### Parameters

| Parameter | Required | Default | Description |
|-----------|----------|---------|-------------|
| `--rpc` | No | `http://127.0.0.1:8899` | Local RPC endpoint to query |
| `--log` | No | - | Path to log file (logs to stdout and file if set) |

### Example

```bash
# Monitor local validator (production)
./bin/failover

# Use a custom RPC endpoint
./bin/failover --rpc http://localhost:8899

# Log to file (output goes to both stdout and file)
./bin/failover --log /home/solana/failover.log
```

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
- What software is the node running (Agave, Firedancer)?
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
2025/12/24 12:52:50.981717 Local RPC: https://api.testnet.solana.com
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

## Failover Triggers (Planned)

The following conditions would trigger a failover:

1. **Primary becomes unhealthy:** Primary validator's `getHealth` returns unhealthy
2. **Gossip unreachable:** Cannot connect to primary's gossip port (node offline)
3. **Slots behind threshold:** Primary is behind by more than N slots (configurable)

---

## Requirements

- **Go 1.21+** (tested with Go 1.21.6)

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
├── bin/failover              # Compiled binary
└── check_delinquent.sh       # Bash script version (legacy)
```
