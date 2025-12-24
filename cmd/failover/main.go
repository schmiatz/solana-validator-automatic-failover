package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/schmiatz/solana-validator-automatic-failover/internal/health"
	"github.com/schmiatz/solana-validator-automatic-failover/internal/rpc"
)

// Config holds the application configuration
type Config struct {
	LocalRPCEndpoint string
}

func main() {
	// Parse command line flags
	rpcEndpoint := flag.String("rpc", "http://127.0.0.1:58000", "Local RPC endpoint to query")
	flag.Parse()

	config := Config{
		LocalRPCEndpoint: *rpcEndpoint,
	}

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.Println("Starting automatic failover manager...")
	log.Printf("Local RPC: %s", config.LocalRPCEndpoint)

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
	result, err := checker.CheckLocal()
	if err != nil {
		log.Printf("Health check failed: %v", err)
	} else {
		log.Printf("Health check result:")
		log.Printf("  Identity: %s", result.Identity)
		log.Printf("  Healthy: %v (from getHealth RPC)", result.Healthy)

		// Display gossip info if available
		if result.Gossip != nil {
			log.Printf("  Gossip status:")
			log.Printf("    In gossip: %v", result.Gossip.InGossip)
			if result.Gossip.GossipAddress != "" {
				log.Printf("    Gossip address: %s", result.Gossip.GossipAddress)
				log.Printf("    TCP reachable: %v", result.Gossip.TCPReachable)
			}
		}
	}

	log.Println("Initial health check complete. Next steps will add continuous monitoring.")
}
