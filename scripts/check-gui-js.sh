#!/usr/bin/env bash
# Extract inline GUI script, syntax-check, and assert no 2FA refresh loop.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
HTML="$ROOT/internal/gui/static/index.html"
OUT=$(mktemp --suffix=.js)
trap 'rm -f "$OUT"' EXIT
python3 - "$HTML" "$OUT" <<'PY'
import re, sys
html = open(sys.argv[1]).read()
scripts = re.findall(r"<script(?:\s[^>]*)?>(.*?)</script>", html, re.S | re.I)
body = "\n".join(s for s in scripts if s.strip())
open(sys.argv[2], "w").write(body)
PY
node --check "$OUT"
echo "OK: GUI script parses"

# Focused harness: extract showTab + refreshSecurity only (avoid full GUI side effects).
node - "$OUT" <<'NODE'
const fs = require('fs');
const src = fs.readFileSync(process.argv[2], 'utf8');

function extractFn(name, text) {
  const re = new RegExp(
    '(?:async\\s+)?function\\s+' + name + '\\s*\\([^)]*\\)\\s*\\{',
  );
  const m = re.exec(text);
  if (!m) throw new Error('missing function ' + name);
  let i = m.index + m[0].length;
  let depth = 1;
  while (i < text.length && depth > 0) {
    const c = text[i++];
    if (c === '{') depth++;
    else if (c === '}') depth--;
  }
  return text.slice(m.index, i);
}

const showTabSrc = extractFn('showTab', src);
const refreshSrc = extractFn('refreshSecurity', src);

let activeTab = 'status';
let fetches = 0;
let sawPoll = false;

global.escapeHtml = (s) => String(s);
global.document = {
  querySelectorAll: () => [],
  getElementById: (id) => {
    if (id === 'securityAlerts') return { textContent: '', innerHTML: '' };
    if (id === 'twoFactorStatus') return { textContent: '' };
    if (id === 'twoFactorEnroll') return { style: { display: 'none' } };
    if (id === 'allowanceWarning') return { style: { display: 'none' }, textContent: '' };
    if (id === 'app') return { classList: { contains: () => true } };
    return { classList: { toggle() {}, contains: () => false }, style: {}, textContent: '' };
  },
};
global.fetch = async (url, opts = {}) => {
  fetches++;
  const headers = opts.headers || {};
  if (String(url).includes('/admin/2fa/status') && headers['X-Uplink-Poll'] === '1') {
    sawPoll = true;
  }
  if (String(url).includes('/admin/2fa/status')) {
    return {
      status: 200, ok: true,
      json: async () => ({ enrolled: false, required: true, need2fa: true, breakGlassWarn: false }),
      text: async () => '',
    };
  }
  if (String(url).includes('/admin/security')) {
    return { status: 403, ok: false, text: async () => 'forbidden' };
  }
  return { status: 200, ok: true, json: async () => ({}), text: async () => '' };
};
global.showLogin = () => {};

eval(showTabSrc + '\n' + refreshSrc + '\n');
if (typeof refreshSecurity !== 'function' || typeof showTab !== 'function') {
  console.error('missing showTab/refreshSecurity after extract');
  process.exit(1);
}

(async () => {
  activeTab = 'status';
  fetches = 0;
  sawPoll = false;
  await refreshSecurity();
  if (fetches >= 5) {
    console.error('GUI 2FA refresh loop: fetches=' + fetches + ' (want < 5)');
    process.exit(1);
  }
  if (!sawPoll) {
    console.error('expected X-Uplink-Poll on /admin/2fa/status');
    process.exit(1);
  }
  // Entering security tab once should not unbounded-loop.
  const before = fetches;
  showTab('security');
  // showTab may kick an async refreshSecurity; wait a tick.
  await new Promise((r) => setTimeout(r, 50));
  const delta = fetches - before;
  if (fetches >= 5 || delta >= 5) {
    console.error('showTab security loop: fetches=' + fetches + ' delta=' + delta);
    process.exit(1);
  }
  console.log('OK: GUI 2FA refresh fetches=' + fetches + ' (< 5), poll header present');
})().catch((e) => { console.error(e); process.exit(1); });
NODE
