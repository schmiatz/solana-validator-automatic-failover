package rpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is a JSON-RPC client for Solana nodes
type Client struct {
	endpoint   string
	httpClient *http.Client
}

// NewClient creates a new RPC client
func NewClient(endpoint string) *Client {
	return &Client{
		endpoint: endpoint,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Request represents a JSON-RPC request
type Request struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// Response represents a JSON-RPC response
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError represents a JSON-RPC error
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("RPC error %d: %s", e.Code, e.Message)
}

// Call makes a JSON-RPC call
func (c *Client) Call(method string, params interface{}) (json.RawMessage, error) {
	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  method,
		Params:  params,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var rpcResp Response
	if err := json.Unmarshal(respBody, &rpcResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if rpcResp.Error != nil {
		return nil, rpcResp.Error
	}

	return rpcResp.Result, nil
}

// BatchCall makes multiple JSON-RPC calls in a single HTTP request
func (c *Client) BatchCall(requests []Request) ([]Response, error) {
	body, err := json.Marshal(requests)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal batch request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var batchResp []Response
	if err := json.Unmarshal(respBody, &batchResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal batch response: %w", err)
	}

	return batchResp, nil
}

// VoteAccountWithSlot contains vote account info and current slot from a batch call
type VoteAccountWithSlot struct {
	CurrentSlot  uint64
	LastVote     uint64
	SlotsBehind  int64
	Delinquent   bool
	NodePubkey   string
	VotePubkey   string
	Found        bool
}

// GetVoteAccountWithSlot fetches vote account info and current slot in a single batch request
// This ensures accurate slots-behind calculation by getting both values atomically
func (c *Client) GetVoteAccountWithSlot(votePubkey string) (*VoteAccountWithSlot, error) {
	requests := []Request{
		{
			JSONRPC: "2.0",
			ID:      1,
			Method:  "getVoteAccounts",
			Params:  []interface{}{map[string]interface{}{"votePubkey": votePubkey, "commitment": "confirmed"}},
		},
		{
			JSONRPC: "2.0",
			ID:      2,
			Method:  "getSlot",
			Params:  []interface{}{map[string]interface{}{"commitment": "confirmed"}},
		},
	}

	responses, err := c.BatchCall(requests)
	if err != nil {
		return nil, err
	}

	if len(responses) != 2 {
		return nil, fmt.Errorf("expected 2 responses, got %d", len(responses))
	}

	result := &VoteAccountWithSlot{VotePubkey: votePubkey}

	// Parse getVoteAccounts response (ID 1)
	var voteAccountsResp *VoteAccountsResult
	for _, resp := range responses {
		if resp.ID == 1 {
			if resp.Error != nil {
				return nil, fmt.Errorf("getVoteAccounts error: %s", resp.Error.Message)
			}
			if err := json.Unmarshal(resp.Result, &voteAccountsResp); err != nil {
				return nil, fmt.Errorf("failed to parse vote accounts: %w", err)
			}
		} else if resp.ID == 2 {
			if resp.Error != nil {
				return nil, fmt.Errorf("getSlot error: %s", resp.Error.Message)
			}
			if err := json.Unmarshal(resp.Result, &result.CurrentSlot); err != nil {
				return nil, fmt.Errorf("failed to parse slot: %w", err)
			}
		}
	}

	if voteAccountsResp == nil {
		return nil, fmt.Errorf("missing vote accounts response")
	}

	// Check delinquent list first
	for _, va := range voteAccountsResp.Delinquent {
		if va.VotePubkey == votePubkey {
			result.Found = true
			result.Delinquent = true
			result.LastVote = va.LastVote
			result.NodePubkey = va.NodePubkey
			result.SlotsBehind = int64(result.CurrentSlot) - int64(va.LastVote)
			return result, nil
		}
	}

	// Check current (healthy) list
	for _, va := range voteAccountsResp.Current {
		if va.VotePubkey == votePubkey {
			result.Found = true
			result.Delinquent = false
			result.LastVote = va.LastVote
			result.NodePubkey = va.NodePubkey
			result.SlotsBehind = int64(result.CurrentSlot) - int64(va.LastVote)
			return result, nil
		}
	}

	// Not found
	result.Found = false
	return result, nil
}

// NodeInfo contains basic node information from batch call
type NodeInfo struct {
	Identity      string
	ClientType    string
	Version       string
	ClusterNodes  []ClusterNode
}

// GetNodeInfo fetches identity, version, and cluster nodes in a single batch request
func (c *Client) GetNodeInfo() (*NodeInfo, error) {
	requests := []Request{
		{JSONRPC: "2.0", ID: 1, Method: "getIdentity", Params: nil},
		{JSONRPC: "2.0", ID: 2, Method: "getVersion", Params: nil},
		{JSONRPC: "2.0", ID: 3, Method: "getClusterNodes", Params: nil},
	}

	responses, err := c.BatchCall(requests)
	if err != nil {
		return nil, err
	}

	info := &NodeInfo{}

	for _, resp := range responses {
		if resp.Error != nil {
			continue // Skip errors, we'll handle missing data
		}

		switch resp.ID {
		case 1: // getIdentity
			var identity IdentityResult
			if err := json.Unmarshal(resp.Result, &identity); err == nil {
				info.Identity = identity.Identity
			}
		case 2: // getVersion
			var version VersionResult
			if err := json.Unmarshal(resp.Result, &version); err == nil {
				if version.SolanaCore != "" {
					info.ClientType = "Agave"
					info.Version = version.SolanaCore
				} else {
					info.ClientType = "Unknown"
				}
			}
		case 3: // getClusterNodes
			var nodes []ClusterNode
			if err := json.Unmarshal(resp.Result, &nodes); err == nil {
				info.ClusterNodes = nodes
			}
		}
	}

	return info, nil
}

// GetHealth checks if the node is healthy
func (c *Client) GetHealth() error {
	_, err := c.Call("getHealth", nil)
	return err
}

// GetSlot returns the current slot
func (c *Client) GetSlot(commitment string) (uint64, error) {
	params := []map[string]string{}
	if commitment != "" {
		params = append(params, map[string]string{"commitment": commitment})
	}

	result, err := c.Call("getSlot", params)
	if err != nil {
		return 0, err
	}

	var slot uint64
	if err := json.Unmarshal(result, &slot); err != nil {
		return 0, fmt.Errorf("failed to parse slot: %w", err)
	}

	return slot, nil
}

// VoteAccount represents a vote account from getVoteAccounts
type VoteAccount struct {
	VotePubkey       string          `json:"votePubkey"`
	NodePubkey       string          `json:"nodePubkey"`
	ActivatedStake   uint64          `json:"activatedStake"`
	Commission       uint8           `json:"commission"`
	EpochVoteAccount bool            `json:"epochVoteAccount"`
	LastVote         uint64          `json:"lastVote"`
	RootSlot         uint64          `json:"rootSlot"`
	EpochCredits     [][]interface{} `json:"epochCredits"`
}

// VoteAccountsResult represents the result of getVoteAccounts
type VoteAccountsResult struct {
	Current    []VoteAccount `json:"current"`
	Delinquent []VoteAccount `json:"delinquent"`
}

// GetVoteAccounts returns vote accounts, optionally filtered by votePubkey
func (c *Client) GetVoteAccounts(votePubkey string, commitment string) (*VoteAccountsResult, error) {
	params := make(map[string]string)
	if votePubkey != "" {
		params["votePubkey"] = votePubkey
	}
	if commitment != "" {
		params["commitment"] = commitment
	}

	var callParams []interface{}
	if len(params) > 0 {
		callParams = append(callParams, params)
	}

	result, err := c.Call("getVoteAccounts", callParams)
	if err != nil {
		return nil, err
	}

	var voteAccounts VoteAccountsResult
	if err := json.Unmarshal(result, &voteAccounts); err != nil {
		return nil, fmt.Errorf("failed to parse vote accounts: %w", err)
	}

	return &voteAccounts, nil
}

// ClusterNode represents a node from getClusterNodes
type ClusterNode struct {
	Pubkey       string  `json:"pubkey"`
	Gossip       *string `json:"gossip"`
	TPU          *string `json:"tpu"`
	RPC          *string `json:"rpc"`
	Version      *string `json:"version"`
	FeatureSet   *uint32 `json:"featureSet"`
	ShredVersion *uint16 `json:"shredVersion"`
}

// GetClusterNodes returns all nodes in the cluster gossip
func (c *Client) GetClusterNodes() ([]ClusterNode, error) {
	result, err := c.Call("getClusterNodes", nil)
	if err != nil {
		return nil, err
	}

	var nodes []ClusterNode
	if err := json.Unmarshal(result, &nodes); err != nil {
		return nil, fmt.Errorf("failed to parse cluster nodes: %w", err)
	}

	return nodes, nil
}

// IdentityResult represents the result of getIdentity
type IdentityResult struct {
	Identity string `json:"identity"`
}

// GetIdentity returns the node's identity pubkey
func (c *Client) GetIdentity() (string, error) {
	result, err := c.Call("getIdentity", nil)
	if err != nil {
		return "", err
	}

	var identity IdentityResult
	if err := json.Unmarshal(result, &identity); err != nil {
		return "", fmt.Errorf("failed to parse identity: %w", err)
	}

	return identity.Identity, nil
}

// Endpoint returns the RPC endpoint URL
func (c *Client) Endpoint() string {
	return c.endpoint
}

// VersionResult represents the result of getVersion
type VersionResult struct {
	SolanaCore string `json:"solana-core,omitempty"`
	FeatureSet uint32 `json:"feature-set,omitempty"`
}

// GetVersion returns the node's version info
func (c *Client) GetVersion() (*VersionResult, error) {
	result, err := c.Call("getVersion", nil)
	if err != nil {
		return nil, err
	}

	var version VersionResult
	if err := json.Unmarshal(result, &version); err != nil {
		return nil, fmt.Errorf("failed to parse version: %w", err)
	}

	return &version, nil
}

// DetectNodeType returns the node type based on version info
func (c *Client) DetectNodeType() (clientType string, version string, err error) {
	versionInfo, err := c.GetVersion()
	if err != nil {
		return "", "", err
	}

	// Check for solana-core key (Agave)
	if versionInfo.SolanaCore != "" {
		return "Agave", versionInfo.SolanaCore, nil
	}

	// If no solana-core, might be Firedancer (needs real node to verify)
	return "Unknown", "", nil
}
