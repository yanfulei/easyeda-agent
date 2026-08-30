#!/usr/bin/env node

import { readFile } from 'node:fs/promises';
import { Server } from '@modelcontextprotocol/sdk/server/index.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import {
  CallToolRequestSchema,
  ListToolsRequestSchema,
} from '@modelcontextprotocol/sdk/types.js';
import {
  buildBlocksArgs,
  buildCallArgs,
  buildDocReloadArgs,
  buildManufacturingReleaseArgs,
  buildPcbCheckArgs,
  buildSchematicGateArgs,
  buildSchematicNetsArgs,
  buildSchematicReconcileArgs,
  buildSpecValidateArgs,
  buildWorkflowArgs,
  actionProcessTimeoutMs,
  actionRequiresProject,
  actionRequiresPinnedDocument,
  ConfirmationStore,
  DOMAIN_NAMES,
  filterActions,
  filterMcpActions,
  runEasyeda,
  toMcpResult,
  validateActionCatalog,
  validateActionInvocation,
} from './core.mjs';

const packageMetadata = JSON.parse(await readFile(new URL('../package.json', import.meta.url), 'utf8'));
if (!packageMetadata.version || typeof packageMetadata.version !== 'string') {
  throw new Error('mcp/package.json must contain a non-empty version');
}

const catalogExecution = await runEasyeda(['actions'], 30_000);
if (!catalogExecution.ok || !Array.isArray(catalogExecution.result)) {
  process.stderr.write(`easyeda-agent-mcp: cannot load action catalog: ${JSON.stringify(catalogExecution)}\n`);
  process.exit(1);
}
let catalog;
try {
  catalog = validateActionCatalog(catalogExecution.result);
}
catch (error) {
  process.stderr.write(`easyeda-agent-mcp: incompatible easyeda CLI action catalog: ${error.message}. Update the CLI and restart the MCP client.\n`);
  process.exit(1);
}
const actions = filterMcpActions(catalog);
const byName = new Map(actions.map((action) => [action.name, action]));
const confirmationStore = new ConfirmationStore();

const server = new Server(
  { name: 'easyeda-agent-mcp', version: packageMetadata.version },
  {
    capabilities: { tools: {} },
    instructions: [
      'Control EasyEDA Pro through easyeda-agent.',
      'Every schematic/PCB action and mutation requires project and doc; foreground context changes require project.',
      'Actions marked needsConfirm use the explicit two-stage confirmation tool.',
      'Inspect before editing and run schematic/PCB checks plus native DRC after editing.',
      'Do not bypass workflow gates or use force-unsafe in real projects.',
    ].join(' '),
  },
);

const commonRouteProperties = {
  project: {
    type: 'string',
    description: 'EasyEDA project name or UUID. Required for normal project work.',
  },
  doc: {
    type: 'string',
    description: 'Target schematic page or PCB name/UUID. Required for every schematic/PCB action and every document mutation.',
  },
  window: {
    type: 'string',
    description: 'Explicit connector windowId; use only to resolve genuine multi-window ambiguity.',
  },
  payload: {
    type: 'object',
    description: 'Typed action payload. Use easyeda_actions to inspect the action inputs.',
    additionalProperties: true,
  },
};

const pinnedDocumentProperties = {
  project: commonRouteProperties.project,
  doc: commonRouteProperties.doc,
  window: commonRouteProperties.window,
};

function domainTool(domain) {
  // Confirmation-gated actions remain discoverable through easyeda_actions,
  // but are absent from ordinary domain enums and only appear on the explicit
  // two-stage tool below.
  const domainActions = actions.filter((action) => action.domain === domain && !action.needsConfirm);
  const pinnedActionNames = domainActions.filter(actionRequiresPinnedDocument).map((action) => action.name);
  const projectActionNames = domainActions.filter(actionRequiresProject).map((action) => action.name);
  const everyActionPinned = pinnedActionNames.length === domainActions.length && domainActions.length > 0;
  const everyActionProject = projectActionNames.length === domainActions.length && domainActions.length > 0;
  const required = ['action'];
  if (everyActionProject) required.push('project');
  if (everyActionPinned) required.push('doc');
  const routeConditions = [];
  if (!everyActionProject && projectActionNames.length > 0) {
    routeConditions.push({
      if: { properties: { action: { enum: projectActionNames } }, required: ['action'] },
      then: { required: ['project'] },
    });
  }
  if (!everyActionPinned && pinnedActionNames.length > 0) {
    routeConditions.push({
      if: { properties: { action: { enum: pinnedActionNames } }, required: ['action'] },
      then: { required: ['project', 'doc'] },
    });
  }
  const conditionalRoute = routeConditions.length > 0 ? { allOf: routeConditions } : {};
  return {
    name: `easyeda_${domain}`,
    title: `EasyEDA ${domain}`,
    description: `Run one typed ${domain} action through easyeda-agent. Use easyeda_actions for input guidance.`,
    inputSchema: {
      type: 'object',
      properties: {
        action: {
          type: 'string',
          enum: domainActions.map((action) => action.name),
          description: 'Exact typed action name.',
        },
        ...commonRouteProperties,
      },
      required,
      ...conditionalRoute,
      additionalProperties: false,
    },
    annotations: {
      readOnlyHint: domainActions.every((action) => !action.mutates),
      destructiveHint: domainActions.some((action) => action.mutates),
      idempotentHint: false,
      openWorldHint: false,
    },
  };
}

const tools = [
  {
    name: 'easyeda_health',
    title: 'EasyEDA connection health',
    description: 'Check the local daemon and connected EasyEDA Pro windows.',
    inputSchema: { type: 'object', properties: {}, additionalProperties: false },
    annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
  },
  {
    name: 'easyeda_confirmed_action',
    title: 'Confirm an EasyEDA action',
    description: 'Prepare, then execute, one catalog action marked needsConfirm. Tokens are short-lived, single-use, and bound to the exact action, target, window, and payload.',
    inputSchema: {
      type: 'object',
      properties: {
        operation: { type: 'string', enum: ['prepare', 'execute'] },
        action: {
          type: 'string',
          enum: actions.filter((action) => action.needsConfirm).map((action) => action.name),
          description: 'Exact typed action name marked needsConfirm in the catalog.',
        },
        ...commonRouteProperties,
        confirmationToken: { type: 'string', description: 'Token returned by prepare; required for execute.' },
      },
      required: ['operation', 'action', 'project', 'doc'],
      additionalProperties: false,
    },
    annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: false },
  },
  {
    name: 'easyeda_actions',
    title: 'Discover EasyEDA actions',
    description: `Search the ${actions.length} typed EasyEDA actions and inspect inputs, mutation flags, and confirmation requirements.`,
    inputSchema: {
      type: 'object',
      properties: {
        domain: { type: 'string', enum: DOMAIN_NAMES },
        search: { type: 'string' },
        mutates: { type: 'boolean' },
      },
      additionalProperties: false,
    },
    annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
  },
  ...DOMAIN_NAMES.map(domainTool),
  {
    name: 'easyeda_blocks',
    title: 'EasyEDA circuit blocks',
    description: 'List, search, or show an embedded proven circuit block. Does not require a running daemon.',
    inputSchema: {
      type: 'object',
      properties: {
        operation: { type: 'string', enum: ['list', 'search', 'show'] },
        query: { type: 'string', description: 'Required for search.' },
        id: { type: 'string', description: 'Required for show.' },
      },
      required: ['operation'],
      additionalProperties: false,
    },
    annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
  },
  {
    name: 'easyeda_workflow',
    title: 'EasyEDA guarded workflow',
    description: 'Inspect or advance the persisted project design-flow state machine.',
    inputSchema: {
      type: 'object',
      properties: {
        operation: { type: 'string', enum: ['init', 'status', 'advance', 'confirm', 'reset'] },
        project: { type: 'string' },
        doc: { type: 'string' },
        reconcile: { type: 'boolean', description: 'For status: reconcile persisted state with the live document.' },
        minScore: { type: 'integer', minimum: 0, maximum: 100, description: 'For advance: minimum routability score.' },
        maxCrossings: { type: 'integer', minimum: -1, description: 'For advance: maximum ratline crossings; -1 is unlimited.' },
        confirmation: { type: 'string', enum: ['layout', 'outline'], description: 'Required for confirm.' },
        note: { type: 'string', description: 'Human review note recorded by confirm.' },
        resetAll: { type: 'boolean', description: 'For reset: clear every confirmation.' },
        resetFrom: { type: 'string', description: 'For reset: first stage to clear, inclusive.' },
      },
      required: ['operation', 'project'],
      allOf: [
        {
          if: { properties: { operation: { enum: ['advance', 'confirm'] } }, required: ['operation'] },
          then: { required: ['doc'] },
        },
        {
          if: {
            properties: { operation: { const: 'status' }, reconcile: { const: true } },
            required: ['operation', 'reconcile'],
          },
          then: { required: ['doc'] },
        },
      ],
      additionalProperties: false,
    },
    annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: false },
  },
  {
    name: 'easyeda_spec_validate',
    title: 'Validate EasyEDA S0 specification',
    description: 'Validate one S0 design specification with structured output. Strict mode is on by default.',
    inputSchema: {
      type: 'object',
      properties: {
        path: { type: 'string', description: 'Path to the S0 specification JSON.' },
        strict: { type: 'boolean', description: 'Fail on warnings as well as errors (default true).' },
      },
      required: ['path'],
      additionalProperties: false,
    },
    annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
  },
  {
    name: 'easyeda_schematic_gate',
    title: 'Run strict schematic gate',
    description: 'Run layout-lint, schematic checks, bridge-check, and native DRC in the fixed strict gate order.',
    inputSchema: {
      type: 'object',
      properties: { ...pinnedDocumentProperties, failFast: { type: 'boolean' } },
      required: ['project', 'doc'],
      additionalProperties: false,
    },
    annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
  },
  {
    name: 'easyeda_schematic_nets',
    title: 'Audit schematic nets',
    description: 'Run the strict cross-page net-name and single-pin-net audit.',
    inputSchema: {
      type: 'object',
      properties: { ...pinnedDocumentProperties, includeAll: { type: 'boolean', description: 'Include every net in the report.' } },
      required: ['project', 'doc'],
      additionalProperties: false,
    },
    annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
  },
  {
    name: 'easyeda_schematic_reconcile',
    title: 'Reconcile schematic intent',
    description: 'Mechanically reconcile block-library intent against the live schematic netlist.',
    inputSchema: {
      type: 'object',
      properties: pinnedDocumentProperties,
      required: ['project', 'doc'],
      additionalProperties: false,
    },
    annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
  },
  {
    name: 'easyeda_doc_reload',
    title: 'Save and reload EasyEDA document',
    description: 'Save, close, reopen, and settle one explicitly pinned EasyEDA document.',
    inputSchema: {
      type: 'object',
      properties: pinnedDocumentProperties,
      required: ['project', 'doc'],
      additionalProperties: false,
    },
    annotations: { readOnlyHint: false, destructiveHint: true, idempotentHint: false, openWorldHint: false },
  },
  {
    name: 'easyeda_pcb_check',
    title: 'Run strict PCB check',
    description: 'Run the reconstructed strict PCB DFM/reliability check with structured output.',
    inputSchema: {
      type: 'object',
      properties: {
        ...pinnedDocumentProperties,
        spec: { type: 'string', description: 'Optional S0 specification path used by intent-aware checks.' },
        couplingW: { type: 'number', exclusiveMinimum: 0, maximum: 20 },
      },
      required: ['project', 'doc'],
      additionalProperties: false,
    },
    annotations: { readOnlyHint: true, destructiveHint: false, idempotentHint: true, openWorldHint: false },
  },
  {
    name: 'easyeda_manufacturing_release',
    title: 'Build checked manufacturing release',
    description: 'Save/reload one PCB, require strict PCB check and native DRC to pass, validate Gerber/BOM/CPL, reconcile designators, and atomically write a SHA256 manifest. Never places an order.',
    inputSchema: {
      type: 'object',
      properties: {
        ...pinnedDocumentProperties,
        outputDir: { type: 'string', description: 'New directory to publish; existing directories are refused.' },
        spec: { type: 'string', description: 'Approved S0 specification path; strict validation is always required.' },
        drcTimeoutSeconds: { type: 'integer', minimum: 10, maximum: 600, description: 'Native DRC timeout (default 180).' },
      },
      required: ['project', 'doc', 'spec'],
      additionalProperties: false,
    },
    annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: false, openWorldHint: false },
  },
];

server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools }));

server.setRequestHandler(CallToolRequestSchema, async (request) => {
  const { name, arguments: input = {} } = request.params;
  try {
    if (name === 'easyeda_health') {
      return toMcpResult(await runEasyeda(['daemon', 'health'], 30_000));
    }
    if (name === 'easyeda_actions') {
      const filtered = filterActions(actions, input);
      return toMcpResult({ ok: true, result: { count: filtered.length, actions: filtered } });
    }
    if (name === 'easyeda_confirmed_action') {
      const action = byName.get(input.action);
      if (!action?.needsConfirm) throw new Error(`action ${input.action || '(missing)'} is not confirmable`);
      if (input.operation === 'prepare') {
        return toMcpResult({ ok: true, result: confirmationStore.prepare(action, input) });
      }
      if (input.operation === 'execute') {
        const args = buildCallArgs(action, input);
        const timeoutMs = actionProcessTimeoutMs(action);
        confirmationStore.consume(input.confirmationToken, action, input);
        return toMcpResult(await runEasyeda(args, timeoutMs));
      }
      throw new Error(`unsupported confirmation operation: ${input.operation}`);
    }
    if (name === 'easyeda_blocks') {
      return toMcpResult(await runEasyeda(buildBlocksArgs(input), 30_000));
    }
    if (name === 'easyeda_workflow') {
      return toMcpResult(await runEasyeda(buildWorkflowArgs(input)));
    }
    if (name === 'easyeda_spec_validate') {
      return toMcpResult(await runEasyeda(buildSpecValidateArgs(input), 30_000));
    }
    if (name === 'easyeda_schematic_gate') {
      return toMcpResult(await runEasyeda(buildSchematicGateArgs(input)));
    }
    if (name === 'easyeda_schematic_nets') {
      return toMcpResult(await runEasyeda(buildSchematicNetsArgs(input)));
    }
    if (name === 'easyeda_schematic_reconcile') {
      return toMcpResult(await runEasyeda(buildSchematicReconcileArgs(input)));
    }
    if (name === 'easyeda_doc_reload') {
      return toMcpResult(await runEasyeda(buildDocReloadArgs(input)));
    }
    if (name === 'easyeda_pcb_check') {
      return toMcpResult(await runEasyeda(buildPcbCheckArgs(input)));
    }
    if (name === 'easyeda_manufacturing_release') {
      return toMcpResult(await runEasyeda(buildManufacturingReleaseArgs(input), 900_000));
    }
    if (name.startsWith('easyeda_')) {
      const domain = name.slice('easyeda_'.length);
      if (!DOMAIN_NAMES.includes(domain)) throw new Error(`unknown EasyEDA domain tool: ${name}`);
      const action = byName.get(input.action);
      if (!action || action.domain !== domain) {
        throw new Error(`action ${input.action || '(missing)'} does not belong to domain ${domain}`);
      }
      validateActionInvocation(action, input);
      return toMcpResult(await runEasyeda(buildCallArgs(action, input), actionProcessTimeoutMs(action)));
    }
    throw new Error(`unknown tool: ${name}`);
  }
  catch (error) {
    return toMcpResult({ ok: false, error: { message: error.message } });
  }
});

await server.connect(new StdioServerTransport());
