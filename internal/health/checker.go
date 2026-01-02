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
	NodePubkey  string
	Gossip      *GossipInfo
	Error       error
}

// Check performs a comprehensive health check (for vote account monitoring)
// Uses batch RPC call to get vote accounts and current slot atomically
func (c *Checker) Check(votePubkey string) (*CheckResult, error) {
	result := &CheckResult{}

	// Get vote account info and current slot in a single batch request
	// This ensures accurate slots-behind calculation
	voteInfo, err := c.client.GetVoteAccountWithSlot(votePubkey)
	if err != nil {
		result.Error = fmt.Errorf("failed to get vote account: %w", err)
		return result, result.Error
	}

	if !voteInfo.Found {
		result.Error = fmt.Errorf("vote account %s not found", votePubkey)
		return result, result.Error
	}

	result.CurrentSlot = voteInfo.CurrentSlot
	result.LastVote = voteInfo.LastVote
	result.SlotsBehind = voteInfo.SlotsBehind
	result.Delinquent = voteInfo.Delinquent
	result.NodePubkey = voteInfo.NodePubkey
	result.Healthy = !voteInfo.Delinquent

	// Check gossip status
	result.Gossip = c.checkGossipStatus(voteInfo.NodePubkey)

	return result, nil
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
	Identity   string
	ClientType string
	Version    string
	Healthy    bool
	Gossip     *GossipInfo
	Error      error
}

// CheckLocal performs a health check on the local node (hot spare)
// Uses batch RPC to get identity, version, and cluster nodes in one call
func (c *Checker) CheckLocal() (*LocalCheckResult, error) {
	result := &LocalCheckResult{}

	// Check if node is healthy via getHealth (includes slot comparison internally)
	if err := c.client.GetHealth(); err != nil {
		result.Healthy = false
		result.Error = fmt.Errorf("node not healthy: %w", err)
		return result, result.Error
	}
	result.Healthy = true

	// Get node info in a single batch request (identity, version, cluster nodes)
	nodeInfo, err := c.client.GetNodeInfo()
	if err != nil {
		result.Error = fmt.Errorf("failed to get node info: %w", err)
		return result, result.Error
	}

	result.Identity = nodeInfo.Identity
	result.ClientType = nodeInfo.ClientType
	result.Version = nodeInfo.Version

	// Check gossip status using cluster nodes from batch response
	result.Gossip = c.checkGossipStatusFromNodes(nodeInfo.Identity, nodeInfo.ClusterNodes)

	return result, nil
}

// checkGossipStatusFromNodes checks gossip status using pre-fetched cluster nodes
func (c *Checker) checkGossipStatusFromNodes(nodePubkey string, nodes []rpc.ClusterNode) *GossipInfo {
	info := &GossipInfo{
		NodePubkey: nodePubkey,
	}

	if nodes == nil {
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
