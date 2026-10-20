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
// routes that move funds, change the wallet, router users, permissions or
// config, or access router files are refused here (403 + audit) before
// anything is forwarded.
//
// Route table: proxy-router v7.15.0 (commit 0cb8eb83), registered flat on a
// gin.New() engine (internal/handlers/httphandlers/http.go:60-87; no groups
// or prefixes; controllers in internal/apibus/controller.go:19). gin v1.10
// defaults: case-sensitive match, RedirectTrailingSlash on,
// RedirectFixedPath off, no method-override middleware and no _method
// handling anywhere in the router source. The router registers no PUT or
// PATCH on any route.
//
// Blocked for every method (matched case-insensitively on segment boundaries):
//
//	/blockchain/send/*   internal/blockchainapi/controller.go:61 POST send/eth (send_eth)
//	                     internal/blockchainapi/controller.go:62 POST send/mor (send_mor)
//	/blockchain/approve  internal/blockchainapi/controller.go:60 POST (approve)
//	/ipfs, /ipfs/*       internal/proxyapi/controller_http.go:105-111 (ipfs_*); router
//	                     file-access routes are denied.
//	/v1/audio/transcriptions  internal/proxyapi/controller_http.go:100 (audio_transcription);
//	                     router file-access route, denied.
//	/proxy/provider/ping       internal/proxyapi/controller_http.go:88 (no auth) and
//	/proxy/sessions/initiate   :89 (initiate_session): the body carries a caller-chosen
//	                     providerUrl (requests.go:13-16, 24-30); the router dials it,
//	                     sends a signed message and returns the reply or error
//	                     (proxy_sender.go:139-146, 261-275, 580-633). Uplink does not
//	                     use either route; denied.
//	/docker/build, /docker/build/*  internal/proxyapi/controller_http.go:114-115 (docker_build);
//	                     router file-access route, denied.
//	/docker/container/start  internal/proxyapi/controller_http.go:116 (docker_manage);
//	                     host volume mounts, denied.
//	/blockchain/sessions             (exact) controller.go:96 POST open session
//	/blockchain/bids/{id}/session    (exact) controller.go:97 POST open session by bid
//	/blockchain/models/{id}/session  (exact) controller.go:98 POST open session by model
//	                     The body field directPayment selects paying the provider from
//	                     the stake (controller.go:700, 760-762, 888-890); Uplink opens
//	                     its own sessions with directPayment pinned false. The open
//	                     path also raises the MOR allowance (service.go:458).
//
// Blocked for every method except GET and HEAD (reads pass through):
//
//	/wallet, /wallet/*   walletapi/controller.go:30-32 POST privateKey, mnemonic,
//	                     derivationPath (set_wallet); :33 DELETE /wallet (remove_wallet).
//	                     Read: :29 GET /wallet returns address, kind and derivation
//	                     path only (controller.go:45-70, structs.go:17-26).
//	/auth, /auth/*       authapi/controller_http.go:47 POST /auth/users (add_user), :48
//	                     DELETE /auth/users (remove_user), :50 POST /auth/users/request,
//	                     :51 POST /auth/users/confirm, :54 POST /auth/allowance/requests,
//	                     :55 POST /auth/allowance/confirm, :57 POST /auth/allowance/revoke.
//	                     Reads: :52 GET /auth/users (username, perms, confirmed,
//	                     allowances; :168-186), :56 GET /auth/allowance/requests
//	                     (:292-309), :59 GET /auth/users/:username/txs (tx hashes;
//	                     :352-371), :60 GET /auth/cookie/path (a file path, not its
//	                     content; :381-410). None returns a password or changes state.
//	/config, /config/*   system/controller.go:81 POST /config/models/reload, :85 POST
//	                     /config/ethNode, :86 DELETE /config/ethNode (system_config).
//	                     Read: :82 GET /config (sanitized; :196-221).
//	/blockchain/providers  (exact) controller.go:68 POST create_provider:
//	                       service.go:581-593 raises the MOR allowance, then stakes
//	/blockchain/models     (exact) controller.go:76 POST create_model:
//	                       service.go:618-630 raises the MOR allowance, then stakes
//	/blockchain/bids       (exact) controller.go:80 POST create_bid:
//	                       service.go:726-743 raises the MOR allowance for the bid
//	                       fee, which is not returned (Marketplace.sol:86-87)
//
// Every caller of the approve / increase-allowance / transfer paths
// (service.go:458, 593, 630, 743, 1088, 1166, 1187) is behind a blocked
// route above. Allowed endpoints do not dispatch to a blocked action from
// the body or query: there is no generic tx/call or batch route. The only
// body-selected fund movement is directPayment on the open-session routes
// (blocked). GET /blockchain/models/{id}/session/estimate (controller.go:99)
// takes a directPayment query flag but only estimates. POST /v1/agents/tools
// (controller_http.go:95, 415-431) forwards a tool call to the provider of an
// existing session; it signs no transfer.
//
// Outbound addresses: the only request fields that name an address for the
// router to dial are providerUrl (ping, initiate), urls on POST /config/ethNode
// (system/structs.go:9), endpoint on POST /blockchain/providers
// (structs/req.go:117) and the docker build/start bodies; all are denied
// above. Chat, agent and health routes dial the provider recorded for the
// session or model, not an address from the request.
//
// Methods: the router registers only GET, POST and DELETE, so only GET,
// HEAD, POST and DELETE are forwarded; any other method is refused (403 +
// audit).
var nodeDenyRules = []struct {
	pattern []string // lowercase segments; "*" matches one segment
	exact   bool     // true: whole path; false: prefix on segment boundary
	readsOK bool     // true: GET and HEAD pass, every other method is blocked
}{
	{[]string{"blockchain", "send"}, false, false},
	{[]string{"blockchain", "approve"}, false, false},
	{[]string{"wallet"}, false, true},
	{[]string{"auth"}, false, true},
	{[]string{"config"}, false, true},
	{[]string{"ipfs"}, false, false},
	{[]string{"proxy", "provider", "ping"}, false, false},
	{[]string{"proxy", "sessions", "initiate"}, false, false},
	{[]string{"v1", "audio", "transcriptions"}, false, false},
	{[]string{"docker", "build"}, false, false},
	{[]string{"docker", "container", "start"}, false, false},
	{[]string{"blockchain", "sessions"}, true, false},
	{[]string{"blockchain", "bids", "*", "session"}, true, false},
	{[]string{"blockchain", "models", "*", "session"}, true, false},
	{[]string{"blockchain", "providers"}, true, true},
	{[]string{"blockchain", "models"}, true, true},
	{[]string{"blockchain", "bids"}, true, true},
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
func nodePathBlocked(p, method string) bool {
	segs := strings.Split(strings.ToLower(strings.TrimPrefix(p, "/")), "/")
	for _, rule := range nodeDenyRules {
		if rule.readsOK && (method == http.MethodGet || method == http.MethodHead) {
			continue
		}
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
		badMethod := !nodeMethodForwarded(r.Method)
		if err != nil || badMethod || nodePathBlocked(np, r.Method) {
			shown, reason := np, "blocked"
			if err != nil {
				shown, reason = auditSafePath(raw), "ambiguous"
			} else if badMethod {
				reason = "method"
			}
			m := r.Method
			if len(m) > 16 {
				m = m[:16]
			}
			s.auditEvent("node_passthrough_block", s.lockout.ClientIP(r), "denied", m+" /node"+shown+" ("+reason+")")
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "this router route is not available through Uplink"})
			return
		}
		proxy.ServeHTTP(w, r.WithContext(contextWithNodePath(r.Context(), np)))
	})
}

// nodeMethodForwarded reports whether a method may reach the router.
func nodeMethodForwarded(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodDelete:
		return true
	}
	return false
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
