package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mr-tron/base58"
	"github.com/schmiatz/solana-validator-automatic-failover/internal/health"
	"github.com/schmiatz/solana-validator-automatic-failover/internal/rpc"
)

// Config holds the application configuration
type Config struct {
	LocalRPCEndpoint string
	LogFile          string
	VotePubkey       string
	MaxVoteLatency   int64
	RetryCount       int
	IdentityKeypair  string
	ConfigPath       string // For Frankendancer
	LedgerPath       string // For Agave
	ClientType       string // Detected client type (Agave/Frankendancer)
	PagerDutyKey     string // PagerDuty routing key for alerts
	WebhookURL       string // Generic webhook URL
	WebhookBody      string // Custom webhook body template
	NodeMode         string // ACTIVE or STANDBY
	PreviousIdentity string // Identity before failover
}

func main() {
	// Parse command line flags
	rpcEndpoint := flag.String("rpc", "http://127.0.0.1:8899", "Local RPC endpoint to query")
	logFile := flag.String("log", "", "Path to log file (logs to stdout and file if set)")
	votePubkey := flag.String("votepubkey", "", "Vote account public key to monitor (required)")
	maxVoteLatency := flag.Int64("max-vote-latency", 0, "Max slots behind before triggering failover (0 = disabled)")
	retryCount := flag.Int("retry-count", 3, "Number of retries before triggering failover")
	identityKeypair := flag.String("identity-keypair", "", "Path to identity keypair JSON file (required)")
	configPath := flag.String("config", "", "Path to config.toml (required for Frankendancer)")
	ledgerPath := flag.String("ledger", "", "Path to validator ledger directory (required for Agave)")
	pagerdutyKey := flag.String("pagerduty-key", "", "PagerDuty routing key for alerts on failover")
	webhookURL := flag.String("webhook-url", "", "Generic webhook URL to POST on failover")
	webhookBody := flag.String("webhook-body", "", "Custom webhook body (supports {reason}, {identity} placeholders)")
	flag.Parse()

	// Validate required parameters
	if *votePubkey == "" {
		log.Fatal("Error: --votepubkey is required")
	}
	if *identityKeypair == "" {
		log.Fatal("Error: --identity-keypair is required")
	}

	config := Config{
		LocalRPCEndpoint: *rpcEndpoint,
		LogFile:          *logFile,
		VotePubkey:       *votePubkey,
		MaxVoteLatency:   *maxVoteLatency,
		RetryCount:       *retryCount,
		IdentityKeypair:  *identityKeypair,
		ConfigPath:       *configPath,
		LedgerPath:       *ledgerPath,
		PagerDutyKey:     *pagerdutyKey,
		WebhookURL:       *webhookURL,
		WebhookBody:      *webhookBody,
	}

	// Set up logging
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	// If log file is specified, write to both stdout and file
	if config.LogFile != "" {
		logFileHandle, err := os.OpenFile(config.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Fatalf("Failed to open log file %s: %v", config.LogFile, err)
		}
		defer logFileHandle.Close()

		// Write to both stdout and log file
		multiWriter := io.MultiWriter(os.Stdout, logFileHandle)
		log.SetOutput(multiWriter)
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
		name    string
		passed  bool
		errMsg  string
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
		healthCheck.errMsg = "Node is not healthy"
		failedCheck = &healthCheck
	}
	checks = append(checks, healthCheck)

	// Gossip check
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
	cliPassed := requiredCmd == "" || isCommandAvailable(requiredCmd)
	cliCheck := checkResult{name: "CLI", passed: cliPassed}
	if !cliPassed {
		cliCheck.errMsg = fmt.Sprintf("Required command '%s' not found in PATH", requiredCmd)
		if failedCheck == nil {
			failedCheck = &cliCheck
		}
	}
	checks = append(checks, cliCheck)

	// Config check (client-specific parameters)
	configPassed := true
	configErrMsg := ""
	switch config.ClientType {
	case "Frankendancer":
		if config.ConfigPath == "" {
			configPassed = false
			configErrMsg = "--config is required for Frankendancer nodes"
		}
	case "Agave":
		if config.LedgerPath == "" {
			configPassed = false
			configErrMsg = "--ledger is required for Agave nodes"
		}
	}
	configCheck := checkResult{name: "Config", passed: configPassed, errMsg: configErrMsg}
	if !configPassed && failedCheck == nil {
		failedCheck = &configCheck
	}
	checks = append(checks, configCheck)

	// Keypair check - read and validate
	keypairPubkey, err := getPubkeyFromKeypair(config.IdentityKeypair)
	keypairReadable := err == nil
	keypairCheck := checkResult{name: "Keypair", passed: keypairReadable}
	if !keypairReadable {
		keypairCheck.errMsg = fmt.Sprintf("Cannot read identity keypair: %v", err)
		if failedCheck == nil {
			failedCheck = &keypairCheck
		}
	}
	checks = append(checks, keypairCheck)

	// Identity check - keypair must not be currently active on this node
	identityPassed := keypairReadable && keypairPubkey != localResult.Identity
	identityCheck := checkResult{name: "Identity", passed: identityPassed}
	if keypairReadable && keypairPubkey == localResult.Identity {
		identityCheck.errMsg = "Provided keypair is already active on this node"
		if failedCheck == nil {
			failedCheck = &identityCheck
		}
	}
	checks = append(checks, identityCheck)

	// Vote account check - determine ACTIVE/STANDBY mode
	voteAccountResult, voteErr := checker.Check(config.VotePubkey)
	votePassed := voteErr == nil
	voteCheck := checkResult{name: "Vote", passed: votePassed}
	if voteErr != nil {
		voteCheck.errMsg = fmt.Sprintf("Cannot check vote account: %v", voteErr)
		if failedCheck == nil {
			failedCheck = &voteCheck
		}
	}
	checks = append(checks, voteCheck)

	// Determine node mode and set config
	modePassed := true
	modeErrMsg := ""
	if votePassed {
		if voteAccountResult.NodePubkey == localResult.Identity {
			config.NodeMode = "ACTIVE"
		} else {
			config.NodeMode = "STANDBY"
		}
		config.PreviousIdentity = localResult.Identity

		// Mode-specific keypair validation
		if config.NodeMode == "ACTIVE" && keypairReadable && keypairPubkey == voteAccountResult.NodePubkey {
			modePassed = false
			modeErrMsg = "On ACTIVE node, --identity-keypair must be different from voting identity"
		} else if config.NodeMode == "STANDBY" && keypairReadable && keypairPubkey != voteAccountResult.NodePubkey {
			modePassed = false
			modeErrMsg = fmt.Sprintf("On STANDBY node, --identity-keypair must match voting identity (%s)", voteAccountResult.NodePubkey)
		}
	}
	modeCheck := checkResult{name: "Mode", passed: modePassed, errMsg: modeErrMsg}
	if !modePassed && failedCheck == nil {
		failedCheck = &modeCheck
	}
	checks = append(checks, modeCheck)

	// === Print the table ===
	log.Println("╔══════════════════════════════════════════════════════════════════════════════╗")
	log.Println("║                        Automatic Failover Manager                            ║")
	log.Println("╠══════════════════════════════════════════════════════════════════════════════╣")
	log.Printf("║  Vote Account     %-58s║", config.VotePubkey)
	log.Printf("║  Status           %-58s║", config.NodeMode)
	if config.MaxVoteLatency > 0 {
		log.Printf("║  Latency Limit    %-58s║", fmt.Sprintf("%d slots", config.MaxVoteLatency))
	} else {
		log.Printf("║  Latency Limit    %-58s║", "delinquency only (~150 slots)")
	}
	clientVersion := fmt.Sprintf("%s %s", localResult.ClientType, localResult.Version)
	log.Printf("║  Client           %-58s║", clientVersion)
	log.Printf("║  Active Identity  %-58s║", localResult.Identity)
	log.Printf("║  Failover Key     %-58s║", keypairPubkey)

	// Build checks line - split into two rows if needed
	checksLine1 := ""
	checksLine2 := ""
	for i, c := range checks {
		mark := "✓"
		if !c.passed {
			mark = "✗"
		}
		checkStr := c.name + " " + mark + "  "
		if i < 4 {
			checksLine1 += checkStr
		} else {
			checksLine2 += checkStr
		}
	}
	log.Printf("║  Checks           %-58s║", strings.TrimSpace(checksLine1))
	if checksLine2 != "" {
		log.Printf("║                   %-58s║", strings.TrimSpace(checksLine2))
	}
	log.Println("╚══════════════════════════════════════════════════════════════════════════════╝")

	// If any check failed, print error and exit
	if failedCheck != nil {
		log.Fatalf("Error: %s", failedCheck.errMsg)
	}

	// === Continue with monitoring ===

	if checkDelinquencyWithRetries(checker, config.VotePubkey, config.RetryCount) {
		// Delinquent after retries, trigger failover
		triggerFailover("vote account is delinquent", &config)
		return
	}

	// Step 7: Continuous monitoring
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
			log.Printf("Vote account is NOT delinquent (slots behind: %d)", result.SlotsBehind)
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

	// Only trigger if we confirmed delinquency at least once
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

	// Only trigger if we confirmed latency exceeded at least once
	return exceededCount > 0
}

// monitorVoteAccount continuously monitors vote account for delinquency and latency
func monitorVoteAccount(ctx context.Context, checker *health.Checker, config *Config) {
	const checkInterval = 1 * time.Second

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	if config.MaxVoteLatency > 0 {
		log.Printf("Monitoring every %v (latency threshold: %d slots)...", checkInterval, config.MaxVoteLatency)
	} else {
		log.Printf("Monitoring every %v (delinquency only)...", checkInterval)
	}

	for {
		select {
		case <-ctx.Done():
			log.Println("Monitoring stopped due to shutdown signal")
			return
		case <-ticker.C:
			result, err := checker.Check(config.VotePubkey)
			if err != nil {
				log.Printf("Error checking vote account: %v", err)
				continue
			}

			// Always print current status
			log.Printf("Current slot: %d | Last vote: %d | Vote latency: %d",
				result.CurrentSlot, result.LastVote, result.SlotsBehind)

			// Check for delinquency
			if result.Delinquent {
				log.Printf("WARNING: Vote account is DELINQUENT!")

				// Verify with retries before triggering failover
				if checkDelinquencyWithRetries(checker, config.VotePubkey, config.RetryCount) {
					triggerFailover("vote account is delinquent", config)
					return
				}
				log.Println("Delinquency recovered, continuing monitoring...")
				continue
			}

			// Check latency threshold (if set)
			if config.MaxVoteLatency > 0 && result.SlotsBehind > config.MaxVoteLatency {
				log.Printf("WARNING: Vote latency threshold exceeded! (threshold: %d)", config.MaxVoteLatency)

				// Verify with retries before triggering failover
				if checkLatencyWithRetries(checker, config.VotePubkey, config.MaxVoteLatency, config.RetryCount) {
					triggerFailover(fmt.Sprintf("vote latency exceeded threshold (%d slots)", config.MaxVoteLatency), config)
					return
				}
				log.Println("Latency recovered, continuing monitoring...")
			}
		}
	}
}

// triggerFailover executes the failover command
func triggerFailover(reason string, config *Config) {
	log.Printf("=== FAILOVER TRIGGERED ===")
	log.Printf("Reason: %s", reason)

	var cmd *exec.Cmd
	var cmdStr string

	switch config.ClientType {
	case "Frankendancer":
		// fdctl set-identity --config <path/to/config.toml> <path/to/keypair.json> --force
		cmdStr = "fdctl set-identity --config " + config.ConfigPath + " " + config.IdentityKeypair + " --force"
		cmd = exec.Command("fdctl", "set-identity", "--config", config.ConfigPath, config.IdentityKeypair, "--force")
	case "Agave":
		// agave-validator --ledger </path/to/validator-ledger> set-identity <path/to/keypair.json>
		cmdStr = "agave-validator --ledger " + config.LedgerPath + " set-identity " + config.IdentityKeypair
		cmd = exec.Command("agave-validator", "--ledger", config.LedgerPath, "set-identity", config.IdentityKeypair)
	default:
		log.Fatalf("Error: Unknown client type '%s', cannot execute failover", config.ClientType)
	}

	log.Printf("Executing: %s", cmdStr)

	// Get identity pubkey for alerts
	identityPubkey, _ := getPubkeyFromKeypair(config.IdentityKeypair)

	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Error executing failover command: %v", err)
		if len(output) > 0 {
			log.Printf("Command output: %s", string(output))
		}
		sendAlerts(config, reason, identityPubkey, false, "set-identity command failed")
		os.Exit(1)
	}

	if len(output) > 0 {
		log.Printf("Command output: %s", string(output))
	}
	log.Println("Failover command executed successfully")

	// Verify identity switch via RPC
	verifyIdentitySwitch(config, reason)
}

// verifyIdentitySwitch confirms the identity was switched by querying the RPC
func verifyIdentitySwitch(config *Config, reason string) {
	log.Println("Verifying identity switch via RPC...")

	// Get expected pubkey from keypair file
	expectedPubkey, err := getPubkeyFromKeypair(config.IdentityKeypair)
	if err != nil {
		log.Printf("Warning: Could not read keypair for verification: %v", err)
		return
	}

	// Query current identity from RPC
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

	var keypair []byte
	if err := json.Unmarshal(data, &keypair); err != nil {
		return "", fmt.Errorf("failed to parse keypair JSON: %w", err)
	}

	if len(keypair) != 64 {
		return "", fmt.Errorf("invalid keypair length: expected 64 bytes, got %d", len(keypair))
	}

	// Public key is the last 32 bytes
	pubkey := keypair[32:64]
	return base58.Encode(pubkey), nil
}

// sendAlerts sends notifications via configured alert channels
func sendAlerts(config *Config, reason string, newIdentity string, success bool, errorMsg string) {
	// Determine transition direction
	var transition string
	if config.NodeMode == "ACTIVE" {
		transition = "ACTIVE→STANDBY"
	} else {
		transition = "STANDBY→ACTIVE"
	}

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

	// Include vote pubkey in summary for visibility in Slack integration
	if success {
		summary = fmt.Sprintf("[%s] Failover %s for %s: %s", transition, status, votePubkey, reason)
	} else {
		summary = fmt.Sprintf("[%s] Failover %s for %s: %s - %s", transition, status, votePubkey, reason, errorMsg)
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
		// Replace placeholders in custom body
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
		// Default payload (Slack-compatible)
		var text string
		if success {
			text = fmt.Sprintf("[%s] Failover %s\nReason: %s\nVote account: %s\nPrevious identity: %s\nNew identity: %s",
				transition, status, reason, votePubkey, previousIdentity, newIdentity)
		} else {
			text = fmt.Sprintf("[%s] Failover %s\nReason: %s\nVote account: %s\nPrevious identity: %s\nNew identity: %s\nError: %s",
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
