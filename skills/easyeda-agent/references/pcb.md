
# EasyEDA PCB

Drive `easyeda-agent` typed actions. Run `easyeda actions` for the live machine-readable
list. Prefer typed actions; only fall back to `debug.exec_js` when a typed action is
missing **and** the user explicitly accepts a debug path.

> **PCB design rules live in this skill's references** — especially
> [`pcb-layout-conventions.md`](./pcb-layout-conventions.md)
> (placement priority P0–P7, stackup-conditioned decoupling, thermal/SI/DFM/grid rules,
> each with a data-detectable check). This operational skill **links** to it — single
> source, never copy the rules here.

> **本文导航**:块的 PCB 约束(先查)· 坐标系与模型 · Workflow · Actions(Navigation / Board /
> View / Read·inspect / Routing / Copper pour / Keep-out regions / Filled region / Sch→PCB sync /
> Layout adjust)· Board outline(板框)· Auto-layout · Guardrails。

## 块的 PCB 约束(先查)

板上任何来自**电路块**的模块,其 PCB 约束在块里——`easyeda blocks show <id>` 读四张 map。做 PCB
前先把本板用到的块 show 一遍,把 `severity=must` 的约束抄进对应阶段:

- `placement` → **P2** 板边 / 朝向(edge/side/orientation;非对称连接器 USB/SD/IPEX 朝外,须用户确认)
- `pcb_layout` → **P2** 去耦/晶振贴脚距离(`*-adjacency`)· **P8** EP 热过孔/接地缝合(`ep-*`)·
  **P4** RF keepout / 巴伦镜像(`rf-*` / `balun-mirror`)
- `signals` → **P7.0** 差分 / 阻抗 / 等长
- `silk` → **P9** 逐脚标注

通用启发式布局会漏掉 CC1101 巴伦镜像、ESP32 模组 EP 热过孔、去耦 ≤2mm 贴脚这类块专属约束——design-flow
的 P 阶段会逐个引用,这里是提醒:**做 PCB 前先 show 一遍本板的块**。

## Coordinate system & model (load-bearing)

- **Data unit = `1 mil`** (schematics are `10 mil` / 0.01in — different). **y-UP**: +y renders upward.
- **Component `x`/`y` = the ANCHOR (footprint origin), usually NOT the bbox center** — and the anchor-to-center offset rotates with the part, so the mismatch is worst on rotated parts (#105). **Plan in bbox centers, write with center semantics**: `pcb list --include-bbox` returns each part's `center` `{x,y}` (bbox geometric center, CLI-computed) alongside `bbox`; `pcb modify --id <pid> --center --x <cx> --y <cy>` takes the DESIRED center and converts to anchor via the live bbox. `--center` refuses a same-call rotation change (rotating alters the offset the conversion reads) — rotate first, then `--center` in a second call. Raw `--patch '{"x":…,"y":…}'` stays anchor semantics.
- Every component is bound to a **layer** (`TOP` / `BOTTOM`). **No left/right mirror — only flip** (change layer via `pcb.component.modify`).
- **No programmatic undo.** Snapshot before/after into the audit log; pull a **fresh `primitiveId`** right before mutating.
- `pcb.component.delete` returns a boolean meaning *"operation completed"*, **not** *"actually deleted something"* — don't rely on it; verify with `pcb.components.list`.
- Layout actions (`align` / `distribute` / `grid_snap` / `components.move` / `components.arrange`) act on the **current selection** by default; pass `primitiveIds` to target a specific set. With nothing selected and no `primitiveIds`, they error (0 targets).

## Workflow

1. `easyeda daemon health` → confirm a connected window (route by `--project <name>`; `--window <windowId>` only for fine control). Context is live — refreshed on every action AND, with connector ≥ v0.5.7, pushed by the heartbeat within ~3s of a UI tab-switch (so health follows the UI even with no command run). `connectorVersionOk: false` flags a stale connector loaded in an open window (fully quit + relaunch EasyEDA).
2. `easyeda doc ls --project <name>` → see every openable doc (★=active). If the active doc isn't the target PCB, `easyeda doc switch <PCB-name|uuid> --project <name>` (cross-type PCB↔schematic works). **With 2+ windows open, `--project`/`--window` is REQUIRED** — without it the command only auto-targets when exactly one window is connected, else errors `no EasyEDA connector is available` (a momentary connector reconnect can also trigger this — just retry). (Low-level equivalent: `document.current` → `pcb.documents.list` → `document.open <pcbUuid>`.)
3. **Inspect before mutating**: `pcb.components.list` (`includeBBox`+`includePads`), `pcb.layers.list` (read `copperLayerCount`), `pcb.nets.list`, `pcb.board.info`.
4. Small additive operations; **verify each** by readback + `pcb.drc.check`.
5. **Confirm** before destructive ops (`delete`, `import_changes`, bulk `arrange`) and before saving.
6. Summarize moved/changed primitives, warnings, and artifacts.

## Actions

### Navigation

- `pcb.documents.list` — all PCB documents in the project (uuid + name); pair with `document.open`.
- `document.open` — open any document (schematic page or PCB) by uuid; the cross-type switch entry.
- `pcb.board.info` — current Board (schematic↔PCB linkage) + current PCB; the prerequisite context for `import_changes`.

### Board (板子/组合 — the schematic↔PCB binding)

A **Board groups exactly one schematic + one PCB** — that is how the two are kept
together, and what `import_changes` follows. Boards are identified by **name**, not
uuid. CLI: `easyeda board …`. Maps to `eda.dmt_Board.*`.

- `board.list` / `board.current` — all boards (name + bound schematic + pcb) / the current one. A board can hold only a PCB or only a schematic — the missing side is reported as `null`.
- `board.create` — bind a schematic and/or PCB into a new board (`--schematic` / `--pcb`). The fix for a floating/unlinked PCB before `import_changes`.
- `board.rebind` — repair a **stale/orphaned** Board binding (e.g. a rebuild-from-empty PCB left the Board pointing at a deleted schematic uuid, crashing `board list` and faking a DRC Netlist Error): deletes the old Board (by `--name`, else current) and re-creates it bound to `--schematic` (+ `--pcb`), rolling back on failure; `--force` to move a schematic already bound elsewhere. 曾被 daemon 挡成 `UNKNOWN_ACTION`(protocol 目录漏登记),现已可用——不必再走 `board delete` + `board create` 手工两步。CLI: `easyeda board rebind --schematic <schUuid> --pcb <pcbUuid>`.
- `easyeda pcb new-board` (`board.new_pcb`) — new board + fresh empty PCB page bound to a schematic. **A schematic belongs to only ONE board**, so this refuses if the target schematic is already bound (it would MOVE it out, orphaning the old board's PCB — the "原理图没了" trap). Work inside the existing board instead; pass `--force` only to move it deliberately.
- `board.rename` — rename a board (`--name` → `--new`).
- `board.copy` — duplicate a board (its schematic + PCB).
- `board.delete` — delete a board by name (**confirm** — no undo).

### View (canvas — shared with the schematic editor)

Act on the focused canvas; the editor view shortcuts. CLI: `easyeda view …`.

- `view.fit` — zoom to fit all primitives (适应全部, the `K` shortcut) → `easyeda view fit`.
- `view.fit_selection` — zoom to fit the current selection → `easyeda view fit-selection`.
- `view.zoom` — pan/zoom to a center coordinate and/or scale percent (`--x/--y/--scale`; omitted keeps current).
- `view.region` — zoom to a rectangular region (`--left/--right/--top/--bottom`, mil).

### Read / inspect

- `pcb.components.list` — placed footprints. `includeBBox` → per-component rendered extent (for overlap/spacing reasoning); via the CLI (`pcb list --include-bbox`) each bbox'd part also carries `center` `{x,y}` — the bbox geometric center, CLI-computed — use it (not the anchor `x`/`y`) when planning positions; `includePads` → pads + net (the net-by-name connectivity) + **real copper `width`/`height`** (mil, axis-aligned after pad rotation; omitted for complex-polygon pads → consumers fall back to a nominal size). Connector ≥0.12.1; check/route clearance math uses these real extents.
- `pcb.layers.list` — layers (id/name/type), `currentLayer`, and `copperLayerCount` (2-layer vs 4+-layer — gates the decoupling rules).
- `pcb.nets.list` — nets (`net` / `length` / `color`).
- `pcb.report` — **read-only design report** driven by per-net copper length: every net's routed length, each **net class**'s aggregate length, **differential-pair** P/N lengths + `skew` (`|lenP−lenN|`), and **equal-length-group** per-net lengths + `spread` (`max−min`). No DRC run — the quantitative companion to `pcb.drc.check` for routing-quality gates (diff skew / length matching). Pure read.
- `easyeda pcb check` — **reconstructed DFM (design-for-manufacture) audit** — the PCB sibling of `sch check`, and the quality checks the native `pcb drc` (rule clearance) does NOT flag. Copper rules compute **purely Go-side** from placed copper (`pcb.line.list` + `pcb.via.list` + `pcb.components.list --include-pads`) and never mutate; the silkscreen rule reads `pcb.silk.list` (text layer + mirror + **reverse + rotation + fontSize**), the antenna rule reads `pcb.region.list` (region bbox + rule types) + component bboxes. Rules: **dangling-end** (a track end anchored to no pad/via/track → floating copper), **acute-angle** (two same-net same-layer segments bend <90° → acid trap), **non-orthogonal** (a single track off the 0/45/90/135° grid → free-angle routing, WARN — catches lazy pad-to-pad diagonals), **track-over-pad** (a track body crosses a pad center it doesn't terminate on, same layer: cross-net = **ERROR** short, same-net = WARN), **silkscreen-flipped** (a silkscreen text 放反 — three modes: a designator on the opposite silk layer from its component **ERROR**; a top/bottom text whose **mirror OR reverse** flag reads backwards **ERROR**; a reference designator (`key=="Designator"`) not reading **upright** — 180° upside-down / 90°·270° sideways — **WARN**), **overlapping-via** (two vias stacked), **single-layer-via** (a *signal* via that changes no layer — power/GND stitch vias are skipped, they connect to a pour not a track), **width-mismatch** (a 2-pin part with asymmetric neck-down → INFO), **duplicate-segment** (collinear overlapping redundant copper), **antenna-keepout** (an antenna component — ESP WROOM/WROVER module, an `ANT*` part, or a **discrete chip antenna** matched by device name `2450AT`/`ANT-SMD` (#123: auto-designators like AE1 defeated the ANT* test) — whose footprint lacks a no-copper keep-out region on **every** copper layer → WARN, naming the missing layer; copper under an antenna detunes it. Requires top (L1) + bottom (L2) no-copper regions, plus the inner planes via `no-inner-electrical` on 4+-layer boards — a top-only keep-out still lets the bottom pour fill under the antenna), **netless-pour** (a copper pour bound to **no net** — dead copper that occupies board area but connects nothing, issue #34; arises from `pcb pour` without `--net`, or pouring directly on a flipped PLANE layer → WARN, remove with `pcb pour-clean --netless`), **via-crosses-plane** (a via whose net differs from an inner **PLANE/内电层**'s net, issue #30 — official bug [easyeda/pro-api-sdk#32](https://github.com/easyeda/pro-api-sdk/issues/32): a via created **after** the plane exists gets **no anti-pad** cut into the negative plane, DRC reports Plane Zone to Via / Hole to Plane Zone and `pour-rebuild` alone doesn't repair it → WARN with fix guidance: prefer removing the via and routing on outer layers, or `easyeda doc reload` then `pcb pour-rebuild`, then confirm with `pcb drc`. Reads the stackup via `pcb.layers.list` (`type=="PLANE"`) + plane nets from `pcb.pour.list`. **Best-effort**: the API exposes no anti-pad/creation-order data, so a via placed *before* the plane flip — proper anti-pad, clean DRC — is flagged too; treat `pcb drc` as the arbiter of which flagged vias are actually broken. A PLANE layer with **no net-bound pour visible** gets its own **INFO** (not WARN, not `--strict`-gated — issue #110: after `doc reload` a PLANE-layer pour is loaded into the negative-plane store and becomes **invisible to `pcb.pour.list`**, with no extension-API read path, so "plane net unknown" is usually a reload artifact, not a defect; treat `pcb drc` Connection=0 as the arbiter before adding any pour — blindly re-pouring stacks duplicates. If the plane is genuinely empty: pour while the layer is SIGNAL, then flip), dangling-end anchors a track endpoint by **via area** too (a same-net endpoint anywhere inside the via copper counts as anchored — track↔via conducts on its own; the former **via-bond** ERROR rule that flagged bare track↔via junctions was removed after [pro-api-sdk#31](https://github.com/easyeda/pro-api-sdk/issues/31) proved to be our misdiagnosis — the "floating" symptom was stale pour connectivity, fixed by `pcb pour-rebuild`, not by fills), **floating-track-island** (a connected **group** of ≥2 tracks/vias in which no endpoint anchors to any pad — dangling-end's blind spot, members anchor each other → WARN listing all member ids for `pcb track-delete`; islands under a same-net pour are exempt), **power-not-poured** (a power/GND net with ≥2 pads that has **no same-net pour and is bound to no PLANE** → WARN — power should be delivered by copper area, not thin tracks, the #1 DRC source; fix `pcb pour-fit --net N` on 2-layer / `pcb power-planes` on 4-layer; single-pad nets and already-poured nets are exempt. **#117 nuance**: when the board carries an inner **PLANE layer with unknown net** — its pour is platform-invisible after `doc reload`, #110 — a GND-class finding degrades to **INFO** (non-blocking, not `--strict`-gated): that plane almost certainly IS the GND pour, so verify with `pcb drc` Connection=0 instead of re-running `power-planes`), **width-under-spec** (a routed **power** track thinner than its net-class spec width — 公制圆整阶梯 branch 0.25mm / trunk 0.4mm / high-current 0.5mm (≈9.84/15.75/19.69mil, 规范 §1.2), see `pcb net-classes` → WARN, one aggregated finding per net with the thinnest offender; **fine-pitch narrowing and via-stitch stubs are exempt**, and signal nets are not checked since their spec is the live default and fine-pitch narrowing is legitimate), **silk-over-pad** (silk text whose estimated extent covers a same-side pad — fab clips silk on exposed copper → WARN; fix with `pcb silk-align`/`pcb silk-set`; text extent from string length × the REAL `fontSize` (40mil fallback), pads tested against their real width/height, 规范 §11.2), **decap-too-far** (a 2-pad C\* with one pad on a power rail + one on GND sitting >100mil/2.5mm from the nearest same-rail U\* pin → WARN — a decap must hug its IC ≤2mm; rails with no IC pad (bulk/input caps) and signal-signal caps are exempt, 规范 §3.1), **via-in-pad** (a **same-net** via ON a pad center → WARN — solder wicks down the barrel AND this project proved via-on-pad ≠ connected; offset with a dog-bone stub; cross-net via↔pad stays the clearance rule's ERROR, 规范 §2.3), **copper-near-edge** (routed track/via copper within the live copper-to-edge rule of the board-outline bbox — fallback 8mil routed edge → WARN, aggregated per net with the worst offender, 规范 §5.1; needs `pcb.outline.get`, skipped without an outline), **fiducial-missing** (an SMT-scale board — ≥30 top pads — with <3 `FID*`/`MARK*` fiducial parts → **INFO** only, since JLC panel rails add their own marks; local marks matter for fine-pitch, 规范 §9), **zone-violation** (#126: a part claimed by a `pcb zones set` functional-zone module whose bbox center sits **outside its zone's board sub-rectangle** → WARN with the module/zone named, 规范 §3.3 模拟/数字分区 — the S0 spec's partitioning decision finally verified at P2; only runs when the project has zone claims, and an edge-bound part on the wrong side keeps getting flagged until the claim or the edge assignment is fixed). 规范 §refs point into `docs/pcb-design-rules.md` (the fact-standard手册 the check messages cite). `--json` for the full list; `--strict` exits non-zero on any WARN/ERROR (gate-able). Complements `pcb layout-lint` (placement/routability) + `pcb drc` (rule clearance). Arcs are out of scope for v1 (line/via/pad only; auto/short-routed copper is line segments); through-hole cross-layer track-over-pad shorts are a known blind spot (pad layer reported per side). Core + tests in `internal/app/pcb_check.go`.
- `easyeda pcb drc` (`pcb.drc.check`) — native rule-clearance DRC, normalized to `{passed, violations}`. **`--json` flattens** the panel's nested tree into one row per violation `{rule, objType, ruleName, net, x, y, layer, objs, message}` with **x/y in real mil** (raw leaves store mil/10 — the flattener owns the ×10) — pipe to `jq`, feed `objs` ids straight into `pcb via-delete`/`track-delete`. **`--timeout <s>`** (default 60) bounds the wait AND is forwarded to the daemon, which answers with a structured error *before* the HTTP client gives up. ⚠️ **Foreground constraint**: a background/occluded EasyEDA window **never finishes** the DRC canvas recompute — on timeout, bring the window to the FOREGROUND and run **once**; do **not** retry in a loop (each retry piles another recompute onto the webview). The daemon enforces this: a second `pcb drc` on a window whose first hasn't settled is rejected immediately (`ACTION_BUSY`).
- `pcb.drc.rules` — read the active PCB's **DRC rule configuration** (clearances, track widths, via sizes, …) **without running a check**. Use to feed real rule values into layout reasoning / gates, or to see what `pcb.drc.check` enforces. The daemon parses the (deeply-nested, untyped) result into `{clearance, trackWidth, trackWidthMin, viaDrill, viaDiameter}` in mil (`internal/app/pcb_rules.go`); `route-short`/`auto-place` consume it so they conform to the board's spec.
- `easyeda pcb net-classes [--json]` — print the **net-class → spec track-width ladder** (规范线宽) the daemon uses: `signal` (live default) / `power-branch` (3V3·1V8, 0.25mm≈9.84mil) / `power-trunk` (+5V, 0.4mm≈15.75mil) / `high-current` (VBUS·VIN·VBAT, 0.5mm≈19.69mil) / `gnd` (prefer pour). Roles are classified by net name/voltage (`pcb_netclass.go`); power-rung widths are **公制圆整** (0.05mm grid, 规范手册 §1.2 — not mil fragments like 10/15/20), seeded from the live rules and clamped ≥ the fab minimum (signal stays the raw live value, never rounded). `route-short` sizes each net by this table and `pcb check` width-under-spec gates under-sized power tracks. (A block's declared per-net `track_width_mil` overrides the heuristic — phase-2 consumption.)
- `easyeda pcb drc-rules-set --pour-clearance <mil>` — the **write side** of `drc-rules` (v1 knob: pour/plane copper clearance, **raise-only** — never loosens a stricter board). Patches `Plane` `lineClearance` in `copperRegion` (both pad models) + `innerPlane` of the current rule configuration, writes it back, verifies by re-read; follow with `pcb pour-rebuild` so existing pours reflow. A write on an immutable system preset (`JLCPCB Capability(...)`) turns it into a per-board `自定义配置` copy — expected. **Part of the solidified fix for the fresh-PCB pour-reflow divergence**: a newly created PCB reflows ~3% under the configured clearance (10mil → ~9.7mil) AND skips thermal spokes; `--pour-clearance 12` restores margin over the 10mil DRC floor.
  > **Fresh-PCB trap — the rules snapshot**: a PCB document **created in the current session and never reloaded** computes pour reflow from a **creation-time rules snapshot** — rule writes (readback shows them!), `pour-rebuild`, and tab-switching away/back all have NO effect on the reflow. Only a real close+reopen (`easyeda doc reload` — saves first, no edits lost) refreshes it; after the reload, `pcb pour-rebuild` reflows under the live rules (clearance AND thermal spokes). Already-reloaded documents (e.g. any board that survived an EasyEDA restart) honor rule writes immediately. The esp32-mini playbook encodes the full recipe: `rules-pour-margin` → pours → `reload-pcb` (`doc reload`) → `pour-rebuild-2`; verified on a fresh board: DRC 55 → **1** (remainder = the known add-component netlist false positive).
  > **Raw-API trap** (if scripting rules via `debug exec` instead): `eda.pcb_Drc.overwriteCurrentRuleConfiguration()` takes the **BARE config content** — `getCurrentRuleConfiguration()` returns `{name, config}`, and passing that whole wrapper **silently no-ops** (resolves `undefined`, readback unchanged). Pass `cfg.config` → returns `true`.
  > **Fab-rule baseline: [`fab-rules-jlcpcb.json`](fab-rules-jlcpcb.json)** — the canonical JLCPCB fabrication capabilities (min trace/space, via drill+pad, annular ring, copper-to-edge, silk, by layer count + copper weight), captured from JLCPCB's published capabilities. JLCPCB is the fab behind EasyEDA Pro, so a live board's `pcb.drc.rules` converges with this file's **recommended** column (verified on ceshi: clear 6mil / width 10mil / via 0.3–0.6mm). **Always prefer the live rule; use this JSON as the fallback seed + as clamp floors** (never emit a track/via/gap below the `manufacturingMin`). The **`boardTypeRulesLive`** section holds the AUTHORITATIVE real per-board-type rules exported from JLCEDA (single / double / multi-layer / metal-core), fingerprint-classified + confirmed against named exports — `defaultPcbRules` uses the **doubleLayer** row (clear 6 / width 10 / min 5 / via 0.3–0.6mm / copper-to-edge 10). Controlled impedance is intentionally omitted (not derivable from platform data — see task #27).

### Routing (copper tracks + vias)

**已移至 [`pcb-routing.md`](pcb-routing.md)** —— 本节内容整体搬出，减少每次调用的上下文成本（RFC #178）。需要时读那个文件。

### Schematic → PCB sync + component CRUD

**已移至 [`pcb-layout.md`](pcb-layout.md)** —— 本节内容整体搬出，减少每次调用的上下文成本（RFC #178）。需要时读那个文件。

## PCB mutation → `doc reload` 门(铁律 5,daemon 机械强制)

改完铜再读,读到的是**旧引擎状态**:每个 PCB 文档有自己的枚举缓存,
rip-up / route / delete / via / track / pour 这类 mutation 之后,
`pcb list` / `line.list` / `via.list` / `pour.list` / `nets.list` / `drc.check` / `report`
都可能返回 mutation 之前的画面,直到文档被真正关闭重开。

**这条现在是机械门,不是提醒。** daemon 在 `/action` 派发层直接**拒绝**这种读,
返回错误码 **`STALE_READ`**,消息里带着该跑的下一条命令:

```
STALE_READ: pcb.components.list —— PCB 自 pcb.line.create 后未 reload,读到的是旧引擎状态。
下一步: easyeda doc reload --project <name>
(绕过: --force-stale-read "<理由>",入审计)
```

- **修法就一条**:`easyeda doc reload --project <name>`(它自己会先 save,不丢改动)。
  确定性复位 = `rip-up → save → reload`。
- **`pcb pour-rebuild` 也解锁**:它本来就是「铺铜连通性 stale」的修法。
  DRC 手术后同网(多为 GND)Connection Error 暴增,先跑它,那不是真断。
- **不会误伤的**:`pcb save`、`pcb pour-rebuild`、任何 `--dry-run` 预览(issue #112),
  以及只改视图的 `view-side` / `layers set-current` / `layers visibility`
  —— 这些不脏化枚举,不会 arm 这道门。
- **`pcb snapshot` 不被拦**(它是画布的照片,不是枚举),但仍会带 `staleRisk` 提示。
  注意:截图发白/发旧的修法是**把窗口切前台**,不是 reload——两回事。
- **绕过**:`--force-stale-read "<理由>"`(**不是** `--force` —— 那是布线阶段门 #132,
  两者互不相干别混用)。只授权本次调用,**入审计**
  (审计里记成 `daemon.stale_read.force`),窗口仍然是脏的,下一条无 force 的读照样被拒。

> 为什么升成硬门:49 天 171554 条审计记录里,这条规则此前只发一句非阻塞警告,
> 结果 **1780 次脏读、18.1% 违反率**——agent 看见警告照读。被机器拒绝的规则不漏,
> 靠记忆的规则漏。

## Guardrails

- Confirm before `pcb.component.delete`, `pcb.import_changes`, or a bulk `arrange`/auto-layout plan.
- Save automatically at an already-defined passed stage and verify `saved:true`; pause first
  only when the user explicitly requested step-by-step approval.
- Do not claim completion after a mutation until readback / DRC verifies it (or state the remaining risk).
- No undo — record before/after into the audit log so a move can be reversed by re-applying the old coordinates.
- Treat `File`/`Blob` outputs (gerber/pick-and-place/3D) as artifacts.
