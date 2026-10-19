package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRouterWalletFileBesideAuth(t *testing.T) {
	dir := t.TempDir()
	auth := filepath.Join(dir, "router.auth")
	_ = os.WriteFile(auth, []byte("uplink:pw-0123456789abcdef\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "router.wallet.json"), []byte(`{"address":"0x4444444444444444444444444444444444444444"}`), 0o644)
	c := &Config{RouterAuthFile: auth}
	loadRouterWallet(c)
	if c.RouterWalletAddress != "0x4444444444444444444444444444444444444444" {
		t.Fatalf("got %q", c.RouterWalletAddress)
	}
	// Malformed address is ignored, never shown.
	_ = os.WriteFile(filepath.Join(dir, "router.wallet.json"), []byte(`{"address":"0x12"}`), 0o644)
	c = &Config{RouterAuthFile: auth}
	loadRouterWallet(c)
	if c.RouterWalletAddress != "" {
		t.Fatalf("malformed accepted: %q", c.RouterWalletAddress)
	}
}
