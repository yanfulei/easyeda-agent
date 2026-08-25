# esp32Mini 端到端 · 本轮记账（广度优先：只记录，段末统一修）

> 开始于 2026-08-24。工具链：CLI/daemon `v1.1.1-14-g9614046`，连接器 `1.1.2`（热重载装入，未走重导入）。
> EasyEDA `3.2.149.88089769`，工程 `ceshi`（从零，0 器件）。
> 模式：纯净模式 —— 输入是 `esp32MiniRequire.md` 原始需求，S0 spec 从零推导，未复用上一轮的
> `.easyeda/s0-ceshi.prev-run.json`。

## 页 uuid 映射（页名改不动，见 F1）

| 计划页名 | 实际页名 | uuid |
|---|---|---|
| POWER | P1 | `da7496751e2144f6` |
| MCU | P2 | `02a8ba989be213d9` |
| USB_DL | P3 | `6e8b6ad4d38fedd6` |

原理图文档 `schematic1` = `ba82812d93547e61`；PCB1 = `f0606a1fc75364a9`。

---

## 挂账

### F1 — `sch page-rename` 完全不生效，且报文把「平台拒绝」说成「异步未同步」

**现象**：`easyeda sch page-rename` 对三页全部失败，两条读路径（`doc ls` / `sch pages`）
和一次 `doc reload` 之后页名仍是 P1/P2/P3。

**取证**（`debug exec` 直调平台）：

| 调用 | 返回 |
|---|---|
| `dmt_Schematic.modifySchematicPageName(page,'USB_DL')` | `false` |
| `dmt_Schematic.modifySchematicPageName(page,'USBDL')`（无下划线） | `false` |
| `dmt_Schematic.modifySchematicPageName(page,'AAA')`（极简名） | `false` |
| `dmt_Schematic.modifySchematicPageName(page,'POWER')`（该页已切前台） | `false` |
| `dmt_Schematic.modifySchematicName(sch,'SCH_MAIN')` | `false` |
| `dmt_Pcb.modifyPcbName(pcb,'PCB_MAIN')` | **`true`** |
| `dmt_Board.modifyBoardName('Board1',…)` | `false`（板名不存在，预期） |

所以**不是全局改名回归** —— `dmt_Pcb` 改名正常，专门是 `dmt_Schematic` 改名族
（页 + 文档）在 3.2.149 上恒返 `false`。传参签名与官方类型定义一致
（`api show dmt_Schematic` 核对过），页 uuid 也与 `getAllSchematicPagesInfo()` 一致。
`api search "名称"` 确认没有第二条改页名的路径。

**我们侧的缺陷（可修）**：`extension/src/actions.ts` 的 `schematicPageRename` 拿到
平台的 `ok=false` 后**照样走「已提交但元数据未同步」那条警告文案**，还把 `ok` 原样透传。
把「平台明确拒绝」讲成「稍后重试就好」，导致我先按缓存问题排查（save / reload / 等待）
白跑了三轮。判据应当分开：`ok===false` = 平台拒绝，直说拒绝并给下一步；
`ok===true && verified===false` 才是异步缓存。

**影响**：S1 过门判据「页名 = 功能名」拿不到。下游按 uuid 定位不受影响，
但 `sch status` 的 S1 会恒停在 ◐，读图人也看不出页的职责。

**绕行**：本轮按 uuid 推进，页名留作人工在 EasyEDA 工程树里双击改（3 处）。

---

### F2 — `block-apply` 的落点会把组挤出图纸左沿（硬门没拦住）

**现象**：`block.tactile_boot_reset` 落到 `origin relocated: 400,300 → 120,500`，
随后 `clusters` 报 `ERROR out-of-sheet SW1 左沿 4 < 12` —— SW1 的体积
`x=[4,190]`，本体在框内，marker 印不出来。

同一次运行还预警了 `right 侧 1 支 marker 要 1 条 lane(需 107)，推让后通道只有 68`，
说明求解器**知道**右侧腾不出通道，于是把整块往左推，一路推出了图纸左边界。
布线前那道硬门只判重叠与引脚重合，**不判「组是否探出图纸可用区」**，所以放行了。
判据不一致：`clusters` 事后能判出来的事，落地门当场判不出来——两把尺。

**期望**：落点搜索把「图纸可用区」当作和器件本体同级的硬约束（marker 晕圈已经在算了，
边界没算），或落地门补上 out-of-sheet 判据并回滚。

**同批次两处 tight**：`SW2 ↔ U1 间隙 5 < 20`、`C2 ↔ R2 间隙 0 < 20`。

### F3 — 旋转自检探针旗 `__ROTPROBE__` 残留在画布上，污染 bridge-check

**现象**：MCU 页首次 `bridge-check` 报 `1 orphan-flag: nets=[__ROTPROBE__]`，
flag id `f0c11283e4f34e95`。`clusters` 同时报「1 支 marker 既不沾任何导线、离谁都远」。

**根因**：`extension/src/actions.ts:4995 detectRotationNegation()` 在
`(990000, 990000)` 建一支 `__ROTPROBE__` 旗探测平台是否把 rotation 存成负值，
读完后 `await eda.sch_PrimitiveComponent.delete([pid])` —— **但这次删除没生效**
（平台 delete 撒谎的老毛病，见 memory `platform-delete-lies-and-pin-truth-table`），
探针旗就永久留在页上。

**影响**：每次 `bridge-check` 都会报一条 orphan-flag → `sch gate --strict` 直接 FAIL。
这是**每块新页第一次写入时都会踩**的（探针是 per-connector-session 一次性，
但落在哪一页就污染哪一页）。手工 `sch prim-delete --ids f0c11283e4f34e95` 删掉后
bridge-check 立刻回到 0 problem tree —— 所以不是删不掉，是探针自己没回读复核。

**期望**：探针删除后回读确认，没删掉就重试/上报；或者干脆别用「建一支真旗」来探测。

### F4 — `zone-plan` 给的「照抄即可」命令没验边界，抄下来被硬拒 / 把区推出纸外

交接文档判据 4 明写「**报文的可执行性本身是验收项**」。本轮两次不成立：

1. USB_DL 页 `U ↔ J_USB` 那条给的是
   `easyeda sch zone move --zone J_USB --dy -130`
   → 执行结果：`目的地压图签 keepout:平移后 bbox (406,150)..(940,340) 与图签区
   (468,0)..(1170,198) 相交 — 硬拒(--force 也不放行)`。
2. 同页 `esp32_autodownload(Q1)` 压图签安全带那条给的是
   `easyeda sch zone move --zone esp32_autodownload(Q1) --dx -25`
   → 该区当前 frame 左沿是 12，再左移 25 = **-13，出纸面左界**。报文自己算的是
   「右沿超出安全带 22，所以左让 25」，只验了要躲开的那条边，没验对侧的纸面边距。

**期望**：`zone-plan` 生成建议位移时，把「图签安全带 + 四边纸面边距 + 其它区」一起代进去
校验一次，验不过就不要给这条命令（或改给另一侧的等效位移）。现在给出的是
「照抄必被拒」的命令，比不给更糟——它让人以为还有救。

另：报文里的图签矩形写的是 `(468,0)..(1170,198)`（原始图签），而实际判定用的是
**安全带 `(438,-30)..(1200,228)`**。两个数字都出现在同一页报文里，读的人会按前者算，
按后者被拒。

### F5 — `zone-plan` 说「容量是够的」，`zone-arrange` 说「装不下要拆页」，同一页两个结论

USB_DL 页同一时刻：

- `zone-plan` 收尾行：`✗ plan has violations — **容量是够的**,是摆放/间距问题`
- `zone-arrange` 收尾行：`phase B 落位:blocked —— J_USB 无处可放…verdict: blocked(出路:
  进一步收敛该区,或 sch page-new 拆页)`

我按可用区手算复核过，**`zone-arrange` 是对的**：图签安全带把纸面切成
L 区 `426×801` 与 T 区 `720×585`；J_USB 框 635 宽、U 框 718 宽，两者都只能进 T 区，
并排 635+718 超宽、竖叠 328+259+带高超高；用 zone-arrange 收敛后的形状
(J_USB 381×454 / U 369×354) 并排仍是 750 > 720。所以这一页**真的装不下**。

`zone-plan` 的「容量是够的」是拿**面积**比的，没有把图签安全带切出来的 L 形可用区
考虑进去 —— 这句话会把人往「再挪挪就好」的方向带，而正确出路是拆页。
判据应当统一：能不能装下由同一个装箱器回答。

### F6 — `sch disconnect` 只认引脚号，`sch autoconnect` 认脚名，同一个 `DESIGNATOR:PIN` 语法两把尺

`sch read` 报 Q1 的引脚是 `[('3','C',…),('1','B',…),('2','E',…)]`（号/名/网）。

| 命令 | 传法 | 结果 |
|---|---|---|
| `sch autoconnect --pin U1:IO2` | 脚名 | ✓ |
| `sch block-apply`（内部走 autoconnect，`internal_nets` 写的是 `Q1.B/.C/.E`） | 脚名 | ✓ |
| `sch disconnect --pin Q1:B` | 脚名 | ✗ `Pin Q1:B not found on the current schematic.` |
| `sch disconnect --pin Q1:1` | 脚号 | ✓ |

两条命令的 `--pin` help 文案也不一样：`autoconnect` 写明「number or name」，
`disconnect` 只写 `DESIGNATOR:PIN, e.g. U1:5`。但它们是一对**互逆操作**——
用脚名连上去的，用同一个脚名断不下来。排查时 6 次「not found」很容易被读成
「页选错了」（我就是先怀疑了页），实际是解析器不同。

**期望**：`disconnect` 复用 `autoconnect` 的引脚解析（号或名，含 `*` 语义），
或至少在名字匹配不到时把「本件可用的脚号/脚名」列出来。

### F7 — `zone-arrange` phase A 自己生成违反 R5 的方案然后被自己拦死，整页无法收敛

POWER 页 `zone-arrange` 一行报错就退出：

```
phase A(POWER_IN): J2: 端子重叠 TERM_5V(left) × GND(left) —— R5 硬不变式(自短路防线)
```

J2 是 KF301-5.0-2P，两只脚同在本体一条边（都朝左）。`sch_zone_follow.go` 里
`zfCheckCollinear` 明确写着这类端子**合法**（「KF301 这类两脚同侧端子正是如此」，
只要两脚不同轴），但 `zfCheckTermOverlap`（R5）比的是**端子 bbox 是否相交** ——
两支左向标签的 y 只差一个脚距，标签盒当场重叠，于是硬失败。

关键在于**这是规划器自己造的**：phase A 会「重生短桩」，不读画布现状。证据 ——
我先把 J2 的 GND 旗用 `sch connect --direction down` 改成朝下（画布上确认生效：
J2 组体积由 `x=[50,160] y=[345,366]` 变成 `x=[20,160] y=[274,366]`），
**再跑 zone-arrange，报文一字未变**，仍说 `GND(left)`。

自由度是有的：R5 的重叠只差一个标签高度，**梯次桩长**（两支同向桩取不同 offset，
正是 `plan-freedom-subset-exec-freedom` 那条）或把其中一支改派到本体另一条边都能解。
现在的行为是「生成 → 自检不过 → 抛错退出」，没有让位重试，整页零产出。

**绕行**：跳过 zone-arrange，照 `zone-plan` 的逐条建议用 `sch zone move` 挪（成功）。

### F8（小） — `standard-parts.json` 的角色键名与实际值不符

`ind.22uh_1210` 的 `value` 是 **2.2µH**（NLCV32T-2R2M-PF / C250183），块注释也写
「2.2µH,Isat≥2A」。键名写成 `22uh` 会让人按 22µH 读——在 1.5MHz buck 上差了 10 倍。
电气没错，只是键名误导；建议改名 `ind.2u2_1210`（改名要连块引用一起改）。

### F9 — `sch gate --strict` 的 `verdict=pass` 结构性不可达（交接文档给的 S4–S6 验收判据本身就够不着）

在一个**完全干净**的页上实测（AUTODL 页，4 件；layout-lint 0 overlap / clusters 0 重叠 /
bridge-check 0 problem）：

```
sch gate --strict → FAIL
  阻塞项:
  • check: 1 个 warn finding: missing-titleblock×1
  • drc:   4 warn-level DRC violation(平台只回聚合数,逐条明细请在 EasyEDA 的 DRC 面板查看)
sch gate（非 strict）→ PASS
```

两个阻塞项**都没有可执行的修法**：

1. **`missing-titleblock`** —— check 的处方是
   `sch titleblock --data '{"Name":"…","Drawed":"…"}'`，而 design-flow S6′ 明确写着
   **「⚠图签写入当前禁用:写路径损毁 sheet 引用→重启丢图框」**（memory
   `sheet-loss-three-mechanisms` 同结论：根治前禁写）。`sch titleblock --help` 里
   **没有任何禁用提示**，照 check 的处方跑就会撞上流程明令禁止的写路径。
   → 判据在要求一件流程禁止的事；两处口径必须统一（要么 check 把它降成 info 并注明
   「图签写入禁用期间不阻塞」，要么解禁写入）。
2. **平台 DRC warn** —— `sch_Drc.check` 只回聚合数（memory
   `schematic-drc-aggregate-only`），逐条明细 API 不暴露。`--strict` 把它算成阻塞，
   等于要求清零一个**看不见内容**的计数。多页设计里这个数还会被跨页 net_port
   结构性抬高（本页 4 件 6 脚全连，仍报 4 条）。

**结论**：交接文档 §二 给 S4–S6 定的「逐页 `sch gate --strict` 出 `verdict=pass`」
在当前代码/平台组合下**任何板子都达不到**。本轮改用「非 strict 逐页 PASS +
把可清的 warn 清零 + 两类结构性挂账如实报」作为该段验收。

### F10 — `sch drc` 说「gate passes」，`sch gate --strict` 拿同一批数说「阻塞」

同一页同一时刻：

```
$ easyeda sch drc  … ✓ 0 fatal, 4 warning(s) — **gate passes** (warnings should still be reviewed)
$ easyeda sch gate --strict  … • drc: 4 warn-level DRC violation → 阻塞
```

单跑的那条命令**自己宣布 gate 过了**，聚合门却拿同一个数判阻塞。谁是门要有唯一口径：
`sch drc` 不该替 gate 下「gate passes」的结论（它不知道调用者用不用 `--strict`），
或者两边共用同一个判定函数。

### F11 — `sch zone move` 搬 note 走的是 `debug.exec_js`，失败后区被搬成半截；且会认领邻区的 note

`sch zone move --zone POWER_IN --dy 55` 回执：

```
✓ zone-move 对账:网表逐引脚一致、无新增 bridge(4 移动 / 0 no-op)
搬移文本 d7abac3f8e831116 失败(器件/导线已移动,文本半移 — 处理后可单独 `sch note` 补):
  debug.exec_js failed: exec_js failed.; additionally, autolayout: move zone note response
  omitted document context; refusing to assume it ran on da7496751e2144f6
```

两个问题：

1. **note 的搬移没有 typed action，走的是 `debug.exec_js`** —— 违反铁律 2 的精神
   （只用 typed action，exec_js 是逃生口），而且它失败时器件已经搬完了，
   区处于「件在新位置、说明在旧位置」的半移状态。
2. **认领错了对象**：`d7abac3f8e831116` 是 **sy8089 那个区**的说明，不是 POWER_IN 的。
   `--text-pad`（默认 60）按邻近判归属，把隔壁区的说明认成了自己的。
   POWER_IN 自己的说明 `dae393790bb7ddfc` 反而在这一串操作后从画布上消失了
   （`prim-delete` 报 `total: 0`，`text-list` 里也没有）。

### F12（小） — `sch group-move --ids` 单件搬会被电气自检拒，但没给「怎么才能搬单件」

`sch group-move --ids <单个器件id> --dx …` → `✗ 电气自检:平移改变了网表(--ids 只搬点名的
图元,器件的桩线/旗不会自动跟随)`。自检拦得对（不该让它改网表），但报文只说了
「不会自动跟随」，没说**要跟随该传什么**。实际可行路径是三条之一：
① 把该件的桩线/旗 id 一起列进 `--ids`；② 先 `sch group create --members <件>` 再
`--group` 搬；③ `disconnect` → `sch modify` → 重 `connect`。报文该直接给出其中一条。

### F13 —— **最严重**：`sch group-move --ids` 报「电气自检失败」却不回滚，把三个器件搬走留下桩线，6 只脚静默悬空

```
$ easyeda sch group-move --ids 687f8bd7797a1910 --dx -130 --dy 0    # D2
✗ 电气自检:平移改变了网表(--ids 只搬点名的图元,器件的桩线/旗不会自动跟随)
```

三条命令（D2/D3/C9）**全部报这个失败**。随后 `sch list` 显示：

| 件 | 命令前 | 命令后 | 我请求的位移 |
|---|---|---|---|
| D2 | 380,510 | **250**,510 | dx -130 |
| D3 | 380,360 | **250**,360 | dx -130 |
| C9 | 600,440 | **250,610** | dx -350 dy +170 |

**三次位移一个不差地全落地了**。`sch check` 随即从 0 悬空脚跳到
`6 floating pin(s)/3 comp`，`bridge-check` 报 6 棵 orphan-tree（旗+桩线成树但不触引脚）。
也就是说：命令**检测到了**自己把网表改坏，**报告了失败**，然后**把坏掉的画布留在原地**。

这是本轮最危险的一条 —— 它踩中「假失败定律」的反面：不是「报失败其实成功」，
而是「报失败、写生效、且写是坏的」。如果我当时信了那句 `✗` 直接往下走，
板子会带着 6 只悬空脚进 PCB。

**修复方向**：自检既然是在写之后跑的，失败路径必须真回滚（把件挪回原坐标），
或者干脆前置——先算出「桩线/旗跟不跟得上」，跟不上就**拒绝执行**（一个字节都别写）。
现在这个「先写后检、检出不管」是最坏的组合。

（顺带印证 F12：报文说「桩线/旗不会自动跟随」，但没告诉你怎么才能带上它们。）

### F14 — `sch destagger --apply` 仍会造成共线合并短路，且「自动恢复」没恢复干净

memory `marker-move-breaks-on-wire-merge` 记的坑本轮**原样复现**。POWER 页
`destagger` 计划把一支 GND 旗从 `left/20` 改到 `down/45`，`--apply` 之后：

```
destagger 内核执行失败(内核已按快照自动恢复,详见错误):destagger 对账不过(判据是电气不是坐标):
1 个新增 wire-bridge(真短路)[C6_N3,GND] …2 个 pin 待手工恢复:L1:1→C6_N3 U3:3→C6_N3
```

回执一边说「**内核已按快照自动恢复**」，一边列出 2 只待手工恢复的 pin。实测是
**没恢复**：紧接着的 `bridge-check` 报 `ERROR wire-bridge nets=[C6_N3,GND] pins=[U3:2,U3:3]`，
`sch check` 报 `multi-net-wire`。板上真真切切有一条短路。

我按处方拆树重连修好了；但随后手工用 `sch connect --pin U3:2 --direction down --offset 45`
（就是 destagger 想做的那件事）**又立刻造出同一条短路** —— 说明这个落点本身就是错的，
destagger 的规划器没把「桩线会不会和邻居共线合并」算进去。

**结论**：`destagger --apply` 在相邻引脚场景下仍不可用；`marker-overlap` 属视觉项，
本轮按「不修、如实报」处理（3 条：POWER×1 / USB_DL×2）。
「自动恢复」这句话在没真恢复时不能印出来。

### F15 — `sch status --all-pages` 结构上只能读到当前活动页，多页工程恒报 3/4 读不到

```
P1 ✓ … / P2 读不到 —— page drift: response came from da7496751e2144f6, want 02a8ba989be213d9
P3 读不到 … / P4 读不到 …
```

切到哪一页，就只有那一页能读，其余三页全是 page drift（memory
`exec-js-context-lags-page-switch`）。**它的处理是对的** —— 整张判定降级成 `?`，
明说「此时任何『已就绪』都不可信」，没有拿一页宣布全局就绪。但结果是这条命令在
多页工程里**给不出有效判定**，而它正是 SOP 里「随时问我在哪一步」的入口。

修复方向：`sch status` 逐页 `doc switch` + settle 后再读（`sch gate --doc <页>` 就是这么干的，
四页逐页跑全都读到了），而不是靠一次读拿全部页。

### F16 — `import-changes` 的 attrs 回填只覆盖 4/32 件，28 件没有 LCSC C 号

```
sync-attrs: 4/4 PCB component(s) backfilled from the device library
ℹ sync-attrs: 28 part(s) without an LCSC C-number skipped:
  [C1 C2 C3 C4 C5 C6 C7 C8 C9 D1 D2 D3 J1 J2 L1 LED1 R1 R2 R3 R4 R5 R8 R9 SW1 SW2 U1 U2 U3]
```

这 28 件**在 `standard-parts.json` 里全都有 C 号**（块就是照它选的型），只是没进 PCB 件属性。
BOM 导出会缺料号 —— 虽然 `scripts/bom-enrich.py` 能事后补，但「库里明明有、导入却丢了」
是链路断在中间，不是工具缺失。

### F17（小） — `layout-lint` 把 `gap 40.0` 判成 `< 40.0`

```
WARN  tight   C2 ↔ Q2  (top side) gap 40.0 mil (< 40.0)
WARN  tight   D3 ↔ U2  (top side) gap 40.0 mil (< 40.0)
```

等于号被算进「小于」。门槛是 `min-gap 40`，恰好 40.0 应当放行。浮点比较该用
`gap < min - eps`，或至少别把 `40.0 (< 40.0)` 这种自相矛盾的话印出来 —— 排查时
会让人以为读数有问题。

### F18 — `layout-lint --gate` 失败时不给下一步，违反铁律 14

```
routability gate: ❌ FAIL — score 44 < min 60; crossings 14 > max 8
routability gate FAILED: score 44 < min 60; crossings 14 > max 8
```

两行同义反复，**没有一条可执行的下一步**。对照 `sch gate` 失败时给的：

```
下一步:
  → 几何重叠:`sch autolayout` 重排,或 `sch modify` 单件挪位…
  → 按 finding 类型分治:duplicate-net-marker 喂 `sch prim-delete`…
```

铁律 14 明写「撞上去的拒绝消息**自带下一条该跑的命令**」。P6 这道门没做到。
它至少该说清：交叉超标先看 `layout-lint` 的逐条 crossing 归因、判断哪些是拓扑性的，
以及 `--max-crossings/--min-score` 是有意提供的降级旋钮（用了要写进交付摘要）。

### F19 — `pcb silk-align` 连接器侧崩溃

```
"code": "EDA_CALL_FAILED",
"message": "Failed to list components for silk-align.",
"detail": "Cannot read properties of null (reading 'map')"
```

32 件已布局、`pcb list` 正常返回的板子上，`silk-align` 读件表就 `null.map` 崩了。
本轮到此停手，未进一步复现（reload 后重试没跑）。

---

## 本轮走到哪、没走到哪

| 段 | 结果 |
|---|---|
| 准备:CLI/daemon/连接器对齐到 `v1.1.1-14-g9614046` / 连接器 `1.1.2` | ✅ 走仓库内热重载链路装入,未做卸载/重导入/重启 EasyEDA |
| 判据 2:`workflow pages --reap` + `spec backfill` | ✅ 成立 |
| 判据 3:同页 wroom1 → led 落点 | ✅ 成立,旧的 `LED1 ↔ U2 图元重叠 6×2` 未复现;晕圈预留警告如预期出现 |
| 判据 4:zone-draw 拒画三档阶梯 | ⚠️ 部分成立 —— 分得开的区确实画得出;但「照抄即可」的命令两次不可执行(F4),且与 zone-arrange 结论矛盾(F5) |
| 判据 1:连接器队列 | ⚠️ **未取到关键证据** —— 全程 `seqAbandoned` 恒为 0,`ACTION_ABANDONED` 一次没出现,即没发生停摆,也就没验证到放弃闸真的会响。倒是看到了新的负载降级播报(`connector looks DEGRADED under load … Worst road(s): schematic.pin.disconnect`),沉默换成了当场说清 |
| S0 方案书 | ✅ `spec validate --strict` 无问题;三条架构决策经用户拍板 |
| S1–S3 落块 | ✅ 4 页(USB_DL 装不下,经用户拍板拆出第 4 页 AUTODL),每页 bridge-check 0 problem tree |
| S4–S6 门禁 | ⚠️ 逐页 `sch gate` **PASS(非 strict)**;`--strict` 结构性不可达(F9) |
| 网表逐网对黄金表 | ✅ 22 张网、无同轨异名、无单引脚网;跨页 `+3V3`/`EN`/`IO0`/`MCU_TX·RX`/`USB_VBUS` 均真连上 |
| P0–P6 布局 | ⚠️ 门 **PASS(降级阈值 `--max-crossings 16 --min-score 40`,经用户批准)**;0 短路/0 重叠/0 间距不足/0 无烙铁通道;布局质量 80.4/100 [good];四档已封章 |
| P7 布线 / P8 叠层+电源+铺铜 / P9 引脚级丝印与 LED 极性 / P10 DRC+check | ❌ **未做** |
| BOM 导出 | ❌ 未做 |

**这块板不是成品**:需求里的「LED 旁 +/- 极性丝印」「4 层电源树(GND 内电层 + 3V3 内电层)」
都在未做的那几段里。

### 降级决策(必须随板交付)

1. **`sch gate` 用非 strict 档**。`--strict` 下每页恒有 `missing-titleblock`(唯一处方被
   design-flow 禁用)+ 平台 DRC 聚合 warn(无逐条明细,无法清零)——见 F9。
2. **P6 可布性门用 `--max-crossings 16 --min-score 40`**(默认 8/60)。三轮重排把交叉从
   45 压到 14 后收敛;残留 14 处逐类归因:自动下载交叉耦合 7 / 模组引脚顺序扇出 4 /
   DTR·RTS 跨 USB_VBUS 2 / CC1×CC2 1 —— 都要靠 4 层板的换层在布线期解决,挪件消不掉。
   真正判据推迟到 P10 的 DRC + post-route check。
3. **页名仍是 P1/P2/P3/P4**,功能名靠 `.easyeda/s0-ceshi.json` 的 `_pageUuids` 表钉住 —— 平台
   改名 API 恒返 false(F1)。

### 遗留的画布状态(下一场直接接手)

- 工程 `ceshi`,4 页原理图 + PCB1(2800×1600 mil 圆角板框,四角 M3 孔,天线全层 keepout 已建)
- 32 件全部在板框内、已封章;`pre_route_passed` 已确认
- 下一步就是 P7:`pcb route-critical` → 电源/差分先布并锁定 → 稠密档交人工点原生自动布线
