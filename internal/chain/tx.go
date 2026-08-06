package chain

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// withdrawUserStakes(address,uint8)
const withdrawSelector = "a98a7c6b"

// Signer sends Diamond txs from the consumer wallet (same role as the
// Lambda consumer_wallet_housekeeping withdraw step).
type Signer struct {
	RPC     string
	ChainID int64
	Key     *ecdsa.PrivateKey
	From    common.Address
	HTTP    *http.Client
}

func NewSigner(rpcURL, hexKey string, chainID int64) (*Signer, error) {
	hexKey = strings.TrimPrefix(strings.TrimSpace(hexKey), "0x")
	key, err := crypto.HexToECDSA(hexKey)
	if err != nil {
		return nil, fmt.Errorf("wallet key: %w", err)
	}
	return &Signer{
		RPC:     rpcURL,
		ChainID: chainID,
		Key:     key,
		From:    crypto.PubkeyToAddress(key.PublicKey),
		HTTP:    &http.Client{Timeout: 2 * time.Minute},
	}, nil
}

// EncodeWithdrawUserStakes builds calldata for withdrawUserStakes(user, iterations).
func EncodeWithdrawUserStakes(user string, iterations uint8) ([]byte, error) {
	addr, err := decodeAddress(user)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 4+32+32)
	sel, _ := hex.DecodeString(withdrawSelector)
	copy(out[0:4], sel)
	copy(out[4:36], addr)
	out[67] = iterations
	return out, nil
}

// WithdrawUserStakes sends one reclaim tx. Returns tx hash.
func (s *Signer) WithdrawUserStakes(ctx context.Context, diamond, user string, iterations uint8) (string, error) {
	data, err := EncodeWithdrawUserStakes(user, iterations)
	if err != nil {
		return "", err
	}
	return s.sendContractTx(ctx, diamond, data)
}

func (s *Signer) sendContractTx(ctx context.Context, to string, data []byte) (string, error) {
	toAddr := common.HexToAddress(to)
	nonce, err := s.ethUint(ctx, "eth_getTransactionCount", s.From.Hex(), "pending")
	if err != nil {
		return "", err
	}
	tip := big.NewInt(10_000_000) // 0.01 gwei — Base-appropriate (matches Lambda)
	base, err := s.ethUint(ctx, "eth_gasPrice")
	if err != nil {
		return "", err
	}
	// maxFee = 2*base + tip (cheap headroom without huge reservation)
	maxFee := new(big.Int).Mul(base, big.NewInt(2))
	maxFee.Add(maxFee, tip)

	gasLimit, err := s.estimateGas(ctx, toAddr, data)
	if err != nil {
		return "", err
	}
	// Headroom for deep on-hold scans.
	gasLimit = gasLimit + gasLimit/5 + 50_000

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   big.NewInt(s.ChainID),
		Nonce:     nonce.Uint64(),
		GasTipCap: tip,
		GasFeeCap: maxFee,
		Gas:       gasLimit,
		To:        &toAddr,
		Value:     big.NewInt(0),
		Data:      data,
	})
	signed, err := types.SignTx(tx, types.NewLondonSigner(big.NewInt(s.ChainID)), s.Key)
	if err != nil {
		return "", err
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return "", err
	}
	hash, err := s.ethCallString(ctx, "eth_sendRawTransaction", "0x"+hex.EncodeToString(raw))
	if err != nil {
		return "", err
	}
	if err := s.waitReceipt(ctx, hash, 90*time.Second); err != nil {
		return hash, err
	}
	return hash, nil
}

func (s *Signer) estimateGas(ctx context.Context, to common.Address, data []byte) (uint64, error) {
	n, err := s.ethUint(ctx, "eth_estimateGas", map[string]any{
		"from": s.From.Hex(),
		"to":   to.Hex(),
		"data": "0x" + hex.EncodeToString(data),
	})
	if err != nil {
		return 0, err
	}
	return n.Uint64(), nil
}

func (s *Signer) waitReceipt(ctx context.Context, hash string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		var rec *struct {
			Status string `json:"status"`
		}
		err := s.rpc(ctx, &rec, "eth_getTransactionReceipt", hash)
		if err != nil {
			return err
		}
		if rec != nil {
			if rec.Status == "0x1" || rec.Status == "0x01" {
				return nil
			}
			return fmt.Errorf("tx %s reverted", hash)
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("tx %s: receipt timeout", hash)
}

func (s *Signer) ethUint(ctx context.Context, method string, params ...any) (*big.Int, error) {
	var hexStr string
	if err := s.rpc(ctx, &hexStr, method, params...); err != nil {
		return nil, err
	}
	hexStr = strings.TrimPrefix(hexStr, "0x")
	if hexStr == "" {
		return big.NewInt(0), nil
	}
	n, ok := new(big.Int).SetString(hexStr, 16)
	if !ok {
		return nil, fmt.Errorf("%s: bad hex %q", method, hexStr)
	}
	return n, nil
}

func (s *Signer) ethCallString(ctx context.Context, method string, params ...any) (string, error) {
	var out string
	if err := s.rpc(ctx, &out, method, params...); err != nil {
		return "", err
	}
	return out, nil
}

func (s *Signer) rpc(ctx context.Context, result any, method string, params ...any) error {
	if params == nil {
		params = []any{}
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.RPC, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	if envelope.Error != nil {
		return fmt.Errorf("%s: %s", method, envelope.Error.Message)
	}
	if string(envelope.Result) == "null" {
		// typed nil for pointer results
		return json.Unmarshal([]byte("null"), result)
	}
	return json.Unmarshal(envelope.Result, result)
}

// IsNothingToWithdraw reports the Diamond "no releasable stake" revert.
func IsNothingToWithdraw(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "sessionuseramounttowithdrawiszero") ||
		strings.Contains(s, "amounttowithdrawiszero") ||
		// common estimateGas wording when selector isn't decoded
		(strings.Contains(s, "execution reverted") && strings.Contains(s, "a98a7c6b"))
}
