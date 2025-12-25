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

	log.Println("Starting automatic failover manager...")
	log.Printf("Local RPC: %s", config.LocalRPCEndpoint)
	log.Printf("Monitoring vote account: %s", config.VotePubkey)
	if config.MaxVoteLatency > 0 {
		log.Printf("Max vote latency threshold: %d slots", config.MaxVoteLatency)
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

	// Step 1: Wait for local node to be healthy before proceeding
	if err := checker.WaitForHealthy(ctx); err != nil {
		log.Fatalf("Failed waiting for node health: %v", err)
	}

	// Perform detailed health check of local node (batched: identity, version, cluster nodes)
	localResult, err := checker.CheckLocal()
	if err != nil {
		log.Fatalf("Health check failed: %v", err)
	}

	// Store client type in config
	config.ClientType = localResult.ClientType

	// Display node info
	if localResult.ClientType != "" {
		log.Printf("Client: %s", localResult.ClientType)
		log.Printf("Version: %s", localResult.Version)
	}

	log.Printf("Local node health check result:")
	log.Printf("  Identity: %s", localResult.Identity)
	log.Printf("  Healthy: %v (from getHealth RPC)", localResult.Healthy)

	// Display gossip info if available
	if localResult.Gossip != nil {
		log.Printf("  Gossip status:")
		log.Printf("    In gossip: %v", localResult.Gossip.InGossip)
		if localResult.Gossip.GossipAddress != "" {
			log.Printf("    Gossip address: %s", localResult.Gossip.GossipAddress)
			log.Printf("    TCP reachable: %v", localResult.Gossip.TCPReachable)
		}
	}

	// Step 2: Check if required CLI tool is available in PATH
	requiredCmd := getRequiredCommand(localResult.ClientType)
	if requiredCmd != "" {
		if !isCommandAvailable(requiredCmd) {
			log.Fatalf("Error: Required command '%s' not found in PATH", requiredCmd)
		}
		log.Printf("Required command '%s' found in PATH", requiredCmd)
	}

	// Validate client-specific parameters
	switch config.ClientType {
	case "Frankendancer":
		if config.ConfigPath == "" {
			log.Fatal("Error: --config is required for Frankendancer nodes")
		}
	case "Agave":
		if config.LedgerPath == "" {
			log.Fatal("Error: --ledger is required for Agave nodes")
		}
	}

	// Step 3: Check if provided identity keypair matches current node identity
	keypairPubkey, err := getPubkeyFromKeypair(config.IdentityKeypair)
	if err != nil {
		log.Fatalf("Error reading identity keypair: %v", err)
	}
	log.Printf("Identity keypair pubkey: %s", keypairPubkey)

	if keypairPubkey == localResult.Identity {
		log.Fatal("Error: The provided identity keypair is already active on this node.")
	}
	log.Printf("Identity check passed: provided keypair is not active")

	// Step 4: Check if vote account is delinquent at startup
	log.Printf("Checking if vote account %s is delinquent...", config.VotePubkey)

	if checkDelinquencyWithRetries(checker, config.VotePubkey, config.RetryCount) {
		// Delinquent after retries, trigger failover
		triggerFailover("vote account is delinquent", &config)
		return
	}

	// Step 5: Continuous monitoring
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
			log.Printf("Current slot: %d | Last vote: %d | Slots behind: %d",
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
					triggerFailover("vote latency exceeded threshold", config)
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
		// fdctl set-identity --config <path/to/config.toml> <path/to/keypair.json>
		cmdStr = "fdctl set-identity --config " + config.ConfigPath + " " + config.IdentityKeypair
		cmd = exec.Command("fdctl", "set-identity", "--config", config.ConfigPath, config.IdentityKeypair)
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
func sendAlerts(config *Config, reason string, identity string, success bool, errorMsg string) {
	if config.PagerDutyKey != "" {
		sendPagerDutyAlert(config.PagerDutyKey, reason, identity, success, errorMsg)
	}

	if config.WebhookURL != "" {
		sendWebhookAlert(config.WebhookURL, config.WebhookBody, reason, identity, success, errorMsg)
	}
}

// sendPagerDutyAlert sends an alert to PagerDuty
func sendPagerDutyAlert(routingKey string, reason string, identity string, success bool, errorMsg string) {
	log.Println("Sending PagerDuty alert...")

	var summary string
	var severity string
	if success {
		summary = fmt.Sprintf("Validator failover SUCCESS: %s", reason)
		severity = "warning"
	} else {
		summary = fmt.Sprintf("Validator failover FAILED: %s - %s", reason, errorMsg)
		severity = "critical"
	}

	status := "success"
	if !success {
		status = "failed"
	}

	payload := map[string]interface{}{
		"routing_key":  routingKey,
		"event_action": "trigger",
		"payload": map[string]interface{}{
			"summary":  summary,
			"severity": severity,
			"source":   fmt.Sprintf("validator-%s", identity[:8]),
			"custom_details": map[string]string{
				"reason":       reason,
				"new_identity": identity,
				"status":       status,
				"error":        errorMsg,
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
func sendWebhookAlert(webhookURL string, customBody string, reason string, identity string, success bool, errorMsg string) {
	log.Printf("Sending webhook alert to %s...", webhookURL)

	var jsonData []byte
	var err error

	status := "success"
	if !success {
		status = "failed"
	}

	if customBody != "" {
		// Replace placeholders in custom body
		body := strings.ReplaceAll(customBody, "{reason}", reason)
		body = strings.ReplaceAll(body, "{identity}", identity)
		body = strings.ReplaceAll(body, "{status}", status)
		body = strings.ReplaceAll(body, "{error}", errorMsg)
		jsonData = []byte(body)
	} else {
		// Default payload
		payload := map[string]interface{}{
			"event":     "failover_triggered",
			"reason":    reason,
			"identity":  identity,
			"status":    status,
			"error":     errorMsg,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
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
