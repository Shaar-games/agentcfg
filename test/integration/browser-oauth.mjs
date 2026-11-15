// Optional real-browser OAuth regression driver. Uses only Node built-ins and
// an explicitly supplied Chromium binary/profile; never discovers user profiles.
import { spawn, spawnSync } from 'node:child_process';
import { readFile } from 'node:fs/promises';
import path from 'node:path';

const input = JSON.parse(await new Promise((resolve, reject) => {
  let text = '';
  process.stdin.setEncoding('utf8');
  process.stdin.on('data', chunk => { text += chunk; if (text.length > 65536) reject(new Error('input_limit')); });
  process.stdin.on('end', () => resolve(text));
  process.stdin.on('error', reject);
}));
if (!path.isAbsolute(input.executable) || !path.isAbsolute(input.profile)) throw new Error('absolute_test_paths_required');
const callback = new URL(input.callbackURL);
const authorization = new URL(input.url);
const provider = new URL(input.providerURL);
const deadline = Date.now() + Math.min(input.timeoutMs ?? 20000, 30000);
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
const violations = new Set();
const result = { completed: false, cspViolations: [], browser: '', submitted: false, responses: [] };
let browser, cdp;
let rejectedForm = false;

class CDP {
  constructor(socket) {
    this.socket = socket;
    this.sequence = 0;
    this.pending = new Map();
    this.listeners = [];
    socket.addEventListener('message', event => {
      const message = JSON.parse(String(event.data));
      if (message.id) {
        const pending = this.pending.get(message.id);
        if (!pending) return;
        this.pending.delete(message.id);
        clearTimeout(pending.timer);
        message.error ? pending.reject(new Error('cdp_command_failed')) : pending.resolve(message.result);
      } else {
        for (const listener of this.listeners) listener(message);
      }
    });
    socket.addEventListener('close', () => {
      for (const pending of this.pending.values()) {
        clearTimeout(pending.timer);
        pending.reject(new Error('cdp_closed'));
      }
      this.pending.clear();
    });
  }
  static async connect(url) {
    const socket = new WebSocket(url);
    await new Promise((resolve, reject) => {
      const timeout = setTimeout(() => reject(new Error('cdp_connection_timeout')), 5000);
      socket.addEventListener('open', () => { clearTimeout(timeout); resolve(); }, { once: true });
      socket.addEventListener('error', () => { clearTimeout(timeout); reject(new Error('cdp_connection_failed')); }, { once: true });
    });
    return new CDP(socket);
  }
  send(method, params = {}, sessionId) {
    return new Promise((resolve, reject) => {
      const id = ++this.sequence;
      const timer = setTimeout(() => { this.pending.delete(id); reject(new Error('cdp_command_timeout')); }, 5000);
      this.pending.set(id, { resolve, reject, timer });
      this.socket.send(JSON.stringify({ id, method, params, ...(sessionId ? { sessionId } : {}) }));
    });
  }
}

try {
  browser = spawn(input.executable, [
    '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
    '--disable-background-networking', '--disable-component-update', '--disable-default-apps',
    '--disable-sync', '--disable-breakpad', '--metrics-recording-only',
    '--password-store=basic', '--use-mock-keychain',
    '--remote-debugging-address=127.0.0.1', '--remote-debugging-port=0',
    `--user-data-dir=${input.profile}`, `--disk-cache-dir=${path.join(input.profile, 'cache')}`,
    'about:blank',
  ], { windowsHide: true, stdio: ['ignore', 'ignore', 'pipe'] });
  // Browser stderr can contain URLs with test codes. Consume it without logging.
  browser.stderr.resume();
  let launchFailed = false;
  browser.once('error', () => { launchFailed = true; });
  let endpoint;
  while (Date.now() < deadline) {
    if (launchFailed || browser.exitCode !== null) throw new Error('browser_start_failed');
    try {
      const lines = (await readFile(path.join(input.profile, 'DevToolsActivePort'), 'utf8')).trim().split(/\r?\n/);
      if (/^\d+$/.test(lines[0]) && lines[1]?.startsWith('/devtools/browser/')) {
        endpoint = `ws://127.0.0.1:${lines[0]}${lines[1]}`;
        break;
      }
    } catch { /* the isolated browser has not published its port yet */ }
    await pause(40);
  }
  if (!endpoint) throw new Error('browser_start_timeout');
  cdp = await CDP.connect(endpoint);
  result.browser = (await cdp.send('Browser.getVersion')).product;
  const target = await cdp.send('Target.createTarget', { url: 'about:blank' });
  const { sessionId } = await cdp.send('Target.attachToTarget', { targetId: target.targetId, flatten: true });
  const send = (method, params) => cdp.send(method, params, sessionId);
  cdp.listeners.push(message => {
    if (message.method === 'Runtime.bindingCalled' && message.params.name === 'agentcfgTestCSP') {
      violations.add(message.params.payload);
    }
    if (message.method === 'Audits.issueAdded') {
      const issue = message.params.issue.details.contentSecurityPolicyIssueDetails;
      if (issue?.violatedDirective) violations.add(issue.violatedDirective.split(' ')[0]);
    }
    if (message.method === 'Log.entryAdded' && message.params.entry.source === 'security' && /form-action/.test(message.params.entry.text)) {
      violations.add('form-action');
    }
    if (message.method === 'Network.responseReceived') {
      const response = message.params.response;
      const target = new URL(response.url);
      const stage = target.origin === authorization.origin ? 'gateway' : target.origin === provider.origin ? 'provider' : target.origin === callback.origin ? 'client-callback' : 'other';
      if (result.responses.length < 20) result.responses.push({ stage, status: response.status });
      if (stage === 'gateway' && target.pathname === '/oauth/authorize' && response.status >= 400) rejectedForm = true;
    }
  });
  await send('Page.enable');
  await send('Runtime.enable');
  await send('Audits.enable');
  await send('Log.enable');
  await send('Network.enable');
  await send('Runtime.addBinding', { name: 'agentcfgTestCSP' });
  await send('Page.addScriptToEvaluateOnNewDocument', {
    source: "addEventListener('securitypolicyviolation',event=>agentcfgTestCSP(event.effectiveDirective));",
  });
  await send('Page.navigate', { url: input.url });
  while (Date.now() < deadline) {
    if (rejectedForm) throw new Error('gateway_rejected_form');
    if (violations.size) throw new Error('content_security_policy');
    const expression = `JSON.stringify({consent:!!document.querySelector('button[name="decision"][value="allow"]'),done:location.origin===${JSON.stringify(callback.origin)}&&location.pathname===${JSON.stringify(callback.pathname)}})`;
    let state;
    try {
      const evaluated = await send('Runtime.evaluate', { expression, returnByValue: true });
      state = JSON.parse(evaluated.result.value);
    } catch {
      // A successful navigation can replace the execution context mid-evaluation.
      await pause(25);
      continue;
    }
    if (state.done) {
      result.completed = true;
      break;
    }
    if (state.consent && !result.submitted) {
      result.submitted = true;
      await send('Runtime.evaluate', { expression: "document.querySelector('button[name=\"decision\"][value=\"allow\"]').click()" });
    }
    await pause(40);
  }
  if (!result.completed) throw new Error('navigation_timeout');
  if (violations.size) throw new Error('content_security_policy');
} catch (error) {
  // Only fixed driver categories are emitted, never navigation URLs or codes.
  const allowed = new Set(['browser_start_failed', 'browser_start_timeout', 'cdp_connection_timeout', 'cdp_connection_failed',
    'cdp_command_failed', 'cdp_closed', 'cdp_command_timeout', 'content_security_policy', 'navigation_timeout', 'gateway_rejected_form']);
  result.failure = allowed.has(error.message) ? error.message : 'browser_driver_failed';
  process.exitCode = 1;
} finally {
  result.cspViolations = [...violations];
  if (cdp) {
    await cdp.send('Browser.close').catch(() => {});
    cdp.socket.close();
  }
  if (browser?.pid) {
    const stopDeadline = Date.now() + 3000;
    while (browser.exitCode === null && browser.signalCode === null && Date.now() < stopDeadline) await pause(25);
    if (browser.exitCode === null && browser.signalCode === null) {
      if (process.platform === 'win32') {
        spawnSync(path.join(process.env.SystemRoot ?? 'C:\\Windows', 'System32', 'taskkill.exe'), ['/PID', String(browser.pid), '/T', '/F'], { windowsHide: true, stdio: 'ignore' });
      } else {
        browser.kill('SIGKILL');
      }
    }
  }
  process.stdout.write(JSON.stringify(result) + '\n');
}
