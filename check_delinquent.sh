#!/bin/bash

# Vote pubkey to check
VOTE_PUBKEY="DvAmv1VbS2GNaZiSwQjyyjQqx1UUR283HMrgh3Txh1DA"

# Threshold: trigger failover if behind this many slots
SLOTS_BEHIND_THRESHOLD=50

# RPC endpoints
PRIMARY_RPC="http://localhost:58000"
#FALLBACK_RPC="https://api.mainnet-beta.solana.com"
FALLBACK_RPC="https://api.testnet.solana.com"
# JSON-RPC batch payload (getVoteAccounts + getSlot in one request)
# Using "confirmed" commitment for both calls to get near-realtime data
PAYLOAD='[
  {"jsonrpc": "2.0","id": 1,"method": "getVoteAccounts","params": [{"votePubkey": "'"$VOTE_PUBKEY"'", "commitment": "confirmed"}]},
  {"jsonrpc": "2.0","id": 2,"method": "getSlot","params": [{"commitment": "confirmed"}]}
]'

# Function to make the RPC call and check response
check_rpc() {
    local rpc_url="$1"
    local response
    local http_code

    # Make request, capture body and HTTP code separately
    response=$(curl -s -w "\n%{http_code}" "$rpc_url" -X POST -H 'Content-Type: application/json' -d "$PAYLOAD")
    http_code=$(echo "$response" | tail -n1)
    body=$(echo "$response" | sed '$d')

    if [[ "$http_code" == "200" ]]; then
        echo "$body"
        return 0
    else
        return 1
    fi
}

# Function to query RPC (tries primary, then fallback)
query_rpc() {
    local response
    response=$(check_rpc "$PRIMARY_RPC")
    if [[ $? -ne 0 ]]; then
        echo "INFO: Primary RPC ($PRIMARY_RPC) failed, trying fallback..." >&2
        response=$(check_rpc "$FALLBACK_RPC")
        if [[ $? -ne 0 ]]; then
            echo "ERROR: Cannot reach any RPC endpoint" >&2
            return 1
        fi
        echo "INFO: Successfully queried RPC: $FALLBACK_RPC" >&2
    else
        echo "INFO: Successfully queried RPC: $PRIMARY_RPC" >&2
    fi
    echo "$response"
    return 0
}

# Continuous monitoring loop
MAX_ATTEMPTS=3
echo "Starting continuous monitoring (checking every 3 seconds)..."

while true; do
    attempt=1
    
    while [[ $attempt -le $MAX_ATTEMPTS ]]; do
        echo "--- Attempt $attempt of $MAX_ATTEMPTS ---"
        
        response=$(query_rpc)
        if [[ $? -ne 0 ]]; then
            echo "RPC query failed, will retry in 3 seconds..."
            sleep 3
            continue
        fi

        # Parse batch response: [0] = getVoteAccounts, [1] = getSlot
        current_slot=$(echo "$response" | jq -r '.[1].result // empty' 2>/dev/null)
        
        # Check if delinquent array has any entries
        last_vote=$(echo "$response" | jq -r '.[0].result.delinquent[0].lastVote // empty' 2>/dev/null)

        if [[ -n "$last_vote" ]]; then
            slots_behind=$((current_slot - last_vote))
            echo "WARNING: Vote account $VOTE_PUBKEY is DELINQUENT (last vote: slot $last_vote, current: $current_slot, behind: $slots_behind slots)"
            
            if [[ $attempt -lt $MAX_ATTEMPTS ]]; then
                echo "Waiting 3 seconds before retry..."
                sleep 3
                ((attempt++))
            else
                echo "FINAL: Vote account remains DELINQUENT after $MAX_ATTEMPTS attempts"
                echo "Executing failover: switching to staked identity..."
                fdctl set-identity --config /home/sfadmin/firedancer-config.toml /home/sfadmin/sf-keys/staked-identity-keypair.json
                echo "Failover executed, exiting."
                exit 1
            fi
        else
            # Get lastVote from current (healthy) validators
            last_vote_current=$(echo "$response" | jq -r '.[0].result.current[0].lastVote // empty' 2>/dev/null)
            if [[ -n "$last_vote_current" && -n "$current_slot" ]]; then
                slots_behind=$((current_slot - last_vote_current))
                
                if [[ $slots_behind -ge $SLOTS_BEHIND_THRESHOLD ]]; then
                    echo "WARNING: Vote account $VOTE_PUBKEY is behind by $slots_behind slots (>= $SLOTS_BEHIND_THRESHOLD threshold)"
                    
                    if [[ $attempt -lt $MAX_ATTEMPTS ]]; then
                        echo "Waiting 3 seconds before retry..."
                        sleep 3
                        ((attempt++))
                        continue
                    else
                    echo "FINAL: Vote account remains behind after $MAX_ATTEMPTS attempts"
                    echo "Executing failover: switching to staked identity..."
                    fdctl set-identity --config /home/sfadmin/firedancer-config.toml /home/sfadmin/sf-keys/staked-identity-keypair.json
                    echo "Failover executed, exiting."
                    exit 1
                    fi
                else
                    echo "OK: Vote account $VOTE_PUBKEY is not delinquent (last vote: slot $last_vote_current, current: $current_slot, behind: $slots_behind slots)"
                    break
                fi
            else
                echo "OK: Vote account $VOTE_PUBKEY is not delinquent"
                break
            fi
        fi
    done
    
    sleep 3
done

