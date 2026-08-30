# 快速开始 & 使用注意事项

easyeda-agent 的四个自有 release 组件必须**同版本、同时在位**;EasyEDA Pro 是宿主:

| 部件 | 是什么 | 装在哪 |
|---|---|---|
| **CLI / daemon** (`easyeda`) | 掌管 typed action 协议、状态、审计、产物、校验 | 本机 `PATH`(默认 `/usr/local/bin`) |
| **连接器插件** (`.eext`) | 极薄桥接层,跑在 EasyEDA 内,把动作转成官方 `eda.*` 调用 | EasyEDA Pro「扩展管理」 |
| **Skill** (`easyeda-agent`) | AI 客户端里的工作流、参考、脚本、规范 | `~/.claude/skills` 和/或 `~/.codex/skills` |
| **MCP adapter** (`easyeda-agent`) | Codex 等 MCP 客户端的结构化工具入口(可选,推荐 Codex 使用) | `~/.local/share/easyeda-agent/mcp` |
| **EasyEDA Pro** | 官方编辑器,需开启「允许外部交互」 | 桌面应用 |

> **一句话记牢**:升级不是只升 CLI —— **CLI、连接器 `.eext`、Skill、MCP 要一起升到同一 release**,
> 否则 `daemon health` 会把落后的连接器标成 stale(`connectorVersionOk:false`),动作会打不通。

---

## 首次安装(5 步)

### 1. 装 CLI + Skill + MCP(一条命令)

```bash
curl -fsSL https://raw.githubusercontent.com/yanfulei/easyeda-agent/main/install.sh | sh
```

一键脚本会：
- 安装/更新 `easyeda` CLI/daemon 到 `PATH`;
- **自动检测已装的 AI 客户端**,把 `easyeda-agent` skill 装到对应目录 —— Codex(`~/.codex/skills/easyeda-agent`)、Claude Code(`~/.claude/skills/easyeda-agent`);
- 检测到 Codex 时安装带锁定依赖的 MCP bundle,幂等注册 `easyeda-agent` MCP;
- 打印连接器 `.eext` 的下载地址。

可用环境变量控制 skill 安装目标:

```bash
EASYEDA_INSTALL_SKILLS=codex,claude  ... | sh   # 指定目标
EASYEDA_INSTALL_SKILLS=none          ... | sh   # 跳过 skill(只装 CLI)
EASYEDA_SKILL_PRESERVE=1             ... | sh   # 升级时保留本地改动
EASYEDA_INSTALL_MCP=none             ... | sh   # 跳过 MCP 安装/注册
EASYEDA_VERSION=v0.18.2              ... | sh   # 锁定版本,跳过 GitHub API 查询
```

> 装不上、报 `403`?脚本要调一次 `api.github.com` 查 latest release,匿名额度是每
> IP 每小时 60 次,公司出口 / NAT / CI 很容易撞满。要么 `export GITHUB_TOKEN=<token>`
> (或 `GH_TOKEN`;已 `gh auth login` 的话脚本会自动取 `gh auth token`),要么用
> `EASYEDA_VERSION=<tag>` 直接锁版本绕开 API。

### 2. 启动 daemon

```bash
easyeda daemon start        # 前台阻塞运行,Ctrl-C 退出;建议单开一个终端常驻
```

daemon 默认只监听固定端口 `60832`(`0xEDA0`),连接器钉住该端口并带退避、自愈重连;
`60833-60841` 仅保留给显式的非标准多 daemon 配置,默认不扫描。

### 3. 导入连接器 `.eext`

从 [GitHub Release](https://github.com/yanfulei/easyeda-agent/releases/latest) 下载
`easyeda-agent-connector.eext`(**与 CLI 严格同版**),或从[**立创官方插件市场**](https://jlc-ext.com/item/zhoushoujian/easyeda-agent-connector)一键安装(平台可原地自动更新,但**版本可能滞后 CLI** —— 需严格同版时以 GitHub Release 的 `.eext` 为准),然后:

> EasyEDA Pro → **扩展管理 → 导入扩展** → 选中 `.eext` 文件

### 4. 开启「允许外部交互」

> EasyEDA Pro → **设置 → 允许外部交互 (Allow external interaction)**

不开这一项,连接器的 WebSocket 永远连不到本地 daemon。

### 5. 在 AI 客户端里用 Skill

```
/easyeda-agent          # 原理图 + PCB 全流程
```

支持 MCP 的客户端还可以使用 release 内的 stdio 适配层。MCP 是**可选调用入口**,
不是替代 CLI/daemon 或 Skill 的第五套状态;它仍经过同一套 typed action、审计和
workflow gate。

一键安装器检测到 Codex 时已经完成安装和注册。验证:

```bash
codex mcp get easyeda-agent --json
```

源码开发才需要手工 clone、`npm --prefix mcp ci --ignore-scripts`,再用
`codex mcp add easyeda-agent ...` 把同名配置覆盖到源码路径;重复 add 不会产生重复 server。

新开一个 AI 客户端会话后即可发现工具。当前至少 19 个工具,包括连接健康、action 发现、安全 action domain、
电路块和 guarded workflow;MCP 不暴露任意 JavaScript 的 `debug.exec_js`。

---

## 验证运行栈是否对齐

```bash
easyeda daemon health
```

关注返回里的 `connectorVersionOk`:
- `true` —— 连接器与 daemon 同版,一切就绪;
- `false` —— 连接器**落后**(常见于升级只升了 CLI 没重导 `.eext`,或旧窗口没重启);
- 字段缺失/`null` —— dev 构建,无法硬比对(正常)。

---

## 升级注意事项(务必同一 release)

1. **完整升级重跑一键安装器** —— 同步 CLI、Skill、MCP 且幂等刷新 Codex 注册:
   ```bash
   curl -fsSL https://raw.githubusercontent.com/yanfulei/easyeda-agent/main/install.sh | sh
   ```
   只升级 CLI 二进制 + Skill 目录时可用 `easyeda update`:
   ```bash
   easyeda update            # 下载本平台二进制 → sha256 校验(有 checksums.txt 时) → 原子替换 + 同步 skill
   easyeda update --check    # 只看不改:cli / skill / connector 三方版本一次列清
   sudo easyeda update       # 二进制装在 /usr/local/bin 等 root 目录时
   ```
   升完 **daemon 仍在跑旧二进制,要重启 daemon**;命令会提示。
   开发机上的 dev 构建(git-describe 版本号)默认不覆盖 —— 这是有意的,`--force` 才强升。
2. **重导连接器 `.eext`** —— EasyEDA 按 **uuid 去重**,光 bump 版本号不够:
   先在「已安装」里**卸载旧连接器**,再导入新 `.eext`(uuid 不变,原地更新)。
   *(这步只针对**侧载**的 GitHub Release `.eext`;若连接器是从[立创插件市场](https://jlc-ext.com/item/zhoushoujian/easyeda-agent-connector)装的,平台会原地自动更新 —— 但市场版本可能滞后 CLI,严格同版仍以 Release `.eext` 为准。)*
3. **完全退出并重启 EasyEDA** —— 重导**不会重载已开着的窗口**;旧窗口会继续跑旧代码、
   和新连接器抢 daemon socket。必须**彻底退出 EasyEDA 再打开**。
4. **`easyeda daemon health` 复核** —— `connectorVersionOk:true` 才算升级到位。

> 大多数改动其实不需要重导 `.eext`(daemon 侧的 typed action / CLI 更新无需碰连接器);
> 只有连接器 manifest / handler 变了才需要重新导入。是否需要,看 Release 说明。

### 自动帮你做的部分(省去手动)

- **Skill 目录自动同步**:`daemon start` 默认带 `--auto-update-skill`,启动时会**后台**
  把已存在的 skill 目录(`~/.claude`、`~/.codex`)拉齐到最新 release,并把每一步打进
  daemon 日志。所以升级 CLI 后即便 skill 没手动更新,daemon 也会补上。尊重
  `EASYEDA_SKILL_PRESERVE=1`(保留本地改动);关掉用 `daemon start --auto-update-skill=false`。
  手动触发/查看:
  ```bash
  easyeda skill status      # 各 skill 目录版本 vs 最新 release
  easyeda skill sync        # 立即同步到最新(--version 锁版本,--preserve 保留本地改动)
  easyeda update --check    # 想连 CLI 二进制和连接器一起看时用这个
  ```
- **连接器落后自动提示**:连接器一注册,daemon 就比对版本;落后时打一条**可操作日志**
  (「stale connector: vX < daemon vY — 重导 .eext + 彻底重启 EasyEDA」)。
  **侧载**(GitHub Release)的连接器 `.eext` **无法**被 daemon 静默替换(sideload 无原地自动更新),
  所以这里只**检测+提示**,重导那步仍需你手动做(见上)。若连接器是从
  [**立创插件市场**](https://jlc-ext.com/item/zhoushoujian/easyeda-agent-connector)装的,
  平台**可原地自动更新** —— 但市场版本可能滞后 CLI,严格同版仍以 GitHub Release 的 `.eext` 为准。

---

## 常见卡点速查

| 症状 | 原因 | 处理 |
|---|---|---|
| 动作全部超时、连不上 | 没开「允许外部交互」 | 设置里打开 |
| 不确定谁落后了 | CLI / skill / 连接器版本不一致 | `easyeda update --check` 一次列清三方 |
| Codex 看不到新 MCP 工具 | MCP bundle/注册仍指向旧 release,或会话未刷新 | 重跑一键安装器,再新开 Codex 会话 |
| `connectorVersionOk:false` | `.eext` 落后 / 旧窗口没重启 | 重导 `.eext` + 彻底重启 EasyEDA |
| 重导 `.eext` 后没生效 | EasyEDA 按 uuid 去重,旧的没卸载 | 「已安装」里先卸载旧的再导入 |
| `easyeda: command not found` | `PATH` 没含安装目录 | 把 `/usr/local/bin` 加进 `~/.zshrc` |
| 国内装 skill 失败 | skillhub.cn 无 CLI 接口 | 用一键脚本,或从 Release 下 `skills.tar.gz` 解压到 skills 目录 |

延伸阅读:[功能清单与路线图](FEATURES.md) · [架构](architecture.md) · [开发环境与调试手册](dev-environment.md)
