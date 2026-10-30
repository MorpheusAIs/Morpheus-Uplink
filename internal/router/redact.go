package router

import "regexp"

var (
	redactURL  = regexp.MustCompile(`(?:https?|wss?)://\S+`)
	redactID32 = regexp.MustCompile(`0x[0-9a-fA-F]{64}`)
)

// Redact prepares router error text for logs and status messages: every
// http, https, ws or wss URL becomes <url> (an RPC URL can carry a key in
// its path or query) and
// every 32-byte hex value (bid, session or transaction id) is cut to its
// first 10 characters, the short form the logs use. Classification never
// reads this text; it reads StatusError.Body, which stays raw.
func Redact(s string) string {
	s = redactURL.ReplaceAllString(s, "<url>")
	return redactID32.ReplaceAllStringFunc(s, func(m string) string { return m[:10] })
}

// SafeError is err's text passed through Redact ("" for nil).
func SafeError(err error) string {
	if err == nil {
		return ""
	}
	return Redact(err.Error())
}
