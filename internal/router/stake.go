package router

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
)

// TokenSupply returns current MOR total supply (wei) from the router.
func (c *Client) TokenSupply() (*big.Int, error) {
	var raw json.RawMessage
	if err := c.doJSON(http.MethodGet, "/blockchain/token/supply", nil, &raw); err != nil {
		return nil, err
	}
	return parseBigField(raw, "supply")
}

// TodaysBudget returns today's emissions budget (wei) from the router.
func (c *Client) TodaysBudget() (*big.Int, error) {
	var raw json.RawMessage
	if err := c.doJSON(http.MethodGet, "/blockchain/sessions/budget", nil, &raw); err != nil {
		return nil, err
	}
	return parseBigField(raw, "budget")
}

// SessionStakeWei is the on-chain MOR pull for a session:
//
//	stake ≈ (supply × pricePerSecond × duration) ÷ todaysBudget
//
// (same formula as proxy-router computeSessionTokenAmount / EstimateOpenSessionStake).
func SessionStakeWei(supply, budget, pricePerSecond *big.Int, durationSec int64) (*big.Int, error) {
	if supply == nil || budget == nil || pricePerSecond == nil {
		return nil, fmt.Errorf("missing supply, budget, or price")
	}
	if budget.Sign() == 0 {
		return nil, fmt.Errorf("zero emissions budget")
	}
	if durationSec <= 0 {
		return nil, fmt.Errorf("invalid duration")
	}
	cost := new(big.Int).Mul(pricePerSecond, big.NewInt(durationSec))
	num := new(big.Int).Mul(supply, cost)
	return new(big.Int).Div(num, budget), nil
}

func parseBigField(raw json.RawMessage, key string) (*big.Int, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	field, ok := obj[key]
	if !ok || len(field) == 0 {
		return nil, fmt.Errorf("missing %s in response", key)
	}
	// string "123" or number 123 or {"Int":"..."}-like — accept string/number.
	var asString string
	if err := json.Unmarshal(field, &asString); err == nil {
		n, ok := new(big.Int).SetString(asString, 10)
		if !ok {
			return nil, fmt.Errorf("bad %s %q", key, asString)
		}
		return n, nil
	}
	var asNum json.Number
	if err := json.Unmarshal(field, &asNum); err == nil {
		n, ok := new(big.Int).SetString(asNum.String(), 10)
		if !ok {
			return nil, fmt.Errorf("bad %s %s", key, asNum.String())
		}
		return n, nil
	}
	// lib.BigInt often marshals as a plain decimal string already handled;
	// some builds wrap { "int": "..." }.
	var wrap map[string]string
	if err := json.Unmarshal(field, &wrap); err == nil {
		for _, v := range wrap {
			if n, ok := new(big.Int).SetString(v, 10); ok {
				return n, nil
			}
		}
	}
	return nil, fmt.Errorf("unrecognized %s encoding: %s", key, string(field))
}

// StakesOnHold is claimable (available) and still-locked (hold) MOR wei for
// the router wallet via GET /blockchain/stakes/onhold.
type StakesOnHold struct {
	Claimable *big.Int
	Locked    *big.Int
}

// UserStakesOnHold calls the router (permission get_stakes_on_hold).
func (c *Client) UserStakesOnHold(iterations uint8) (StakesOnHold, error) {
	if iterations == 0 {
		iterations = 255
	}
	var res struct {
		Available json.RawMessage `json:"available"`
		Hold      json.RawMessage `json:"hold"`
	}
	path := fmt.Sprintf("/blockchain/stakes/onhold?iterations=%d", iterations)
	if err := c.doJSON(http.MethodGet, path, nil, &res); err != nil {
		return StakesOnHold{}, err
	}
	claimable, err := parseBigIntJSON(res.Available)
	if err != nil {
		return StakesOnHold{}, fmt.Errorf("available: %w", err)
	}
	locked, err := parseBigIntJSON(res.Hold)
	if err != nil {
		return StakesOnHold{}, fmt.Errorf("hold: %w", err)
	}
	return StakesOnHold{Claimable: claimable, Locked: locked}, nil
}

// WithdrawUserStakes reclaims claimable daylocked MOR via the router
// (permission withdraw_user_stakes). The router signs with its wallet.
func (c *Client) WithdrawUserStakes(iterations uint8) (string, error) {
	if iterations == 0 {
		iterations = 255
	}
	var res struct {
		Tx string `json:"tx"`
	}
	body := map[string]any{"iterations": iterations}
	if err := c.doJSON(http.MethodPost, "/blockchain/stakes/withdraw", body, &res); err != nil {
		return "", err
	}
	if res.Tx == "" {
		return "", fmt.Errorf("withdraw: empty tx hash")
	}
	return res.Tx, nil
}

// EthBalanceWei parses the ETH field from GET /blockchain/balance (wei string).
func (c *Client) EthBalanceWei() (*big.Int, error) {
	raw, err := c.Balance()
	if err != nil {
		return nil, err
	}
	var bal struct {
		ETH json.RawMessage `json:"eth"`
		Eth json.RawMessage `json:"ETH"`
	}
	if json.Unmarshal(raw, &bal) != nil {
		return nil, fmt.Errorf("balance: parse")
	}
	for _, field := range []json.RawMessage{bal.ETH, bal.Eth} {
		if len(field) == 0 {
			continue
		}
		n, err := parseBigIntJSON(field)
		if err != nil {
			return nil, err
		}
		return n, nil
	}
	return nil, fmt.Errorf("balance: missing eth")
}
