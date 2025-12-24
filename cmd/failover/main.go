package main

import (
	"context"
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/schmiatz/solana-validator-automatic-failover/internal/health"
	"github.com/schmiatz/solana-validator-automatic-failover/internal/rpc"
)

// Config holds the application configuration
type Config struct {
	LocalRPCEndpoint string
	LogFile          string
	VotePubkey       string
	MaxVoteLatency   int64
}

func main() {
	// Parse command line flags
	rpcEndpoint := flag.String("rpc", "http://127.0.0.1:8899", "Local RPC endpoint to query")
	logFile := flag.String("log", "", "Path to log file (logs to stdout and file if set)")
	votePubkey := flag.String("votepubkey", "", "Vote account public key to monitor (required)")
	maxVoteLatency := flag.Int64("max-vote-latency", 0, "Max slots behind before triggering failover (0 = disabled)")
	flag.Parse()

	// Validate required parameters
	if *votePubkey == "" {
		log.Fatal("Error: --votepubkey is required")
	}

	config := Config{
		LocalRPCEndpoint: *rpcEndpoint,
		LogFile:          *logFile,
		VotePubkey:       *votePubkey,
		MaxVoteLatency:   *maxVoteLatency,
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

	// Detect node type and version
	clientType, version, err := localClient.DetectNodeType()
	if err != nil {
		log.Printf("Warning: Could not detect node type: %v", err)
	} else {
		log.Printf("Client: %s", clientType)
		log.Printf("Version: %s", version)
	}

	log.Println("Performing detailed health check...")

	// Perform detailed health check of local node
	localResult, err := checker.CheckLocal()
	if err != nil {
		log.Printf("Health check failed: %v", err)
	} else {
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
	}

	// Step 2: Check if vote account is delinquent at startup
	log.Printf("Checking if vote account %s is delinquent...", config.VotePubkey)

	if checkDelinquencyWithRetries(checker, config.VotePubkey) {
		// Delinquent after retries, trigger failover
		triggerFailover("vote account is delinquent")
		return
	}

	log.Println("Vote account is not delinquent, starting continuous monitoring...")

	// Step 3: Continuous monitoring
	monitorVoteAccount(ctx, checker, config.VotePubkey, config.MaxVoteLatency)
}

// checkDelinquencyWithRetries checks if vote account is delinquent with 2 retries (1 second apart)
// Returns true if still delinquent after all retries
func checkDelinquencyWithRetries(checker *health.Checker, votePubkey string) bool {
	const maxAttempts = 3
	const retryInterval = 1 * time.Second

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result, err := checker.Check(votePubkey)
		if err != nil {
			log.Printf("Attempt %d/%d: Error checking vote account: %v", attempt, maxAttempts, err)
			if attempt < maxAttempts {
				time.Sleep(retryInterval)
			}
			continue
		}

		if !result.Delinquent {
			log.Printf("Attempt %d/%d: Vote account is NOT delinquent (last vote: %d, slots behind: %d)",
				attempt, maxAttempts, result.LastVote, result.SlotsBehind)
			return false
		}

		log.Printf("Attempt %d/%d: Vote account IS DELINQUENT (last vote: %d, slots behind: %d)",
			attempt, maxAttempts, result.LastVote, result.SlotsBehind)

		if attempt < maxAttempts {
			log.Printf("Retrying in %v...", retryInterval)
			time.Sleep(retryInterval)
		}
	}

	return true
}

// checkLatencyWithRetries checks if vote latency exceeds threshold with 2 retries (1 second apart)
// Returns true if still exceeds threshold after all retries
func checkLatencyWithRetries(checker *health.Checker, votePubkey string, maxLatency int64) bool {
	const maxAttempts = 3
	const retryInterval = 1 * time.Second

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result, err := checker.Check(votePubkey)
		if err != nil {
			log.Printf("Attempt %d/%d: Error checking vote account: %v", attempt, maxAttempts, err)
			if attempt < maxAttempts {
				time.Sleep(retryInterval)
			}
			continue
		}

		if result.SlotsBehind <= maxLatency {
			log.Printf("Attempt %d/%d: Vote latency OK (slots behind: %d, threshold: %d)",
				attempt, maxAttempts, result.SlotsBehind, maxLatency)
			return false
		}

		log.Printf("Attempt %d/%d: Vote latency EXCEEDED (slots behind: %d, threshold: %d)",
			attempt, maxAttempts, result.SlotsBehind, maxLatency)

		if attempt < maxAttempts {
			log.Printf("Retrying in %v...", retryInterval)
			time.Sleep(retryInterval)
		}
	}

	return true
}

// monitorVoteAccount continuously monitors vote account for delinquency and latency
func monitorVoteAccount(ctx context.Context, checker *health.Checker, votePubkey string, maxLatency int64) {
	const checkInterval = 1 * time.Second

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	if maxLatency > 0 {
		log.Printf("Monitoring every %v (latency threshold: %d slots)...", checkInterval, maxLatency)
	} else {
		log.Printf("Monitoring every %v (latency threshold: disabled, delinquency only)...", checkInterval)
	}

	for {
		select {
		case <-ctx.Done():
			log.Println("Monitoring stopped due to shutdown signal")
			return
		case <-ticker.C:
			result, err := checker.Check(votePubkey)
			if err != nil {
				log.Printf("Error checking vote account: %v", err)
				continue
			}

			// Check for delinquency
			if result.Delinquent {
				log.Printf("WARNING: Vote account is DELINQUENT! (slots behind: %d)", result.SlotsBehind)

				// Verify with retries before triggering failover
				if checkDelinquencyWithRetries(checker, votePubkey) {
					triggerFailover("vote account is delinquent")
					return
				}
				log.Println("Delinquency recovered, continuing monitoring...")
				continue
			}

			// Check latency threshold (if set)
			if maxLatency > 0 && result.SlotsBehind > maxLatency {
				log.Printf("WARNING: Vote latency threshold exceeded! (slots behind: %d, threshold: %d)",
					result.SlotsBehind, maxLatency)

				// Verify with retries before triggering failover
				if checkLatencyWithRetries(checker, votePubkey, maxLatency) {
					triggerFailover("vote latency exceeded threshold")
					return
				}
				log.Println("Latency recovered, continuing monitoring...")
			}
		}
	}
}

// triggerFailover executes the failover command
func triggerFailover(reason string) {
	log.Printf("=== FAILOVER TRIGGERED ===")
	log.Printf("Reason: %s", reason)
	log.Printf("I would do the set-identity now")
	// TODO: Implement actual fdctl set-identity command
}
