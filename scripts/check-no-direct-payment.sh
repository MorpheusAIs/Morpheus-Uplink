#!/usr/bin/env bash
# Design acceptance test 10 (static half): Uplink never asks the router for a
# direct-payment session. The runtime half is the request-body guard in
# internal/router (unit-tested) plus the e2e mock counter.
#  - no non-test Go, shell or template sets directPayment / DirectPayment true
#  - the open-session request type has no settable DirectPayment field
#  - SESSION_DIRECT_PAYMENT is not offered as an operator knob in templates
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
bad=0
hits=$(grep -rniE '(direct_?payment|isDirectPaymentFromUser)"?[[:space:]]*[:=][[:space:]]*"?(true|1)\b' \
  --include='*.go' --include='*.sh' --include='*.yml' --include='*.html' --include='*.js' \
  cmd internal deploy scripts 2>/dev/null | grep -v '_test\.go:' | grep -v '^scripts/check-no-direct-payment\.sh:' | grep -v '^scripts/testdata/' || true)
if [ -n "$hits" ]; then echo "BAD: direct payment set true:"; echo "$hits"; bad=1; fi
if awk '/^type OpenSessionRequest struct/,/^}/' internal/router/client.go | grep -q 'DirectPayment'; then
  echo "BAD: OpenSessionRequest must not have a DirectPayment field"; bad=1
fi
grep -q '"directPayment"' internal/router/client.go || { echo "BAD: open-session body must pin directPayment"; bad=1; }
grep -q 'requestsDirectPayment(payload)' internal/router/client.go || { echo "BAD: router request-body guard missing"; bad=1; }
if grep -rn 'SESSION_DIRECT_PAYMENT=' deploy docker-compose.local.yml 2>/dev/null; then
  echo "BAD: templates must not set SESSION_DIRECT_PAYMENT"; bad=1
fi
# Self-test: the pattern must catch a planted assignment.
printf 'x := router.OpenSessionRequest{DirectPayment: true}\n' | grep -qiE '(direct_?payment|isDirectPaymentFromUser)"?[[:space:]]*[:=][[:space:]]*"?(true|1)\b' \
  || { echo "BAD: self-test pattern"; bad=1; }
[ "$bad" = 0 ] && echo "OK: no direct-payment path (static check)"
exit "$bad"
