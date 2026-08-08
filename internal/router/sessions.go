package router

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Session is a proxy-router /blockchain/sessions/* row (capitalized JSON
// fields match the router's Go struct encoding).
type Session struct {
	ID           string
	ModelAgentID string
	Stake        *big.Int
	OpenedAt     int64
	EndsAt       int64
	ClosedAt     int64
}

type sessionDTO struct {
	Id           string          `json:"Id"`
	ModelAgentId string          `json:"ModelAgentId"`
	Stake        json.RawMessage `json:"Stake"`
	OpenedAt     json.RawMessage `json:"OpenedAt"`
	EndsAt       json.RawMessage `json:"EndsAt"`
	ClosedAt     json.RawMessage `json:"ClosedAt"`
}

func parseBigIntJSON(raw json.RawMessage) (*big.Int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return big.NewInt(0), nil
	}
	s := strings.TrimSpace(string(raw))
	s = strings.Trim(s, `"`)
	n := new(big.Int)
	if _, ok := n.SetString(s, 10); !ok {
		return nil, fmt.Errorf("invalid integer %q", s)
	}
	return n, nil
}

func (d sessionDTO) toSession() (Session, error) {
	stake, err := parseBigIntJSON(d.Stake)
	if err != nil {
		return Session{}, err
	}
	opened, err := parseBigIntJSON(d.OpenedAt)
	if err != nil {
		return Session{}, err
	}
	ends, err := parseBigIntJSON(d.EndsAt)
	if err != nil {
		return Session{}, err
	}
	closed, err := parseBigIntJSON(d.ClosedAt)
	if err != nil {
		return Session{}, err
	}
	return Session{
		ID:           d.Id,
		ModelAgentID: d.ModelAgentId,
		Stake:        stake,
		OpenedAt:     opened.Int64(),
		EndsAt:       ends.Int64(),
		ClosedAt:     closed.Int64(),
	}, nil
}

// GetSession fetches one session by id (open or closed) from the router.
func (c *Client) GetSession(sessionID string) (Session, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Session{}, fmt.Errorf("empty session id")
	}
	var res struct {
		Session sessionDTO `json:"session"`
	}
	path := "/blockchain/sessions/" + url.PathEscape(sessionID)
	if err := c.doJSON(http.MethodGet, path, nil, &res); err != nil {
		return Session{}, err
	}
	s, err := res.Session.toSession()
	if err != nil {
		return Session{}, err
	}
	if s.ID == "" {
		s.ID = sessionID
	}
	return s, nil
}

// ActualDurationSec is wall-clock seconds the session was open on-chain
// (ClosedAt − OpenedAt). Returns 0 when timestamps are missing or inconsistent.
func (s Session) ActualDurationSec() int64 {
	if s.OpenedAt <= 0 || s.ClosedAt <= 0 || s.ClosedAt < s.OpenedAt {
		return 0
	}
	return s.ClosedAt - s.OpenedAt
}

// WalletAddress returns the consumer wallet used by the proxy-router.
func (c *Client) WalletAddress() (string, error) {
	var res struct {
		Address string `json:"address"`
	}
	if err := c.doJSON(http.MethodGet, "/wallet", nil, &res); err != nil {
		return "", err
	}
	if res.Address == "" {
		return "", fmt.Errorf("wallet: empty address")
	}
	return res.Address, nil
}

func (c *Client) listSessionsPage(user string, offset, limit int, order string) ([]Session, error) {
	q := url.Values{}
	q.Set("user", user)
	q.Set("offset", strconv.Itoa(offset))
	q.Set("limit", strconv.Itoa(limit))
	q.Set("order", order)
	var res struct {
		Sessions []sessionDTO `json:"sessions"`
	}
	path := "/blockchain/sessions/user?" + q.Encode()
	if err := c.doJSON(http.MethodGet, path, nil, &res); err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(res.Sessions))
	for _, dto := range res.Sessions {
		s, err := dto.toSession()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// ListOpenSessions scans newest-first for sessions with ClosedAt == 0.
// Stops after several consecutive pages of only closed sessions (same idea
// as the proxy-router's rehydration early-exit).
func (c *Client) ListOpenSessions(user string) ([]Session, error) {
	const pageSize = 100
	const stopAfterClosedPages = 3

	var open []Session
	offset := 0
	closedPages := 0
	for {
		page, err := c.listSessionsPage(user, offset, pageSize, "desc")
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		closedInPage := 0
		for _, s := range page {
			if s.ClosedAt == 0 {
				open = append(open, s)
			} else {
				closedInPage++
			}
		}
		if closedInPage == len(page) {
			closedPages++
			if closedPages >= stopAfterClosedPages {
				break
			}
		} else {
			closedPages = 0
		}
		offset += len(page)
		if len(page) < pageSize {
			break
		}
	}
	return open, nil
}
