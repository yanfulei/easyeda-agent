import { execFile } from 'node:child_process';
import { createHash, randomUUID } from 'node:crypto';
import { promisify } from 'node:util';

const execFileAsync = promisify(execFile);

export const DOMAIN_NAMES = [
  'artifact',
  'board',
  'document',
  'pcb',
  'project',
  'schematic',
  'system',
];

const FORBIDDEN_ACTION_TOKENS = new Set([
  'checkout',
  'order',
  'orders',
  'payment',
  'payments',
  'submit',
  'submitted',
  'submission',
]);

const CONFIRMATION_TTL_MS = 2 * 60_000;

export function easyedaBinary() {
  return process.env.EASYEDA_BIN || 'easyeda';
}

export async function runEasyeda(args, timeoutMs = 300_000) {
  try {
    const { stdout, stderr } = await execFileAsync(easyedaBinary(), args, {
      encoding: 'utf8',
      maxBuffer: 32 * 1024 * 1024,
      timeout: timeoutMs,
    });
    return {
      ok: true,
      result: parseOutput(stdout),
      stderr: stderr.trim() || undefined,
    };
  }
  catch (error) {
    return {
      ok: false,
      error: {
        message: error.message,
        code: error.code ?? null,
        stdout: parseOutput(error.stdout || ''),
        stderr: String(error.stderr || '').trim() || undefined,
      },
    };
  }
}

export function parseOutput(stdout) {
  const text = String(stdout).trim();
  if (!text) return null;
  try {
    return JSON.parse(text);
  }
  catch {
    return text;
  }
}

export function filterActions(actions, { domain, search, mutates } = {}) {
  const needle = String(search || '').trim().toLowerCase();
  return actions.filter((action) => {
    if (domain && action.domain !== domain) return false;
    if (typeof mutates === 'boolean' && action.mutates !== mutates) return false;
    if (!needle) return true;
    return [action.name, action.description, ...(action.inputs || [])]
      .join(' ')
      .toLowerCase()
      .includes(needle);
  });
}

function actionNameTokens(name) {
  return String(name || '')
    .replace(/([a-z0-9])([A-Z])/g, '$1.$2')
    .toLowerCase()
    .split(/[^a-z0-9]+/)
    .filter(Boolean);
}

// Keep the generic MCP surface catalog-driven, with only a narrow permanent
// deny rule for capabilities that must never be reachable from an agent.
export function isMcpActionAllowed(action) {
  if (!action || !DOMAIN_NAMES.includes(action.domain)) return false;
  if (action.name === 'debug.exec_js' || action.domain === 'debug') return false;
  return !actionNameTokens(action.name).some((token) => FORBIDDEN_ACTION_TOKENS.has(token));
}

export function filterMcpActions(actions) {
  return actions.filter(isMcpActionAllowed);
}

export function validateActionCatalog(actions) {
  if (!Array.isArray(actions)) throw new Error('action catalog must be an array');
  const seen = new Set();
  for (const action of actions) {
    const name = String(action?.name || '').trim();
    if (!name) throw new Error('action catalog contains an entry without a name');
    if (seen.has(name)) throw new Error(`action catalog contains duplicate name ${name}`);
    seen.add(name);
    if (!DOMAIN_NAMES.includes(action.domain) && action.domain !== 'debug') {
      throw new Error(`action ${name} has unsupported domain ${action.domain || '(missing)'}`);
    }
    for (const field of ['mutates', 'changesContext', 'needsWindow', 'needsConfirm']) {
      if (typeof action[field] !== 'boolean') {
        throw new Error(`action ${name} has invalid ${field}`);
      }
    }
    actionTimeoutMs(action);
  }
  return actions;
}

export function actionDocumentType(action) {
  const name = String(action?.name || '');
  if (name.startsWith('pcb.')) return 'pcb';
  if (name.startsWith('schematic.')) return 'schematic';
  return '';
}

export function actionRequiresPinnedDocument(action) {
  return Boolean(action?.mutates) || Boolean(actionDocumentType(action));
}

export function actionRequiresProject(action) {
  return actionRequiresPinnedDocument(action) || Boolean(action?.changesContext);
}

export function validateActionInvocation(action, input = {}, { confirmed = false } = {}) {
  if (!isMcpActionAllowed(action)) {
    throw new Error(`action ${action?.name || '(missing)'} is not exposed through MCP`);
  }
  if (actionRequiresPinnedDocument(action) && (!String(input.project || '').trim() || !String(input.doc || '').trim())) {
    throw new Error(`action ${action.name} requires both project and doc for an exact document binding`);
  }
  if (actionRequiresProject(action) && !String(input.project || '').trim()) {
    throw new Error(`action ${action.name} changes editor context and requires project for stable routing`);
  }
  if (action.needsConfirm && !confirmed) {
    throw new Error(`action ${action.name} requires explicit two-stage confirmation via easyeda_confirmed_action`);
  }
}

export function actionTimeoutMs(action) {
  const timeoutMs = action?.timeoutMs;
  if (!Number.isSafeInteger(timeoutMs) || timeoutMs <= 0) {
    throw new Error(`action ${action?.name || '(missing)'} has invalid catalog timeoutMs`);
  }
  return timeoutMs;
}

export function actionProcessTimeoutMs(action) {
  return actionTimeoutMs(action) + 30_000;
}

export function buildCallArgs(action, input = {}) {
  if (!action?.name) throw new Error('action catalog entry is required');
  const args = [];
  if (input.project) args.push('--project', input.project);
  if (input.doc) args.push('--doc', input.doc);
  args.push('call', action.name, '--timeout', `${actionTimeoutMs(action)}ms`);
  if (input.payload && Object.keys(input.payload).length > 0) {
    args.push('--payload', JSON.stringify(input.payload));
  }
  if (input.window) args.push('--window', input.window);
  return args;
}

function canonicalize(value) {
  if (Array.isArray(value)) return value.map(canonicalize);
  if (value && typeof value === 'object') {
    return Object.fromEntries(Object.keys(value)
      .filter((key) => value[key] !== undefined)
      .sort()
      .map((key) => [key, canonicalize(value[key])]));
  }
  return value;
}

function sha256(value) {
  return createHash('sha256').update(JSON.stringify(canonicalize(value))).digest('hex');
}

function confirmationBinding(action, input) {
  return {
    action: action.name,
    target: {
      project: String(input.project || '').trim(),
      document: String(input.doc || '').trim(),
      documentType: actionDocumentType(action),
      window: String(input.window || '').trim(),
    },
    payload: input.payload || {},
  };
}

export class ConfirmationStore {
  constructor({ ttlMs = CONFIRMATION_TTL_MS, now = Date.now, tokenFactory = randomUUID } = {}) {
    this.ttlMs = ttlMs;
    this.now = now;
    this.tokenFactory = tokenFactory;
    this.entries = new Map();
  }

  prepare(action, input = {}) {
    validateActionInvocation(action, input, { confirmed: true });
    if (!action.needsConfirm) throw new Error(`action ${action.name} does not require confirmation`);
    this.prune();
    const issuedAtMs = this.now();
    const expiresAtMs = issuedAtMs + this.ttlMs;
    const binding = confirmationBinding(action, input);
    const requestSha256 = sha256(binding);
    const token = this.tokenFactory();
    this.entries.set(token, { requestSha256, expiresAtMs });
    return {
      confirmationToken: token,
      action: action.name,
      target: binding.target,
      payload: canonicalize(binding.payload),
      payloadSha256: sha256(binding.payload),
      requestSha256,
      expiresAt: new Date(expiresAtMs).toISOString(),
      singleUse: true,
    };
  }

  consume(token, action, input = {}) {
    validateActionInvocation(action, input, { confirmed: true });
    if (!action.needsConfirm) throw new Error(`action ${action.name} does not require confirmation`);
    const key = String(token || '').trim();
    if (!key) throw new Error('confirmationToken is required for execute');
    const entry = this.entries.get(key);
    if (!entry) throw new Error('confirmation token is unknown, expired, or already consumed');
    if (this.now() >= entry.expiresAtMs) {
      this.entries.delete(key);
      throw new Error('confirmation token expired; prepare the action again');
    }
    if (entry.requestSha256 !== sha256(confirmationBinding(action, input))) {
      throw new Error('confirmation token does not match this action, target, window, and payload');
    }
    // Consume before dispatch so an ambiguous CLI failure can never be retried
    // into a duplicate destructive mutation with the same approval.
    this.entries.delete(key);
  }

  prune() {
    const now = this.now();
    for (const [token, entry] of this.entries) {
      if (now >= entry.expiresAtMs) this.entries.delete(token);
    }
  }
}

export function buildWorkflowArgs(input) {
  const project = requireText(input, 'project');
  const needsLiveDocument = ['advance', 'confirm'].includes(input.operation)
    || (input.operation === 'status' && input.reconcile === true);
  const doc = needsLiveDocument ? requireText(input, 'doc') : String(input.doc || '').trim();
  const args = ['--project', project];
  if (doc) args.push('--doc', doc);
  args.push('workflow', input.operation);
  switch (input.operation) {
    case 'init':
      break;
    case 'status':
      args.push('--json');
      if (input.reconcile) args.push('--reconcile');
      break;
    case 'advance':
      if (Number.isInteger(input.minScore)) args.push('--min-score', String(input.minScore));
      if (Number.isInteger(input.maxCrossings)) args.push('--max-crossings', String(input.maxCrossings));
      break;
    case 'confirm':
      if (!['layout', 'outline'].includes(input.confirmation)) {
        throw new Error('confirmation must be layout or outline');
      }
      args.push(input.confirmation);
      if (input.note) args.push('--note', input.note);
      break;
    case 'reset':
      if (input.resetAll === true) args.push('--all');
      else if (input.resetFrom) args.push('--from', input.resetFrom);
      else throw new Error('reset requires resetAll=true or resetFrom');
      break;
    default:
      throw new Error(`unsupported workflow operation: ${input.operation}`);
  }
  return args;
}

export function buildBlocksArgs(input) {
  switch (input.operation) {
    case 'list':
      return ['blocks', 'ls'];
    case 'search':
      if (!input.query) throw new Error('query is required for blocks search');
      return ['blocks', 'search', input.query];
    case 'show':
      if (!input.id) throw new Error('id is required for blocks show');
      return ['blocks', 'show', input.id];
    default:
      throw new Error(`unsupported blocks operation: ${input.operation}`);
  }
}

function requireText(input, field) {
  const value = String(input?.[field] ?? '').trim();
  if (!value) throw new Error(`${field} is required`);
  return value;
}

function routedPrefix(input, { doc = true } = {}) {
  const args = ['--project', requireText(input, 'project')];
  if (doc) args.push('--doc', requireText(input, 'doc'));
  return args;
}

function appendWindow(args, input) {
  if (input.window) args.push('--window', String(input.window));
  return args;
}

// Composite builders deliberately return argv arrays for execFile. They never
// accept a command string or arbitrary argv, keeping the MCP surface narrower
// than the generic typed-action tools.
export function buildSpecValidateArgs(input) {
  const args = ['spec', 'validate', requireText(input, 'path'), '--json'];
  if (input.strict !== false) args.push('--strict');
  return args;
}

export function buildSchematicGateArgs(input) {
  const args = routedPrefix(input);
  args.push('sch', 'gate', '--strict', '--json');
  if (input.failFast === true) args.push('--fail-fast');
  return appendWindow(args, input);
}

export function buildSchematicNetsArgs(input) {
  const args = routedPrefix(input);
  args.push('sch', 'nets', '--strict', '--json');
  if (input.includeAll === true) args.push('--all');
  return appendWindow(args, input);
}

export function buildSchematicReconcileArgs(input) {
  const args = routedPrefix(input);
  args.push('sch', 'reconcile', '--json');
  return appendWindow(args, input);
}

export function buildDocReloadArgs(input) {
  const project = requireText(input, 'project');
  const doc = requireText(input, 'doc');
  return appendWindow(['--project', project, 'doc', 'reload', doc, '--json'], input);
}

export function buildPcbCheckArgs(input) {
  const args = routedPrefix(input);
  args.push('pcb', 'check', '--strict', '--json');
  if (input.spec) args.push('--spec', String(input.spec));
  if (input.couplingW !== undefined) args.push('--coupling-w', String(input.couplingW));
  return appendWindow(args, input);
}

export function buildManufacturingReleaseArgs(input) {
  const args = routedPrefix(input);
  const spec = requireText(input, 'spec');
  args.push('manufacturing', 'release-bundle', '--spec', spec);
  if (input.outputDir) args.push('--out-dir', String(input.outputDir));
  if (input.drcTimeoutSeconds !== undefined) {
    const timeout = input.drcTimeoutSeconds;
    if (!Number.isInteger(timeout) || timeout < 10 || timeout > 600) {
      throw new Error('drcTimeoutSeconds must be an integer from 10 to 600');
    }
    args.push('--drc-timeout', String(timeout));
  }
  return appendWindow(args, input);
}

export function toMcpResult(execution) {
  const rawValue = execution.ok ? execution.result : execution.error;
  const value = execution.ok && execution.stderr
    ? { result: rawValue, warnings: execution.stderr }
    : rawValue;
  const result = {
    content: [{ type: 'text', text: JSON.stringify(value, null, 2) }],
    isError: !execution.ok,
  };
  if (execution.ok && value && typeof value === 'object' && !Array.isArray(value)) {
    result.structuredContent = value;
  }
  return result;
}
