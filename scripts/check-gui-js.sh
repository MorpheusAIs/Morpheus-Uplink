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

# Masked keys ("sk-p…d04f") are display-only: the Probe fetch, the curl box
# and copy/connect paths must send or show the full key, never the masked one.
node - "$OUT" <<'NODE'
const fs = require('fs');
const src = fs.readFileSync(process.argv[2], 'utf8');
function extract(name) {
  const m = new RegExp('(?:async\\s+)?function\\s+' + name + '\\s*\\([^)]*\\)\\s*\\{').exec(src);
  if (!m) throw new Error('missing function ' + name);
  let i = m.index + m[0].length, d = 1;
  while (i < src.length && d > 0) { const c = src[i++]; if (c === '{') d++; else if (c === '}') d--; }
  return src.slice(m.index, i);
}
const FULL = 'sk-p' + 'a'.repeat(40) + 'd04f';
const MASKED = 'sk-p\u2026d04f';
let sent = [], reveals = 0, clip = [];
const els = {};
global.document = { getElementById: (id) => (els[id] = els[id] || { value: '', textContent: '', classList: { add() {}, remove() {} } }) };
global.navigator = { clipboard: { writeText: async (t) => { clip.push(t); } } };
global.showLogin = () => {};
global.flashIconCopied = () => {};
global.probeDurationSec = () => 600;
global.baseURL = () => 'https://u.example/v1';
global.shellSingleQuote = (s) => s;
global.fetch = async (url, opts = {}) => {
  if (String(url).startsWith('/admin/keys?reveal=1')) {
    reveals++;
    return { status: 200, ok: true, json: async () => ({ masked: false, masterKey: 'sk-m' + 'b'.repeat(40), promptKey: FULL, keys: [] }) };
  }
  const h = (opts.headers || {}).Authorization;
  // Real fetch rejects non-ISO-8859-1 header values.
  if (h && /[^\x00-\xff]/.test(h)) throw new TypeError('non ISO-8859-1 code point');
  sent.push(h);
  return { ok: true, status: 200, text: async () => '{}' };
};
eval([
  "let realKeys = {}; const KEY_PLACEHOLDER = 'YOUR_API_KEY'; let selectedKeyId = 'prompt'; let selectedModel = 'm'; let curlDirty = false; const MIN_SESSION_DURATION = 600;",
  "let pgKeys = [{ id: 'prompt', secret: '" + MASKED + "' }];",
  ...['isMaskedKey', 'usableKey', 'rememberKeys', 'fullKey', 'selectedKeySecret', 'buildCurl', 'updateCurlPreview',
      'parseCurlCommand', 'postChatCompletion', 'runCurlCommand', 'runPlayground', 'copySecret', 'connectKeySecret', 'copyConnectKey'].map(extract),
  'global.T = { rememberKeys, buildCurl, runPlayground, runCurlCommand, copySecret, copyConnectKey, get realKeys() { return realKeys; } };',
].join('\n'));
function fail(m) { console.error('FAIL: ' + m); process.exit(1); }
(async () => {
  // Default (masked) list: nothing usable is remembered.
  T.rememberKeys({ masked: true, masterKey: 'sk-m\u2026bbbb', promptKey: MASKED, keys: [{ id: 'k1', secret: 'sk-e\u2026eeee' }] });
  if (Object.keys(T.realKeys).length) fail('masked keys remembered as real');
  els.pgPrompt = { value: 'hi' }; els.pgMaxTokens = { value: '16' }; els.pgDuration = { value: '600', classList: { add() {} } };
  const curl0 = T.buildCurl();
  if (curl0.includes('\u2026') || !curl0.includes('Bearer YOUR_API_KEY')) fail('curl box before reveal: ' + curl0);
  await T.runPlayground();
  if (sent.length !== 1 || sent[0] !== 'Bearer ' + FULL) fail('Probe sent ' + JSON.stringify(sent) + ' / ' + els.pgOut.textContent);
  if (reveals !== 1) fail('expected one reveal, got ' + reveals);
  if (!els.pgCurl.value.includes('Bearer ' + FULL) || els.pgCurl.value.includes('\u2026')) fail('curl box after reveal: ' + els.pgCurl.value);
  // A pasted curl with the masked form runs with the full key.
  els.pgCurl.value = T.buildCurl().replace(FULL, MASKED);
  await T.runCurlCommand();
  if (sent[1] !== 'Bearer ' + FULL) fail('curl run sent ' + sent[1] + ' / ' + els.pgOut.textContent);
  // Copy and connect paths copy the full key.
  els.connectKeySelect = { value: 'prompt' };
  await T.copySecret('prompt'); await T.copyConnectKey();
  if (clip.length !== 2 || clip.some((c) => c !== FULL)) fail('clipboard got ' + JSON.stringify(clip));
  if (reveals !== 1) fail('reveal not cached: ' + reveals);
  console.log('OK: masked keys never sent or copied; Probe, curl box, copy and connect use the full key');
})().catch((e) => fail(e.stack || e));
NODE
