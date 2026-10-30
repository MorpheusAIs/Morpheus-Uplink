package api

import (
	"encoding/json"
	"regexp"
	"unicode/utf8"
)

// Router error text is relayed to clients only after scrubRouterText: the
// proxy-router's error strings can carry provider hosts and IPs, 0x
// addresses and ids, URLs and upstream stack text. The text goes to the
// ops log only, with URLs and 32-byte ids masked (relayRouterError).
var routerScrubRules = []struct {
	re   *regexp.Regexp
	with string
}{
	// Stack-like tails: everything from the first marker on.
	{regexp.MustCompile(`(?s)(goroutine \d+ \[|panic: |[\w./-]+\.go:\d+).*$`), "[trace]"},
	{regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.\-]*://[^\s"'<>]+`), "[url]"},
	{regexp.MustCompile(`(?i)\b0x[0-9a-f]{40,}\b`), "[id]"},
	// IP addresses, IPv6 first: bracketed (optional zone and port), then
	// IPv4-mapped, then bare IPv6 with four or more groups or a "::"
	// (optional zone; clock times such as 12:34:56 are left alone), then
	// IPv4 with an optional port.
	{regexp.MustCompile(`\[[0-9A-Fa-f:.]*:[0-9A-Fa-f:.]*(?:%[0-9A-Za-z_.\-]+)?\](?::\d{1,5})?`), "[ip]"},
	{regexp.MustCompile(`(?i)(?:\b[0-9a-f]{1,4}|)(?::[0-9a-f]{0,4}){1,6}:(?:\d{1,3}\.){3}\d{1,3}\b`), "[ip]"},
	{regexp.MustCompile(`(?i)(?:\b(?:[0-9a-f]{1,4}:){3,7}[0-9a-f]{1,4}\b|\b[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*::(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*\b)?|::[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*\b)(?:%[0-9a-z_.\-]+)?`), "[ip]"},
	{regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}(?::\d{1,5})?\b`), "[ip]"},
	// Host names (labels + an alphabetic TLD) or localhost, optional port.
	{regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}\b(?::\d{1,5})?|\blocalhost(?::\d{1,5})?\b`), "[host]"},
}

// scrubRouterText removes addresses, ids, URLs, hosts and trace text.
func scrubRouterText(s string) string {
	for _, r := range routerScrubRules {
		s = r.re.ReplaceAllString(s, r.with)
	}
	return s
}

// maxRelayBody caps a relayed router JSON body after scrubbing.
const maxRelayBody = 4 << 10

// scrubJSON scrubs every string (keys and leaves) of a decoded JSON value.
func scrubJSON(v any) any {
	switch t := v.(type) {
	case string:
		return scrubRouterText(t)
	case []any:
		for i := range t {
			t[i] = scrubJSON(t[i])
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[scrubRouterText(k)] = scrubJSON(x)
		}
		return out
	default:
		return v
	}
}

// scrubbedJSON decodes raw, scrubs it and re-encodes it; ok is false when
// raw is not JSON or the result is over maxRelayBody.
func scrubbedJSON(raw []byte) (json.RawMessage, bool) {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil, false
	}
	out, err := json.Marshal(scrubJSON(v))
	if err != nil || len(out) > maxRelayBody {
		return nil, false
	}
	return out, true
}

// truncate caps s at n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
