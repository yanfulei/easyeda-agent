import test from 'node:test';
import assert from 'node:assert/strict';
import path from 'node:path';
import { readFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { StdioClientTransport } from '@modelcontextprotocol/sdk/client/stdio.js';

const packageDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const serverPath = process.env.EASYEDA_MCP_SERVER || path.join(packageDir, 'src', 'server.mjs');

test('stdio MCP initializes, lists tools, and invokes offline discovery', async () => {
  assert.ok(process.env.EASYEDA_BIN, 'EASYEDA_BIN must point to the local CLI for integration tests');
  const transport = new StdioClientTransport({
    command: process.execPath,
    args: [serverPath],
    cwd: packageDir,
    env: { ...process.env, EASYEDA_BIN: process.env.EASYEDA_BIN },
  });
  const client = new Client({ name: 'easyeda-agent-mcp-test', version: '1.0.0' });

  try {
    await client.connect(transport);
    const packageMetadata = JSON.parse(await readFile(path.join(packageDir, 'package.json'), 'utf8'));
    assert.equal(client.getServerVersion()?.version, packageMetadata.version);
    const listed = await client.listTools();
    assert.ok(listed.tools.length >= 19);
    assert.ok(listed.tools.some((tool) => tool.name === 'easyeda_pcb'));
    assert.ok(listed.tools.some((tool) => tool.name === 'easyeda_confirmed_action'));
    assert.ok(listed.tools.some((tool) => tool.name === 'easyeda_manufacturing_release'));
    assert.ok(listed.tools.some((tool) => tool.name === 'easyeda_schematic_gate'));
    assert.ok(listed.tools.some((tool) => tool.name === 'easyeda_pcb_check'));
    assert.ok(!listed.tools.some((tool) => tool.name === 'easyeda_debug'));
    const releaseTool = listed.tools.find((tool) => tool.name === 'easyeda_manufacturing_release');
    assert.ok(releaseTool.inputSchema.required.includes('spec'));

    const allActions = await client.callTool({
      name: 'easyeda_actions',
      arguments: {},
    });
    assert.equal(allActions.isError, false);
    assert.ok(!allActions.structuredContent.actions.some((action) => action.domain === 'debug'));
    assert.ok(!allActions.structuredContent.actions.some((action) => /(?:^|[._-])(order|checkout|payment|submit)(?:[._-]|$)/i.test(action.name)));

    const discovered = await client.callTool({
      name: 'easyeda_actions',
      arguments: { domain: 'schematic', search: 'check', mutates: false },
    });
    assert.equal(discovered.isError, false);
    assert.ok(Array.isArray(discovered.structuredContent.actions));
    assert.ok(discovered.structuredContent.actions.some((action) => action.name === 'schematic.check'));

    const rejectedMutation = await client.callTool({
      name: 'easyeda_schematic',
      arguments: { action: 'schematic.page.create', payload: { name: 'Unsafe' } },
    });
    assert.equal(rejectedMutation.isError, true);
    assert.match(rejectedMutation.content[0].text, /requires both project and doc/);

    const rejectedPcbRead = await client.callTool({
      name: 'easyeda_artifact',
      arguments: { action: 'pcb.export.gerber' },
    });
    assert.equal(rejectedPcbRead.isError, true);

    const rejectedSchematicRead = await client.callTool({
      name: 'easyeda_schematic',
      arguments: { action: 'schematic.components.list' },
    });
    assert.equal(rejectedSchematicRead.isError, true);

    const rejectedUnconfirmed = await client.callTool({
      name: 'easyeda_schematic',
      arguments: {
        action: 'schematic.page.delete',
        project: 'Probe',
        doc: 'P1',
        payload: { schematicUuid: 'sch-1', pageUuid: 'page-1' },
      },
    });
    assert.equal(rejectedUnconfirmed.isError, true);

    const prepared = await client.callTool({
      name: 'easyeda_confirmed_action',
      arguments: {
        operation: 'prepare',
        action: 'schematic.page.delete',
        project: 'Probe',
        doc: 'P1',
        payload: { schematicUuid: 'sch-1', pageUuid: 'page-1' },
      },
    });
    assert.equal(prepared.isError, false);
    assert.equal(prepared.structuredContent.singleUse, true);

    const rejectedChangedConfirmation = await client.callTool({
      name: 'easyeda_confirmed_action',
      arguments: {
        operation: 'execute',
        action: 'schematic.page.delete',
        project: 'Probe',
        doc: 'P1',
        payload: { schematicUuid: 'sch-1', pageUuid: 'different-page' },
        confirmationToken: prepared.structuredContent.confirmationToken,
      },
    });
    assert.equal(rejectedChangedConfirmation.isError, true);
    assert.match(rejectedChangedConfirmation.content[0].text, /does not match/);

    const blocks = await client.callTool({
      name: 'easyeda_blocks',
      arguments: { operation: 'search', query: 'led' },
    });
    assert.equal(blocks.isError, false);
  }
  finally {
    await client.close();
  }
});
