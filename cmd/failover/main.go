package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	osuser "os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/mr-tron/base58"
	"github.com/schmiatz/solana-validator-automatic-failover/internal/health"
	"github.com/schmiatz/solana-validator-automatic-failover/internal/rpc"
)

// TOMLConfig maps the TOML configuration file fields.
// Field names match Config struct so applyTOMLConfig can map them directly.
type TOMLConfig struct {
	VotePubkey            string `toml:"vote-pubkey"`
	RPC                   string `toml:"rpc"`
	MaxVoteLatency        int64  `toml:"max-vote-latency"`
	RetryCount            int    `toml:"retry-count"`
	IdentityKeypair       string `toml:"identity-keypair"`
	LedgerPath            string `toml:"ledger"`
	FdctlConfigPath       string `toml:"fdctl-config"`
	RemoteSSH             string `toml:"remote-ssh"`
	SSHKey                string `toml:"ssh-key"`
	SSHPort               int    `toml:"ssh-port"`
	RemoteIdentityKeypair string `toml:"remote-identity-keypair"`
	RemoteLedgerPath      string `toml:"remote-ledger"`
	RemoteFdctlConfig     string `toml:"remote-fdctl-config"`
	SSHTimeout            int    `toml:"ssh-timeout"`
	SSHRetries            int    `toml:"ssh-retries"`
	PreFailoverHook       string `toml:"pre-failover-hook"`
	PostFailoverHook      string `toml:"post-failover-hook"`
	PagerDutyKey          string `toml:"pagerduty-key"`
	WebhookURL            string `toml:"webhook-url"`
	WebhookBody           string `toml:"webhook-body"`
	LogFile               string `toml:"log"`
}

// applyTOMLConfig sets Config fields from TOML values (only non-zero values)
func applyTOMLConfig(config *Config, t *TOMLConfig) {
	setStr := func(dst *string, src string) {
		if src != "" {
			*dst = src
		}
	}
	setInt := func(dst *int, src int) {
		if src != 0 {
			*dst = src
		}
	}
	setInt64 := func(dst *int64, src int64) {
		if src != 0 {
			*dst = src
		}
	}
	setStr(&config.VotePubkey, t.VotePubkey)
	setStr(&config.LocalRPCEndpoint, t.RPC)
	setInt64(&config.MaxVoteLatency, t.MaxVoteLatency)
	setInt(&config.RetryCount, t.RetryCount)
	setStr(&config.IdentityKeypair, t.IdentityKeypair)
	setStr(&config.LedgerPath, t.LedgerPath)
	setStr(&config.FdctlConfigPath, t.FdctlConfigPath)
	setStr(&config.RemoteSSH, t.RemoteSSH)
	setStr(&config.SSHKey, t.SSHKey)
	setInt(&config.SSHPort, t.SSHPort)
	setStr(&config.RemoteIdentityKeypair, t.RemoteIdentityKeypair)
	setStr(&config.RemoteLedgerPath, t.RemoteLedgerPath)
	setStr(&config.RemoteFdctlConfig, t.RemoteFdctlConfig)
	setInt(&config.SSHTimeout, t.SSHTimeout)
	setInt(&config.SSHRetries, t.SSHRetries)
	setStr(&config.PreFailoverHook, t.PreFailoverHook)
	setStr(&config.PostFailoverHook, t.PostFailoverHook)
	setStr(&config.PagerDutyKey, t.PagerDutyKey)
	setStr(&config.WebhookURL, t.WebhookURL)
	setStr(&config.WebhookBody, t.WebhookBody)
	setStr(&config.LogFile, t.LogFile)
}

// Config holds the runtime configuration
type Config struct {
	LocalRPCEndpoint      string
	LogFile               string
	LogFileHandle         *os.File
	VotePubkey            string
	MaxVoteLatency        int64
	RetryCount            int
	IdentityKeypair       string
	FdctlConfigPath       string // For Frankendancer (local)
	LedgerPath            string // For Agave (local)
	ClientType            string // Detected client type (Agave/Frankendancer)
	RemoteSSH             string // user@host for SSH to active node
	SSHKey                string // SSH private key path
	SSHPort               int    // SSH port
	RemoteIdentityKeypair string // Path to unstaked keypair on active node
	RemoteLedgerPath      string // Ledger path on active node
	RemoteFdctlConfig     string // Fdctl config path on active node
	SSHTimeout            int    // SSH connection timeout in seconds
	SSHRetries            int    // SSH retry count when active node is alive
	FencingConfigured     bool   // Whether SSH fencing is fully configured
	ActiveNodeTPU         string // TPU address of active node (IP:port)
	ActiveNodeIP          string // IP of active node
	ActiveNodePubkey      string // Identity pubkey of active node
	PreFailoverHook       string // Bash command to run before failover (non-zero exit aborts)
	PostFailoverHook      string // Bash command to run after successful failover (best-effort)
	PagerDutyKey          string
	WebhookURL            string
	WebhookBody           string
	PreviousIdentity      string // Local identity before failover
	Hostname              string
	IsTTY                 bool
}

func main() {
	// Define CLI flags
	configFile := flag.String("config", "", "Path to TOML configuration file")
	rpcEndpoint := flag.String("rpc", "", "Local RPC endpoint to query (default: http://127.0.0.1:8899)")
	logFile := flag.String("log", "", "Path to log file (logs to stdout and file if set)")
	votePubkey := flag.String("votepubkey", "", "Vote account public key to monitor")
	maxVoteLatency := flag.Int64("max-vote-latency", 0, "Max slots behind before triggering failover (0=disabled)")
	retryCount := flag.Int("retry-count", 0, "Number of retries before triggering failover (default: 3)")
	identityKeypair := flag.String("identity-keypair", "", "Path to staked identity keypair JSON file")
	fdctlConfigPath := flag.String("fdctl-config", "", "Path to fdctl config.toml (Frankendancer)")
	ledgerPath := flag.String("ledger", "", "Path to validator ledger directory (Agave)")
	remoteSSH := flag.String("remote-ssh", "", "SSH target for active node (user@host)")
	sshKey := flag.String("ssh-key", "", "Path to SSH private key")
	sshPort := flag.Int("ssh-port", 0, "SSH port for active node (default: 22)")
	remoteIdentityKeypair := flag.String("remote-identity-keypair", "", "Path to unstaked keypair on active node")
	remoteLedgerPath := flag.String("remote-ledger", "", "Ledger path on active node (Agave)")
	remoteFdctlConfig := flag.String("remote-fdctl-config", "", "fdctl config path on active node (Frankendancer)")
	sshTimeout := flag.Int("ssh-timeout", 0, "SSH connection timeout in seconds (default: 5)")
	sshRetries := flag.Int("ssh-retries", 0, "SSH retry count when active node is alive (default: 2)")
	preFailoverHook := flag.String("pre-failover-hook", "", "Bash command to run before failover (non-zero exit aborts failover)")
	postFailoverHook := flag.String("post-failover-hook", "", "Bash command to run after successful failover (best-effort)")
	pagerdutyKey := flag.String("pagerduty-key", "", "PagerDuty routing key for alerts on failover")
	webhookURL := flag.String("webhook-url", "", "Generic webhook URL to POST on failover")
	webhookBody := flag.String("webhook-body", "", "Custom webhook body (supports {reason}, {identity} placeholders)")
	flag.Parse()

	// Track which flags were explicitly set on command line
	flagsSet := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { flagsSet[f.Name] = true })

	// Start with defaults
	config := Config{
		LocalRPCEndpoint: "http://127.0.0.1:8899",
		RetryCount:       3,
		SSHPort:          22,
		SSHTimeout:       5,
		SSHRetries:       2,
	}

	// Load TOML config if provided
	if *configFile != "" {
		var tomlCfg TOMLConfig
		if _, err := toml.DecodeFile(*configFile, &tomlCfg); err != nil {
			log.Fatalf("Error reading config file %s: %v", *configFile, err)
		}
		applyTOMLConfig(&config, &tomlCfg)
	}

	// CLI flags override TOML values (only if explicitly set)
	if flagsSet["rpc"] {
		config.LocalRPCEndpoint = *rpcEndpoint
	}
	if flagsSet["log"] {
		config.LogFile = *logFile
	}
	if flagsSet["votepubkey"] {
		config.VotePubkey = *votePubkey
	}
	if flagsSet["max-vote-latency"] {
		config.MaxVoteLatency = *maxVoteLatency
	}
	if flagsSet["retry-count"] {
		config.RetryCount = *retryCount
	}
	if flagsSet["identity-keypair"] {
		config.IdentityKeypair = *identityKeypair
	}
	if flagsSet["fdctl-config"] {
		config.FdctlConfigPath = *fdctlConfigPath
	}
	if flagsSet["ledger"] {
		config.LedgerPath = *ledgerPath
	}
	if flagsSet["remote-ssh"] {
		config.RemoteSSH = *remoteSSH
	}
	if flagsSet["ssh-key"] {
		config.SSHKey = *sshKey
	}
	if flagsSet["ssh-port"] {
		config.SSHPort = *sshPort
	}
	if flagsSet["remote-identity-keypair"] {
		config.RemoteIdentityKeypair = *remoteIdentityKeypair
	}
	if flagsSet["remote-ledger"] {
		config.RemoteLedgerPath = *remoteLedgerPath
	}
	if flagsSet["remote-fdctl-config"] {
		config.RemoteFdctlConfig = *remoteFdctlConfig
	}
	if flagsSet["ssh-timeout"] {
		config.SSHTimeout = *sshTimeout
	}
	if flagsSet["ssh-retries"] {
		config.SSHRetries = *sshRetries
	}
	if flagsSet["pre-failover-hook"] {
		config.PreFailoverHook = *preFailoverHook
	}
	if flagsSet["post-failover-hook"] {
		config.PostFailoverHook = *postFailoverHook
	}
	if flagsSet["pagerduty-key"] {
		config.PagerDutyKey = *pagerdutyKey
	}
	if flagsSet["webhook-url"] {
		config.WebhookURL = *webhookURL
	}
	if flagsSet["webhook-body"] {
		config.WebhookBody = *webhookBody
	}

	// Validate required parameters
	if config.VotePubkey == "" {
		log.Fatal("Error: --votepubkey is required")
	}
	if config.IdentityKeypair == "" {
		log.Fatal("Error: --identity-keypair is required")
	}

	// Get hostname for alerts
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	config.Hostname = hostname

	// Detect if stdout is a TTY (terminal)
	isTTY := false
	if fileInfo, err := os.Stdout.Stat(); err == nil {
		isTTY = (fileInfo.Mode() & os.ModeCharDevice) != 0
	}
	config.IsTTY = isTTY

	// Set up logging
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	// If log file is specified, open it for direct writes
	if config.LogFile != "" {
		logFileHandle, err := os.OpenFile(config.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Fatalf("Failed to open log file %s: %v", config.LogFile, err)
		}
		defer logFileHandle.Close()
		config.LogFileHandle = logFileHandle
	}

	// Create context that listens for shutdown signals
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		log.Printf("Received signal %v, shutting down...", sig)
		cancel()
	}()

	// Create RPC client for local node
	localClient := rpc.NewClient(config.LocalRPCEndpoint)

	// Create health checker
	checker := health.NewChecker(localClient)

	// === Collect all check results ===
	type checkResult struct {
		name   string
		passed bool
		errMsg string
	}
	var checks []checkResult
	var failedCheck *checkResult

	// Check 1: Wait for local node to be healthy
	if err := checker.WaitForHealthy(ctx); err != nil {
		log.Fatalf("Failed waiting for node health: %v", err)
	}

	// Get detailed health info
	localResult, err := checker.CheckLocal()
	if err != nil {
		log.Fatalf("Health check failed: %v", err)
	}
	config.ClientType = localResult.ClientType

	// Health check
	healthCheck := checkResult{name: "Health", passed: localResult.Healthy}
	if !localResult.Healthy {
		healthCheck.errMsg = fmt.Sprintf("Node at %s is not healthy", config.LocalRPCEndpoint)
		failedCheck = &healthCheck
	}
	checks = append(checks, healthCheck)

	// Gossip check (local node)
	gossipPassed := localResult.Gossip != nil && localResult.Gossip.InGossip && localResult.Gossip.TCPReachable
	gossipCheck := checkResult{name: "Gossip", passed: gossipPassed}
	if !gossipPassed {
		gossipCheck.errMsg = "Node not visible in gossip or gossip port unreachable"
		if failedCheck == nil {
			failedCheck = &gossipCheck
		}
	}
	checks = append(checks, gossipCheck)

	// CLI check
	requiredCmd := getRequiredCommand(localResult.ClientType)
	pathPassed := requiredCmd == "" || isCommandAvailable(requiredCmd)
	pathCheck := checkResult{name: "PATH", passed: pathPassed}
	if !pathPassed {
		pathCheck.errMsg = fmt.Sprintf("Required command '%s' not found in PATH", requiredCmd)
		if failedCheck == nil {
			failedCheck = &pathCheck
		}
	}
	checks = append(checks, pathCheck)

	// Client-specific parameter check (local path)
	paramPassed := true
	paramErrMsg := ""
	paramName := "Config"
	switch config.ClientType {
	case "Frankendancer":
		paramName = "Config"
		if config.FdctlConfigPath == "" {
			paramPassed = false
			paramErrMsg = "--fdctl-config is required for Frankendancer nodes"
		}
	case "Agave":
		paramName = "Ledger"
		if config.LedgerPath == "" {
			paramPassed = false
			paramErrMsg = "--ledger is required for Agave nodes"
		}
	}
	paramCheck := checkResult{name: paramName, passed: paramPassed, errMsg: paramErrMsg}
	if !paramPassed && failedCheck == nil {
		failedCheck = &paramCheck
	}
	checks = append(checks, paramCheck)

	// Keypair check - read and validate against vote account
	keypairPubkey, err := getPubkeyFromKeypair(config.IdentityKeypair)
	keypairReadable := err == nil
	keypairCheck := checkResult{name: "Keypair", passed: false}
	if !keypairReadable {
		keypairCheck.errMsg = fmt.Sprintf("Cannot read identity keypair: %v", err)
		if failedCheck == nil {
			failedCheck = &keypairCheck
		}
	}
	checks = append(checks, keypairCheck)

	// Vote account check
	voteAccountResult, voteErr := checker.Check(config.VotePubkey)
	votePassed := voteErr == nil
	voteCheck := checkResult{name: "Vote", passed: votePassed}
	if voteErr != nil {
		voteCheck.errMsg = fmt.Sprintf("Cannot check vote account: %v", voteErr)
		if failedCheck == nil {
			failedCheck = &voteCheck
		}
	} else if keypairReadable {
		// Validate keypair matches vote account's staked identity
		if keypairPubkey != voteAccountResult.NodePubkey {
			voteCheck.passed = false
			voteCheck.errMsg = fmt.Sprintf("Keypair pubkey (%s) does not match vote account identity (%s)",
				keypairPubkey, voteAccountResult.NodePubkey)
			if failedCheck == nil {
				failedCheck = &voteCheck
			}
		} else {
			keypairCheck.passed = true // Keypair is valid and matches
		}
	}
	// Update the checks slice with final keypair status
	checks[4] = keypairCheck
	checks = append(checks, voteCheck)

	// Standby check - local node must NOT be running the staked identity
	standbyPassed := true
	standbyErrMsg := ""
	if votePassed && keypairReadable {
		if localResult.Identity == voteAccountResult.NodePubkey {
			standbyPassed = false
			standbyErrMsg = "Local node is running the staked identity - this tool must run on the STANDBY node"
		}
		config.PreviousIdentity = localResult.Identity
		config.ActiveNodePubkey = voteAccountResult.NodePubkey
	}
	standbyCheck := checkResult{name: "Standby", passed: standbyPassed, errMsg: standbyErrMsg}
	if !standbyPassed && failedCheck == nil {
		failedCheck = &standbyCheck
	}
	checks = append(checks, standbyCheck)

	// Active node check - find active node in gossip, save TPU address
	activePassed := false
	activeErrMsg := "Could not find active node in gossip"
	if votePassed && localResult.ClusterNodes != nil {
		for _, node := range localResult.ClusterNodes {
			if node.Pubkey == voteAccountResult.NodePubkey {
				activePassed = true
				tpuAddr := node.TPU
				if tpuAddr == nil {
					tpuAddr = node.TpuQuic // fallback for nodes that only advertise QUIC TPU
				}
				if tpuAddr != nil {
					config.ActiveNodeTPU = *tpuAddr
					host, _, err := net.SplitHostPort(*tpuAddr)
					if err == nil {
						config.ActiveNodeIP = host
					}
				} else {
					activePassed = false
					activeErrMsg = "Active node found in gossip but has no TPU address (neither tpu nor tpuQuic advertised)"
				}
				break
			}
		}
	}
	activeCheck := checkResult{name: "Active", passed: activePassed, errMsg: activeErrMsg}
	if !activePassed && failedCheck == nil {
		failedCheck = &activeCheck
	}
	checks = append(checks, activeCheck)

	// Auto-detect SSH target from gossip IP if not explicitly configured
	if config.RemoteSSH == "" && config.ActiveNodeIP != "" {
		currentUser, err := osuser.Current()
		if err == nil {
			config.RemoteSSH = currentUser.Username + "@" + config.ActiveNodeIP
		}
	}

	// Check if fencing is properly configured
	config.FencingConfigured = config.RemoteSSH != "" && config.RemoteIdentityKeypair != "" &&
		(config.RemoteLedgerPath != "" || config.RemoteFdctlConfig != "")

	// SSH check - verify connectivity and remote binary (only if fencing is configured)
	if config.FencingConfigured {
		// Determine which binary is needed on the remote node
		var remoteCmd string
		if config.RemoteFdctlConfig != "" {
			remoteCmd = "fdctl"
		} else {
			remoteCmd = "agave-validator"
		}

		sshPassed := false
		sshErrMsg := ""
		// Test SSH connectivity
		_, err := sshExec(&config, "echo ok")
		if err != nil {
			sshErrMsg = fmt.Sprintf("SSH connection to %s failed: %v", config.RemoteSSH, err)
		} else {
			// SSH works, now check if the required binary exists on remote
			_, err := sshExec(&config, "which "+remoteCmd)
			if err != nil {
				sshErrMsg = fmt.Sprintf("'%s' not found in PATH on remote node %s", remoteCmd, config.RemoteSSH)
			} else {
				// Verify remote-identity-keypair is NOT the active voting identity
				// Read the keypair file on the remote node and extract the pubkey
				catOutput, err := sshExec(&config, "cat "+config.RemoteIdentityKeypair)
				if err != nil {
					sshErrMsg = fmt.Sprintf("Cannot read remote-identity-keypair %s on %s: %v", config.RemoteIdentityKeypair, config.RemoteSSH, err)
				} else {
					remotePubkey, err := pubkeyFromJSON([]byte(catOutput))
					if err != nil {
						sshErrMsg = fmt.Sprintf("Cannot parse remote-identity-keypair %s: %v", config.RemoteIdentityKeypair, err)
					} else if remotePubkey == voteAccountResult.NodePubkey {
						sshErrMsg = fmt.Sprintf("remote-identity-keypair (%s) is the ACTIVE voting identity — fencing would be a no-op (must be an unstaked key)", remotePubkey)
					} else {
						sshPassed = true
					}
				}
			}
		}
		sshCheck := checkResult{name: "SSH", passed: sshPassed, errMsg: sshErrMsg}
		if !sshPassed && failedCheck == nil {
			failedCheck = &sshCheck
		}
		checks = append(checks, sshCheck)
	}

	// === Print the table ===
	tableRow := func(label, value string) string {
		content := fmt.Sprintf("  %-16s  %s", label, value)
		return fmt.Sprintf("║%-78s║", content)
	}

	printLine := func(line string) {
		fmt.Println(line)
		if config.LogFileHandle != nil {
			fmt.Fprintf(config.LogFileHandle, "%s\n", line)
		}
	}

	// Print header with timestamp
	log.Println("Starting Automatic Failover Manager:")
	if config.LogFileHandle != nil {
		timestamp := time.Now().Format("2006/01/02 15:04:05.000000")
		fmt.Fprintf(config.LogFileHandle, "%s Starting Automatic Failover Manager:\n", timestamp)
	}

	printLine("╔══════════════════════════════════════════════════════════════════════════════╗")
	printLine("║                        Automatic Failover Manager                            ║")
	printLine("╠══════════════════════════════════════════════════════════════════════════════╣")
	printLine(tableRow("Vote Account", config.VotePubkey))
	printLine(tableRow("Active Node", config.ActiveNodePubkey))
	if config.MaxVoteLatency > 0 {
		printLine(tableRow("Latency Limit", fmt.Sprintf("%d slots", config.MaxVoteLatency)))
	} else {
		printLine(tableRow("Latency Limit", "delinquency"))
	}
	clientVersion := fmt.Sprintf("%s %s", localResult.ClientType, localResult.Version)
	printLine(tableRow("Client", clientVersion))
	printLine(tableRow("Local Identity", localResult.Identity))
	printLine(tableRow("Failover Key", keypairPubkey))

	// Fencing line
	fencingValue := "Disabled"
	if config.FencingConfigured {
		fencingValue = fmt.Sprintf("SSH %s:%d", config.RemoteSSH, config.SSHPort)
	} else if config.RemoteSSH != "" {
		fencingValue = fmt.Sprintf("SSH %s:%d (incomplete config)", config.RemoteSSH, config.SSHPort)
	}
	printLine(tableRow("Fencing", fencingValue))

	// Alerting line
	alertingValue := "Disabled"
	if config.PagerDutyKey != "" {
		alertingValue = "PagerDuty"
	} else if config.WebhookURL != "" {
		alertingValue = "Webhook"
	}
	printLine(tableRow("Alerting", alertingValue))

	// Hooks line
	hooksValue := "Disabled"
	if config.PreFailoverHook != "" && config.PostFailoverHook != "" {
		hooksValue = "Pre + Post"
	} else if config.PreFailoverHook != "" {
		hooksValue = "Pre"
	} else if config.PostFailoverHook != "" {
		hooksValue = "Post"
	}
	printLine(tableRow("Hooks", hooksValue))

	// Logfile line
	logfileValue := "Disabled"
	if config.LogFile != "" {
		logfileValue = config.LogFile
	}
	printLine(tableRow("Logfile", logfileValue))

	// Build checks lines (4 per row)
	var checksLines []string
	currentLine := ""
	for i, c := range checks {
		mark := "✓"
		if !c.passed {
			mark = "✗"
		}
		currentLine += c.name + " " + mark + "  "
		if (i+1)%4 == 0 || i == len(checks)-1 {
			checksLines = append(checksLines, strings.TrimSpace(currentLine))
			currentLine = ""
		}
	}
	for i, line := range checksLines {
		if i == 0 {
			printLine(tableRow("Checks", line))
		} else {
			printLine(fmt.Sprintf("║%-78s║", "                    "+line))
		}
	}
	printLine("╚══════════════════════════════════════════════════════════════════════════════╝")

	// Print fencing warning if not configured
	if !config.FencingConfigured {
		log.Println("WARNING: SSH fencing is not fully configured. Failover will only proceed when active node TPU is unreachable.")
		log.Println("  Configure --remote-ssh, --remote-identity-keypair, and --remote-ledger/--remote-fdctl-config for full fencing support.")
	}

	// If any check failed, print all failures and exit
	if failedCheck != nil {
		for _, c := range checks {
			if !c.passed && c.errMsg != "" {
				log.Printf("FAILED: [%s] %s", c.name, c.errMsg)
			}
		}
		log.Fatalf("Startup aborted: %d check(s) failed, first failure: %s", func() int {
			n := 0
			for _, c := range checks {
				if !c.passed {
					n++
				}
			}
			return n
		}(), failedCheck.errMsg)
	}

	// === Continue with monitoring ===

	if checkDelinquencyWithRetries(checker, config.VotePubkey, config.RetryCount) {
		triggerFailover("vote account is delinquent", &config)
		return
	}

	// Continuous monitoring
	monitorVoteAccount(ctx, checker, &config)
}

// checkDelinquencyWithRetries checks if vote account is delinquent with retries (1 second apart)
// Returns true if confirmed delinquent after all retries, false if recovered or on errors
func checkDelinquencyWithRetries(checker *health.Checker, votePubkey string, maxAttempts int) bool {
	const retryInterval = 1 * time.Second

	delinquentCount := 0

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result, err := checker.Check(votePubkey)
		if err != nil {
			if attempt == 1 {
				log.Printf("Error checking vote account: %v", err)
			} else {
				log.Printf("Attempt %d/%d: Error checking vote account: %v", attempt, maxAttempts, err)
			}
			if attempt < maxAttempts {
				time.Sleep(retryInterval)
			}
			continue
		}

		if !result.Delinquent {
			return false
		}

		delinquentCount++
		if attempt == 1 {
			log.Printf("Vote account IS DELINQUENT (slots behind: %d)", result.SlotsBehind)
		} else {
			log.Printf("Attempt %d/%d: Vote account IS DELINQUENT (slots behind: %d)",
				attempt, maxAttempts, result.SlotsBehind)
		}

		if attempt < maxAttempts {
			log.Printf("Retrying in %v...", retryInterval)
			time.Sleep(retryInterval)
		}
	}

	return delinquentCount > 0
}

// checkLatencyWithRetries checks if vote latency exceeds threshold with retries (1 second apart)
// Returns true if confirmed exceeded after all retries, false if recovered or on errors
func checkLatencyWithRetries(checker *health.Checker, votePubkey string, maxLatency int64, maxAttempts int) bool {
	const retryInterval = 1 * time.Second

	exceededCount := 0

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result, err := checker.Check(votePubkey)
		if err != nil {
			if attempt == 1 {
				log.Printf("Error checking vote account: %v", err)
			} else {
				log.Printf("Attempt %d/%d: Error checking vote account: %v", attempt, maxAttempts, err)
			}
			if attempt < maxAttempts {
				time.Sleep(retryInterval)
			}
			continue
		}

		if result.SlotsBehind <= maxLatency {
			log.Printf("Vote latency OK (slots behind: %d, threshold: %d)", result.SlotsBehind, maxLatency)
			return false
		}

		exceededCount++
		if attempt == 1 {
			log.Printf("Vote latency EXCEEDED (slots behind: %d, threshold: %d)", result.SlotsBehind, maxLatency)
		} else {
			log.Printf("Attempt %d/%d: Vote latency EXCEEDED (slots behind: %d, threshold: %d)",
				attempt, maxAttempts, result.SlotsBehind, maxLatency)
		}

		if attempt < maxAttempts {
			log.Printf("Retrying in %v...", retryInterval)
			time.Sleep(retryInterval)
		}
	}

	return exceededCount > 0
}

// monitorVoteAccount continuously monitors vote account for delinquency and latency
func monitorVoteAccount(ctx context.Context, checker *health.Checker, config *Config) {
	const checkInterval = 1 * time.Second

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	// Print monitoring header with timestamp
	log.Println("Starting Votelatency Monitoring every 1s:")
	if config.LogFileHandle != nil {
		timestamp := time.Now().Format("2006/01/02 15:04:05.000000")
		fmt.Fprintf(config.LogFileHandle, "%s Starting Votelatency Monitoring every 1s:\n", timestamp)
	}

	// Latency counters
	var lowCount, mediumCount, highCount uint64

	// Helper to write to log file
	logToFile := func(format string, args ...interface{}) {
		if config.LogFileHandle != nil {
			timestamp := time.Now().Format("2006/01/02 15:04:05.000000")
			fmt.Fprintf(config.LogFileHandle, timestamp+" "+format+"\n", args...)
		}
	}

	// Helper to write detailed log (for non-TTY mode, outputs to stdout)
	writeDetailedLog := func(format string, args ...interface{}) {
		timestamp := time.Now().Format("2006/01/02 15:04:05.000000")
		message := fmt.Sprintf(format, args...)
		if config.IsTTY {
			logToFile(format, args...)
		} else {
			fmt.Printf("%s %s\n", timestamp, message)
			logToFile(format, args...)
		}
	}

	// Helper to get latency category
	getCategory := func(latency int64) string {
		if latency <= 2 {
			return "Low"
		} else if latency <= 10 {
			return "Medium"
		}
		return "High"
	}

	// Helper to print empty TTY box frame for inline updates
	printBoxFrame := func() {
		if config.IsTTY {
			fmt.Println("╔══════════════════════════════════════════════════════════════════════════════╗")
			fmt.Println("║                                                                              ║")
			fmt.Println("║                                                                              ║")
			fmt.Println("╚══════════════════════════════════════════════════════════════════════════════╝")
		}
	}

	// Print initial box frame for inline update area
	printBoxFrame()

	for {
		select {
		case <-ctx.Done():
			fmt.Println()
			log.Println("Monitoring stopped due to shutdown signal")
			logToFile("Monitoring stopped due to shutdown signal")
			return
		case <-ticker.C:
			result, err := checker.Check(config.VotePubkey)
			if err != nil {
				log.Printf("Error checking vote account: %v", err)
				logToFile("Error checking vote account: %v", err)
				continue
			}

			// Update latency counters and get category
			latency := result.SlotsBehind
			category := getCategory(latency)
			if latency <= 2 {
				lowCount++
			} else if latency <= 10 {
				mediumCount++
			} else {
				highCount++
			}

			if config.IsTTY {
				countsContent := fmt.Sprintf("  Counts   Low[≤2]: %-6d │   Medium[3-10]: %-6d │   High[11+]: %-5d",
					lowCount, mediumCount, highCount)
				statusContent := fmt.Sprintf("  Status   Slot: %-10d │   Last vote: %-10d │   Latency: %-3d",
					result.CurrentSlot, result.LastVote, latency)
				countsLine := fmt.Sprintf("║%-78s║", countsContent)
				statusLine := fmt.Sprintf("║%-78s║", statusContent)

				fmt.Print("\033[3A")
				fmt.Print("\033[K")
				fmt.Println(countsLine)
				fmt.Print("\033[K")
				fmt.Println(statusLine)
				fmt.Print("\033[K")
				fmt.Println("╚══════════════════════════════════════════════════════════════════════════════╝")

				logToFile("Slot: %d | Last vote: %d | Category: %s | Latency: %d",
					result.CurrentSlot, result.LastVote, category, latency)
			} else {
				writeDetailedLog("Slot: %d | Last vote: %d | Category: %s | Latency: %d",
					result.CurrentSlot, result.LastVote, category, latency)
			}

			// Check for delinquency
			if result.Delinquent {
				fmt.Println()
				log.Printf("WARNING: Vote account is DELINQUENT!")
				logToFile("WARNING: Vote account is DELINQUENT!")

				if checkDelinquencyWithRetries(checker, config.VotePubkey, config.RetryCount) {
					triggerFailover("vote account is delinquent", config)
					return
				}
				log.Println("Delinquency recovered, continuing monitoring...")
				logToFile("Delinquency recovered, continuing monitoring...")
				printBoxFrame()
				continue
			}

			// Check latency threshold (if set)
			if config.MaxVoteLatency > 0 && result.SlotsBehind > config.MaxVoteLatency {
				fmt.Println()
				log.Printf("WARNING: Vote latency threshold exceeded! (threshold: %d)", config.MaxVoteLatency)
				logToFile("WARNING: Vote latency threshold exceeded! (threshold: %d)", config.MaxVoteLatency)

				if checkLatencyWithRetries(checker, config.VotePubkey, config.MaxVoteLatency, config.RetryCount) {
					triggerFailover(fmt.Sprintf("vote latency exceeded threshold (%d slots)", config.MaxVoteLatency), config)
					return
				}
				log.Println("Latency recovered, continuing monitoring...")
				logToFile("Latency recovered, continuing monitoring...")
				printBoxFrame()
			}
		}
	}
}

// probeTPU checks if the active node's TPU port is reachable via TCP
func probeTPU(address string) bool {
	if address == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// fenceActiveNode attempts to fence the active node before failover.
// Returns (true, nil) if safe to proceed with failover.
// Returns (false, error) if fencing failed and failover must be aborted.
func fenceActiveNode(config *Config) (bool, error) {
	log.Println("Phase 1: Checking active node liveness via TPU port...")

	alive := probeTPU(config.ActiveNodeTPU)

	if !alive {
		log.Printf("Active node TPU (%s) is UNREACHABLE - validator appears to be down", config.ActiveNodeTPU)

		// Best-effort: try SSH fencing anyway
		if config.FencingConfigured {
			log.Println("Attempting best-effort SSH fencing...")
			output, err := sshSetIdentity(config)
			if err != nil {
				log.Printf("SSH fencing failed (expected if node is down): %v", err)
			} else {
				log.Printf("SSH fencing succeeded: %s", strings.TrimSpace(output))
				// Try to copy tower file since SSH works
				if err := copyTowerFile(config); err != nil {
					log.Printf("Tower file copy failed: %v", err)
				}
			}
		}

		// Safe to proceed - validator is down, no double-sign risk
		return true, nil
	}

	// Validator is ALIVE - must fence via SSH
	log.Printf("Active node TPU (%s) is REACHABLE - validator is still running!", config.ActiveNodeTPU)
	log.Println("SSH fencing is REQUIRED before failover can proceed")

	if !config.FencingConfigured {
		return false, fmt.Errorf("active validator is alive (TPU reachable at %s) but SSH fencing is not configured", config.ActiveNodeTPU)
	}

	// Try SSH fencing with retries
	totalAttempts := 1 + config.SSHRetries
	for attempt := 1; attempt <= totalAttempts; attempt++ {
		if attempt > 1 {
			log.Printf("Waiting 5 seconds before SSH retry %d/%d...", attempt, totalAttempts)
			time.Sleep(5 * time.Second)

			// Re-check TPU - maybe node went down during the wait
			if !probeTPU(config.ActiveNodeTPU) {
				log.Println("Active node TPU became unreachable during retry wait - safe to proceed")
				return true, nil
			}
		}

		log.Printf("SSH fencing attempt %d/%d...", attempt, totalAttempts)
		output, err := sshSetIdentity(config)
		if err != nil {
			log.Printf("SSH fencing attempt %d failed: %v", attempt, err)
			continue
		}

		log.Printf("SSH fencing succeeded: %s", strings.TrimSpace(output))

		// Copy tower file for safety
		if err := copyTowerFile(config); err != nil {
			log.Printf("Tower file copy failed (non-fatal): %v", err)
		}

		return true, nil
	}

	// All retries exhausted - unsafe to proceed
	return false, fmt.Errorf("active validator is alive (TPU reachable) but all %d SSH fencing attempts failed - manual intervention required", totalAttempts)
}

// sshSetIdentity switches the active node's identity to the unstaked key via SSH
func sshSetIdentity(config *Config) (string, error) {
	var command string

	if config.RemoteFdctlConfig != "" {
		command = fmt.Sprintf("fdctl set-identity --config %s %s --force",
			config.RemoteFdctlConfig, config.RemoteIdentityKeypair)
	} else if config.RemoteLedgerPath != "" {
		command = fmt.Sprintf("agave-validator --ledger %s set-identity %s",
			config.RemoteLedgerPath, config.RemoteIdentityKeypair)
	} else {
		return "", fmt.Errorf("no remote ledger or fdctl-config configured")
	}

	log.Printf("SSH executing on %s: %s", config.RemoteSSH, command)
	return sshExec(config, command)
}

// sshOptions returns the common SSH/SCP options for the config.
// For SCP, pass portFlag="-P"; for SSH, pass portFlag="-p".
func sshOptions(config *Config, portFlag string) []string {
	args := []string{
		"-o", fmt.Sprintf("ConnectTimeout=%d", config.SSHTimeout),
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "BatchMode=yes",
		portFlag, fmt.Sprintf("%d", config.SSHPort),
	}
	if config.SSHKey != "" {
		args = append(args, "-i", config.SSHKey)
	}
	return args
}

// sshExec runs a command on the remote host via SSH
func sshExec(config *Config, command string) (string, error) {
	args := sshOptions(config, "-p")
	args = append(args, config.RemoteSSH, command)

	cmd := exec.Command("ssh", args...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// copyTowerFile copies the tower file from the active node to the local node via SCP.
// This prevents potential vote conflicts on restart by preserving the vote history.
// Currently only supported for Agave validators.
func copyTowerFile(config *Config) error {
	// Tower copy requires both nodes to have ledger paths configured
	if config.RemoteLedgerPath == "" || config.LedgerPath == "" {
		log.Println("Tower file copy skipped (requires ledger paths for both local and remote nodes)")
		return nil
	}

	// Agave tower file: tower-1_9-<identity>.bin
	towerFileName := fmt.Sprintf("tower-1_9-%s.bin", config.ActiveNodePubkey)
	remotePath := filepath.Join(config.RemoteLedgerPath, towerFileName)
	localPath := filepath.Join(config.LedgerPath, towerFileName)

	log.Printf("Copying tower file: %s", towerFileName)

	args := sshOptions(config, "-P")
	source := fmt.Sprintf("%s:%s", config.RemoteSSH, remotePath)
	args = append(args, source, localPath)

	cmd := exec.Command("scp", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("scp failed: %v, output: %s", err, strings.TrimSpace(string(output)))
	}

	log.Printf("Tower file copied successfully to %s", localPath)
	return nil
}

// triggerFailover executes the two-phase failover:
// Phase 1: Fence the active node (switch its identity or confirm it's dead)
// Phase 2: Switch local identity to staked key
func triggerFailover(reason string, config *Config) {
	log.Printf("=== FAILOVER TRIGGERED ===")
	log.Printf("Reason: %s", reason)

	if config.LogFileHandle != nil {
		timestamp := time.Now().Format("2006/01/02 15:04:05.000000")
		fmt.Fprintf(config.LogFileHandle, "%s === FAILOVER TRIGGERED === Reason: %s\n", timestamp, reason)
	}

	// Pre-failover hook (can abort failover)
	if config.PreFailoverHook != "" {
		log.Printf("Running pre-failover hook: %s", config.PreFailoverHook)
		output, err := runHook(config, config.PreFailoverHook, reason)
		if len(output) > 0 {
			log.Printf("Pre-failover hook output: %s", strings.TrimSpace(output))
		}
		if err != nil {
			log.Printf("=== FAILOVER ABORTED by pre-failover hook ===")
			log.Printf("Hook failed: %v", err)
			identityPubkey, _ := getPubkeyFromKeypair(config.IdentityKeypair)
			sendAlerts(config, reason, identityPubkey, false, fmt.Sprintf("pre-failover hook aborted failover: %v", err))
			os.Exit(1)
		}
		log.Println("Pre-failover hook completed successfully")
	}

	// Phase 1: Fence the active node
	fenced, err := fenceActiveNode(config)
	if !fenced {
		log.Printf("=== FAILOVER ABORTED ===")
		log.Printf("Cannot safely fence active node: %v", err)
		log.Println("Manual intervention required! The active node is alive but unreachable via SSH.")
		log.Println("To resolve: manually switch the active node's identity or shut down its validator process.")

		// Send critical alert about failed fencing
		identityPubkey, _ := getPubkeyFromKeypair(config.IdentityKeypair)
		sendAlerts(config, reason, identityPubkey, false, fmt.Sprintf("fencing failed: %v", err))
		os.Exit(1)
	}

	// Phase 2: Switch local identity to staked key
	log.Println("Phase 2: Switching local identity to staked key...")

	identityPubkey, _ := getPubkeyFromKeypair(config.IdentityKeypair)

	var cmd *exec.Cmd
	var cmdStr string

	switch config.ClientType {
	case "Frankendancer":
		cmdStr = "fdctl set-identity --config " + config.FdctlConfigPath + " " + config.IdentityKeypair + " --force"
		cmd = exec.Command("fdctl", "set-identity", "--config", config.FdctlConfigPath, config.IdentityKeypair, "--force")
	case "Agave":
		cmdStr = "agave-validator --ledger " + config.LedgerPath + " set-identity " + config.IdentityKeypair
		cmd = exec.Command("agave-validator", "--ledger", config.LedgerPath, "set-identity", config.IdentityKeypair)
	default:
		alertErr := fmt.Sprintf("unknown client type '%s', cannot execute set-identity", config.ClientType)
		sendAlerts(config, reason, identityPubkey, false, alertErr)
		log.Fatalf("Error: %s", alertErr)
	}

	log.Printf("Executing: %s", cmdStr)

	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Error executing failover command: %v", err)
		if len(output) > 0 {
			log.Printf("Command output: %s", string(output))
		}
		alertErr := fmt.Sprintf("local set-identity failed: %v", err)
		if len(output) > 0 {
			alertErr = fmt.Sprintf("local set-identity failed: %v (output: %s)", err, strings.TrimSpace(string(output)))
		}
		sendAlerts(config, reason, identityPubkey, false, alertErr)
		os.Exit(1)
	}

	if len(output) > 0 {
		log.Printf("Command output: %s", string(output))
	}
	log.Println("Failover command executed successfully")

	// Verify identity switch via RPC
	verifyIdentitySwitch(config, reason)

	// Post-failover hook (best-effort, does not affect outcome)
	if config.PostFailoverHook != "" {
		log.Printf("Running post-failover hook: %s", config.PostFailoverHook)
		output, err := runHook(config, config.PostFailoverHook, reason)
		if len(output) > 0 {
			log.Printf("Post-failover hook output: %s", strings.TrimSpace(output))
		}
		if err != nil {
			log.Printf("Warning: Post-failover hook failed (non-fatal): %v", err)
		} else {
			log.Println("Post-failover hook completed successfully")
		}
	}
}

// verifyIdentitySwitch confirms the identity was switched by querying the RPC
func verifyIdentitySwitch(config *Config, reason string) {
	log.Println("Verifying identity switch via RPC...")

	expectedPubkey, err := getPubkeyFromKeypair(config.IdentityKeypair)
	if err != nil {
		log.Printf("Warning: Could not read keypair for verification: %v", err)
		return
	}

	client := rpc.NewClient(config.LocalRPCEndpoint)
	currentIdentity, err := client.GetIdentity()
	if err != nil {
		log.Printf("Warning: Could not query identity for verification: %v", err)
		return
	}

	if currentIdentity == expectedPubkey {
		log.Printf("Identity switch VERIFIED: node is now running as %s", currentIdentity)
		sendAlerts(config, reason, expectedPubkey, true, "")
	} else {
		log.Printf("WARNING: Identity mismatch! Expected %s but got %s", expectedPubkey, currentIdentity)
		log.Println("The set-identity command may not have taken effect")
		sendAlerts(config, reason, expectedPubkey, false, fmt.Sprintf("identity mismatch: expected %s, got %s", expectedPubkey, currentIdentity))
		os.Exit(1)
	}
}

// runHook executes a hook command via bash -l -c with failover context as environment variables.
// Returns the combined output and any error.
func runHook(config *Config, hookCmd string, reason string) (string, error) {
	cmd := exec.Command("bash", "-l", "-c", hookCmd)
	cmd.Env = append(os.Environ(),
		"FAILOVER_REASON="+reason,
		"FAILOVER_VOTE_PUBKEY="+config.VotePubkey,
		"FAILOVER_ACTIVE_NODE="+config.ActiveNodePubkey,
		"FAILOVER_ACTIVE_IP="+config.ActiveNodeIP,
		"FAILOVER_LOCAL_IDENTITY="+config.PreviousIdentity,
		"FAILOVER_NEW_IDENTITY="+func() string {
			pk, _ := getPubkeyFromKeypair(config.IdentityKeypair)
			return pk
		}(),
		"FAILOVER_HOSTNAME="+config.Hostname,
	)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// getRequiredCommand returns the required CLI command based on client type
func getRequiredCommand(clientType string) string {
	switch clientType {
	case "Agave":
		return "agave-validator"
	case "Frankendancer":
		return "fdctl"
	default:
		return ""
	}
}

// isCommandAvailable checks if a command is available in PATH
func isCommandAvailable(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

// getPubkeyFromKeypair reads a Solana keypair JSON file and returns the public key as base58
// Solana keypair format: JSON array of 64 bytes (first 32 = secret key, last 32 = public key)
func getPubkeyFromKeypair(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read keypair file: %w", err)
	}
	return pubkeyFromJSON(data)
}

func pubkeyFromJSON(data []byte) (string, error) {
	var keypair []byte
	if err := json.Unmarshal(data, &keypair); err != nil {
		return "", fmt.Errorf("failed to parse keypair JSON: %w", err)
	}

	if len(keypair) != 64 {
		return "", fmt.Errorf("invalid keypair length: expected 64 bytes, got %d", len(keypair))
	}

	pubkey := keypair[32:64]
	return base58.Encode(pubkey), nil
}

// sendAlerts sends notifications via configured alert channels
func sendAlerts(config *Config, reason string, newIdentity string, success bool, errorMsg string) {
	transition := fmt.Sprintf("[%s STANDBY→ACTIVE]", config.Hostname)

	if config.PagerDutyKey != "" {
		sendPagerDutyAlert(config.PagerDutyKey, config.VotePubkey, config.PreviousIdentity, newIdentity, transition, reason, success, errorMsg)
	}

	if config.WebhookURL != "" {
		sendWebhookAlert(config.WebhookURL, config.WebhookBody, config.VotePubkey, config.PreviousIdentity, newIdentity, transition, reason, success, errorMsg)
	}
}

// sendPagerDutyAlert sends an alert to PagerDuty
func sendPagerDutyAlert(routingKey, votePubkey, previousIdentity, newIdentity, transition, reason string, success bool, errorMsg string) {
	log.Println("Sending PagerDuty alert...")

	var summary string
	var severity string
	status := "SUCCESS"
	if !success {
		status = "FAILED"
		severity = "critical"
	} else {
		severity = "warning"
	}

	if success {
		summary = fmt.Sprintf("%s Failover %s for %s: %s", transition, status, votePubkey, reason)
	} else {
		summary = fmt.Sprintf("%s Failover %s for %s: %s - %s", transition, status, votePubkey, reason, errorMsg)
	}

	payload := map[string]interface{}{
		"routing_key":  routingKey,
		"event_action": "trigger",
		"payload": map[string]interface{}{
			"summary":  summary,
			"severity": severity,
			"source":   fmt.Sprintf("validator-%s", newIdentity[:8]),
			"custom_details": map[string]string{
				"transition":        transition,
				"reason":            reason,
				"vote_account":      votePubkey,
				"previous_identity": previousIdentity,
				"new_identity":      newIdentity,
				"status":            status,
				"error":             errorMsg,
			},
		},
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Warning: Failed to marshal PagerDuty payload: %v", err)
		return
	}

	resp, err := http.Post(
		"https://events.pagerduty.com/v2/enqueue",
		"application/json",
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		log.Printf("Warning: Failed to send PagerDuty alert: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.Println("PagerDuty alert sent successfully")
	} else {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("Warning: PagerDuty returned status %d: %s", resp.StatusCode, string(body))
	}
}

// sendWebhookAlert sends an alert to a generic webhook URL
func sendWebhookAlert(webhookURL, customBody, votePubkey, previousIdentity, newIdentity, transition, reason string, success bool, errorMsg string) {
	log.Printf("Sending webhook alert to %s...", webhookURL)

	var jsonData []byte
	var err error

	status := "SUCCESS"
	if !success {
		status = "FAILED"
	}

	if customBody != "" {
		body := strings.ReplaceAll(customBody, "{reason}", reason)
		body = strings.ReplaceAll(body, "{identity}", newIdentity)
		body = strings.ReplaceAll(body, "{status}", status)
		body = strings.ReplaceAll(body, "{error}", errorMsg)
		body = strings.ReplaceAll(body, "{transition}", transition)
		body = strings.ReplaceAll(body, "{vote_account}", votePubkey)
		body = strings.ReplaceAll(body, "{previous_identity}", previousIdentity)
		body = strings.ReplaceAll(body, "{new_identity}", newIdentity)
		jsonData = []byte(body)
	} else {
		var text string
		if success {
			text = fmt.Sprintf("%s Failover %s\nReason: %s\nVote account: %s\nPrevious identity: %s\nNew identity: %s",
				transition, status, reason, votePubkey, previousIdentity, newIdentity)
		} else {
			text = fmt.Sprintf("%s Failover %s\nReason: %s\nVote account: %s\nPrevious identity: %s\nNew identity: %s\nError: %s",
				transition, status, reason, votePubkey, previousIdentity, newIdentity, errorMsg)
		}
		payload := map[string]string{
			"text": text,
		}
		jsonData, err = json.Marshal(payload)
		if err != nil {
			log.Printf("Warning: Failed to marshal webhook payload: %v", err)
			return
		}
	}

	resp, err := http.Post(webhookURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		log.Printf("Warning: Failed to send webhook alert: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.Println("Webhook alert sent successfully")
	} else {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("Warning: Webhook returned status %d: %s", resp.StatusCode, string(body))
	}
}
