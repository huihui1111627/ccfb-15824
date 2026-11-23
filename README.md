# CircusNet — 马戏团网络的虚拟化控制面

一个用 Go 写的、可运行的网络虚拟化控制面模拟器：几十顶帐篷的交换机用隧道组成二层叠加网络，
地址学习靠"控制面路由"在交换机之间交换，交换机、设备、链路全部在程序里模拟，
并带一个可以操作、可以逐跳核对转发路径的网页面板。

```bash
go test ./...                 # 单元测试 + 需求逐条验证 + 种子化压力测试
go run ./cmd/simcheck         # 固定种子的全量压力测试（几十顶帐篷、几千台设备）
go run ./cmd/circusnet        # 打开网页面板 http://127.0.0.1:8080
```

## 结构

```
internal/model    设备、交换机、虚拟网络、冗余组、链路的领域类型
internal/fabric   控制面：路由类型/RD/RT、RIB、导入策略、FIB、底层最短路
internal/sim      拓扑生成、收敛引擎、真实位置模型、转发校验、压力测试
internal/web      HTTP API 与单页面板
cmd/simcheck      固定种子压力测试与报告
cmd/circusnet     网页面板服务
```

## 需求与实现对应

1. **本地学到 MAC 后发带 IP 的主机路由，其他交换机凭路由代答 ARP**
   `buildLocalRoutes` 为每个单归属设备生成本地 `host` 路由（MAC+IP，`RouteHost`）；
   `VNFIB.Lookup` 在其他交换机上直接命中主机路由，`VerifyAll` 同时审计"每台交换机对每个已知主机都能
   本地解析"（ARP 代答率），测试 `TestARPStaysLocal` / `TestBroadcastAccounting`。

2. **RD/RT 由交换机标识与网络编号自动生成，按 RT 导入，不属于本机虚拟网络的路由不进转发表**
   `fabric.RD(loopback, vni)`、`fabric.VNRT(vni)`；`ImportPolicy.Accept` 同时校验 VNI 与 RT；
   `ActiveRoutes` 只返回本机虚拟网络接受的路由，`buildSwitchFIB` 只装这些。
   邻接表保留收到的路由（BGP Adj-RIB-In 语义），导入策略在装表时生效，所以加入虚拟网络后立刻生效。
   测试 `TestRTImportPolicy`。

3. **外部网段用独立前缀路由类型，与主机路由分开存；更具体的主机路由优先**
   `RoutePrefix` 与 `RouteHost` 是不同路由类型；FIB 中主机绑定表与外部前缀表分开存放；
   `Lookup` 先查主机绑定再查前缀，外部主机 /32 覆盖 /16 前缀。
   前缀的出口按"本地边界优先，否则最小交换机标识"确定，主机 /32 仍可指向另一台边界。
   测试 `TestHostBeatsPrefix`。

4. **冗余组：两台交换机各发一条带组标识的路由；断一条链路只撤这一条组路由，整组一次性切换**
   `RouteGroup` 携带组标识与整组设备列表，两台成员各发一条；
   接入链路断开时 `buildLocalRoutes` 自然少发一条组路由，`buildAllLocalRoutes` 的差分只产生
   1 条撤消；远端 FIB 重新解析整组出口到存活成员。
   测试 `TestGroupFailoverSingleWithdrawal`（断言恰好 1 条撤消、整组设备切换、转发全部正确）。

5. **冗余组内交换机从对端收到的广播不再往该组接入口转发**
   成员路由携带本机的冗余组标签，`egressSuppressedFor` 用标签判断"入口是否在该组共享网段"，
   只抑制该组接入口。测试 `TestGroupSplitHorizon`。

6. **每台交换机为每个加入的虚拟网络发一条成员路由；其他交换机据此建隧道；退出即撤路由、拆隧道**
   `RouteMember` 携带 `Ready`（端口是否开放）与冗余组标签；隧道由远端成员路由生成，
   成员路由撤消后隧道消失。测试 `TestMemberRoutesBuildTunnels`。

7. **不同虚拟网络之间在本机完成互通，网关地址全网一致**
   每个虚拟网络的网关是 anycast 地址，所有交换机配置相同（`Config.FullMeshVNs`）；
   `forwardRouted` 在入口交换机本地完成跨虚拟网络一跳，再从同一台交换机走目的虚拟网络的表。
   测试 `TestAnycastGatewayLocalRouting`。

8. **交换机重启先等路由收敛再开端口，期间旧路由按存活时间清理**
   重启提升 epoch、清空自己的邻接表、端口保持关闭；`step` 只在"本轮完全无变化"时才尝试开端口，
   并且要求 RIB 中没有陈旧路由、所有可达对端都已收到成员路由。
   旧 epoch 路由立即不用于转发，`Expire` 按 TTL 删除；新 epoch 到达时触发对端全表刷新。
   测试 `TestRestartWaitsForConvergence` / `TestRestartAgesOutPreRestartRoutes` / `TestStaleRouteTTL`。

## 转发校验怎么算"对得上"

校验器不复用控制面代码，而是**独立**从真实位置推导期望路径：

* `truthGraph` 只用"交换机在线、端口开放、链路 up"的物理拓扑做 BFS；
* `truthAttachments` 给出设备当前真正的接入交换机（单归属、冗余组共享网段、外部网段边界）；
* `expectedEgress` 按文档化的放置策略给出应到达的最后一跳；
* `Forward` 真正按 FIB 逐跳走（隧道算一个逻辑跳），把每段隧道的物理路径拼接起来；
* `checkPhysical` 断言：终点等于期望、物理路径无重复交换机（无环路/无重复帧）、
  每段链路 up、物理路径与真实最短路完全一致。

结果：几千台设备的全量两两校验（`-exhaustive`）在收敛后 0 失败、0 环路、0 广播。

## 压力测试

```bash
go run ./cmd/simcheck                        # 32 顶帐篷 / 4000 台设备 / 50+ 随机变更
go run ./cmd/simcheck -devices 600 -tents 10 -exhaustive   # 小规模全量两两校验
go run ./cmd/simcheck -json report.json      # 输出完整 JSON 报告
```

每次变更后都会：收敛 → 抽样两两校验 → 统计收敛轮次、撤消数、广播量。
报告按变更类型汇总平均收敛轮次，并统计"一次撤消完成整组切换"的次数。
最后可加 `-exhaustive` 做一次全量两两校验。

固定随机种子保证可复现；`internal/sim/requirements_test.go` 里的 `TestSeededStress` 和
`cmd/simcheck/main_test.go` 里的 `TestStressIsDeterministic` 也在 `go test` 中跑。

### 上次跑出来的数字（`go run ./cmd/simcheck`，种子 20261124）

| 项目 | 数值 |
| --- | --- |
| 规模 | 32 顶帐篷 / 154 台交换机 / 4012 台设备（1400 双归属、12 外部主机）/ 6 个虚拟网络 / 175 个冗余组 / 195 条链路 |
| 随机变更 | 60 步（25 次挪设备、12 次断链路、6 次断电、5 次重启、4 次断接入链路、2 次退出虚拟网络） |
| 两两校验 | 4 828 097 次，0 失败，0 环路，0 广播帧 |
| ARP 代答 | 30 997 264 / 30 997 264 = 100% |
| 收敛 | 平均 2.27 轮/变更，最长 5 轮（重启交换机） |
| 冗余组切换 | 4 次"一条撤消、整组切换" |
| 耗时 | 约 28 秒（并行收敛与校验） |

收敛轮次按变更类型：挪设备 2 轮、断/恢复链路 2–3 轮、接入链路故障 2 轮、交换机断电 2 轮、
交换机重启 4.4 轮（重启需要多等一轮确认安静再开端口）、退出虚拟网络 2 轮。

## 网页面板

`go run ./cmd/circusnet` 后打开 <http://127.0.0.1:8080>：

* **地址表**：按交换机列出学到的设备硬件地址、网络地址、路由类型与来源、出口；
* **路由类型与来源**：host / prefix / group / member 四类，含 RD、RT、发起交换机、学习自哪台交换机；
* **隧道列表**：本地/远端、虚拟网络、下一跳、跳数与完整物理路径，可一键断/恢复；
* **时间线**：设备迁移、链路断开/恢复、接入链路断开/恢复、交换机断电/重启、加入/退出虚拟网络；
  冗余组面板可以断开一条接入链路，页面会显示"只撤一条组路由、整组设备改走对端"；
* **转发路径校验**：输入两台设备的 MAC 或 IP，页面展示逐跳路径、物理路径、期望结果是否一致、广播次数。

顶部可执行"随机变更 / 挪一批设备 / 全网校验"，每次操作后自动收敛并给出校验摘要。
