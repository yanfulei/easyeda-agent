# esp32MiniRequire 端到端回归 — 缺陷清单（2026-08-26）

跑的是 `esp32MiniRequire.md` 第一节客户原始需求，走 `design-flow.md` 的 S0–S6 + P0–P7.0。
工程 `ceshi`，CLI/daemon `v1.2.0-12-gefbcd28-dirty`，连接器 `1.2.7`，EasyEDA `3.2.149.88089769`。

**状态**：原理图 4 页全部 `sch gate` PASS / `sch check` 0 findings / 20 网全命中黄金表；
PCB 推进到 P7.0（关键网已布并锁定），停在「交用户点原生自动布线」的停点。

下面 12 条按严重度排序。每条都是本轮真实踩到的，附现场证据。

---

## P0 级（数据破坏 / 静默错误）

### #1 恢复段被它自己要修的故障挡在门外（锚件七条连接全丢）

> **归因已订正（2026-08-27）**：最初写的是「命令没有回滚」。查证后并非如此——
> 恢复段**跑了**，是我用 `grep '✓|✗' | head -2` 过滤输出，把它的报告和最终错误
> 一起截掉了。真因见下：恢复段自己被队列阻塞挡住了。连接确实丢了，缺陷成立。

**现象**：`sch group-move --project ceshi --doc MCU_CORE --group g4 --dy 20`
（g4 组内只有 U2 一个 ESP32-S3-WROOM-1 模组，带 9 支 marker）。
命令走「删净 → 重连」，重连阶段报：

```
✗ U2:2 → +3V3 [power]: schematic.power.connect_pin failed: Failed to create pin-stub wire (525,435)→(525,455) after retry — check for a primitive already occupying the endpoint.
✗ U2:1 → GND  [gnd]:   schematic.power.connect_pin failed: Failed to create pin-stub wire (615,260)→(635,260) after retry — ...
```

然后**没有回滚**。回读画布：

```
U2 pins → 只剩 pin2 挂在临时网 $2N60
bridge-check → 1 orphan-stub (U2:2)
```

U2 的 `3V3 / GND(×2) / EPAD / EN / IO0 / TXD0 / RXD0 / IO2` **七条连接全丢**，
而器件位置也没移成（U2 仍在 570,420）。等于花了一次破坏换零收益。

**真因**（同页 `zone move` 的完整输出暴露的）：

```
warn: zone-move 移动失败 —— 立即按快照对全页 4 个引脚重连
zone move 刚移失败:…;恢复重连本身失败(--doc guard: schematic.pages.list failed:
  the connector's action queue is blocked by connect_pin (req_4509) for 4s)
```

恢复段每一步要过 `--doc guard`（一次 `pages.list`），而队列正被**刚才失败的那条
connect_pin** 堵着 —— 火警现场的救火动作，被火本身挡在门外。

而 daemon 的拒绝回执自己就写着 `"this action was NOT sent"`（没发出去，重发安全）
和 `"Next step: wait — this refusal stops the moment the queue drains"`。CLI 侧没等。

**已修**（commit `f286d44`）：在 `requestActionTimed`（所有动作的唯一底层出口，
`--doc guard` 也从这里走）统一等——按错误码判、退避递增、预算 90s，烧完把原始拒绝
抛回去。真机重跑当初炸掉的那个组，删 38 个图元→移 6 件→重连 19 只 pin→对账绿。

---

### #2 交付件「漏了一部分」查不出来 —— 判据是「有没有」不是「够不够」

> **归因已订正（2026-08-27）**：最初写的是「`zone-draw` 清 stale 时把 note 连带删了」。
> 修复时逐条查证推翻了它：`zone-draw` 的清理只删自己记账的 frame id，代码是对的；
> 三条 note 在两次 `zone move` 后也都还在（`zone move` 搬文本走 delete+recreate，
> 但失败时是**安全失败**——没删就没搬）。真相是那两条说明**当初就没写成功**，
> 而我用 `grep '"ok"'` 过滤输出、失败时格式不同就没看见。
> 让它一路溜到交付的，是下面这条判据。

**现象**：`sch check` 的两条交付件判据都只判「一个都没有吗」：

| 页 | 实际 | check 结果 |
|---|---|---|
| MCU_CORE | 6 个模块，只画上 2 个分区框（4 个 text create 失败） | **0 findings** |
| MCU_CORE | 3 个模块，只写成 2 条说明（第 3 条静默失败） | **0 findings** |

判据是 `frameRects == 0` / `notes == 0`，所以「缺 4 个框」与「全都有」是同一个答案。
用户当场问「zone 框选所有元素都不缺了吗」，工具答不上来。

**已修**（commit `580fc3f`）：zones>0 时按模块数判缺口并点名差几个；zones==0
（没有模块记账、证不出该有几个）退回老口径。报文里附上「画框/写说明会部分失败，
写完必须回读复核，别只看退出码」。正负对照都验过。

**仍未解决的上游**：`sch note` / `zone-draw` 的**单条失败不够显眼**——
退出码为 0、失败信息混在正常输出里。判据能兜住漏写，但写的时候仍看不出来。

---

### #3 `spec backfill` 被跨轮次遗留的虚拟组污染

**现象**：`sch block-apply block.ams1117_ldo_3v3 --spec .easyeda/s0-ceshi.json`
只落了 4 个件（U1/C1/C2/C3），却把 spec 里**另外两个还没动过的模块**改写了：

```
spec ✓ MCU.parts:U2 C4 C5 R1 C6 R2 → C1 C10 C11 C12 C2 C3 R1 R10 R11 R2 U1 U4
spec ✓ USB_SERIAL.parts:U3 J2 C7 C8 R4 R5 D3 → C4 C5 D1 J1 R4 R5 U2
```

当时画布上**总共只有 4 个器件**。

**根因**：`~/.easyeda-agent/workflow/ceshi.json` 的 `groupsByPage` 里存着
**5 个已经不存在的页**（上一轮回归留下的，工程早已重建）：

```
02a8ba989be213d9  esp32s3_wroom1_module(C1)/U_3V3 …
48b3fb13df9f2d2e  esp32_autodownload(Q1)
6e8b6ad4d38fedd6  ch340c_usb_serial(C4)/…
da7496751e2144f6  sy8089_buck_3v3(C6) / POWER_IN
fee77ee900c492ff  esp32s3_wroom1_module(esp32_wifi)/…
```

backfill 跨页扫组、按块 id 匹配，匹配上了这些幽灵组。

**影响**：spec 静默错误。文档里已经写明「位号对不上不会报错，只会让 zones set/partition 打分/
连接器规则**静默少算一个模块**，报告照样绿」——这条路径正好制造了这种错误。

**修法建议**：backfill 前用 `sch pages` 取活页集合，`groupsByPage` 里页不在其中的直接跳过并告警；
或 `workflow init` / 工程重建时清理孤儿页记账。

---

### #4 `place-constrained` 把主控规划到板框外

**现象**：板框 2400×1700，`easyeda pcb place-constrained --dry-run`：

```
U2  → (1985, -1090) rot=0 edge=right      ← y 为负，748×1030 的模组整件在板框外
J2  → (1640,  195) edge=bottom            ← spec interfaces 里 J2 声明 edge=top，被忽略
J1  → (2125,  165) edge=bottom
LED1→ (1775,  585) edge=user-facing       ← 0805 指示灯被判成边缘接口件
```

dry-run 阶段即可复现，不需要落地。三个问题：主件出框、spec 的 `edge` 声明被忽略、
`user-facing` 判据把普通指示灯算进边缘件档。

**影响**：分档布局这条主路径在本板上完全不可用，只能手工逐件 `pcb modify` 摆 28 个件。

---

## P1 级（判据失灵 / 拦不住也放不开）

### #5 `sch group-move --ids`：读不到网表就静默放行

> **归因已订正（2026-08-27）**：原写「自检失败不回滚」。查证后**那不是缺陷**——
> 移动已经发生，如实报告而非回滚正是设计意图（#151 部分应用约定），代码注释里写着。
> 真问题只有 A 这一条。



**A. 漏检**：`--ids` 移动 R1/C5/R2 时旗线不跟随（`movedFlags: []` / `movedWires: []`），
造成 **6 个 orphan-tree**，但命令没报任何警告。更麻烦的是
**`sch nets --strict` 也看不出来**（孤儿旗仍带着网名，网表照样 20 网全绿），
只有 `bridge-check` 抓到。

**B. 不回滚**：移动 LED1 时自检报了：

```
✗ 电气自检:平移改变了网表(--ids 只搬点名的图元,器件的桩线/旗不会自动跟随)
  网 GND 成员变了;丢失 LED1.2
  网 LED1_N2 成员变了;丢失 LED1.1
```

`ok: false`，但**画布已经改了**：件移走、旗线留在原地成孤儿。与 issue #151
「部分应用约定」（画布已变就该报 `ok + notApplied`，不该报失败）自相矛盾。

**真因**（真机复现确认）：网表自检本身有效（单件移动能抓到），漏检走的是另一条路——

```go
before, _, berr := readLiveNets(cfg, window)
if berr != nil { ...stderr warning... }
...
if berr != nil { return nil }   // ← 读不到网表 → 移动照做 → 自检整个跳过，退出码 0
```

连接器负载高时 `readLiveNets` 很容易失败，于是静默断网被当成功放过去。

**已修**（commit `4b90f6b`）：明确报「平移已执行但电气自检没跑成」+ 非零退出 +
给出 bridge-check / autoconnect / 改用 `--group` 三条下一步。口径同 gate 的 blocked ≠ pass。
`group list` 的同类问题（在场校验读不到时 note 只在 stderr）一并修。

---

### #6 `zone-arrange` 的 R5 判死了让位器管不到的重叠（两把尺）

**现象**：POWER 页反复输出同一行，整页停手：

```
phase A(PWR_INPUT): J2: 端子重叠 VIN_EXT(left) × GND(left) —— R5 硬不变式(自短路防线)
```

J2 是 KF301 两脚端子（两脚竖排）。三个子问题：

1. **画布现状明明合法**——我已经把 `VIN_EXT=left / GND=down` 摆好了，
   它却重新规划成两旗同向 left，然后撞自己的 R5；
2. **不给可执行的下一步**——只有这一行，没有「该跑什么命令」；
3. **`--json` 在这条错误路径上不输出 JSON**，仍是这行纯文本。

**真因**（靠新加的 phase A 诊断一眼看出来的）：**两把尺，两处**——

- 判据函数不一致：`zfCheckTermOverlap` 用裸 `boxesOverlap`「碰到就算」，而让位循环用
  `zfMarkerCollides`（+ 噪声地板）。`0 < 重叠 ≤ 1.0` 这条缝里，让位器认为让好了、检查器判死。
- 比较的**集合**不一致（真凶）：侧面的 `netport` 按首版规则**不参与让位**（拉进来会把区框
  撑爆），所以也不进 `placedBySide`；同侧的 `netflag` 于是以为左边没人、用默认桩长放下，
  正好压在 port 标签上。而 R5 检查**所有**端子。J2 恰好是 `VIN_EXT=netport` +
  `GND=netflag` 两脚同在左缘。

**已修**（commit `b220cf3`）：把「参不参与让位」提成唯一谓词 `zfTermYields`，让位循环与 R5
共问同一个函数；R5 只在双方都参与让位时才判。真正的自短路（同向且共线）由
`zfCheckPassiveOpposed` 单独把关，负对照测试钉住。三个子问题（判死、不给出路、`--json`
不出 JSON）一并解决。真机：当初卡死 4 轮的 POWER 页现在 **verdict=pass**。

---

### #7 `__ROTPROBE__` 探测旗残留在画布上 —— 已修（commit `e2df7fa`，Go 侧）

**现象**：`bridge-check` 报

```
WARN orphan-flag ORPHAN_FLAG nets=[__ROTPROBE__]  flags: 2693f1f2f947e107
```

这是连接器 `detectRotationNegation` 的一次性探测 flag，用完没删。

> **归因订正（2026-08-27）**：原写「它触发了 #6 的 R5 拒绝」——**不成立**。
> 删掉探测旗后 #6 的错误照旧，#6 有自己的真因（两把尺）。它的真实影响见下。

**真实影响**（真机复现确认）：`bridge-check` 把它算成一条 orphan-flag，于是
`sch gate --strict`（S5 逐页门用的就是它）**FAIL** —— 一块电路完全正确的板子
过不了自己的门。

**根因**：`detectRotationNegation()` 里那句 `delete` **没有回读验证**，而「删除撒谎」
是平台已知病；delete 抛错还会被 `catch` 吞掉，探测旗就永久留下。

**已修**（Go 侧收口，立即生效、覆盖已装的旧连接器）：探测残留单独归类、不计进板子的
orphan 账，但**报出来**并给一条可直接抄的 `prim-delete`。判据是精确网名（用户自己的
`PROBE_5V` 不会被误摘）；挂着真网名的树不算残留。真机：`gate --strict` FAIL → PASS。

**仍未做（治本）**：连接器侧给那句 delete 补回读验证 + 重试。需要重打 `.eext` 并重装。

---

### #8 位号被平台重编后组记账不跟随 —— **误报，已撤回**

> **归因已订正（2026-08-27）**：真机复现（往组里塞一个画布上不存在的位号）显示
> stale 标记**工作正常**：`ZZ99(stale) ⚠ 1 stale (designator not found)`。
> 当初没看到标记，最可能是那一次在场校验读失败（note 只打在 stderr，我没注意）——
> 那条已作为 #5 的一部分修掉（stdout 也说清「本次没做在场校验」）。
> 位号确实被重编了（ESD D1→D3，画布真值），但**工具有能力报出来**，不是缺陷。

<details><summary>原始记录（保留备查）</summary>


**现象**：手工 `sch place --designator D1` 与已落块的 ch340c ESD（当时也叫 D1）冲突，
平台把 ESD 重编为 **D3**。画布真值：

```
USB_DEBUG 页 → C7,C8,D3,J1,Q1,Q2,R4,R5,R6,R7,U3
```

但 `sch group list` 仍然显示：

```
g1  "ch340c_usb_serial(C7)/D_ESD"  1 member(s): D1
```

文档说 `list` 会把 absent members 标 stale，实测**没有标**。

---

### #9 `zone-plan` 的 `labelCollisions` 没有归因 —— 已修（commit `99e9bdc`）

同一份 validation 输出里，`partitionOverlap` 和 `titleBlockHits` 都带 `…Detail`
（点名是哪两个区、差多少、给一条 `sch zone move` 命令），只有 `labelCollisions` 光秃秃一个计数：

```json
{"sheetOverflow":0,"partitionOverlap":0,"titleBlockHits":0,"moduleOutsideZone":0,"labelCollisions":1,"sheetMarginHits":0}
```

`zone-draw` 又因为它拒绝画框。结果是「拦住了但不告诉你拦在哪」，只能靠改字号试。

**已修**：逐条报出哪个区的标题带压住哪个模块、重叠多少，并给两条出路（把模块往下让
最小让量的 `group-move --dy`，或缩小区名字号）。测试钉住「计数与归因条数必须相等」，
防止下次再漏某一项。

---

## P2 级（体验 / 文档 / 可读性）

### #10 `sch titleblock --help` 的字段名是错的

`--help` 示例：

```
easyeda sch titleblock --data '{"Title":{"value":"电源模块"},"Designer":{"value":"Mika"}}'
```

照抄执行 → `ok: true`，但 `sch check` 的 `missing-titleblock` 不消。
实际生效的字段名要看 `sch check` 的报错提示才知道：

```
`sch titleblock --data '{"Name":"…","Drawed":"…","Description":"…"}'`
```

（`Name` / `Drawed` / `Description`，值是裸字符串不是 `{"value":…}`）。
源码里两处写法就是不一致的：`internal/app/cmd_sch.go:175` 是错的示例，
`internal/app/cmd_sch_marker_geom.go:732` 是对的提示。

**影响**：写入静默无效（返回 ok），只有 check 兜住。

---

### #11 块内部网自动命名，导致差分对识别失效

`block-apply` 对没有 PORT 的块内部网按实例首位号命名，于是
**USB D+ / D- 变成了 `C7_N4` / `C7_N5`**。后果：

1. 原理图上看网名完全不知道这是什么；
2. `pcb route-critical` 的差分对**双源识别**（块 `signals` map + 名字模式）
   两条路都匹配不上 → `"diff": "no diff pairs identified"`，USB 差分被当普通信号布；
3. 手工 `pcb diff-pair create --name USB0 --positive C7_N4 --negative C7_N5`
   建好约束后，`route-critical` **仍然**报 `no diff pairs identified`（没读这个约束源）。

**修法建议**：块定义里给关键内部网加显式 `name`（`block.ch340c_usb_serial` 应声明
USB_DP/USB_DM），或让 `block-apply` 支持绑定内部网名；同时 `route-critical`
应把 `pcb diff-pair list` 作为第三个识别源。

---

### #12 `zone-draw` 的 `text create returned undefined`（偶发，非确定性）

**现象**：MCU_CORE 页 6 个区里固定 4 个失败，连跑两次同样是这 4 个：

```
partial: 4/6 zone(s) not applied
  - esp32s3_wroom1_module: text create returned undefined (already retried once after a settle read)
  - U_3V3 / U_IO0 / tactile_boot_reset(SW1): 同上
```

一度以为是确定性的（怀疑区名长度/字符），**但订正**：AUTO_DOWNLOAD 页也报过同样的错，
等 15 秒后原样重跑就成功了。所以是负载 / 时序相关，现有的「retry once after a settle read」
退避不够。

---

## 附：成本画像里的一条硬数据

`easyeda audit cost --day 2026-08-26 --since 07:36 --until 11:06`（已入台账）：

```
schematic.power.connect_pin   1131.2s(56.2%)   286 次  均 3.96s  失败 63
```

**`connect_pin` 吃掉 56% 的 daemon 耗时，失败率 22%（63/286）**。
这些失败绝大多数是「假失败」——报 `connector did not respond` 但写其实已经落地
（本轮至少 6 次实测：EPAD / TXD0 / EN / IO2 / SW2:2 / R6:1 都是报错后回读发现已连上）。
还有一类是 `Netport create did not settle within 7000ms — the stuck-at-99% hang`。

后果是每次写完都必须回读对账，无法信任返回值；本轮相当一部分时间花在
「等队列解堵 → 回读 → 补漏」的循环上。这条不一定是我们能修的（平台侧），
但它是所有「删净→重连」类命令（#1 / #5 / `zone move` / `destagger` / `group tidy`）
不稳定的**共同上游**——它们的失败几乎都发生在重连阶段。

---

## 一并记下的两条小观察

- **`doc ls` 与 `health` 会矛盾**：PCB1 从 `doc ls` 列表里消失，同时 `health` 报
  `documentType: pcb`（当前文档就是它）。此时 `--doc PCB1` / `--doc <uuid>` 全部报
  `no document named or with uuid`，只能省掉 `--doc` 靠活动文档兜住。
- **`doc switch` 的参数形式与其它命令不一致**：它是位置参数（`doc switch PCB1`），
  而全局是 `--doc PCB1`；写成 `doc switch --doc PCB1` 报 `accepts 1 arg(s), received 0`。
