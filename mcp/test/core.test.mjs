import test from 'node:test';
import assert from 'node:assert/strict';
import {
  actionDocumentType,
  actionProcessTimeoutMs,
  actionRequiresProject,
  actionRequiresPinnedDocument,
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
  ConfirmationStore,
  filterActions,
  filterMcpActions,
  isMcpActionAllowed,
  parseOutput,
  toMcpResult,
  validateActionCatalog,
  validateActionInvocation,
} from '../src/core.mjs';

test('buildCallArgs pins project/doc and keeps payload structured', () => {
  const action = { name: 'pcb.drc.run', domain: 'pcb', timeoutMs: 120_000 };
  assert.deepEqual(
    buildCallArgs(action, {
      project: 'Motor',
      doc: 'PCB1',
      window: 'win-1',
      payload: { rebuild: true },
    }),
    ['--project', 'Motor', '--doc', 'PCB1', 'call', 'pcb.drc.run', '--timeout', '120000ms', '--payload', '{"rebuild":true}', '--window', 'win-1'],
  );
  assert.equal(actionProcessTimeoutMs(action), 150_000);
  assert.throws(() => buildCallArgs({ name: 'pcb.drc.run', domain: 'pcb' }), /invalid catalog timeoutMs/);
});

test('filterActions applies domain, mutation, and text filters', () => {
  const actions = [
    { name: 'pcb.drc.run', domain: 'pcb', mutates: false, description: 'native check', inputs: [] },
    { name: 'pcb.track.create', domain: 'pcb', mutates: true, description: 'route copper', inputs: ['net'] },
    { name: 'schematic.check', domain: 'schematic', mutates: false, description: 'lint', inputs: [] },
  ];
  assert.deepEqual(filterActions(actions, { domain: 'pcb', mutates: true, search: 'copper' }), [actions[1]]);
});

test('MCP action policy blocks raw code and fabrication submission families', () => {
  const safe = { name: 'pcb.export.gerber', domain: 'pcb', mutates: false };
  const actions = [
    safe,
    { name: 'debug.exec_js', domain: 'debug', mutates: true },
    { name: 'pcb.order.submit', domain: 'pcb', mutates: true },
    { name: 'pcb.submitOrder', domain: 'pcb', mutates: true },
    { name: 'artifact.checkout.create', domain: 'artifact', mutates: true },
    { name: 'project.payment_status', domain: 'project', mutates: false },
  ];
  assert.deepEqual(filterMcpActions(actions), [safe]);
  assert.equal(isMcpActionAllowed(safe), true);
  assert.equal(isMcpActionAllowed(actions[1]), false);
});

test('action catalog validation fails at startup on stale or ambiguous CLI contracts', () => {
  const valid = {
    name: 'schematic.components.list', domain: 'schematic', mutates: false,
    changesContext: false, needsWindow: true, needsConfirm: false, timeoutMs: 60_000,
  };
  assert.deepEqual(validateActionCatalog([valid]), [valid]);
  assert.throws(() => validateActionCatalog([{ ...valid, timeoutMs: undefined }]), /invalid catalog timeoutMs/);
  assert.throws(() => validateActionCatalog([valid, { ...valid }]), /duplicate name/);
  assert.throws(() => validateActionCatalog([{ ...valid, needsWindow: undefined }]), /invalid needsWindow/);
  assert.throws(() => validateActionCatalog([{ ...valid, domain: 'unknown' }]), /unsupported domain/);
});

test('every schematic/PCB action and every mutation requires an exact document route', () => {
  const pcbRead = { name: 'pcb.export.gerber', domain: 'artifact', mutates: false };
  const schematicRead = { name: 'schematic.components.list', domain: 'schematic', mutates: false };
  const schematicWrite = { name: 'schematic.component.place', domain: 'schematic', mutates: true };
  assert.equal(actionRequiresPinnedDocument(pcbRead), true);
  assert.equal(actionDocumentType(pcbRead), 'pcb');
  assert.equal(actionRequiresPinnedDocument(schematicRead), true);
  assert.equal(actionRequiresPinnedDocument(schematicWrite), true);
  assert.throws(() => validateActionInvocation(pcbRead, {}), /requires both project and doc/);
  assert.throws(() => validateActionInvocation(pcbRead, { project: 'P' }), /requires both project and doc/);
  assert.doesNotThrow(() => validateActionInvocation(pcbRead, { project: 'P', doc: 'PCB1' }));
  assert.throws(() => validateActionInvocation(schematicRead, {}), /requires both project and doc/);
  assert.doesNotThrow(() => validateActionInvocation(schematicRead, { project: 'P', doc: 'P1' }));
});

test('foreground context actions require stable project routing without pretending to mutate', () => {
  for (const name of ['document.open', 'document.close', 'schematic.page.open']) {
    const action = {
      name, domain: name.startsWith('schematic.') ? 'schematic' : 'document',
      mutates: false, changesContext: true,
    };
    assert.equal(actionRequiresProject(action), true);
    if (name.startsWith('document.')) {
      assert.equal(actionRequiresPinnedDocument(action), false);
    }
    assert.throws(() => validateActionInvocation(action, {}), /requires (project|both project and doc)/);
    const routed = { project: 'Probe' };
    if (name.startsWith('schematic.')) routed.doc = 'POWER_PROBE';
    assert.doesNotThrow(() => validateActionInvocation(action, routed));
  }
  const diagnostic = { name: 'document.current', domain: 'document', mutates: false, changesContext: false };
  assert.equal(actionRequiresProject(diagnostic), false);
  assert.doesNotThrow(() => validateActionInvocation(diagnostic, {}));
});

test('needsConfirm actions require a matching short-lived single-use token', () => {
  let now = Date.parse('2026-08-30T00:00:00Z');
  const store = new ConfirmationStore({
    ttlMs: 1_000,
    now: () => now,
    tokenFactory: () => 'confirmation-1',
  });
  const action = {
    name: 'pcb.route.delete', domain: 'pcb', mutates: true, needsConfirm: true, timeoutMs: 60_000,
  };
  const input = {
    project: 'P', doc: 'PCB1', window: 'win-1', payload: { primitiveIds: ['p2', 'p1'], kind: 'track' },
  };

  assert.throws(() => validateActionInvocation(action, input), /two-stage confirmation/);
  const prepared = store.prepare(action, input);
  assert.equal(prepared.confirmationToken, 'confirmation-1');
  assert.equal(prepared.target.documentType, 'pcb');
  assert.deepEqual(prepared.payload, { kind: 'track', primitiveIds: ['p2', 'p1'] });
  assert.equal(prepared.singleUse, true);
  assert.throws(() => store.consume('confirmation-1', action, {
    ...input, payload: { primitiveIds: ['p1'], kind: 'track' },
  }), /does not match/);
  assert.doesNotThrow(() => store.consume('confirmation-1', action, {
    ...input, payload: { kind: 'track', primitiveIds: ['p2', 'p1'] },
  }));
  assert.throws(() => store.consume('confirmation-1', action, input), /already consumed/);

  const expiring = new ConfirmationStore({ ttlMs: 1_000, now: () => now, tokenFactory: () => 'confirmation-2' });
  expiring.prepare(action, input);
  now += 1_000;
  assert.throws(() => expiring.consume('confirmation-2', action, input), /expired/);
});

test('workflow and blocks arguments are shell-free arrays', () => {
  assert.deepEqual(buildWorkflowArgs({ project: 'P', doc: 'PCB1', operation: 'status', reconcile: true }),
    ['--project', 'P', '--doc', 'PCB1', 'workflow', 'status', '--json', '--reconcile']);
  assert.deepEqual(buildWorkflowArgs({ project: 'P', doc: 'PCB1', operation: 'confirm', confirmation: 'layout', note: 'reviewed' }),
    ['--project', 'P', '--doc', 'PCB1', 'workflow', 'confirm', 'layout', '--note', 'reviewed']);
  assert.throws(() => buildWorkflowArgs({ project: 'P', operation: 'advance' }), /doc is required/);
  assert.throws(() => buildWorkflowArgs({ project: 'P', operation: 'confirm', confirmation: 'layout' }), /doc is required/);
  assert.throws(() => buildWorkflowArgs({ project: 'P', operation: 'status', reconcile: true }), /doc is required/);
  assert.deepEqual(buildWorkflowArgs({ project: 'P', operation: 'status' }),
    ['--project', 'P', 'workflow', 'status', '--json']);
  assert.throws(() => buildWorkflowArgs({ project: 'P', operation: 'reset' }), /reset requires/);
  assert.deepEqual(buildBlocksArgs({ operation: 'search', query: 'usb serial' }), ['blocks', 'search', 'usb serial']);
});

test('parseOutput and MCP result preserve structured JSON', () => {
  assert.deepEqual(parseOutput('{"ok":true}'), { ok: true });
  assert.equal(parseOutput('plain'), 'plain');
  const result = toMcpResult({ ok: true, result: { passed: true } });
  assert.deepEqual(result.structuredContent, { passed: true });
  assert.equal(result.isError, false);

  const warned = toMcpResult({ ok: true, result: { passed: true }, stderr: 'staleRisk' });
  assert.deepEqual(warned.structuredContent, { result: { passed: true }, warnings: 'staleRisk' });
});

test('composite tools build fixed shell-free argv', () => {
  const route = { project: 'Power; echo no', doc: 'PCB 1', window: 'win-1' };
  assert.deepEqual(buildSpecValidateArgs({ path: 'spec dir/s0.json' }),
    ['spec', 'validate', 'spec dir/s0.json', '--json', '--strict']);
  assert.deepEqual(buildSpecValidateArgs({ path: 's0.json', strict: false }),
    ['spec', 'validate', 's0.json', '--json']);
  assert.deepEqual(buildSchematicGateArgs({ ...route, failFast: true }),
    ['--project', 'Power; echo no', '--doc', 'PCB 1', 'sch', 'gate', '--strict', '--json', '--fail-fast', '--window', 'win-1']);
  assert.deepEqual(buildSchematicNetsArgs({ ...route, includeAll: true }),
    ['--project', 'Power; echo no', '--doc', 'PCB 1', 'sch', 'nets', '--strict', '--json', '--all', '--window', 'win-1']);
  assert.deepEqual(buildSchematicReconcileArgs(route),
    ['--project', 'Power; echo no', '--doc', 'PCB 1', 'sch', 'reconcile', '--json', '--window', 'win-1']);
  assert.deepEqual(buildDocReloadArgs(route),
    ['--project', 'Power; echo no', 'doc', 'reload', 'PCB 1', '--json', '--window', 'win-1']);
  assert.deepEqual(buildPcbCheckArgs({ ...route, spec: 's0.json', couplingW: 3 }),
    ['--project', 'Power; echo no', '--doc', 'PCB 1', 'pcb', 'check', '--strict', '--json', '--spec', 's0.json', '--coupling-w', '3', '--window', 'win-1']);
  assert.deepEqual(buildManufacturingReleaseArgs({ ...route, outputDir: 'release dir', spec: 's0.json', drcTimeoutSeconds: 240 }),
    ['--project', 'Power; echo no', '--doc', 'PCB 1', 'manufacturing', 'release-bundle', '--spec', 's0.json', '--out-dir', 'release dir', '--drc-timeout', '240', '--window', 'win-1']);
});

test('composite builders reject missing route fields and invalid DRC timeout', () => {
  assert.throws(() => buildSchematicGateArgs({ project: 'P' }), /doc is required/);
  assert.throws(() => buildDocReloadArgs({ doc: 'PCB1' }), /project is required/);
  assert.throws(() => buildManufacturingReleaseArgs({ project: 'P', doc: 'PCB1' }), /spec is required/);
  assert.throws(() => buildManufacturingReleaseArgs({ project: 'P', doc: 'PCB1', spec: 's0.json', drcTimeoutSeconds: 3 }), /10 to 600/);
});
