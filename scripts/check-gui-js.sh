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
const viewSrc = extractFn('tfViewState', src);
const filterSrc = extractFn('filterAlerts', src);

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
    if (id === 'twoFactorManage') return { classList: { toggle() {} } };
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
      json: async () => ({ enrolled: false, on: true, need2fa: true, breakGlassWarn: false }),
      text: async () => '',
    };
  }
  if (String(url).includes('/admin/security')) {
    return { status: 403, ok: false, text: async () => 'forbidden' };
  }
  return { status: 200, ok: true, json: async () => ({}), text: async () => '' };
};
global.showLogin = () => {};
global.UI_COPY = new Proxy({}, { get: (_, k) => (k === 'tfEnrolled' || k === 'tfConfigWarn' ? (() => String(k)) : String(k)) });
global.reenrollPending = () => false;
global.tfEnrollRequested = false;
global.tfState = { on: false, enrolled: false };
global.setRecoveryHint = () => {};

eval(showTabSrc + '\n' + viewSrc + '\n' + filterSrc + '\n' + refreshSrc + '\n');
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
const WALLET = '0x' + 'ab12'.repeat(10);
let sent = [], reveals = 0, clip = [], stepUpPending = 0, stepUps = 0, stepUpAnswer = true, logins = 0;
const els = {};
global.document = { getElementById: (id) => (els[id] = els[id] || { value: '', textContent: '', classList: { add() {}, remove() {} } }) };
Object.defineProperty(globalThis, 'navigator', { value: { clipboard: { writeText: async (t) => { clip.push(t); } } }, configurable: true, writable: true });
global.showLogin = () => { logins++; };
global.openStepUp = async () => { stepUps++; if (stepUpAnswer) stepUpPending = 0; return stepUpAnswer; };
global.flashIconCopied = () => {};
global.probeDurationSec = () => 600;
global.baseURL = () => 'https://u.example/v1';
global.shellSingleQuote = (s) => s;
global.fetch = async (url, opts = {}) => {
  if (String(url).startsWith('/admin/keys?reveal=1')) {
    reveals++;
    if (stepUpPending) return { status: 401, ok: false, text: async () => 'step_up_required\n' };
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
  "let pgKeys = [{ id: 'prompt', secret: '" + MASKED + "' }]; let secrets = { wallet: '' };",
  ...['isMaskedKey', 'usableKey', 'rememberKeys', 'revealKeys', 'fullKey', 'selectedKeySecret', 'buildCurl', 'updateCurlPreview',
      'parseCurlCommand', 'postChatCompletion', 'runCurlCommand', 'runPlayground', 'copyText', 'copySecret', 'copyWallet', 'connectKeySecret', 'copyConnectKey'].map(extract),
  'global.T = { rememberKeys, revealKeys, buildCurl, runPlayground, runCurlCommand, copySecret, copyWallet, copyConnectKey, get realKeys() { return realKeys; }, secrets };',
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
  // Wallet copy: the full address (the page shows it shortened), not via a key reveal.
  T.secrets.wallet = WALLET;
  await T.copyWallet(null);
  if (clip[2] !== WALLET) fail('wallet copy got ' + JSON.stringify(clip[2]));
  if (reveals !== 1) fail('wallet copy fetched a key reveal');
  // A stale session (401 step_up_required) asks for step-up once, then retries.
  stepUpPending = 1; reveals = 0;
  const ks = await T.revealKeys();
  if (!ks || ks.promptKey !== FULL || stepUps !== 1 || reveals !== 2 || logins !== 0) fail('step-up retry: ' + JSON.stringify({ ks, stepUps, reveals, logins }));
  // Cancelled step-up: no keys, no logout, no loop.
  stepUpPending = 1; stepUpAnswer = false; reveals = 0; stepUps = 0;
  if ((await T.revealKeys()) !== null || stepUps !== 1 || reveals !== 1 || logins !== 0) fail('cancelled step-up');
  console.log('OK: masked keys never sent or copied; Probe, curl box, copy and connect use the full key; wallet copy copies the full address; a stale reveal asks for step-up once');
})().catch((e) => fail(e.stack || e));
NODE

# Status page: tiles, reclaim placement, unlock time, no low-wallet warning,
# health reason, model labels, centralized copy, no Swagger link, and the
# login recovery toggle.
TZ=America/New_York node - "$OUT" "$HTML" <<'NODE'
const fs = require('fs');
const src = fs.readFileSync(process.argv[2], 'utf8');
const html = fs.readFileSync(process.argv[3], 'utf8');
function extract(name) {
  const m = new RegExp('(?:async\\s+)?function\\s+' + name + '\\s*\\([^)]*\\)\\s*\\{').exec(src);
  if (!m) throw new Error('missing function ' + name);
  let i = m.index + m[0].length, d = 1;
  while (i < src.length && d > 0) { const c = src[i++]; if (c === '{') d++; else if (c === '}') d--; }
  return src.slice(m.index, i);
}
function constBlock(name) {
  const i = src.indexOf('const ' + name + ' = {');
  if (i < 0) throw new Error('missing const ' + name);
  let j = src.indexOf('{', i) + 1, d = 1;
  while (j < src.length && d > 0) { const c = src[j++]; if (c === '{') d++; else if (c === '}') d--; }
  return src.slice(i, j) + ';';
}
function fail(m) { console.error('FAIL: ' + m); process.exit(1); }
const els = {};
const elem = (id) => (els[id] = els[id] || { id, innerHTML: '', textContent: '', open: false, attrs: {},
  classList: { _s: new Set(), toggle(c, on) { on ? this._s.add(c) : this._s.delete(c); }, add(c) { this._s.add(c); }, contains(c) { return this._s.has(c); } },
  setAttribute(k, v) { this.attrs[k] = v; } });
global.document = { getElementById: elem };
let store = {};
global.localStorage = { getItem: (k) => (k in store ? store[k] : null), setItem: (k, v) => { store[k] = String(v); }, removeItem: (k) => { delete store[k]; } };
let clip = [];
Object.defineProperty(globalThis, 'navigator', { value: { clipboard: { writeText: async (t) => { clip.push(t); } } }, configurable: true, writable: true });
eval([
  constBlock('UI_COPY'),
  'let allModels = []; let modelStakes = {}; let modelStakesDurationSec = 600; let defaultSessionDuration = 600; const MIN_SESSION_DURATION = 600; let selectedModel = "";',
  'let stakeEstimateError = null; let catalogState = { state: "ok" }; let authed = true;',
  'const RECOVERY_HINT_KEY = "uplink.2fa.recoveryLeft"; let loginPassword = ""; let recoveryMode = false;',
  'function flashIconCopied() {} function probeDurationSec() { return 600; }',
  ...['escapeHtml', 'escapeAttr', 'tipHTML', 'setTip', 'fmtMor', 'fmtUnlockLocal', 'weiIsPositive', 'morTileHTML', 'renderMorTiles',
      'routerProblem', 'renderMasterKeyBanner', 'statusHealth', 'renderSysLine',
      'modelBySendableId', 'stakeKeyFor', 'stakeRecord', 'modelTitle', 'connectDefaultDurationSec', 'connectStakeMor', 'fmtMorRate', 'fmtDuration',
      'connectModelOptionLabel', 'populateConnectModels', 'copyConnectModel', 'recoveryHint', 'setLoginStep', 'setRecoveryMode'].map(extract),
  'global.T = { UI_COPY, renderMorTiles, fmtUnlockLocal, statusHealth, renderSysLine, renderMasterKeyBanner,',
  '  populateConnectModels, copyConnectModel, setLoginStep, setModels(m, st) { allModels = m; modelStakes = st || {}; } };',
].join('\n'));
const MOR = (n) => String(BigInt(Math.round(n * 1e4)) * 10n ** 14n);
(async () => {
  // Four tiles; Reclaim only when To claim > 0; on-hold subline in local time.
  const unlock = '2026-10-21T00:00:00Z';
  T.renderMorTiles({ walletWei: MOR(13.5), activeWei: '0', onHoldWei: MOR(0.36), nextUnlockUTC: unlock, claimWei: '0', now: new Date('2026-10-20T15:00:00Z') });
  let h = els.morBuckets.innerHTML;
  if ((h.match(/class="tile"/g) || []).length !== 4) fail('want 4 tiles: ' + h);
  if (/id="hkBtn"/.test(h)) fail('Reclaim shown with nothing to claim');
  if (!/id="hkNote"/.test(h)) fail('hkNote missing');
  if (!h.includes('Back by 8:00 PM EDT')) fail('on-hold subline: ' + h);
  for (const k of ['tileWalletTip', 'tileActiveTip', 'tileOnHoldTip', 'tileClaimTip']) {
    if (!h.includes('aria-label="' + T.UI_COPY[k].replace(/'/g, "'") + '"')) fail('tile tooltip missing aria-label: ' + k);
  }
  T.renderMorTiles({ walletWei: MOR(1), activeWei: MOR(0.5), onHoldWei: '0', claimWei: MOR(0.12) });
  h = els.morBuckets.innerHTML;
  if (!/id="hkBtn"/.test(h)) fail('Reclaim missing with an amount to claim');
  if (h.indexOf('id="hkBtn"') < h.lastIndexOf('class="tile"')) fail('Reclaim must sit in the To claim tile');
  if (h.includes('Back by')) fail('on-hold subline shown with nothing on hold');
  if (T.fmtUnlockLocal(unlock, new Date('2026-10-19T12:00:00Z')).indexOf('Tue') !== 0) fail('weekday prefix when not today: ' + T.fmtUnlockLocal(unlock, new Date('2026-10-19T12:00:00Z')));
  if (/onHold.*00:00 UTC.*next/i.test(src)) fail('old on-hold repeat paragraph still present');

  // Add MOR is a plain section: always collapsed, no low-wallet logic.
  if (/LOW_WALLET|isWalletLow|lowWalletThreshold|fundSummaryLow|renderFund|fold\.low/.test(html)) fail('low-wallet warning code still present');
  if (!/<details id="fundDetails" class="fold">/.test(html)) fail('Add MOR must be a collapsed <details> without open');

  // 2FA on/off switch: a real switch on Security, and turning off goes
  // through the password + code form (postFactor), never a bare request.
  if (!/<input type="checkbox" id="tfSwitch" role="switch" onchange="toggleTwoFactor\(this\.checked\)">/.test(html)) fail('Security must have the 2FA switch');
  if (!/postFactor\('\/admin\/2fa\/remove', f\)/.test(src) || (src.match(/\/admin\/2fa\/remove/g) || []).length !== 1) fail('Turn off 2FA must call /admin/2fa/remove only via postFactor');

  // MASTER_KEY_ADMIN banner follows status.twoFactor.masterKeyAdmin.
  if (!/id="masterKeyAdminBanner"[^>]*class="[^"]*hidden|class="[^"]*hidden[^"]*" id="masterKeyAdminBanner"/.test(html)) fail('master key banner must start hidden');
  T.renderMasterKeyBanner({ twoFactor: { masterKeyAdmin: true } });
  if (els.masterKeyAdminBanner.classList.contains('hidden') || els.masterKeyAdminBanner.textContent !== T.UI_COPY.masterKeyAdminOn) fail('master key banner not shown when on');
  T.renderMasterKeyBanner({ twoFactor: { masterKeyAdmin: false } });
  if (!els.masterKeyAdminBanner.classList.contains('hidden')) fail('master key banner shown when off');
  T.renderMasterKeyBanner({});
  if (!els.masterKeyAdminBanner.classList.contains('hidden')) fail('master key banner shown without the flag');

  // Status line: Healthy, or the plain reason.
  if (T.statusHealth({}).text !== 'Healthy') fail('healthy');
  if (T.statusHealth({ routerError: 'x' }).text !== T.UI_COPY.routerDown) fail('router down reason');
  if (T.statusHealth({ routerAuth: { ok: false } }).text !== T.UI_COPY.routerAuthFail) fail('router auth reason');
  T.renderSysLine('1.0.15', '7.17.0', T.statusHealth({}), 0);
  const line = els.stats.innerHTML.replace(/<[^>]+>/g, '');
  if (line !== '\u25cfHealthy·0 open sessions·Uplink v1.0.15·Router v7.17.0') fail('status line: ' + line);

  // Model dropdown: name + lock, no hash; copy copies the full ID.
  const ID = '0x' + 'ab'.repeat(32);
  T.setModels([{ id: ID, name: 'glm-5.2', blockchainId: ID }, { id: 'x2', name: 'glm-5.2' }], { [ID]: { stakeMor: 0.36 } });
  els.connectModelSelect = { value: '', innerHTML: '' };
  T.populateConnectModels();
  const opts = els.connectModelSelect.innerHTML;
  if (opts.replace(/value="[^"]*"/g, '').includes('0x')) fail('model option shows the ID/hash: ' + opts);
  if (!opts.includes('glm-5.2 (2)')) fail('duplicate names need a plain suffix: ' + opts);
  els.connectModelSelect.value = ID;
  await T.copyConnectModel();
  if (clip[clip.length - 1] !== ID) fail('copy must copy the full model ID, got ' + clip[clip.length - 1]);

  // Copy is centralized: every data-copy / data-tip-key names a UI_COPY string.
  for (const m of html.matchAll(/data-(?:copy|tip-key)="([^"]+)"/g)) {
    if (typeof T.UI_COPY[m[1]] !== 'string') fail('UI_COPY missing key ' + m[1]);
  }
  for (const k of ['tilesShared', 'tileWalletTip', 'tileActiveTip', 'tileOnHoldTip', 'tileClaimTip']) {
    if (!T.UI_COPY[k]) fail('missing copy ' + k);
  }
  if (!/lock/i.test(T.UI_COPY.tilesShared) || !/back to this wallet/i.test(T.UI_COPY.tilesShared)) fail('shared tooltip must keep lock-not-spend meaning');
  if (/swagger/i.test(html)) fail('GUI must not link to Swagger');
  if (/allowance|resync/i.test(html)) fail('dropped allowance/resync UI is back');
  if (/outflow/i.test(html)) fail('outflow wording in GUI');

  // Login: hide "Use a recovery code" when this browser knows none are left.
  els.totpCode = { value: 'x', focus() {}, removeAttribute() {}, setAttribute() {}, classList: { add() {}, remove() {} } };
  els.loginStepPassword = elem('loginStepPassword'); els.loginStepCode = elem('loginStepCode');
  store['uplink.2fa.recoveryLeft'] = '0';
  T.setLoginStep('code');
  if (!els.recoveryToggle.classList.contains('hidden')) fail('recovery toggle shown with 0 codes');
  store['uplink.2fa.recoveryLeft'] = '3';
  T.setLoginStep('code');
  if (els.recoveryToggle.classList.contains('hidden')) fail('recovery toggle hidden with codes left');
  delete store['uplink.2fa.recoveryLeft'];
  T.setLoginStep('code');
  if (els.recoveryToggle.classList.contains('hidden')) fail('recovery toggle hidden when unknown');
  console.log('OK: status tiles, reclaim placement, unlock time, Add MOR collapsed, health line, model labels, copy keys, login recovery toggle');
})().catch((e) => fail(e.stack || e));
NODE

# 2FA manage form and the reset -> re-login -> fresh enrollment flow.
node - "$OUT" "$HTML" <<'NODE'
const fs = require('fs');
const src = fs.readFileSync(process.argv[2], 'utf8');
const html = fs.readFileSync(process.argv[3], 'utf8');
function extract(name) {
  const re = new RegExp('(?:async\\s+)?function\\s+' + name + '\\s*\\(');
  const m = re.exec(src);
  if (!m) throw new Error('missing function ' + name);
  let i = src.indexOf('{', m.index + m[0].length - 1);
  let d = 0;
  for (; i < src.length; i++) { if (src[i] === '{') d++; else if (src[i] === '}' && --d === 0) { i++; break; } }
  return src.slice(m.index, i);
}
function fail(m) { console.error('FAIL: ' + m); process.exit(1); }
const mkEl = (id) => ({ id, value: '', textContent: '', disabled: false, style: {}, focus() {},
  classList: { _s: new Set(), toggle(c, on) { on ? this._s.add(c) : this._s.delete(c); }, add(c) { this._s.add(c); }, remove(c) { this._s.delete(c); }, contains(c) { return this._s.has(c); } } });
const els = {};
global.document = { getElementById: (id) => (els[id] = els[id] || mkEl(id)), body: mkEl('body') };
const calls = { showTab: [], refreshSecurity: 0, startTotpEnroll: 0, setEnrollMsg: [] };
global.T = {};
eval([
  "const UI_COPY = { enrollFirst: 'ENROLL_FIRST', tfDisableNeedsFactor: 'NEEDS_FACTOR', tfDisableConfirm: 'CONFIRM_OFF', tfNotEnrolled: '2FA is off: login needs only the password.', tfPending: 'PENDING', tfPendingLocked: 'PENDING_LOCKED', tfEnrolled: (n) => 'ON ' + n, recoveryNoneLeft: 'NONE_LEFT', tfBreakGlass: '', tfConfigWarn: () => '' };",
  'let tfStatusAnswer = {}; function showLogin() {} function setRecoveryHint() {} function reenrollPending() { return false; }',
  'async function fetch(url) { return String(url).includes("/admin/2fa/status") ? { status: 200, ok: true, json: async () => tfStatusAnswer } : { status: 200, ok: true, json: async () => ({ alerts: [] }) }; }',
  'let tfEnrollRequested = false; let tfState = { on: false, enrolled: false };',
  'const posted = []; async function postFactor(path, f) { posted.push(path); return { ok: false, status: 401, text: async () => "", headers: { get: () => null } }; }',
  'function confirm() { return true; }',
  "let activeTab = 'security';",
  'function showTab(id) { calls.showTab.push(id); activeTab = id; }',
  'function refreshSecurity() { calls.refreshSecurity++; }',
  'function startTotpEnroll() { calls.startTotpEnroll++; }',
  'function setManageMsg(t) { document.getElementById("manageMsg").textContent = t || ""; }',
  'function setEnrollMsg(t) { calls.setEnrollMsg.push(t); document.getElementById("enrollMsg").textContent = t || ""; }',
  'function hideRecoveryCodes() { document.getElementById("recoveryCodes").textContent = ""; }',
  'function hideEnrollSecret() { document.getElementById("enrollSecret").textContent = ""; }',
  'let enrollOnly = false; let enrollAutoStarted = false;',
  ...['manageFields', 'updateManageButtons', 'isEnrollRequired', 'enterEnrollmentOnly', 'leaveEnrollmentOnly', 'resetSecurityView', 'factorError', 'toggleTwoFactor', 'disableTwoFactor', 'tfViewState', 'filterAlerts'].map(extract),
  extract('refreshSecurity').replace('async function refreshSecurity', 'async function renderSecurity'),
  'Object.assign(T, { updateManageButtons, resetSecurityView, enterEnrollmentOnly, factorError, toggleTwoFactor, posted, renderSecurity, setStatus: (v) => { tfStatusAnswer = v; }, setRequested: (v) => { tfEnrollRequested = v; }, setTF: (v) => { tfState = v; }, requested: () => tfEnrollRequested, state: () => ({ enrollOnly, enrollAutoStarted, activeTab }), setTab: (t) => { activeTab = t; } });',
].join('\n'));
const $ = (id) => document.getElementById(id);
(async () => {
  // Markup: buttons start disabled; both fields re-check on every input.
  for (const id of ['regenBtn', 'resetBtn', 'disableBtn']) {
    if (!new RegExp('id="' + id + '"[^>]*\\bdisabled\\b').test(html)) fail(id + ' must start disabled');
  }
  for (const id of ['managePw', 'manageCode']) {
    if (!new RegExp('id="' + id + '"[^>]*oninput="updateManageButtons\\(\\)"').test(html)) fail(id + ' must call updateManageButtons on input');
  }
  // Buttons enable only with BOTH fields non-empty.
  const states = [['', ''], ['pw', ''], ['', '123456'], ['pw', '   '], ['pw', '123456']];
  for (const [pw, code] of states) {
    $('managePw').value = pw; $('manageCode').value = code; T.updateManageButtons();
    const want = !!(pw && code.trim());
    if ($('regenBtn').disabled === want || $('resetBtn').disabled === want || $('disableBtn').disabled === want) fail('buttons for pw=' + JSON.stringify(pw) + ' code=' + JSON.stringify(code));
  }
  // Logout / reset: nothing from the previous session survives.
  $('twoFactorStatus').textContent = '2FA enrolled · 10 recovery codes left.';
  $('twoFactorManage').classList.toggle('hidden', false);
  $('securityAlerts').textContent = 'login ok';
  $('manageMsg').textContent = 'old';
  T.resetSecurityView();
  if ($('twoFactorStatus').textContent !== 'Loading…') fail('stale 2FA status after showLogin');
  if (!$('twoFactorManage').classList.contains('hidden')) fail('manage form visible after showLogin');
  if ($('securityAlerts').textContent !== 'Loading…' || $('manageMsg').textContent) fail('stale alerts or message after showLogin');
  if ($('managePw').value || $('manageCode').value || !$('resetBtn').disabled) fail('manage fields not cleared');
  if (!extract('showLogin').includes('resetSecurityView();')) fail('showLogin must call resetSecurityView');
  // Malformed security switch (configWarn, names only) is shown on Security.
  if (!/if \(Array\.isArray\(tf\.configWarn\) && tf\.configWarn\.length\) msg \+= UI_COPY\.tfConfigWarn\(tf\.configWarn\)/.test(src)) fail('Security must show configWarn');
  // Re-login while 2FA is on and not enrolled (after a reset): Security only, reloaded, new enrollment started once.
  if (!/if \(data\.need2fa\) \{\s*enterEnrollmentOnly\(\)/.test(src)) fail('finishLogin must enter enrollment-only mode on need2fa');
  T.setTab('security');
  T.enterEnrollmentOnly();
  if (!document.body.classList.contains('enroll-only')) fail('enroll-only class not set');
  if (calls.refreshSecurity !== 1) fail('Security must reload when already on that tab (stale view bug): ' + calls.refreshSecurity);
  if (calls.startTotpEnroll !== 1) fail('new enrollment must start');
  T.enterEnrollmentOnly(true);
  if (calls.startTotpEnroll !== 1 || calls.refreshSecurity !== 1) fail('enrollment must not restart on refresh');
  if (!/body\.enroll-only \.tab:not\(\[data-tab="security"\]\) \{ display: none; \}/.test(html)) fail('other tabs must hide in enroll-only mode');
  T.resetSecurityView();
  if (document.body.classList.contains('enroll-only') || T.state().enrollAutoStarted) fail('enroll-only must clear on logout');
  T.setTab('status'); calls.showTab = [];
  T.enterEnrollmentOnly();
  if (calls.showTab[0] !== 'security' || calls.startTotpEnroll !== 2) fail('must switch to Security and enroll from another tab');
  // Reset/regen answered 403 "2fa enrollment required": show enrollment, not the raw error.
  T.resetSecurityView(); T.setTab('security');
  const msg = await T.factorError({ status: 403, text: async () => '2fa enrollment required\n', statusText: 'Forbidden' });
  if (msg !== 'ENROLL_FIRST' || !document.body.classList.contains('enroll-only')) fail('403 enrollment required must open enrollment: ' + msg);
  if (await T.factorError({ status: 401, text: async () => '', headers: { get: () => null } }) !== 'Password or code not accepted.') fail('401 message');
  // A wrong enrollment code is not a lost session.
  if (!/codeCheck: true,\s*method: 'POST',/.test(extract('confirmTotpEnroll'))) fail('enroll confirm must not log out on a wrong code');
  // 2FA switch: on starts the enrollment; off with 2FA on needs password
  // and code first and leaves the switch on; with both it posts remove.
  T.resetSecurityView(); calls.startTotpEnroll = 0;
  T.toggleTwoFactor(true);
  if (calls.startTotpEnroll !== 1 || !T.requested()) fail('switch on must start an enrollment');
  T.toggleTwoFactor(false);
  if (T.requested() || T.posted.length) fail('switch off before confirming must only cancel the enrollment');
  T.setTF({ on: true, enrolled: true });
  $('managePw').value = ''; $('manageCode').value = ''; $('tfSwitch').checked = false;
  T.toggleTwoFactor(false);
  if (T.posted.length || $('tfSwitch').checked !== true || $('manageMsg').textContent !== 'NEEDS_FACTOR') fail('switch off without password and code must not post and must stay on');
  $('managePw').value = 'pw'; $('manageCode').value = '123456';
  T.toggleTwoFactor(false);
  await new Promise((r) => setTimeout(r, 0));
  if (T.posted.join() !== '/admin/2fa/remove' || $('tfSwitch').checked !== true) fail('switch off with password and code must post remove once: ' + T.posted);
  T.setTF({ on: true, enrolled: false });
  T.toggleTwoFactor(false);
  if (T.posted.length !== 1 || $('tfSwitch').checked !== true) fail('2FA on without enrollment (after reset) cannot be switched off');
  // One state at a time: off shows the recommendation; pending (switch on
  // or just turned on, no confirmed code) hides it and every "off" text.
  const view = async (st, requested) => {
    T.resetSecurityView(); T.setRequested(requested); T.setStatus(st);
    await T.renderSecurity();
    return { text: $('twoFactorStatus').textContent, noteHidden: $('twoFactorOffNote').classList.contains('hidden'),
      checked: $('tfSwitch').checked, enroll: $('twoFactorEnroll').style.display };
  };
  let v = await view({ on: false, enrolled: false }, false);
  if (v.noteHidden || v.checked || v.enroll !== 'none' || !/off/.test(v.text)) fail('off state: ' + JSON.stringify(v));
  for (const [st, req, want] of [[{ on: true, enrolled: false }, false, 'PENDING_LOCKED'], [{ on: false, enrolled: false }, true, 'PENDING']]) {
    v = await view(st, req);
    if (!v.noteHidden) fail('recommendation shown while enrollment is pending: ' + JSON.stringify(st));
    if (v.text !== want || /\boff\b/i.test(v.text) || !v.checked || v.enroll !== 'grid') fail('pending state: ' + JSON.stringify(v));
  }
  v = await view({ on: true, enrolled: true, recoveryRemaining: 10 }, false);
  if (!v.noteHidden || v.text !== 'ON 10' || !v.checked || v.enroll !== 'none') fail('on state: ' + JSON.stringify(v));
  // Manage buttons share the Regenerate size (no small danger pills).
  if (!/#twoFactorManage button\.danger-ghost \{ padding: 8px 14px; font-size: 13px; \}/.test(html)) fail('Reset / Turn off must match the Regenerate button size');
  // Enrollment: QR code, then the secret under it, then Start, then the code form.
  const order = ['id="enrollQr"', 'id="enrollSecret"', 'onclick="startTotpEnroll()"', 'id="enrollConfirmForm"'].map((k) => html.indexOf(k));
  if (order.some((i, n) => i < 0 || (n && i < order[n - 1]))) fail('enrollment layout order: ' + order);
  console.log('OK: 2FA buttons need password and code; reset clears the view; re-login goes straight to a fresh enrollment; 2FA switch; one 2FA state at a time');
})().catch((e) => fail(e.stack || e));
NODE

# Security alerts: time | bold action | details, newest first, filter box.
TZ=UTC node - "$OUT" "$HTML" <<'NODE'
const fs = require('fs');
const src = fs.readFileSync(process.argv[2], 'utf8');
const html = fs.readFileSync(process.argv[3], 'utf8');
function extract(name) {
  const m = new RegExp('function\\s+' + name + '\\s*\\(').exec(src);
  if (!m) throw new Error('missing function ' + name);
  let i = src.indexOf('{', m.index + m[0].length - 1), d = 0;
  for (; i < src.length; i++) { if (src[i] === '{') d++; else if (src[i] === '}' && --d === 0) { i++; break; } }
  return src.slice(m.index, i);
}
function fail(m) { console.error('FAIL: ' + m); process.exit(1); }
const mkEl = () => ({ value: '', classList: { _s: new Set(), toggle(c, on) { on ? this._s.add(c) : this._s.delete(c); }, contains(c) { return this._s.has(c); } } });
const els = { alertFilter: mkEl(), alertsNoMatch: mkEl(), securityAlerts: { rows: [], querySelectorAll() { return this.rows; } } };
// DOM stub: nodes record className, textContent and children; there is no
// innerHTML, so any use of it in alertRowsTable shows up below.
const mkNode = (tag) => ({ tag, className: '', textContent: '', children: [], appendChild(c) { this.children.push(c); return c; } });
global.document = { getElementById: (id) => els[id] || null, createElement: mkNode };
global.T = {};
eval(['alertTime', 'alertDetail', 'alertRowsTable', 'filterAlerts'].map(extract).join('\n')
  + '\nObject.assign(T, { alertRowsTable, filterAlerts });');
if (/alertRowsHtml|box\.innerHTML = alert/.test(html)) fail('security alerts must not be rendered through innerHTML');
if (!html.includes('box.replaceChildren(alertRowsTable(alerts));')) fail('security alerts must use alertRowsTable');
if (/innerHTML/.test(extract('alertRowsTable'))) fail('alertRowsTable must use textContent only');
if (!/id="alertFilter"[^>]*type="search"[^>]*oninput="filterAlerts\(\)"/.test(html)) fail('filter box must call filterAlerts on input');
if (html.indexOf('id="alertFilter"') > html.indexOf('id="securityAlerts"')) fail('filter box must sit above the list');
if (!/\.alerts-table \.alert-at \{[^}]*monospace/.test(html)) fail('time column must be monospace');
// Out of order on purpose; one message repeats the event time.
const alerts = [
  { at: '2026-10-20T14:05:09.120Z', kind: 'login', result: 'ok', ip: '203.0.113.7', message: 'admin login' },
  { at: '2026-10-20T15:30:00Z', kind: 'balance_drop', message: 'MOR balance decreased by 1.5 MOR at 2026-10-20T15:30:00Z' },
  { at: '2026-10-19T23:59:59.5Z', kind: '2fa_enroll', message: 'enrolled (2026-10-19 23:59:59)' },
  { at: '2026-10-19T08:00:00Z', kind: 'key_create', message: 'key created (agent-1)' },
];
const out = T.alertRowsTable(alerts);
if (out.tag !== 'table' || out.className !== 'alerts-table' || out.children[0].tag !== 'tbody') fail('table shape');
const rows = out.children[0].children.map((tr) => {
  if (tr.className !== 'alert-row' || tr.children.length !== 3) fail('row shape');
  const [at, kind, detail] = tr.children;
  if (at.className !== 'alert-at' || kind.className !== 'alert-kind' || kind.children[0].tag !== 'strong' || detail.className !== 'alert-detail') fail('cell shape');
  return { at: at.textContent, kind: kind.children[0].textContent, detail: detail.textContent };
});
if (rows.length !== 4) fail('want 4 rows of time | bold action | details');
// Markup in an audit field stays text.
const evil = T.alertRowsTable([{ at: '2026-10-20T14:05:09Z', kind: '<img src=x onerror=alert(1)>', message: '/admin/<b>x</b>' }]);
const er = evil.children[0].children[0];
if (er.children[1].children[0].textContent !== '<img src=x onerror=alert(1)>' || er.children[2].textContent !== '/admin/<b>x</b>') fail('fields must be set as text');
if (rows.map((r) => r.kind).join(',') !== 'balance_drop,login,2fa_enroll,key_create') fail('rows must be newest first: ' + rows.map((r) => r.kind));
for (const r of rows) {
  if (!/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/.test(r.at)) fail('time format YYYY-MM-DD HH:MM:SS: ' + r.at);
  if (/\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}/.test(r.detail)) fail('details repeat the time: ' + r.detail);
}
if (rows[0].at !== '2026-10-20 15:30:00' || rows[2].at !== '2026-10-19 23:59:59') fail('local time: ' + rows.map((r) => r.at));
if (rows[0].detail !== 'MOR balance decreased by 1.5 MOR') fail('balance detail: ' + rows[0].detail);
if (rows[1].detail !== 'admin login · ok · 203.0.113.7') fail('login detail: ' + rows[1].detail);
if (rows[2].detail !== 'enrolled') fail('enroll detail: ' + rows[2].detail);
if (rows[3].detail !== 'key created (agent-1)') fail('details without a time stay as they are: ' + rows[3].detail);
// Filter: case-insensitive substring over the row; others hidden.
els.securityAlerts.rows = rows.map((r) => Object.assign(mkEl(), { textContent: r.at + r.kind + r.detail }));
const hidden = () => els.securityAlerts.rows.map((r) => r.classList.contains('hidden'));
els.alertFilter.value = 'LOGIN'; T.filterAlerts();
if (hidden().join() !== 'true,false,true,true') fail('filter "LOGIN": ' + hidden());
els.alertFilter.value = 'mor'; T.filterAlerts();
if (hidden().join() !== 'false,true,true,true') fail('filter "mor": ' + hidden());
if (!els.alertsNoMatch.classList.contains('hidden')) fail('no-match note shown while rows match');
els.alertFilter.value = 'nothing-like-this'; T.filterAlerts();
if (hidden().join() !== 'true,true,true,true' || els.alertsNoMatch.classList.contains('hidden')) fail('no-match: ' + hidden());
els.alertFilter.value = '  '; T.filterAlerts();
if (hidden().join() !== 'false,false,false,false' || !els.alertsNoMatch.classList.contains('hidden')) fail('empty filter shows all');
console.log('OK: security alerts newest first, YYYY-MM-DD HH:MM:SS, bold action, no repeated time, filter, textContent only');
NODE

# Status warns when the page origin is not WEB_PUBLIC_URL (text only).
node - "$OUT" "$HTML" <<'NODE'
const fs = require('fs');
const src = fs.readFileSync(process.argv[2], 'utf8');
const html = fs.readFileSync(process.argv[3], 'utf8');
function fn(name) {
  const m = new RegExp('^function\\s+' + name + '\\s*\\(', 'm').exec(src);
  if (!m) throw new Error('missing function ' + name);
  return src.slice(m.index, src.indexOf('\n}', m.index) + 2);
}
function fail(m) { console.error('FAIL: ' + m); process.exit(1); }
const ui = /const UI_COPY = \{[\s\S]*?\n\};/.exec(src);
if (!ui) fail('UI_COPY not found');
const el = { textContent: '', classList: { _h: true, toggle(c, on) { this._h = on; } } };
global.document = { getElementById: (id) => (id === 'originWarn' ? el : null) };
global.location = { origin: 'https://new-host.example' };
eval(ui[0].replace('const UI_COPY', 'global.UI_COPY') + '\n' + ['originWarning', 'renderOriginWarn'].map(fn).join('\n')
  + '\nObject.assign(global, { originWarning, renderOriginWarn });');
if (originWarning({ webPublicUrl: 'https://new-host.example' }, 'https://New-Host.example/') !== '') fail('matching origin must not warn');
if (originWarning({}, 'https://x.example') !== '') fail('no warning without the status field');
const w = originWarning({ webPublicUrl: 'https://old-host.example' }, 'https://new-host.example');
if (!w.includes('open at https://new-host.example') || !w.includes('WEB_PUBLIC_URL is https://old-host.example')) fail('mismatch warning: ' + w);
renderOriginWarn({ webPublicUrl: 'https://old-host.example' });
if (el.classList._h || !el.textContent.includes('old-host.example')) fail('Status warning not shown');
renderOriginWarn({ webPublicUrl: 'https://new-host.example' });
if (!el.classList._h || el.textContent) fail('Status warning not cleared');
if (html.indexOf('id="originWarn"') < 0 || html.indexOf('id="originWarn"') > html.indexOf('id="connectClient"')) fail('originWarn must be on the Status overview');
console.log('OK: Status warns when the page origin is not WEB_PUBLIC_URL');
NODE

# Status warns while name-keyed provider exclusions refuse opens (text only).
node - "$OUT" "$HTML" <<'NODE'
const fs = require('fs');
const src = fs.readFileSync(process.argv[2], 'utf8');
const html = fs.readFileSync(process.argv[3], 'utf8');
function fn(name) {
  const m = new RegExp('^function\\s+' + name + '\\s*\\(', 'm').exec(src);
  if (!m) throw new Error('missing function ' + name);
  return src.slice(m.index, src.indexOf('\n}', m.index) + 2);
}
function fail(m) { console.error('FAIL: ' + m); process.exit(1); }
const ui = /const UI_COPY = \{[\s\S]*?\n\};/.exec(src);
if (!ui) fail('UI_COPY not found');
const el = { textContent: '', classList: { _h: true, toggle(c, on) { this._h = on; } } };
global.document = { getElementById: (id) => (id === 'exclusionsWarn' ? el : null) };
eval(ui[0].replace('const UI_COPY', 'global.UI_COPY') + '\n' + ['exclusionsWarning', 'renderExclusionsWarn'].map(fn).join('\n')
  + '\nObject.assign(global, { exclusionsWarning, renderExclusionsWarn });');
if (exclusionsWarning({}) !== '' || exclusionsWarning({ exclusionsUnresolved: 0 }) !== '') fail('no warning without refusals');
const w = exclusionsWarning({ exclusionsUnresolved: 2 });
if (!w.includes('2 models') || !w.includes('UPLINK_PROVIDER_EXCLUDE') || /0x/.test(w)) fail('warning text: ' + w);
const wc = exclusionsWarning({ exclusionsUnresolved: 64, exclusionsUnresolvedCapped: true });
if (!wc.includes('at least 64 models')) fail('capped warning text: ' + wc);
if (exclusionsWarning({ exclusionsUnresolved: 3, exclusionsUnresolvedCapped: 'true' }).includes('at least')) fail('capped flag must be a real boolean');
renderExclusionsWarn({ exclusionsUnresolved: 1 });
if (el.classList._h || !el.textContent.includes('1 model')) fail('exclusions warning not shown');
renderExclusionsWarn({ exclusionsUnresolved: 0 });
if (!el.classList._h || el.textContent) fail('exclusions warning not cleared');
if (html.indexOf('id="exclusionsWarn"') < 0 || html.indexOf('id="exclusionsWarn"') > html.indexOf('id="connectClient"')) fail('exclusionsWarn must be on the Status overview');
if (!/renderExclusionsWarn\(st\);/.test(src)) fail('status refresh must render the exclusions warning');
console.log('OK: Status warns while name-keyed provider exclusions refuse opens (capped count shown as at least N)');
NODE

# API Keys has no reveal button; copy and Export all fetch the full keys.
if grep -qE 'Reveal full keys|Masked by default|refreshKeys\(true' "$HTML"; then
  echo "FAIL: API Keys still has the reveal button or its note" >&2; exit 1
fi
grep -qF "const ks = await revealKeys();" "$HTML" || { echo "FAIL: Export all must fetch full keys" >&2; exit 1; }
grep -qF "fetch('/admin/step-up'" "$HTML" || { echo "FAIL: step-up dialog must post /admin/step-up" >&2; exit 1; }
grep -qF 'id="enrollPw"' "$HTML" || { echo "FAIL: enrollment needs a password field" >&2; exit 1; }
echo "OK: no reveal button; copy and Export all fetch the full keys on demand"

# S6 settings: env-pinned fields are disabled with the env name; others
# get Save and Reset; values are built with textContent (no innerHTML).
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
function fail(m) { console.error('FAIL: ' + m); process.exit(1); }
const mk = (tag) => ({ tag, children: [], style: {}, disabled: false, value: '', textContent: '',
  appendChild(c) { this.children.push(c); return c; } });
const box = mk('div');
global.document = { getElementById: () => box, createElement: mk };
const fn = extract('renderSettings');
if (/innerHTML/.test(fn)) fail('renderSettings must not use innerHTML');
eval(fn + '\nglobal.renderSettings = renderSettings;');
renderSettings([
  { name: 'forwardIdle', value: 60, default: 180, min: 30, max: 600, source: 'env', env: 'UPLINK_FORWARD_IDLE_SECONDS', pinned: true },
  { name: 'logLevel', value: 'debug', default: 'info', options: ['debug', 'info', 'warn', 'error'], source: 'persisted', env: 'UPLINK_LOG_LEVEL', pinned: false },
]);
const [pinned, free] = box.children;
const pin = pinned.children.find((c) => c.tag === 'input');
if (!pin || !pin.disabled) fail('env-pinned field must be disabled');
if (pinned.children.some((c) => c.tag === 'button')) fail('env-pinned field must have no Save/Reset');
if (!pinned.children.some((c) => c.textContent === 'set by environment (UPLINK_FORWARD_IDLE_SECONDS)')) fail('missing env note');
const sel = free.children.find((c) => c.tag === 'select');
if (!sel || sel.disabled || sel.value !== 'debug') fail('free field must be enabled with its value');
if (free.children.filter((c) => c.tag === 'button').length !== 2) fail('free field needs Save and Reset');
console.log('OK: settings env-pinned fields disabled; others editable');
NODE
