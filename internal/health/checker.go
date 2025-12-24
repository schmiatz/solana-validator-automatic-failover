package health

import (
	"context"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/schmiatz/solana-validator-automatic-failover/internal/rpc"
)

const (
	// Default interval for health check retries when node is unhealthy
	defaultCheckInterval = 3 * time.Second
)

// Checker monitors node health
type Checker struct {
	client *rpc.Client
}

// NewChecker creates a new health checker
func NewChecker(client *rpc.Client) *Checker {
	return &Checker{
		client: client,
	}
}

// GossipInfo contains information about a node's gossip status
type GossipInfo struct {
	InGossip      bool
	GossipAddress string
	TCPReachable  bool
	NodePubkey    string
}

// WaitForHealthy blocks until the local node is healthy or context is cancelled
func (c *Checker) WaitForHealthy(ctx context.Context) error {
	log.Println("Checking local node health...")

	ticker := time.NewTicker(defaultCheckInterval)
	defer ticker.Stop()

	// Check immediately first
	if err := c.client.GetHealth(); err == nil {
		log.Println("Local node is healthy")
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := c.client.GetHealth(); err != nil {
				log.Printf("Node not healthy yet: %v", err)
				continue
			}
			log.Println("Local node is healthy")
			return nil
		}
	}
}

// IsHealthy checks if the node is currently healthy
func (c *Checker) IsHealthy() bool {
	return c.client.GetHealth() == nil
}

// CheckResult contains the result of a health check
type CheckResult struct {
	Healthy     bool
	CurrentSlot uint64
	LastVote    uint64
	SlotsBehind int64
	Delinquent  bool
	Gossip      *GossipInfo
	Error       error
}

// Check performs a comprehensive health check (for vote account monitoring)
func (c *Checker) Check(votePubkey string) (*CheckResult, error) {
	result := &CheckResult{}

	// Get current slot with confirmed commitment
	currentSlot, err := c.client.GetSlot("confirmed")
	if err != nil {
		result.Error = fmt.Errorf("failed to get current slot: %w", err)
		return result, result.Error
	}
	result.CurrentSlot = currentSlot

	// Get vote accounts
	voteAccounts, err := c.client.GetVoteAccounts(votePubkey, "confirmed")
	if err != nil {
		result.Error = fmt.Errorf("failed to get vote accounts: %w", err)
		return result, result.Error
	}

	// Check if in delinquent list
	for _, va := range voteAccounts.Delinquent {
		if va.VotePubkey == votePubkey {
			result.Delinquent = true
			result.LastVote = va.LastVote
			result.SlotsBehind = int64(currentSlot) - int64(va.LastVote)
			result.Healthy = false

			// Check gossip status for delinquent node
			result.Gossip = c.checkGossipStatus(va.NodePubkey)
			return result, nil
		}
	}

	// Check if in current (healthy) list
	for _, va := range voteAccounts.Current {
		if va.VotePubkey == votePubkey {
			result.LastVote = va.LastVote
			result.SlotsBehind = int64(currentSlot) - int64(va.LastVote)
			result.Healthy = true
			result.Delinquent = false

			// Check gossip status for healthy node
			result.Gossip = c.checkGossipStatus(va.NodePubkey)
			return result, nil
		}
	}

	// Vote account not found in either list
	result.Error = fmt.Errorf("vote account %s not found", votePubkey)
	return result, result.Error
}

// checkGossipStatus checks if a node is visible in gossip and if its gossip port is reachable
func (c *Checker) checkGossipStatus(nodePubkey string) *GossipInfo {
	info := &GossipInfo{
		NodePubkey: nodePubkey,
	}

	// Get cluster nodes from gossip
	nodes, err := c.client.GetClusterNodes()
	if err != nil {
		log.Printf("Failed to get cluster nodes: %v", err)
		return info
	}

	// Find our node in the gossip list
	for _, node := range nodes {
		if node.Pubkey == nodePubkey {
			info.InGossip = true
			if node.Gossip != nil {
				info.GossipAddress = *node.Gossip
				// Probe the gossip port to check if node is actually alive
				info.TCPReachable = c.probeGossipPort(*node.Gossip)
			}
			return info
		}
	}

	// Node not found in gossip
	info.InGossip = false
	return info
}

// probeGossipPort attempts to connect to the gossip address to verify liveness
func (c *Checker) probeGossipPort(address string) bool {
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// LocalCheckResult contains the result of a local node health check
type LocalCheckResult struct {
	Identity string
	Healthy  bool
	Gossip   *GossipInfo
	Error    error
}

// CheckLocal performs a health check on the local node (hot spare)
// This checks: getHealth, getIdentity, and gossip status
func (c *Checker) CheckLocal() (*LocalCheckResult, error) {
	result := &LocalCheckResult{}

	// Check if node is healthy via getHealth (includes slot comparison internally)
	if err := c.client.GetHealth(); err != nil {
		result.Healthy = false
		result.Error = fmt.Errorf("node not healthy: %w", err)
		return result, result.Error
	}
	result.Healthy = true

	// Get node identity
	identity, err := c.client.GetIdentity()
	if err != nil {
		result.Error = fmt.Errorf("failed to get identity: %w", err)
		return result, result.Error
	}
	result.Identity = identity

	// Check gossip status for this node
	result.Gossip = c.checkGossipStatus(identity)

	return result, nil
}
