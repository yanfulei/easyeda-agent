# easyeda-agent MCP

Local stdio MCP adapter over the existing `easyeda` CLI/daemon. It exposes a
catalog-driven set of domain and guarded workflow tools. Arbitrary JavaScript
and order/checkout/payment/submit action families are deliberately not exposed.

Release users should run the repository's idempotent installer. It downloads the
sha256-verified MCP bundle with locked production dependencies and registers Codex
when detected:

```bash
curl -fsSL https://raw.githubusercontent.com/yanfulei/easyeda-agent/main/install.sh | sh
codex mcp get easyeda-agent --json
```

For source development:

```bash
npm ci --ignore-scripts
EASYEDA_BIN=/absolute/path/to/easyeda npm test
EASYEDA_BIN=/absolute/path/to/easyeda npm start
```

Manual Codex registration (re-adding the same name updates it in place):

```bash
codex mcp add easyeda-agent \
  --env EASYEDA_BIN=/absolute/path/to/easyeda \
  -- node /absolute/path/to/mcp/src/server.mjs
```

The MCP process does not access EasyEDA directly. Mutations still pass through
the Go daemon, connector, workflow gates, audit log, and official `eda.*` API.
Every schematic/PCB action (including read-only inspection and manufacturing
exports) and every mutating typed action requires both `project` and `doc`. The
CLI resolves these selectors to an exact project/document UUID binding, and
rejects a `pcb.*` action aimed at a schematic (or the reverse). Catalog
`timeoutMs` is forwarded to `easyeda call` instead of using one generic timeout.

`easyeda_manufacturing_release` additionally requires the approved S0 `spec`.
It will not publish a Gerber/BOM/CPL bundle unless strict intent validation,
save/reload, PCB checks, native DRC, snapshot stability, and artifact audits all
pass.

Actions marked `needsConfirm` cannot run through ordinary domain tools. Call
`easyeda_confirmed_action` with `operation=prepare`, review the returned target
and hashes, then call it again with `operation=execute` and the token. Tokens are
short-lived, single-use, and bound to the exact action, project, document,
window, and payload.

After registration, restart the MCP client so it discovers the new server. Run
`easyeda_health` first, then use `easyeda_actions` to select the exact typed
action. The Skill's inspect-before-mutate, save, reload, DRC, and workflow-gate
rules continue to apply to MCP calls.

## DeepSeek Harness (DSH) 集成

DSH 原生支持 skill 与 MCP client 两种形态，本仓库两者都已具备，接入是配置级
工作：详见 [`docs/dsh-integration.md`](../docs/dsh-integration.md)。要点：skill
软链到 `~/.dsh/skills/` 即被发现；MCP 在 profile 的 `cordis.patch.yml` 加一个
`@deepseek-ai/dsh-mcp-client` 实例（`serverName: easyeda`，指向本目录
`src/server.mjs`）即可，工具以 `mcp__easyeda__easyeda_*` 命名。注意 in-box
插件无需 pnpm 安装（fallback 从 dsh 安装目录解析），profile 里误装旧版会遮蔽
fallback。
