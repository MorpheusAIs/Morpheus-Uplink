package api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// /node/* is an admin-gated passthrough to the proxy-router API (Swagger,
// read-only inspection). Uplink calls the router as its admin user, so
// routes that move funds, change the wallet, or change router users or
// permissions are refused here (403 + audit) before anything is forwarded.
//
// Route table: proxy-router v7.15.0 (commit 0cb8eb83), registered flat on a
// gin.New() engine (internal/handlers/httphandlers/http.go:60-87; no groups
// or prefixes; controllers in internal/apibus/controller.go:19). gin v1.10
// defaults: case-sensitive match, RedirectTrailingSlash on,
// RedirectFixedPath off, no method-override middleware and no _method
// handling anywhere in the router source.
//
// Blocked (every method, matched case-insensitively on segment boundaries):
//
//	/blockchain/send/*   internal/blockchainapi/controller.go:61 POST send/eth (send_eth)
//	                     internal/blockchainapi/controller.go:62 POST send/mor (send_mor)
//	/blockchain/approve  internal/blockchainapi/controller.go:60 POST (approve)
//	/wallet, /wallet/*   internal/walletapi/controller.go:29 GET (get_wallet; same path as DELETE)
//	                     internal/walletapi/controller.go:30 POST /wallet/privateKey (set_wallet)
//	                     internal/walletapi/controller.go:31 POST /wallet/mnemonic (set_wallet)
//	                     internal/walletapi/controller.go:32 POST /wallet/derivationPath (set_wallet)
//	                     internal/walletapi/controller.go:33 DELETE /wallet (remove_wallet)
//	/auth, /auth/*       internal/authapi/controller_http.go:47 POST /auth/users (add_user)
//	                     internal/authapi/controller_http.go:48 DELETE /auth/users (remove_user)
//	                     internal/authapi/controller_http.go:50 POST /auth/users/request (no auth)
//	                     internal/authapi/controller_http.go:51 POST /auth/users/confirm (agent_requests)
//	                     internal/authapi/controller_http.go:52 GET /auth/users (agent_requests)
//	                     internal/authapi/controller_http.go:54 POST /auth/allowance/requests (request_allowance)
//	                     internal/authapi/controller_http.go:55 POST /auth/allowance/confirm (agent_requests)
//	                     internal/authapi/controller_http.go:56 GET /auth/allowance/requests (agent_requests)
//	                     internal/authapi/controller_http.go:57 POST /auth/allowance/revoke (agent_requests)
//	                     internal/authapi/controller_http.go:59 GET /auth/users/:username/txs (agent_requests)
//	                     internal/authapi/controller_http.go:60 GET /auth/cookie/path (no auth)
//	/ipfs, /ipfs/*       internal/proxyapi/controller_http.go:105-111 (ipfs_*); router
//	                     file-access routes are denied.
//	/v1/audio/transcriptions  internal/proxyapi/controller_http.go:100 (audio_transcription);
//	                     router file-access route, denied.
//	/docker/build, /docker/build/*  internal/proxyapi/controller_http.go:114-115 (docker_build);
//	                     router file-access route, denied.
//	/docker/container/start  internal/proxyapi/controller_http.go:116 (docker_manage);
//	                     host volume mounts, denied.
//	/blockchain/sessions             (exact) controller.go:96 POST open session
//	/blockchain/bids/{id}/session    (exact) controller.go:97 POST open session by bid
//	/blockchain/models/{id}/session  (exact) controller.go:98 POST open session by model
//	                     The body field directPayment selects paying the provider from
//	                     the stake (controller.go:700, 760-762, 888-890); Uplink opens
//	                     its own sessions with directPayment pinned false.
//
// Allowed endpoints do not dispatch to a blocked action from the body or
// query: there is no generic tx/call or batch route. The only body-selected
// fund movement is directPayment on the open-session routes above (blocked).
// GET /blockchain/models/{id}/session/estimate (controller.go:99) takes a
// directPayment query flag but only estimates. POST /v1/agents/tools
// (controller_http.go:95, 415-431) forwards a tool call to an agent, and
// /proxy/sessions/initiate (controller_http.go:89, 164-172) is the provider
// handshake; neither signs a transfer.
var nodeDenyRules = []struct {
	pattern []string // lowercase segments; "*" matches one segment
	exact   bool     // true: whole path; false: prefix on segment boundary
}{
	{[]string{"blockchain", "send"}, false},
	{[]string{"blockchain", "approve"}, false},
	{[]string{"wallet"}, false},
	{[]string{"auth"}, false},
	{[]string{"ipfs"}, false},
	{[]string{"v1", "audio", "transcriptions"}, false},
	{[]string{"docker", "build"}, false},
	{[]string{"docker", "container", "start"}, false},
	{[]string{"blockchain", "sessions"}, true},
	{[]string{"blockchain", "bids", "*", "session"}, true},
	{[]string{"blockchain", "models", "*", "session"}, true},
}

var errNodePathAmbiguous = errors.New("ambiguous path")

// methodOverrideHeaders are stripped before forwarding (the router honors
// none of them; stripped so no intermediary can either).
var methodOverrideHeaders = []string{
	"X-HTTP-Method-Override", "X-HTTP-Method", "X-Method-Override",
	"X-Original-Method", "X-Original-URL", "X-Rewrite-URL",
}

// rawRequestPath is the path exactly as the client sent it (before Go's
// own decoding), so the checks below see every escape.
func rawRequestPath(r *http.Request) string {
	if u := r.RequestURI; strings.HasPrefix(u, "/") {
		if i := strings.IndexByte(u, '?'); i >= 0 {
			u = u[:i]
		}
		return u
	}
	return r.URL.EscapedPath()
}

// normalizeNodePath turns the raw path after /node into the path that is
// matched and forwarded. %-escapes are decoded exactly once; an escape that
// decodes to '/', '\\', '.', '%' or a control byte is ambiguous (403), as
// are '\\', empty segments (//) and dot segments. One trailing slash is
// stripped. The result is clean (path.Clean is a no-op on it).
func normalizeNodePath(raw string) (string, error) {
	if !strings.HasPrefix(raw, "/") {
		return "", errNodePathAmbiguous
	}
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == '%' {
			if i+2 >= len(raw) {
				return "", errNodePathAmbiguous
			}
			v, err := strconv.ParseUint(raw[i+1:i+3], 16, 8)
			if err != nil {
				return "", errNodePathAmbiguous
			}
			c = byte(v)
			if c == '/' || c == '\\' || c == '.' || c == '%' || c < 0x20 || c >= 0x7f {
				return "", errNodePathAmbiguous
			}
			i += 2
		} else if c == '\\' || c < 0x20 || c >= 0x7f {
			return "", errNodePathAmbiguous
		}
		b.WriteByte(c)
	}
	p := b.String()
	if strings.Contains(p, "//") {
		return "", errNodePathAmbiguous
	}
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	if p == "/" {
		return p, nil
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", errNodePathAmbiguous
		}
	}
	if path.Clean(p) != p {
		return "", errNodePathAmbiguous
	}
	return p, nil
}

// nodePathBlocked reports whether the normalized path hits a deny rule
// (case-insensitive, segment boundaries).
func nodePathBlocked(p string) bool {
	segs := strings.Split(strings.ToLower(strings.TrimPrefix(p, "/")), "/")
	for _, rule := range nodeDenyRules {
		if len(segs) < len(rule.pattern) || (rule.exact && len(segs) != len(rule.pattern)) {
			continue
		}
		match := true
		for i, want := range rule.pattern {
			if want != "*" && segs[i] != want {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// nodeProxy serves /node/*: normalize, refuse blocked or ambiguous paths
// (403 + audit with method and path only), else forward exactly the
// normalized path with the router's credential. Redirects from the router
// are never followed or relayed (502).
func (s *Server) nodeProxy() http.Handler {
	target, _ := url.Parse(s.cfg.RouterURL)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			np, _ := pr.In.Context().Value(nodePathKey{}).(string)
			pr.Out.URL.Path = strings.TrimSuffix(target.Path, "/") + np
			pr.Out.URL.RawPath = ""
			q := pr.In.URL.Query()
			for k := range q {
				if strings.EqualFold(k, "_method") {
					q.Del(k)
				}
			}
			pr.Out.URL.RawQuery = q.Encode()
			for _, h := range methodOverrideHeaders {
				pr.Out.Header.Del(h)
			}
			// Server-to-server call: browser context headers are not
			// forwarded (the router's CORS check rejects a foreign Origin).
			for _, h := range []string{"Cookie", "Authorization", "Origin", "Referer",
				"Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest", "Sec-Fetch-User"} {
				pr.Out.Header.Del(h)
			}
			pr.Out.SetBasicAuth(s.cfg.RouterUser, s.cfg.RouterPass)
			pr.Out.Host = target.Host
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode >= 300 && resp.StatusCode < 400 {
				resp.Body.Close()
				return fmt.Errorf("router answered %d; redirects are not followed", resp.StatusCode)
			}
			// The router's swagger spec declares basePath "/", so swagger-ui's
			// "Try it out" would drop the /node prefix and miss the passthrough.
			if !strings.HasSuffix(resp.Request.URL.Path, "/swagger/doc.json") || resp.StatusCode != http.StatusOK {
				return nil
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return err
			}
			for _, old := range []string{`"basePath": "/"`, `"basePath":"/"`} {
				body = bytes.Replace(body, []byte(old), []byte(`"basePath": "/node"`), 1)
			}
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "router passthrough: " + err.Error()})
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimPrefix(rawRequestPath(r), "/node")
		np, err := normalizeNodePath(raw)
		if err != nil || nodePathBlocked(np) {
			shown, reason := np, "blocked"
			if err != nil {
				shown, reason = auditSafePath(raw), "ambiguous"
			}
			s.auditEvent("node_passthrough_block", s.lockout.ClientIP(r), "denied", r.Method+" /node"+shown+" ("+reason+")")
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "this router route is not available through Uplink"})
			return
		}
		proxy.ServeHTTP(w, r.WithContext(contextWithNodePath(r.Context(), np)))
	})
}

// auditSafePath bounds a refused raw path for the audit line (printable
// ASCII only, at most 200 bytes). Paths never carry secrets; bodies and
// query strings are never logged.
func auditSafePath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p) && b.Len() < 200; i++ {
		if c := p[i]; c >= 0x21 && c < 0x7f {
			b.WriteByte(c)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}
