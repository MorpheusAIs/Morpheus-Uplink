package config

import (
	"errors"
	"math/big"
	"regexp"
	"strings"
)

// Max stMOR per session: the most MOR a new session may lock (stake),
// whatever its length. An open compares the stake for the session length it
// asks for with this cap.

// MaxStMorPerSessionInvalidNote is the Settings and Status note for an
// invalid saved maxStMorPerSession value.
const MaxStMorPerSessionInvalidNote = "Max stMOR per session is invalid: fix the value and restart"

// LegacyMaxStMorPerHour is the former hourly cap's settings key. A saved
// value is converted once at boot (ResolveStakeCap).
const LegacyMaxStMorPerHour = "maxStMorPerHour"

// DefaultMaxStMorPerHour is the hourly rate the default cap is built from:
// the default cap is this many stMOR per hour of the default session.
const DefaultMaxStMorPerHour = 150

// maxMorLimit is the highest allowed limit, 100000 MOR (in wei).
var maxMorLimit = new(big.Int).Mul(big.NewInt(100000), big.NewInt(1e18))

// microMor is the setting's precision (6 decimals), in wei.
var microMor = big.NewInt(1e12)

var reMorLimit = regexp.MustCompile(`^[0-9]+(\.[0-9]{1,6})?$`)

var errMorLimit = errors.New("must be empty (no limit) or a number above 0, at most 100000, with at most 6 decimals")

// ParseMorLimit parses a Max stMOR per session value into wei. "" (after
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

// FormatMorLimit is the setting text for wei (a whole number of micro-MOR,
// above 0): a plain decimal without trailing zeros.
func FormatMorLimit(wei *big.Int) string {
	q, r := new(big.Int).QuoRem(wei, big.NewInt(1e18), new(big.Int))
	frac := strings.TrimRight(leftPad(new(big.Int).Quo(r, microMor).String(), 6), "0")
	if frac == "" {
		return q.String()
	}
	return q.String() + "." + frac
}

func leftPad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return strings.Repeat("0", n-len(s)) + s
}

// PerSessionFromHourly converts an hourly cap (wei per hour) to a
// per-session cap for a session of sec seconds: hourly × sec ÷ 3600,
// rounded up to 6 decimals and kept within 100000 MOR.
func PerSessionFromHourly(hourly *big.Int, sec int) *big.Int {
	w := new(big.Int).Mul(hourly, big.NewInt(int64(sec)))
	w.Add(w, big.NewInt(3600-1)).Quo(w, big.NewInt(3600))
	w.Add(w, new(big.Int).Sub(microMor, big.NewInt(1))).Quo(w, microMor).Mul(w, microMor)
	if w.Sign() <= 0 {
		w.Set(microMor)
	}
	if w.Cmp(maxMorLimit) > 0 {
		w.Set(maxMorLimit)
	}
	return w
}

// DefaultMaxStMorPerSession is the default cap for a default session of
// sec seconds: 150 stMOR per hour of it.
func DefaultMaxStMorPerSession(sec int) string {
	hourly := new(big.Int).Mul(big.NewInt(DefaultMaxStMorPerHour), big.NewInt(1e18))
	return FormatMorLimit(PerSessionFromHourly(hourly, sec))
}

func checkMorLimit(s string) error {
	_, err := ParseMorLimit(s)
	return err
}

// setMaxStMorPerSession stores the raw value and its parse: an invalid
// value leaves the limit marked invalid (every open is refused).
func setMaxStMorPerSession(c *Config, v string) {
	c.MaxStMorPerSession = v
	wei, err := ParseMorLimit(v)
	c.StakeLimitWei, c.StakeLimitInvalid = wei, err != nil
}

// ResolveStakeCap settles the cap at boot, after ApplyPersisted, from the
// saved values and the boot default session length (seconds):
//   - a saved maxStMorPerSession is kept as applied;
//   - else a saved hourly maxStMorPerHour is converted once (hourly ×
//     default hours, rounded up to 6 decimals; "" stays off; an invalid
//     value is kept and marked invalid), and save is the value to persist
//     under maxStMorPerSession (the caller then drops the hourly key);
//   - else the default is 150 × default hours, not saved, so it follows
//     the default session length until the operator saves a value.
func ResolveStakeCap(c *Config, vals map[string]string, defaultSec int) (save *string) {
	if _, ok := vals["maxStMorPerSession"]; ok {
		return nil
	}
	raw, ok := vals[LegacyMaxStMorPerHour]
	if !ok {
		setMaxStMorPerSession(c, DefaultMaxStMorPerSession(defaultSec))
		return nil
	}
	raw = strings.TrimSpace(raw)
	v := raw
	if hourly, err := ParseMorLimit(raw); err == nil && hourly != nil {
		v = FormatMorLimit(PerSessionFromHourly(hourly, defaultSec))
	}
	setMaxStMorPerSession(c, v)
	if c.StakeLimitInvalid {
		c.SettingNotes = append(c.SettingNotes, MaxStMorPerSessionInvalidNote)
	}
	return &v
}
