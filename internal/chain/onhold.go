// Package chain holds optional Base JSON-RPC helpers that the proxy-router
// HTTP API does not expose (notably daylocked / on-hold MOR).
package chain

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// Base mainnet Morpheus Diamond (Inference Contract).
const DefaultDiamond = "0x6aBE1d282f72B474E54527D93b979A4f64d3030a"

// getUserStakesOnHold(address,uint8) selector.
const stakesOnHoldSelector = "967885df"

type OnHold struct {
	Claimable *big.Int // past releaseAt — withdrawUserStakes can take these
	Locked    *big.Int // still daylocked until next UTC day(s)
}

// NextUTCMidnight is the next 00:00 UTC at or after now (daylock boundary).
func NextUTCMidnight(now time.Time) time.Time {
	u := now.UTC()
	next := time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
	return next
}

// UserStakesOnHold calls Diamond.getUserStakesOnHold(user, iterations).
func UserStakesOnHold(rpcURL, diamond, user string, iterations uint8) (OnHold, error) {
	if rpcURL == "" {
		return OnHold{}, fmt.Errorf("rpc URL empty")
	}
	if diamond == "" {
		diamond = DefaultDiamond
	}
	data, err := encodeStakesOnHold(user, iterations)
	if err != nil {
		return OnHold{}, err
	}
	ret, err := ethCall(rpcURL, diamond, data)
	if err != nil {
		return OnHold{}, err
	}
	if len(ret) < 64 {
		return OnHold{}, fmt.Errorf("getUserStakesOnHold: short returndata (%d)", len(ret))
	}
	claimable := new(big.Int).SetBytes(ret[:32])
	locked := new(big.Int).SetBytes(ret[32:64])
	return OnHold{Claimable: claimable, Locked: locked}, nil
}

func encodeStakesOnHold(user string, iterations uint8) (string, error) {
	addr, err := decodeAddress(user)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	buf.WriteString("0x")
	buf.WriteString(stakesOnHoldSelector)
	buf.WriteString(hex.EncodeToString(addr))
	// uint8 left-padded to 32 bytes
	pad := make([]byte, 32)
	pad[31] = iterations
	buf.WriteString(hex.EncodeToString(pad))
	return buf.String(), nil
}

func decodeAddress(user string) ([]byte, error) {
	s := strings.TrimPrefix(strings.ToLower(user), "0x")
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 20 {
		return nil, fmt.Errorf("invalid address %q", user)
	}
	out := make([]byte, 32)
	copy(out[12:], b)
	return out, nil
}

func ethCall(rpcURL, to, data string) ([]byte, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "eth_call",
		"params": []any{
			map[string]string{"to": to, "data": data},
			"latest",
		},
	})
	resp, err := http.Post(rpcURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var out struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, fmt.Errorf("eth_call: %s", out.Error.Message)
	}
	hexStr := strings.TrimPrefix(out.Result, "0x")
	if hexStr == "" {
		return nil, fmt.Errorf("eth_call: empty result")
	}
	return hex.DecodeString(hexStr)
}
