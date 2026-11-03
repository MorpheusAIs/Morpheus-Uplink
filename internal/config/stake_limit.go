package config

import (
	"errors"
	"math/big"
	"regexp"
	"strings"
)

// MaxStMorPerHourInvalidNote is the Settings and Status note for an invalid
// saved maxStMorPerHour value.
const MaxStMorPerHourInvalidNote = "Max stMOR/hr is invalid: fix the value and restart"

// maxMorLimit is the highest allowed limit, 100000 MOR (in wei).
var maxMorLimit = new(big.Int).Mul(big.NewInt(100000), big.NewInt(1e18))

var reMorLimit = regexp.MustCompile(`^[0-9]+(\.[0-9]{1,6})?$`)

var errMorLimit = errors.New("must be empty (no limit) or a number above 0, at most 100000, with at most 6 decimals")

// ParseMorLimit parses a Max stMOR/hr value into wei per hour. "" (after
// trimming) is off: nil and no error. Otherwise the value is a plain
// decimal above 0, with at most 6 decimals, at most 100000.
func ParseMorLimit(s string) (*big.Int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if !reMorLimit.MatchString(s) {
		return nil, errMorLimit
	}
	whole, frac, _ := strings.Cut(s, ".")
	frac += strings.Repeat("0", 18-len(frac))
	wei, ok := new(big.Int).SetString(whole+frac, 10)
	if !ok || wei.Sign() <= 0 || wei.Cmp(maxMorLimit) > 0 {
		return nil, errMorLimit
	}
	return wei, nil
}

func checkMorLimit(s string) error {
	_, err := ParseMorLimit(s)
	return err
}

// setMaxStMorPerHour stores the raw value and its parse: an invalid value
// leaves the limit marked invalid (every open is refused).
func setMaxStMorPerHour(c *Config, v string) {
	c.MaxStMorPerHour = v
	wei, err := ParseMorLimit(v)
	c.StakeLimitWei, c.StakeLimitInvalid = wei, err != nil
}
