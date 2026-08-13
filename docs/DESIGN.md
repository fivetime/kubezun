# kubezun DESIGN — KNaaS 逻辑虚拟节点（KAaaS 平台 B2' 算力线）

> 定稿于 2026-08-06 会话（两轮多 agent 源码研究 + 多轮架构讨论）。
> 本文档是 kubezun 重写的锚：**改代码前先读它；实现中发现文档写错了，先改文档再改代码**（与
> kubetron/kaaas 文档同一约定）。姊妹文档：`/root/kubezoo-gateway/docs/kaaas-platform-architecture-cn.md`
> （平台全貌）、`/root/kubetron/DESIGN-refactor.md`（OVN 数据面）。
>
> 引用约定：`zun.go:NN` 指本仓库现有代码；`(Zun) xxx.py:NN` 指 `/root/k8s-zun-provider/openstack/zun`；
> `(VK) xxx.go:NN` 指 `/root/k8s-zun-provider/virtual-kubelet`；`(kubetron)`、`(kubezoo)` 同理。

---

## 1. 定位与产品矩阵

**kubezun = KAaaS 平台的第二条算力产品线（B2'）：pod 落成 Zun capsule（Kata 隔离），
算力归属租户自己的 OpenStack project。**

> ⚠️ **2026-08-13 重大定案，推翻了本文档此前的核心叙事。** 之前 B2' 的卖点写的是
> "完整集群幻觉"——逻辑节点可见、DaemonSet 扇出、配额即容量。**这三样现在全部放弃。**
> 改动理由见 §1.2，被推翻的旧形态见 §13。读旧版记忆的人请以本节为准。

它与 B1 的关系是**同一租户体验、不同算力来源**，不是体验档位：

| | **B1（kubezoo+kubetron）** | **B2' KNaaS（kubezun + Zun fork）** |
|---|---|---|
| 租户体验 | "能跑 pod 的命名空间"：workload/Service/DNS 完整；`kubectl get no` 为空；无 DaemonSet | **完全相同**——租户看不出区别 |
| 算力 | 平台共享 kata 节点池，K8s 记账 | Zun capsule，**归属租户 OpenStack project**：与 Nova VM 共用同一套 Placement 库存与 project 配额（kaaas §2.4 翻案条件） |
| 网络 | kubetron 全量（port 接入 + 编排） | Zun 原生 port + **kubezun 自建编排**（§7、§14.4） |
| 探针/logs/exec | 原生 kubelet 白拿 | Zun fork：容器内探针 + logs + ExecSync（§6，已实现） |

两者可同租户混用：B1 pod 与 B2' capsule 都满足 podIP==OVN IP 不变式，同一个
EndpointSlice / 同一个 Octavia LB 后面可同时站两种后端，切换后端时 Service 流量无缝过渡。

**产品叙事**：**"同样的 Kubernetes 体验，算力来自你自己的 OpenStack project"**。
平台按租户选后端，租户感知不到。这比"两个体验档位"更省——两档意味着两套租户文档、
两套排障路径；一套体验、可切后端只有一套。也正是 §1.1 说的"分层红利"真正兑现的形态。


### 1.1 为什么必须是 VK + Zun（结构论证）

**Node 对象不是一条记录，是一份契约**：注册者必须履行 kubelet 的全部义务——lease 心跳 +
status 上报；watch 绑定到本节点的 pod 并真正执行；回写 pod status/podIP；提供 :10250
kubelet API（logs/exec）。

- kubezoo 是无状态协议翻译器（自身零 controller，实际工作全靠上游控制面），kubetron 只为
  已被真实 kubelet 运行的 pod 编排网络——两者都不是、也不该是履约人。让 kubezoo 注册
  Node，pod 会永远卡在 Pending。
- 履约代码 = 心跳循环 + pod 控制器 + 执行后端 + kubelet API server，**这四样加起来的名字
  就叫 virtual-kubelet**（作为库引入，无锁定风险）；执行后端要满足"强隔离运行时 + 算力归属
  OpenStack project + 原生 Neutron port + pod 语义"四条硬约束，**现成度最高的实现叫 Zun**。
- 分层红利：provider 接口之上的一切投资（kubezoo 视图、placement、Kyverno 策略、
  kubetron 编排）只与 K8s API 对象交互，对后端零感知。换后端的爆炸半径被严格限制在
  provider 实现 + 探针/logs 通道一层。

⚠️ **本节论证的是"要 Node 就必须有 VK"，不是"要每租户一个 Node"。** Node 是契约这一点
与节点归谁无关——契约由谁履行（VK）和节点服务多少租户（§1.2）是两个正交问题。

### 1.2 为什么放弃每租户虚拟节点与 DaemonSet（2026-08-13）

> ⚠️⚠️ **先澄清一个已经被误读两次的点：放弃的是"每租户一个节点"，不是"节点"。**
> **节点仍然存在，而且不能不存在。** 变的只有三样：不再属于租户、租户看不见、数量不随
> 租户增长（`regions × K × AZs × archs`，§3.4）。
>
> **为什么零节点走不通**（两条都已查证，不是推理）：
> - `PodGC` 会删掉 `spec.NodeName` 指向不存在节点的 pod（k8s `podgc/gc_controller.go:228`
>   `gcOrphaned`，隔离期 40s）。没有 Node 对象，capsule pod 会被上游当孤儿清掉。
> - VK 靠 `spec.nodeName` 的 informer 过滤认领 pod（`nodeutil/client.go:53-58`）。
>   没有节点名就没有认领边界——而这个边界正是 §4 里"唯一不可绕过的授权边界"的依据。
>
> 要真的拿掉 Node，就得让 pod 根本不进上游控制面，那意味着 kubezoo 自己终结整套 pod API
> 并重写控制器栈——是 cell / Kamaji 那个量级的另一种产品形态（§13）。

**不是因为做不到——已经做出来并端到端跑通了——而是因为它的成本与它买到的东西不成比例。**

**成本（实测，非估算）**：虚拟节点是每租户的（`NodePoolFor(tenantID) = tenantID`，
kubezoo-contract `placement.go:57`），所以

$$\text{节点数} \propto \text{租户数}$$

而**一个空闲租户在 K8s 侧是全价**：1 个 Node（实测 4,614 B）+ 1 个 Lease（707 B）+
每 10 秒一次心跳（lease 续约 = `leaseDuration × 0.25`，`lease_controller_v1.go:47`）+
一个 **66–84 MB** 的 VK 进程（实测，⚠️ 旧 §3.5 估的 10–50MB 偏低）。
§3.5 说"空节点在 OpenStack 侧零成本"是对的，但**只对了一半**：K8s 侧不是。

**买到的东西**：`kubectl get no` 非空 + DaemonSet。而

- **"租户零 worker 节点"从来不是差异**——kaaas §2.2 早已论证 B1 同样做到，本文档旧版
  自己也写着"不构成差异"；
- **DaemonSet 的机制价值在逻辑节点上趋近于零**（旧 §9 自己承认），保留的是 chart 兼容；
  而 AWS Fargate 明确不支持 DaemonSet，仍是成功产品——这不是必需品。

**决定性的信号**：这一刀让四个正在讨论的补丁**同时失效**——注入 `spec.nodeName` 绕过调度器、
无限期 toleration 中和 NLC、拉长 lease 降心跳、惰性节点。它们全都是在给"节点数 ∝ 租户数"
这一个前提打补丁。**一个改动让四个补丁一起没用，说明砍在了正确的地方。**

**保住的是唯一那条真差异**：算力归属 OpenStack project。capsule 仍落在租户的 Keystone
project、仍与 Nova VM 抢同一批 Placement 库存、仍走 Zun 的 project 配额。kaaas §2.4 的
翻案条件**一个字没动**。

⚠️ **诚实的代价**：如果有客户是冲着"我看得见我的节点、我能跑 DaemonSet"来的，这一刀砍在
他身上。这是业务判断，不是技术判断。

⚠️ **还有第三档，不在本文档范围内**：Zun 原生的"容器即虚机"——在 Horizon 里点几下建一个
容器、开个终端就能用，面向不需要理解 K8s 和集群概念的用户。它与 B2' 共用同一套
containerd + kata + VMM 和同一份 OpenStack 资源账，只是入口从 kubectl 换成 zun-ui。
本文档只讲 kubezun（K8s 那半）；**那一档的定案、驱动分工与实现进度见
`/root/k8s-zun-provider/openstack/zun/FORK.md` §4**，改 Zun 前先读它。

> ⚠️ **未决，且影响 §7.7 的执行强度**：第三档给租户 Horizon 入口，意味着租户持有该
> OpenStack project 的 Keystone 凭据。若两档共用同一个 project（"同一份 OpenStack 资源账"
> 这句话暗示如此），则**租户可以直接改掉 kubezun 建的 Neutron 安全组**——基于安全组的
> NetworkPolicy 执行对这类租户**本来就是建议性的**。这需要产品侧确认两档是否同 project；
> 若是，必须写进租户文档，而不是假装严密。见 §4.6 的前提声明。

## 2. 分层架构与组件边界

```
租户 kubectl
  │
【视图层】kubezoo-gateway/controller/contract
  证书 OU/SA token 识别租户、ns/name/group 三前缀改写、impersonate <tid>-admin。
  不伪造 Node（三处豁免已删并实测，kaaas 文档 §7.1）；⚠️ 2026-08-13 起虚拟节点
  是平台对象，不带 <tid>- 前缀，租户视图中 `get no` 为空（与 B1 一致）。
  │
【准入层】Kyverno / VAP / MAP
  写路径守门：特权屏蔽、pod-security（替代可绕过的 PSA，kaaas §8.2.1）、placement
  注入、nodeName 禁写。碰不到读路径（kaaas §8.0）。
  │
【调度层】上游原生 kube-scheduler
  按 knaas.io/serverless 污点 + 平台注入的 nodeSelector 落到虚拟节点。
  ⚠️ 落哪个虚拟节点由平台 placement 决定，与租户身份无关（§4.6 才是租户边界）。
  │
【算力层】B1: kata 真实节点池 + 原生 kubelet（主路）
         B2': kubezun 共享虚拟节点 → Zun capsule（本文档）
  │
【数据面】OVN/Octavia（B1 由 kubetron 编排，B2' 由 kubezun 自建编排，两者共存互不干涉）
  Service = Octavia OVN LB（member = OVN IP，EndpointSlice 驱动）、租户 DNS zone、
  VIP 独立子网 + tenant router。K8s Service CIDR 与数据面无关。
  │
【身份/配额】Keystone application credential + Neutron RBAC + K8s ResourceQuota
  ⭐ **租户边界 = OpenStack project，而 namespace 是它在 K8s 侧的名字**
  （namespace → project 多对一，§4.6）。⚠️ 节点不再承担任何租户边界职责。
```

**kubezun 自身的部署形态（2026-08-13 定案，取代"每租户一进程"）**：

$$\text{进程数} = \text{regions} \times K \qquad \text{节点数} = \text{进程数} \times \text{AZs} \times \text{archs}$$

⚠️ **三层关系图与"为什么每层都不能合并"见 §3.4.1**——`region → 进程 → Node` 中间两段
**都是一对多**，"一个 VK 实例 = 一个 Node = 一个 region"只在最简配置下碰巧成立（§3.4.3）。

- **region 是硬边界，不是选择**：一份 `Credentials` 里的 `Region` 解析**全部**服务端点——
  Zun（`zun/client.go:89`）、Neutron（`netpol/client.go:20`）、Cinder/Manila
  （`volume/client.go:19,29`）、Octavia、Barbican。一个进程跨不了 region。
  ⚠️ token 本身不受 region 约束（Keystone 认证与 catalog 选点是两步），所以"一 region
  一进程"是工程上的干净选择，不是身份层强制。
- **K 是唯一自由度**，租户分配到 K 个分片（分配方式见 §2.1）。它**同时**决定两件事，
  往哪边拧都要付另一边的价：
  | K | 爆炸半径 | 节点数 |
  |---|---|---|
  | = 租户数 | 1 个租户 | ∝ 租户数 ← 旧形态，规模墙 |
  | 中间值 | 1/K 租户 | 与租户数**无关** |
  | = 1 | 整个 region | 最省、最脆 |
- **分配方式：声明式，不是哈希取模**（2026-08-13 改，见 §2.1）。⚠️ 曾经写过"K 一次定死、
  不要当运行时可调参数"——**那是哈希取模的限制，声明式没有这个限制**，已作废。
- **一个进程死了会怎样**（已验证，不是推演）：capsule 继续跑；探针继续执行、liveness 继续
  重启容器（zun-compute 自己的周期任务 `manager.py:1393` + `driver.py:992`，不经控制面）。
  **冻结的是**：pod status 回写、pod 创建/删除、NetworkPolicy 同步、Service 的 Octavia
  member 同步。⚠️ 因此**摘流兜底只剩 Octavia health monitor**（`service/reconciler.go:344`，
  TCP/UDP-CONNECT），而它测不出"端口开着但应用坏了"——见 §6 的运维契约。

**§13"单进程多租户 VK"的翻案条件已满足**（该行原文：*凭据外置 + informer 白名单 +
per-node 身份三件事完成后可作为成本优化重评*）：

| 条件 | 状态 |
|---|---|
| informer 白名单 | ⚠️ **只完成了一半，见 §2.2**。ConfigMap/Secret 走 `ObjectReader` 按对象 GET（对），但 Services/EndpointSlices/Ingresses/NetworkPolicies/**全部 Pod**/PVC/PV/StorageClass **仍是全集群缓存**（`vknode.go:143` 的 `scmFactory` 无任何过滤）——**这是要补的第二刀** |
| per-node 身份 | ✅ 不受影响。节点仍然存在（只是不再每租户一个），per-node `:10250` + 独立证书 + `WebhookAuth(nodeName)` 照旧 |
| 凭据外置 | ⏳ **这次要做的**，见 §4.6 |

⚠️ 原否决理由里"全集群 secret 缓存集中"**只对了一半**：ConfigMap/Secret 那一半我们第一天
就绕开了（`ObjectReader`），但另外八类对象至今仍是全集群缓存（§2.2）。真正剩下的代价是
**凭据集中**与**panic 爆炸半径**，这两条由 K 这个旋钮定价，不再是"有或无"。

**凭据纪律**：每租户一份 Keystone application credential（unrestricted=false、限定 role、
设 expires_at）。**严禁 admin 凭据**——Zun admin context 强制 all_projects=True
（(Zun) api/utils.py:70-71）+ DB 查询不加 project 过滤（db/sqlalchemy/api.py:111-118）
+ 按名跨项目查找（同文件 215-228），是现成的跨租户读/删洞。客户端构造直接复用
(kubetron) pkg/neutron/provider.go 的 `NewClientFromAppCred`（gophercloud v2）。
**存放位置与绑定规则见 §4.6——那一节是本次改动的承重件。**

### 2.1 分片分配：声明式，抄 kubetron（2026-08-13 定案）

**租户属于哪个分片是声明出来的，不是算出来的。**

原方案是"租户稳定哈希 % K"。改掉它的理由只有一条，但是决定性的：**改 K 就是重新取模，
绝大多数租户要同时搬家**，而搬家的过渡期里同一租户短暂有两个所有者——直接违反 §7.7.5c。
于是 K 变成一次性的、不可调的、而且没有依据可定的数（§3.5 的规模实测至今未做）。

**kubetron 已经解决过这个问题**（`pkg/webhook/claim_webhook.go:15-27`、
`NamespaceShard()` :77-89）：分片写在 namespace 的 `kubetron-network` ConfigMap
`shard` 键上，缺省落 `DefaultShard`，operator 可以显式覆盖。

抄过来的三条：

| 抄什么 | 为什么 |
|---|---|
| **声明式归属** | **重分配的单位变成一个租户**，一个一个搬，不惊动其他人。"K 一次定死"这条限制随之消失——K 可以随时增加，把租户逐个迁进新分片 |
| **服务端标签过滤** | kubetron 注释：*"a shard neither receives nor stores the other shards' objects"*。见 §2.2 |
| **缺省分片** | 没声明的落缺省，不是启动失败——开通流程有先后，不该因为一个字段没写好就拒绝服务 |

⚠️ **有一条不能抄：kubetron 的归属判定有两套，我们只能有一套。**
它的 claim 是**创建时打标**（之后不变，粘滞），而 Service/DNS reconciler 是
**每次 reconcile 读 ConfigMap**（立即翻转）。于是同一个 namespace 可能出现
"claim 归分片 A、Service 归分片 B"。对 kubetron 或许无害，**对我们是禁止的**——
§7.7.5c 要求一个租户的 peer 同步只能有一个所有者，两套判定不一致**就是**双所有者。
⇒ **单一真相源：租户的分片归属只存一处**（Tenant CRD，与 project id 同处，§4.6.3），
所有控制器读同一个字段。

⚠️ **声明式没有消除交接问题，只缩小了它**。把租户从分片 A 移到 B 仍然必须
**先停 A、确认停干净、再起 B**，不能重叠。区别是：以前这个动作的单位是"全体租户"，
现在是"一个租户"，可以逐个做、可以回滚、出问题只影响一个。

### 2.2 ⚠️ informer 收窄：现存缺陷，不是新形态才有的

`vknode.go:143` 的 `scmFactory` 是 `NewSharedInformerFactoryWithOptions(client, resync)`
——**没有 namespace 限制、没有 tweak、没有任何过滤**。它背后是八类对象：

`services` / `endpointslices` / `ingresses` / `networkpolicies` / **`allPods`** /
`persistentvolumeclaims` / `persistentvolumes` / `storageclasses`

**全部全集群缓存。** 只有 per-node 的那个 pod informer 有 `spec.nodeName` 字段选择器
（`vknode.go:204-206`）。

**这与 §2 自己的论证自相矛盾。** `ObjectReader` 特意不用 lister，注释写着：

> Deliberately not a lister. ... this process serves one tenant: a cache wide enough to
> answer for every namespace the tenant may create is also wide enough to answer for
> another tenant's

**同一个担忧，两种做法**：ConfigMap/Secret 严防死守，Services/NetworkPolicies/全部 Pod/PVC
却照单全收。

**实测（2026-08-13，实验床仅 2 个租户）**：全集群 67 个 pod，属于租户 111111 的只有 11 个
——它的进程缓存了全部 67 个。Services 26 / 6。**在只有两个租户时就已经是 6 倍过取。**

⚠️ **分片形态下这会从"浪费"变成"抵消"**：K 个进程 × 全集群缓存 = 内存与 watch 流量按
`K × 集群规模` 增长，而不是按集群规模。**分片本来是为了省，这样反而更贵。**

**修法**：⚠️ **2026-08-13 查证：服务端过滤这条捷径不存在**——实测 pod/svc 上没有任何
租户标签（kubezoo 不打），informer 也无法按"namespace 的标签"过滤对象。唯一路径 =
**动态 per-namespace informer 工厂 + 聚合 lister**：启停机制 `vknode` 已有
（per-node pod 工厂 + `Namespaces.OnChange`），缺的是跨 namespace 的聚合 Lister。
完整的中型工程，单独排期。
⚠️ **但 `allPods` 是个例外，不能按 namespace 收窄到"本进程服务的租户"**：它的注释写明
*"A policy's peers are pods wherever they run"*。收窄它之前先确认 peer 选择器的作用域——
NetworkPolicy 的 peer 不跨租户（选择器带 namespace 限定，§7.7.1），所以按**分片**收窄
是安全的，按**单个租户**收窄不是。

---

## 3. 逻辑节点规格

**本质：逻辑节点不是机器的化身，也不再是租户的化身，是"调度目标 + 节点语义 API + 一组
拓扑坐标"的呈现物。** 背后没有宿主机，有的是一个 region 里的 Zun 计算池。

> ⚠️ **2026-08-13：节点不再属于租户**（§1.2）。租户看不见节点（与 B1 一致），节点是
> 平台对象，一个节点服务分片内的全部租户。本章通篇按新形态重写；旧形态里"节点 = 租户的
> 配额分区"那套已作废，见 §13。

### 3.1 对象模型（平台对象，租户视图中不可见）

> ✅ **2026-08-13 已实现，但作为与旧形态并存的第二形态**：`--shard`（shard 标签 +
> `knaas.io/serverless=true` 污点、无 pool）与 `--tenant`（pool 标签 + tenant 污点，
> **原样保留**）二选一。⚠️ 不能就地切换——实验床还在跑每租户单元、kubezoo 还在注
> pool selector，改身份会搁浅现有 pod。正式切换 = kubezoo `NodePoolFor` 改造同步上线。
> 节点名四坐标是约定，代码不强制。

```yaml
apiVersion: v1
kind: Node
metadata:
  name: knaas-r1-s07-az1-amd64        # region-分片-AZ-架构，四个坐标都在名字里
  labels:
    type: virtual-kubelet             # 系统 DS 排除锚点（(VK) controller.go:296-302 默认标签）
    topology.kubernetes.io/region: r1 # ⚠️ 必须有：多 region 下 AZ 会重名，见下方警告
    topology.kubernetes.io/zone: az1  # 真实语义：capsule 落该 AZ 的 Zun 资源池
    knaas.io/shard: "07"              # 分片身份，供运维定位；不参与调度
    node-role.kubernetes.io/serverless: ""
    kubernetes.io/os: linux           # ⚠️ well-known 三件套必须齐——大量标准 chart 默认
    kubernetes.io/arch: amd64         #    nodeSelector {kubernetes.io/os: linux}，缺失则
    kubernetes.io/hostname: knaas-r1-s07-az1-amd64   # helm install 全部 Pending 且极难排查
    node.kubernetes.io/instance-type: knaas.serverless
spec:
  taints:
  - key: knaas.io/serverless          # ⚠️ 不用 virtual-kubelet.io/provider 默认污点——
    value: "true"                     #    会被通用 chart 的全容忍规则误踩
    effect: NoSchedule                # 值不再是 <tid>：节点不属于任何租户
status:
  capacity:                           # 静态大额，不再镜像配额（§3.2 已改写）
  addresses:
  - type: InternalIP
    address: <VK 实例 IP>             # logs/exec 经 apiserver 回连 :10250 的前提
  daemonEndpoints:
    kubeletEndpoint: { port: 10250 }
  conditions:
  - type: Ready                       # = VK 存活 ∧ Zun API 可达（§3.3）
  nodeInfo:
    kubeletVersion: v1.36.3-knaas.1   # ⚠️ semver 兼容格式——operator 会解析它做特性门控
    containerRuntimeVersion: zun://kata-3.x   # 诚实声明，不伪装 containerd
    operatingSystem: linux
```

⚠️ **`topology.kubernetes.io/region` 是新增的，且必须先于第二个 region 上线。**
⭐ **2026-08-13 优先级上调**：多 region 不是"可能会有"，是**逻辑流轴的必然结果**——
OVN 的分片单位就是 region（§7.4.2），而 NetworkPolicy 把我们推向那堵墙。
所以这一条**排期内必做**，不是"将来再说"。
今天节点只带 zone，而 PV 的 nodeAffinity 也**只按 zone 匹配**
（`volume/reconciler.go:632-634`）。单 region 下无碍；一旦一个集群里同时挂多个 region，
`r1/az1` 与 `r2/az1` 就会撞——在 r1 建的 Cinder 卷，其 PV 会匹配上 r2 的 az1 节点，
pod 调过去挂不上。症状正是 `reconciler.go:623-627` 那段注释描述的
**"claim 停在 Bound、pod 停在 Pending，两个对象里都没有任何东西说为什么"**——当初写它是
为了防跨 zone，现在会以跨 region 的形式复发，判据完全一样。修法很小（节点加标签 +
`MatchExpressions` 加一条），**但必须在引入第二个 region 之前做，否则是静默错配**。

### 3.2 容量：静态大额 + 双闸门（2026-08-13 改写，取代"配额镜像"）

**旧定案"capacity 实时镜像租户 ResourceQuota"随每租户节点一起作废**——一个共享节点镜像
不了任何单个租户的配额，而"镜像分片内全部租户配额之和"是个没有意义的数。

新形态：capacity 报一个**静态大额**（够大到不成为瓶颈），真正的把关落在两道闸门上，
**两道都是真的**。✅ 已实现（2026-08-13）：`--platform-namespace` 模式下 capacity flags
未设时默认 **cpu=1000 / mem=4Ti / pods=10000**——旧默认 "0" 会注册一个谁都调度不上去的
节点。显式设置仍然生效：

| 闸门 | 位置 | 管什么 |
|---|---|---|
| K8s ResourceQuota | 准入层，per-namespace | 租户能创建多少 pod / 申领多少 CPU 内存 |
| Zun project quota | Zun API，per-project | `zun/common/quota.py` + `quota_usages` 表，按 project 记 containers/cpu/memory/disk |

⚠️ **必须承认这等于回到了 kaaas §2.3 批评过的"静态容量"**——失败点从调度期 Pending 位移到
创建期。但那条批评的前提是"没有别的闸门"，而现在准入层的 ResourceQuota 会在**更早**的地方
拒绝（`kubectl apply` 当场报错，比 Pending 还清楚），Zun 配额是第二道。**换句话说：
静态容量的病没了，不是因为容量变准了，是因为把关搬到了容量之外。**

⚠️ 旧文写"K8s ResourceQuota 是唯一记账闸门，Zun quota 对 capsule 结构性不记账
（count_usage 只数 TYPE_CONTAINER，`objects/container.py:374` + `quota.py:569-582`）"——
**这条仍然成立且更要紧了**：Zun 侧的 capsule 计数缺口现在是第二道闸门上的一个洞，
应作为 fork 工作项补上（§10）。

### 3.3 conditions / addresses

- Ready 是真实健康信号：Zun API 失联即打 NotReady。现有静态恒 Ready + OutOfDisk
  （zun.go:255-299）废弃。⚠️ **受众变了**：租户看不到节点，所以 Ready 现在是**给平台看的**
  ——它不再是"让租户在节点层看到平台侧故障"的通道，租户侧的故障可见性只剩 pod 事件与状态。
  ⚠️ 判定条件里"租户 Neutron 网络异常"也随之失效：一个共享节点跨多个租户的网络，
  拿任一租户的网络状态去决定全节点 Ready 会让一个租户的问题打翻整个分片。
- 没有 DiskPressure/PIDPressure 等机器态 condition（没有机器）。
- InternalIP 现返回 nil（zun.go:303-305）必须修——它是 logs/exec 回连断裂的根因之一。
  ⚠️ 它暴露管理网地址；新形态下节点对租户不可见，**这个顾虑随之消失**，
  旧文提的"在 kubezoo 翻译层改写展示值"不再需要。

### 3.4 数量模型（2026-08-13 改写：节点数与租户数解耦）

#### 3.4.1 三层关系：region → 进程 → Node

⚠️ **中间两段都是一对多，不是一对一。** "一个 VK 实例 = 一个 Node = 一个 region"是
错的——它只在最简配置下碰巧成立，见 3.4.3。

```
region ──1:K──▶ VK 进程 ──1:(AZs × archs)──▶ Node
```

| 层 | 身份由什么组成 | 数量 |
|---|---|---|
| **region** | `(region)` | 有几个 OpenStack region |
| **VK 进程** | `(region, shard)` | `regions × K` |
| **Node** | `(region, shard, AZ, arch)` | `regions × K × AZs × archs` |

**为什么每一层都不能合并：**

- **region → 进程 是 1:K**。进程**不能跨** region（一份 `Credentials` 的 `Region` 解析
  全部服务端点，§2）——这一段是硬的。但反过来不成立：**一个 region 可以有多个进程**，
  因为 K 是爆炸半径旋钮。
- **进程 → Node 是 1:多**。拓扑坐标只能挂在 Node 标签上，而**一个标签只有一个值**——
  一个 Node 没法同时声明 `zone=az1` 和 `zone=az2`。所以一个进程要表达 3 个 AZ 就得注册
  3 个 Node。代码里就是 `--node` **可重复**（`main.go:724-773` `nodeSpecs`），
  它们共享同一个进程、同一份 informer、同一个凭据解析器。
  ⚠️ 实践细节：每个 Node 要**各自的 `:10250` 监听地址**（`nodeSpecs` 对重名与重端口都有
  显式拒绝），所以一个进程会绑多个端口。

#### 3.4.2 公式与坐标

$$\text{节点数} = \underbrace{\text{regions} \times K}_{\text{§2 进程数}} \times \text{AZs} \times \text{archs}$$

**四个坐标各自的理由**（缺一个就有一类语义表达不出来）：

| 坐标 | 为什么必须是一个独立的 Node |
|---|---|
| region | 一份凭据只解析一个 region 的端点（§2）；且卷/网络不跨 region |
| 分片 K | 爆炸半径旋钮（§2）；⚠️ 同一进程不能与别的进程共用一个 Node 对象——`nodeSpecs` 明确拒绝重名（`main.go:752-756`：*"Two controllers on one node object would fight over its status and each treat the other's pods as its own"*） |
| AZ | zone 标签是 PV 亲和与 WaitForFirstConsumer 的唯一依据；一个标签只有一个值 |
| 架构 | 镜像不跨架构；`--arch` 同时定标签与 capsule 的 `architecture`（§3.6） |

**举例**：1 region、3 AZ、2 架构、K=50

```
1 个 region
    └─ 50 个 VK 进程
           └─ 每个 6 个 Node（3 AZ × 2 arch）
                  = 300 个 Node 对象
```

不管背后是 1000 个租户还是 1000 万个。

#### 3.4.3 ⚠️ 退化情况：为什么"一实例=一节点=一 region"读起来是对的

`K=1`、1 个 AZ、1 种架构时，三层塌成一层：

$$1\ \text{region} = 1\ \text{进程} = 1\ \text{Node}$$

**实验床现在就接近这个配置**（1 region、1 AZ、1 arch），所以那个等式在眼前是成立的。
但它是**最简配置的巧合，不是关系本身**——任意一个坐标增加，等式立刻散开。
写运维脚本、算容量、排查"这个 pod 归谁管"时都按 3.4.1 的三层来，不要按眼前看到的那一层。

**⭐ 顺带把不决定节点数的东西也说清：租户数不在上面任何一个公式里。**
这正是本次改动的全部意义（§1.2）。

生命周期：节点由平台声明式创建/销毁，**不再随 Tenant CRD**。缩节点走标准 drain
（capsule 无宿主机绑定，迁移即重建）。

### 3.6 混合架构（已实现，2026-08-07 实测）

镜像不跨架构运行，而这一层原先无人把关：虚拟节点固定自称 amd64，capsule 不带任何
架构信息，Zun 见空就随便放。租户写了 `nodeSelector: kubernetes.io/arch: arm64` 也只是
选中一个说谎的标签，pod 调度成功、镜像执行失败。

**一个节点只服务一种架构**，`--arch` 同时决定两件事，二者必须是同一个值：

1. 节点的 `kubernetes.io/arch` 标签 —— K8s 调度器据此选节点；
2. 该节点所建 capsule 的 `architecture` 字段 —— Zun 转成 `trait:COMPUTE_ARCH_*=required`
   交给 Placement，把不匹配的宿主机在**调度阶段**就排除。

Zun fork 侧配套（`zun/container/driver.py`、`capsules.py`、`schemas/parameter_types.py`）：
CRI 驱动上报 `architecture` 与对应 trait（此前只有 docker 驱动上报），capsule 模板新增
`architecture` 字段。`arm64`/`aarch64`、`amd64`/`x86_64` 归一到同一张表，K8s 词汇与
Linux 词汇落到同一台机器。

实测（2026-08-07，node-04/05/06 三台 x86）：
- 三节点均上报 `COMPUTE_ARCH_X86_64`；
- `architecture: amd64` 的 capsule → 落 incus-node-04，Running，拿到 OVN IP；
- `architecture: arm64` → `There are not enough hosts available.`，**从未被放置**；
- `architecture: sparc` → API 层 400，schema 拒绝。

启动期校验 `--arch`：拼错会注册一个没有任何宿主机能满足的标签，pod 永远 Pending 且
事件里没有任何线索指向这个拼写错误。

> ⚠️ **同租户多节点是本节的副作用，也是三个隐藏缺陷的触发条件**——见 §4.4。

### 3.5 成本模型（每节点边际成本）

| 成本项 | 量级 | 说明 |
|---|---|---|
| etcd 对象 | **5.3 KB**（实测） | Node 4,614 B + Lease 707 B（2026-08-13，`kubectl get -o json \| wc -c`） |
| apiserver 写 QPS | 0.1/s | lease 续约 = `leaseDuration × 0.25`（`lease_controller_v1.go:47`），默认 40s → 每 10s 一写 |
| VK 进程内存 | **66–84 MB**（实测） | ⚠️ 旧文估的 "10–50MB" 偏低。实测两个进程：66 MB（无策略执行）/ 84 MB（开 NetworkPolicy 执行），各带 1 节点、个位数 pod——**这是地板不是均值** |
| 监控基数 | 中 | node 系列指标/告警累积 |
| ~~DS 扇出~~ | — | **已废弃**（§9）：不再支持 DaemonSet，这项成本消失 |

**战略成本已经从"∝ 租户数"降为"∝ regions × K"**（§3.4），所以旧文那三条对策里两条不再需要：

- ~~② 按套餐限节点数~~ —— 节点不再是租户资产，无从限起。
- ~~③ 规模墙的出路 = kubezoo M8 分片 + 多上游集群~~ —— 仍然是多 region/多集群的正路，
  但**不再由 kubezun 的节点数驱动**。
- ① 心跳放宽 **仍然有效，但有一堵墙**：⚠️ **上限是 50 秒,不是 60**。
  `nodeMonitorGracePeriod` 是 KCM 上的**全局** flag、默认 50s、**无按节点粒度**
  （`nodelifecycle/config/v1alpha1/defaults.go:46`）。续约间隔超过它 → 每个 VK 节点
  **永久 NotReady**。要更长就得抬那个全局值，而它同时管着 B1 的真实 kata 池——
  真节点的故障检测会一起变慢。**旧文写的"30–60s"横跨这堵墙，上半截是不能用的。**

⚠️ **"空节点在 OpenStack 侧零成本"这句话只对了一半，是旧形态最大的成本盲点**：
OpenStack 侧确实零成本（不建 capsule 不建 port），但**K8s 侧是全价**——一个 pod 都没有的
租户照样占 1 Node + 1 Lease + 每 10 秒一写 + 一个 75 MB 进程。这正是 §1.2 那一刀的
直接依据。新形态下这句话终于完全成立：空**租户**在两侧都零成本。

#### 3.5.1 ⚠️ B2' 集群的规模剖面与标准集群**相反**——别按节点数找墙（2026-08-13）

§7.4 已经写过一半：*"zun-compute 可独立于 K8s 集群扩展，K8s 侧可以只是不大的控制面集群"*。
把它推到底就是：**B2' 的 K8s 集群里没有 worker，只有控制面 + VK 进程。**

| | 标准 K8s 集群 | **B2' 集群** |
|---|---|---|
| 节点数 | 几千 | `regions × K × AZs × archs`，**几十到几百** |
| 每节点 pod | ~110（kubelet 硬上限） | **无硬上限**——虚拟节点背后没有机器，`pods` capacity 是我们自己报的静态数（§3.2） |
| 墙在哪 | **节点数** | **pod 数 / etcd 对象数 / watch 扇出 / 租户数** |

⚠️ **所以"量单集群虚拟节点天花板"是量错了轴**——节点少得根本碰不到墙，先碰到的是 pod
和租户。而 **K 也不由节点天花板决定，由"一个进程能同步多少 pod"决定**：`syncLoop`
（`sync.go:32`）是周期性的 O(pods) 全扫，跟不上 `syncInterval` 的那一刻就是这个进程的上限。

正确的实测顺序（TODO 阶段 4 已按此改）：

1. **单进程 pod 承载量** —— `syncLoop` 在多少 pod 时开始跟不上周期？**这个数直接定 K**。
   ⚠️ **不需要 kwok**：这是我们自己进程的性能，实验床上灌 pod 就能量。
2. **单集群 pod / 租户容量** —— etcd 对象数、watch 扇出、apiserver 负载。
3. 节点数曲线仍要量，但**是次要的，且大概率富余很多**。

**连带结论：集群是廉价的**（无 worker，控制面还可以托管在 Kamaji 里），所以
"一集群跨多 region" 与 "region == 集群" **两种部署形态都要支持**，不由设计替运维定死。
⇒ 这就是 `topology.kubernetes.io/region` 标签（§3.1）与 `namespace → (project, region)`
绑定（§4.6.1）存在的理由：**代价是一个标签加一个字段，换来两种形态都能用**；
省掉它们只剩 "region == 集群" 一种能用，为一个标签锁死部署形态不划算。

---

## 4. 调度与安全边界

**层级：provider 硬校验是安全边界，准入策略是第二层，mutate 只是便利。**

> ⚠️ **2026-08-13 形态变更对本节的影响**：节点不再属于租户，所以"钉到本租户节点"这套
> 表述全部作废。**但授权边界本身没有变弱——它变得更承重了**：以前 namespace 白名单只回答
> "允不允许"，现在它还要回答"用谁的凭据"（§4.6）。

1. **provider namespace 白名单（唯一不可绕过的授权边界，必需项）**：(VK) PodController 只按
   spec.nodeName 过滤（node/nodeutil/client.go:53-58），该字段创建者可直接写死绕过调度器。
   因此 CreatePod/GetPod/GetPodStatus/GetContainerLogs 等所有入口先校验
   `pod.Namespace ∈ 本进程服务的命名空间集`，不匹配返回 errdefs.NotFound。
   ⚠️ **新形态下这条检查同时选凭据**：一个进程服务多个租户，`authorize(namespace)` 通过
   之后必须解析到**该 namespace 对应的 project 凭据**。选错的后果是拿 A 的凭据操作 B 的
   资源——`vknode/namespaces.go:105-109` 的注释早就点名了这个后果，只是当时防的是启动竞态，
   现在它落在日常主路径上。
2. **Kyverno validate（deny，failurePolicy=Fail）—— 不是兜底，是防 DoS 的必需层**：
   禁租户写 spec.nodeName；RBAC 收回租户对 pods/binding 子资源的 create（第二条逃逸路径）。
   上线前实测 kube-system 控制器 SA 创建的 pod 确实经过策略（核查 resourceFilters/excludeGroups）。
   **旧实测依据（2026-08-07 阶段 2 渗透）及其现在的读法**：租户 A 用 `spec.nodeName` 直写
   或 `nodeSelector: kubezoo.io/pool=B` + B 的 toleration，**K8s 调度层都挡不住**；
   provider 白名单让它停在 ProviderFailed，B 的 project 里零 capsule（执行面安全）。
   但**被拒的 pod 仍计入该节点的 Allocated resources**（实测一个 limits=4CPU/8Gi 的攻击
   pod 占掉 12%）。
   ⚠️ **新形态下这个攻击的形状变了，没有消失**：不再是"A 打 B 的专属节点"，而是
   **"任一租户可以耗尽整个分片共享节点的可调度容量"**——受害面从一个租户扩大到 1/K 租户。
   → **执行面靠 provider 白名单，容量面必须靠准入层的 ResourceQuota 拦截**，两层缺一不可，
   而且容量面这一层现在更要紧。
3. **VAP 保护 Node 写面**：nodes/status 只许 VK 自己的凭据写；受保护标签/污点前缀
   （kubezoo.io/、knaas.io/、node-role、topology.*）只许平台写。
   ⚠️ 新形态下租户**根本看不到节点**（kubezoo 不再暴露），所以"前缀归属使租户拥有自己的
   节点名"这条风险消失；VAP 仍需保留，防的是平台内部的误写与其他控制器。
4. **placement mutate（便利层）**：复用 (kubezoo) convert/placement.go:118-155 机制——剥
   nodeName、注入 nodeSelector + toleration。⚠️ **`NodePoolFor(tenantID) = tenantID` 必须改**
   ——池不再等于租户。该函数注释写明**三处必须一致**（Kyverno 策略 / kubezoo 注入 /
   手工打标），改它要三处同动。⚠️ 不再有"对 DaemonSet 作用于 spec.template"这条（§9 已废弃）。
5. **系统 DS 排除（仍然需要）**：给 kube-proxy/CNI 等 operator:Exists 全容忍 DS 注入
   `requiredDuringScheduling nodeAffinity: type NotIn (virtual-kubelet)`（AKS virtual node
   同款）。⚠️ **租户 DS 废弃不代表这条可以删**——平台自己的 DS 仍会试图落到虚拟节点上。
   托管集群无权改时靠第 1 条兜底。provider 入口 defer recover，防漏网 pod 打挂进程。

**租户调度语义三档**（写入租户文档）：

- ✅ 原样工作：PriorityClass 抢占、Pending 事件、ResourceQuota 报错。
- ⚠️ **不可见/不可控**：租户看不到节点，`kubectl get no` 为空（与 B1 一致）。
  nodeSelector / nodeAffinity / tolerations / topologySpread 由 placement **整体替换**，
  租户写什么都不生效——⚠️ 这**不是新增限制**：`place()` 早就在 `spec.Affinity = nil` /
  `spec.TopologySpreadConstraints = nil`（`placement.go:167-168`）、并整体替换
  nodeSelector 与 tolerations，租户写的从来没到过后端。
- ⚠️ **副本的物理分布**：完全由 Zun 决定。K8s 侧没有任何机制能影响它——**物理 HA 唯一的
  指望是平台默认启用的 Zun 侧反亲和（§4.5），而它尚未实现**：实测 8/8 capsule 堆在一台，
  三台计算节点两台全空。新形态下这条从"锦上添花"升级为**唯一的分布机制**。
- ⛔ 禁止：spec.nodeName 直写；改受保护标签/污点。

---

### 4.4 同一命名空间多个虚拟节点（混合架构的必然结果）

在此之前每租户恰好一个节点，以下三处缺陷永不触发；一旦第二个节点出现（按架构、
按 AZ 都会）就会立刻咬人。三处都已修复并有单测：

> ⚠️ **2026-08-13 起本节的地位升级了**：共享节点形态下，**多节点是常态而不是边角情况**
> （`regions × K × AZs × archs`，§3.4），而且同一个节点上还叠加了多个租户。
> 本节四条从"混合架构才会踩到"变成**每时每刻都在生效的不变式**——改动 orphans / GetPod /
> sync 之前必读。

**① 孤儿清理会删掉兄弟节点正在运行的 capsule。** VK 的 pod informer 按
`spec.nodeName` 过滤（`virtual-kubelet/node/nodeutil/client.go:56`），所以 A 节点根本
看不见 B 节点的 pod，B 的 capsule 在 A 眼里全是孤儿。对策：capsule 打上
`knaas.io/node-name`，清理只判自己名下的。**没有该标签的旧 capsule 一律不动并记日志**
——删错代价是打爆别人的工作负载，留着代价只是配额。

**② 同名重建的 pod 永远拿不到 capsule。** VK 的 `createOrUpdatePod`
（`node/pod.go:89-94`）先问 `GetPod`，再用 `podsEqual` 比 spec。我们为了上报终态而保留
的已删除 pod 记录会被当成活 pod 返回，而重建 pod 的 spec 与它**完全相同** → 判定"无事
可做"，create 和 update 都不调用。StatefulSet 每次重启都复用 pod 名，这是常态不是边角。
对策：终态记录继续保留供状态上报，但对 `GetPod` 隐藏；另外 `UpdatePod` 遇到 UID 不同的
同名 pod 时改为创建。

**③ 状态同步会把旧 capsule 的健康和 IP 套到新 pod 上。** 匹配只按 namespace/name。
对策：capsule 的 `pod-uid` 标签必须与 pod UID 相符，否则视为无 capsule。

**④ pod 级失败必须同时写进容器状态**（这一条与多节点无关，是同一次排查顺带挖出的）。
kubectl 优先显示容器状态而非 pod phase，所以两个方向都会说谎：
- capsule 被 Placement 拒绝 → 没被放置就没有容器，容器状态停在初始值 →
  已确定失败的 pod 永远显示"启动中"（实测有一个这样卡了 3 小时）；
- capsule 在 pod 运行中消失（`CapsuleMissing`）或始终卡在 Creating
  （`CapsuleStuckCreating`）→ 容器仍标记 Running → **pod 显示 1/1 Ready，而它的
  capsule 已经不存在了**——正是本项目要消灭的那类假健康。

对策：这三条路径都改写容器状态（`CapsuleUnschedulable` 带 Zun 原文，另两条带各自原因），
**已终止的容器不动**——它自己的退出信息比任何 pod 级理由都准确。

> ⚠️ 已经处于 `Failed` 的 pod 不会被追认：VK 不再同步终态 pod
> （`node/podcontroller.go` "Ignore the pod if it is in the Failed or Succeeded state"）。

### 4.5 反亲和：平台默认启用，落在 Zun 调度层（2026-08-13 定案）

**定案：同 owner 的 capsule 之间反亲和，平台默认开启，不是租户选项。** 之前 §10 把它记作
"候选"，现在不是了——因为它不是锦上添花，是**当前行为主动地反 HA**。

**实测（2026-08-13，开发环境）**：8 个 capsule **全部**落在 `incus-node-04`，其中包含
一个 3 副本 StatefulSet（keeper-0/1/2）和一个 2 副本 Deployment（coredns）。
⚠️ 判据排除过"只有一台可用"：三个 `zun-compute` 服务全部 `up`、同一个 AZ（`nova`）、
无 disabled，而 node-05/06 的 Placement 用量是 `0 0 0`——**完全空着**。

原因在 `scheduler/filter_scheduler.py:75-105`：`_get_filtered_hosts` 之后**没有任何排序**
（注释写着 "looping over the **sorted** list of possible hosts"，但没有任何代码 sort——
又一句从 Nova 抄来而 Zun 没有对应实现的话），然后 `for host in hosts` 取**第一个** claim
成功的。于是同一工作负载的副本**堆在同一台机器上直到它装满**。三副本的数据库，物理 HA
是零。

**为什么不能在 K8s 侧解决**：两层都堵死。① kubezoo 的 `place()` 把 `spec.Affinity` 和
`spec.TopologySpreadConstraints` 整个丢掉（`placement.go:167-168`），租户写的反亲和从来
没到过后端；② 即使不丢，**K8s 看不见物理机**——一个逻辑节点背后是这个 AZ 里的全部 Zun
计算节点，`topologyKey: hostname` 在 K8s 侧只能区分逻辑节点。**只有 Zun 的调度器同时看得见
副本和物理主机**，所以这一刀只能落在 fork 侧。

**⚠️ 实现约束一：Zun 没有 weigher，"软"表达不出来。** `zun/scheduler/` 下只有 `filters/`，
**没有 `weights/`**；filter 是硬判定，返回空就是 `NoValidHost`。而默认启用**必须是软的**——
单机实验床上做成硬的，第二个副本直接调度失败。两条路：

- **(a) 补 weigher 框架**（推荐）。比听起来便宜：`scheduler/loadables.py` 就是 Nova 那套
  通用加载器，`base_filters.py:58` 的 `BaseFilterHandler` 已经继承它。加 `base_weights.py`
  + `weights/` 是同一个模式再走一遍，且此后所有"偏好"类需求都有地方放。
- **(b) 降级 filter**：先按反亲和过滤，结果为空则退回不过滤。省事，但把"偏好"塞进
  硬判定的框里，下一个偏好还得再塞一次。

**⚠️ 实现约束二：capsule 上没有 owner 标识。** 现有标签只有 `managed-by` / `pod-namespace`
/ `pod-name` / `pod-uid` / `node-name`（`template.go:16-26`）。**`pod-name` 不能用**——
keeper-0/1/2 三个名字互不相同，要的是它们**共同的 owner**。所以 kubezun 侧要补一个
owner key（ownerReference 根的 UID，即 ReplicaSet/StatefulSet 身份），Zun 侧按它分组。
两侧都要动，缺一边都是静默无效。

### 4.6 凭据解析与 namespace↔project 绑定（2026-08-13 定案，本次改动的承重件）

> ✅ **已实现并实验床验收（2026-08-13）**。`pkg/tenant.Resolver`（三态校验含）+
> provider `Capsules` 接口 + 四 controller 的 `ReconcilerFor`/`EachReconciler` 接缝 +
> `--platform-namespace`/`--tenant-label`/`--shard` 装配。**验收**：一个进程带
> 111111+222222 于同一节点，111111 凭据（project 4fb711f8）只见己方 capsule、222222
> （project b0f233fd）只见己方，pod 各 1/1 Running、IP 各来自己方网段——正向对照与隔离
> 是同一次测量。实现中补的三条，写回设计：
>
> 1. **过渡绑定不止凭据**：`knaas.io/network-id`、`knaas.io/vip-subnet-id`、
>    `knaas.io/vip-network-id` 注解随 Secret 走（每租户的网络不可能是进程 flag），
>    解析产物是 `Binding{Session, NetworkID, VIPSubnetID, VIPNetworkID}`。
> 2. **每租户 Reconciler 的 `ServesNamespace` 必须按租户收窄**——共享进程级检查会让
>    A 的 NetworkPolicy 地址组灌进 B 的 pod IP（B 被 A 的策略放行）。
> 3. **逐租户 walk 必须报告覆盖了谁**（`Each(fn(tenant, api))`）：status sync 把
>    "listing 缺席"读作"capsule 没了"并判 Failed，跳过的租户若不可辨认，一次凭据抖动
>    就判死整租户的 pod（cff9f8b 的形状高一层）。
>
> RBAC：VK 的 SA 需要平台命名空间里 secrets 的 **get + patch**（patch 记录首绑）。
> 记录失败非致命——校验惰性到写成功为止，warn 一条。

一个进程服务多个租户，凭据就不能再是进程级的一个值。**这是整个共享节点形态唯一的真代价，
也是唯一的重构面。**

**现状**：`zunClient` 在 `main.go` 每进程建一次（`CredentialsFromEnv()` 读 `OS_*`），
然后灌进 8 个子系统——capsules、块存储、共享存储、netpol、Octavia、Neutron、
KeyManager、Subnets（全文件 19 处引用）。

⚠️ **好消息：卡住的只有凭据这一处。** namespace / 授权那一侧**已经是多租户能力**——
`--namespace-selector` 是标签选择器，写成 `kubezoo.io/tenant in (a,b,c)` 现在就能服务多个
租户的 namespace；`Serves()` 按集合判定，不认租户身份。

**改动**：`zunClient` 从**单值**变成**按 namespace 解析的凭据**。

#### 4.6.1 绑定模型

> **一个 namespace 恰好对应一个 `(project id, region)`；一个 `(project, region)` 可以有
> 多个 namespace。**

即绑定是**多对一**。一个租户的若干 namespace（`<tid>-default`、`<tid>-kube-system` …）
通常映射到同一个二元组，但模型本身不要求。

解析链：`pod.Namespace → (project, region) → Secret → OpenStack clients`。

⚠️ **为什么绑的是二元组而不是只有 project**（2026-08-13 补）：Keystone 的 project 是
**全局的**，同一个 project 可以在多个 region 各有资源；而**卷与网络不跨 region**，
所以一个 namespace 的 pod 必须全部落在**同一个** region。只记 project 而不记 region，
就会出现"凭据对、region 错"——`Credentials.Region` 解析出另一个 region 的端点，
于是网络 ID 找不到、卷挂不上，而两个字段单看都是对的。

⚠️ 而 region 不是一个稳定的小数字：**它由 OVN 容量推着涨**（§7.4.2），
所以"多 region"是必然形态，不是边角情况。

#### 4.6.2 Secret 放平台命名空间，不放租户命名空间

**⛔ 不能放租户命名空间**，理由不是"跨租户风险"（确实没有——那是他自己的 project），而是
**把 project 级的 OpenStack 权限，降到 namespace 级的 K8s 权限就能取到**。一次跨层提权。

⚠️ **"让 kubezoo 在视图层过滤掉"不成立**：Secret **不需要被看见就能被使用**。
一个 pod spec 写 `volumes.secret.secretName: <那个名字>` 即可，而这条路径
**根本不经过 kubezoo**——`provider/files.go:35` 里 kubezun 用**自己的**凭据按
`(pod.Namespace, 名字)` 直接 GET 并写进 capsule。租户从头到尾没有"读"过它，是我们替他读的。
要靠过滤挡就得挡**引用**而非**读取**，那意味着覆盖 `volumes.secret` / `volumes.projected` /
`envFrom.secretRef` / `env.valueFrom.secretKeyRef` / `imagePullSecrets` 每一条路径，
且上游每新增一种引用方式这道防线就静默失效——**正是"封闭白名单 vs 开放禁令"那个已经付过
学费的形状**。

**✅ 放平台命名空间是结构性的**：pod 只能挂**自己命名空间**的 Secret，这是 K8s 内核规则，
不是我们维护的过滤器；不依赖名字保密，也没有"新增引用方式"能绕过。

顺带三个好处：轮换变成一次 K8s 写操作；开通闭环留在 K8s 内（Tenant CRD 控制器建 appcred +
写 Secret）；VK 不必预加载全部租户凭据。

**代价要认**：VK 的 ServiceAccount 需要在该命名空间 `get secrets`，**一个被攻破的 VK 能读到
分片内全部租户的凭据**。这不是新增暴露面（分片进程本来就把它们握在内存里），但相对旧形态
"一进程一凭据"确实是降级——由 §2 的 K 旋钮定价。

#### 4.6.3 project 绑定不可变，但不可变的是 **project id，不是凭据**

| 操作 | 语义 | 规则 |
|---|---|---|
| 换 appcred，**同 project** | 凭据轮换（过期/泄露/定期换） | ✅ **必须允许** |
| 换 project | 身份重绑 | ⛔ 拒绝 |

⚠️ 所以校验对象是**从 token 解出的 project id**，不是 Secret 的哈希或版本。
**把整个 Secret 做成不可变会顺手禁掉轮换**，而轮换正是这套方案要换取的好处之一。

**⚠️ 为什么必须校验：改 project 是静默的、自动的、且不可逆。** kubezun 现在
**从不记录也从不校验自己认到了哪个 project**（全仓零处引用）。凭据一换：

1. `ListManagedAll` 用新 project 列 capsule → **旧 project 的 capsule 全部不可见**；
2. 每个在跑的 pod 都"找不到 capsule" → `sync.go:91-99` 判成 `Failed` /
   `ContainerStatusUnknown`；
3. ReplicaSet/StatefulSet 立刻建替补。**不需要任何人重启,一个同步周期内全部发生。**

留下的烂摊子：旧 project 的 capsule **全都还在跑、继续计费、继续占 IP，而且永远不会被回收**
——孤儿清扫用新凭据 list，根本看不见它们。新建的 pod 大概率也起不来：网络 ID 属于旧 project、
PV 里的卷 ID 属于旧 project（**数据留在那边**）；而 Service 会**复制一份 LB**——
`ensureLoadBalancer` 拿注解里的 ID 去 GetByID 得到 NotFound，代码注释写着
*"Deleted behind our back; fall through and make another"*，于是新 project 里建一个新的，
旧的继续跑、继续持有 VIP、继续计费，租户的 Service 地址变了。旧 project 里那些还活着的
capsule 带着旧的安全组，而**没有任何东西再更新它们 → NetworkPolicy 静默停止生效**。

**判据必须是三态，而两态是最自然的写法**：

```
无记录         → 写入（首次绑定）
有记录且一致   → 正常
有记录且不一致 → fail closed，拒绝启动 + 报警
```

⚠️ **"不一致就覆盖"是错的，而它恰好是随手会写出来的那一版**——因为看起来像"把状态同步成
最新的"。⚠️ 校验位置必须在**同步循环启动之前**：晚一步，第一个周期就已经把所有 pod 判成
`Failed` 了。

**记录落点**：`kubezoo-contract` 的 Tenant CRD（`pkg/apis/tenant/zz.generated.crd.go`）——
比每个 namespace 各打注解更不容易漂移（漂移了就是有的 namespace 认 A、有的认 B）。
⚠️ **不能挂在 Node 上**：新形态里 Node 不再是每租户一个。

#### 4.6.4 重绑的正确顺序（"不可变"没告诉你怎么改）

⚠️ `provider.go:316` 把 `Delete` 的 NotFound 当成功吞掉——这个设计本身对（删除要幂等），
但在换了 project 之后它变成陷阱：用新凭据删旧 project 的 capsule → 404 → kubezun 认为
"已经没了" → pod 从 K8s **干净地删除** → **旧 capsule 继续跑，而最后一份记录它存在过的
东西刚被删掉**。

所以"删除 namespace 重建"这条直觉退路是**反的**。正确顺序：

```
① 仍在旧绑定下：清空工作负载，让 kubezun 用旧凭据真正删掉 capsule
② 确认旧 project 内该 namespace 的 capsule/port/卷/LB 归零
③ 再改绑定
```

这条顺序必须写进运维流程——**不可变约束只让你改不了，没告诉你怎么正确地改**，而先改凭据
再删 namespace 是最自然、也是错的那个顺序。

#### 4.6.5 ⚠️ 一条此前从未写下来的承重前提

> **基于 Neutron 安全组的 NetworkPolicy 执行（§7.7），默认租户没有该 project 的 OpenStack
> 直连凭据。**

实测确认这不是理论问题：**capsule 能连到 Keystone 并拿到真实 API 响应**
（`http://<host>/identity/v3`，devstack 把 Keystone/Neutron 挂在 80 端口的路径下，
不是 5000/9696——用错端口探会得到相反结论）。所以凭据一旦落进 pod 可达的位置，
持有者就能直接改安全组、直接建 capsule（而按定案**孤儿清扫故意不回收非 kubezun 创建的
资源**，那些将永久留存）。

⚠️ 这条前提与 §1.2 提到的第三档（Horizon 入口）**可能已经冲突**，需要产品侧确认。

---

## 5. Pod → Capsule 映射

- **命名**：capsule 名用 pod UID / 安全编码。现有 "ns-name"（zun.go:80,149）有碰撞歧义，
  且在 admin 凭据下是跨租户漏洞（§2 凭据纪律）。
- **网络**：显式传租户网络（现在丢弃网络参数，Zun 自动选第一个可用网络，(Zun)
  capsules.py:221-223）。⚠️ nets 字段经 gophercloud + 目标 microversion 的可传递性需实测
  （待定项）；兜底约定 = 租户 project 内只有一个网络，并验证 get_available_network
  （(Zun) network/neutron.py:267-276）不回退 shared 网络。
- **资源**：K8s **limits**（非 requests）→ Zun 资源——CRI 驱动把 memory 落成 cgroup 硬限
  （(Zun) cri/driver.py:163-178）。types.go:70 json tag "requests" 的适配方向是对的，但
  Limits map 从未初始化导致 nil-map panic（zun.go:202-203/236 区域）——P0 修复。
- **不可支持字段显式拒绝**（errdefs，不静默丢弃，admission/事件给明确报错）：hostNetwork、
  hostPath、privileged/特权 securityContext（capsule 模板 additionalProperties:False 无法
  表达，(Zun) parameter_types.py:461-549；CRI 恒传 privileged=False，cri/driver.py:163-178）、
  projected volume（§8）。现有静默丢弃（zun.go:206-210 TODO）废弃。
- **podIP==OVN IP 不变式**：GetPodStatus 把 capsule 的 Neutron/OVN IP 回填 podIP。守住它，
  EndpointSlice 里自然是 OVN IP，Octavia member 直接可用。
  ⚠️ 它是**必要而不充分**条件：member 还需 subnet ID。kubezun 从 capsule 地址记录的
  `subnet_id` 取（自建 reconciler，§14.4），不经 kubetron 的 NetworkPortClaim。
- **Ready 判定**：capsule Running ∧ 有地址 ∧ 容器 readiness 通过（§6，已实现）。
  应用级 readiness 走**容器内探针 → pod Ready → EndpointSlice**（实测 5 秒内生效）；
  Octavia HM 是 LB 侧的第二层冗余，不是必需路径。
- **状态推送**：实现 PodNotifier/NotifyPods（(VK) podcontroller.go:79-90），替代无通知时库
  回退的全量轮询（(VK) node/sync.go:99-120）；DeletePod 去掉 300×1s 同步阻塞
  （zun.go:419-445），改异步等待 + 通知终态。
- **状态映射**：zunStatusToPodPhase/Conditions/ContainerStatus（zun.go:447-536）是唯一整体
  保留的资产，修三处：Stopped→Running 误映射（zun.go:456）、exit code 恒 0（zun.go:468）、
  startTime 被覆盖（zun.go:325）。
- **孤儿治理**：capsule 打租户不可伪造标记（现靠可伪造 MetaLabels，zun.go:385-393）；
  (VK) deleteDanglingPods（podcontroller.go:635-700）只清理本 VK 管理的 capsule，防误删
  租户手工建的 capsule。

---

## 6. 探针（核心难点，语义拆分方案）

事实链（全部已核实）：
- VK 库**零处执行探针**——真实 kubelet 的 prober 不在库里；
- capsule API 无 exec（(Zun) capsules.py:111-113 `_custom_actions` 为空；CRI driver
  execute_* 全 NotImplementedError，cri/driver.py:397-405）；
- **Zun 侧现有 healthcheck 字段不可用**：仅 exec 型（`cmd`）、仅 docker driver 消费
  （docker/driver.py:325-329），CRI 驱动完全不读；且 Docker healthcheck 只标状态不重启，
  与 K8s liveness 语义不符。capsule 模板的 container schema 更是**零探针字段**
  （parameter_types.py:461-480，additionalProperties:False）。

### 6.0 探针执行位置：实测排除法（2026-08-07）

**结论：所有探针类型最终都必须在容器内执行，唯一通道是 CRI ExecSync。**

| 候选执行位置 | 实测结果 |
|---|---|
| provider（VK 进程，管理网） | ✗ 不在租户 OVN 网络，且挂进去违反 kubetron 双挂纪律 |
| zun-compute 宿主机网络命名空间 | ✗ **实测 ping/curl capsule IP 全部失败**——OVN 租户隔离的正确行为 |
| sandbox netns（`/var/run/netns/cni-*`） | ✗ **实测同样不通**：kata 的 netns 里只有 tap 设备，真正的网络栈在 VM guest 内，netns 自身没有监听 socket（与 runc 的关键区别） |
| **容器内（CRI ExecSync）** | ✓ **实测可行**：`crictl exec` 里 `wget http://127.0.0.1/` 返回页面、`nc -z 127.0.0.1 80` 成功 |
| Octavia HM | △ 仅 TCP/UDP-CONNECT，**探测不到应用语义** |

**这条实测直接否决了 Octavia HM 作为 readiness 权威**：RAFT 脑裂或主从切换失败的节点，端口照常监听、TCP 照常连通，HM 判定健康，但它返回的是陈旧或不一致的数据。有状态应用（DB 集群、etcd/consul 这类 RAFT 系统）的健康**只有应用自己知道**，必须由它自己的探针回答。HM 由此降级为 L4 层的第二道保险，不再是 readiness 的执行者。

**修订后的方案：Zun fork 实现 prober，全部探针类型统一走 ExecSync**

| K8s 探针类型 | 落地方式 |
|---|---|
| `exec` | ExecSync 直接执行用户命令（DB/RAFT 场景最常用，如 `pg_isready`、`etcdctl endpoint health`） |
| `httpGet` | ExecSync 执行探针 helper，在容器内请求 `127.0.0.1:<port><path>` |
| `tcpSocket` | 同上，helper 做 TCP connect |
| `grpc` | 同上，helper 走 gRPC health protocol |

⚠️ helper 必须是**静态二进制**：distroless / scratch 镜像里没有 curl、wget、nc。
**kubetron 已解决同一问题且可直接复用**：`cmd/probe/main.go`（get/tcp/install 自安装，
`-probe-helper-image` 开关），注入方式为 init container 拷进共享 emptyDir。

三类探针语义的归属：liveness 失败 → Zun 侧重启容器（restart 只有 Zun 能闭环）；
readiness 结果 → 回流 container 状态 → provider 读回 → pod Ready condition →
EndpointSlice → Octavia member 上下线（由 kubezun 自建 reconciler 消费，§14.4）；
startup 为前两者的启动期门控。

**声明留在 K8s，执行必须下沉。方案 A（定案）：**

| 探针 | 执行者 | 机制 |
|---|---|---|
| ~~**readiness**~~（已被 §6.0 修订，保留作 L4 二道保险） | Octavia health monitor | **已 PoC 实测成立（2026-08-06）**：官方 ovn-octavia-provider（stable/2026.1，与自研 incus provider 共存，`--provider ovn`）建 LB → member = capsule OVN IP → capsule 内 curl VIP 通；HM TCP 建成后 OVN NB 生成 `Load_Balancer_Health_Check` + `ip_port_mappings`；**停掉一个后端后 15s 内 member 转 ERROR，流量全部转到健康后端（5/5 成功）**——member 上下线正是 readiness 的真实消费者（流量闸门），且绕开"VK 管理面进程路由进重叠 CIDR 租户网"的架构错位。⚠️ 能力边界（源码 `ovn_octavia_provider/common/constants.py:106-113` + 实测）：协议 TCP/UDP/SCTP、算法**仅** SOURCE_IP_PORT、**HM 仅 TCP 与 UDP-CONNECT（HTTP 型被 provider 明确拒绝）** → **httpGet readiness 必须降级为 tcp**，或走下行 Zun 侧执行 |
| **liveness** | Zun fork：zun-compute 补 ExecSync（stub 现成，(Zun) api_pb2_grpc.py:100-103）+ capsule 内探针 + 失败重启 | restart 语义只有 Zun 侧能闭环——这就是 kubelet prober 的位置 |

MVP 阶段 pod Ready = Running ∧ port ACTIVE（§5）；fork 探针落地后，Zun 侧探针结果回流
capsule status → provider 映射进 Ready，补齐 Deployment 滚动更新对 Ready 的依赖。

已否决：B（provider 远程探 OVN IP——要求每租户实例挂进租户网，违反 kubetron 双挂纪律
DESIGN §4.3，仅可作过渡）；C（capsule 内自报 sidecar——注入/上报双通道，restart 仍缺位）。

⚠️ 配套待定项：liveness 失败重启是否保 IP——per-container restart / preserve_on_delete
路径需实测（§14）。

---

## 7. 网络

**定案：Zun 原生 port，不接 kubetron 的 NetworkPortClaim。**

- Zun 在 RunPodSandbox 前内联 create_or_update_port + binding:host_id（(Zun)
  cri/driver.py:96-120），与 claim 模型同构（device_id≈claimUID、port 收养≈spec.portID、
  preserve_on_delete≈Retain）且无时序问题——kubetron 的有序交接/PortActive gate/预热池
  预绑定全是为解决"scheduler 先选节点、Neutron 后知"，Zun 里该问题不存在；强套 claim
  会造 host_id 双主冲突。
- 数据面插接契约与 kubetron **完全同构**：zun-cni 的 VIFOpenVSwitchDriver 把 veth 插进
  br-int 并设 `external-ids:iface-id=<port_id>`（(Zun) network/linux_net.py:36-43）——
  与 ovs-cni 设同一字段、插同一 br-int。**OVN 看不出 capsule port 和 kubetron pod port
  的区别**：安全组、逻辑交换机隔离、FIP、Octavia member、租户 DNS 全部无差别生效。
- **编排层不复用 kubetron，kubezun 自建，两者共存**（定案 2026-08-07，§14.4）。
  kubetron 的 Service reconciler 从 `NetworkPortClaim` 取 member 子网、且对无其注解的
  pod 报错而非跳过（`members.go:100-147`、`:54-57`），capsule 无 claim 必然卡住。
  **共存是安全的**：kubetron 的孤儿 GC 按 `device_owner` tag 过滤
  （`kubetron`/`kubetron:<clusterid>`），Zun port 是 `compute:zun`，不在其列表内。
  kubezun 自建的两块，以 kubetron 同名实现为蓝本（同作者同 gophercloud v2 栈）：
  - Service→Octavia OVN LB reconciler：EndpointSlice 驱动，subnet 取自 capsule 地址的
    `subnet_id`。⚠️ `BatchUpdatePoolMembers` 是**全集合 PUT**，member 列表少一个就清空
    整个 pool——这是照抄时最容易踩的一处。
  - 租户 DNS zone 渲染：分发通道要改（无 kubelet 挂 ConfigMap；DNS 跑成租户网内 capsule，
    控制器直推 zone）。
    **resolver 下发已实测打通（2026-08-06，PoC-4）**：原先 capsule 继承宿主
    resolv.conf（CRI sandbox 无 DNSConfig），fork 分支 `feat/capsule-dns` 把
    `_write_cni_metadata` 已查到的 subnet `dns_nameservers` 收集后填入
    `PodSandboxConfig.dns_config`（CRI v1 field 4），实测 capsule 内
    `/etc/resolv.conf` 变为 Neutron 下发的 nameserver 且解析成功。
    ⚠️ 同一函数 `_get_sandbox_config` 里 `log_directory` 是相邻的 field 3——
    F 工作流的 logs 那一刀与本补丁同一落点，可一并实现；
  - `pkg/neutron/provider.go`：appcred 客户端构造——直接抄；
  - VIP 独立子网 + tenant router 前置拓扑（东西向 LB 同子网 dst-MAC 坑，kubetron DESIGN §5.3）；
  - 预热池水位模型——capsule 池化蓝本（§10）。
  - 建议 kubetron M8 顺带把编排半边做成可独立部署（kubezun-only 形态只拉编排层）。
- **kuryr 三兄弟**：kuryr-libnetwork 不需要（docker driver 专属，(Zun)
  network/kuryr_network.py:50；capsule_driver 默认 cri，(Zun) conf/container_driver.py:32-34）；
  kuryr-lib 作为 pip 依赖保留（(Zun) network/os_vif_util.py:14-15 import 其 VIF 工具）；
  kuryr-kubernetes 与本架构无关（Service→Octavia 职责已归 kubetron）。
  ⚠️ (Zun) conf/network.py:21,24 的 driver 默认值 'kuryr' 是 docker 路径配置，CRI 不经过，
  排障勿被误导。
- **计算节点部署清单**：zun-compute + zun-cni-daemon + containerd + Kata + ovs/ovn-controller。
  无 docker daemon、无 kuryr-libnetwork。
- **运行时栈定案**（2026-08-06）：CRI 层 = containerd（Zun 写死
  `unix:///run/containerd/containerd.sock`，(Zun) cri/driver.py:44-45，无配置项）；租户
  capsule = **Kata 3.x + QEMU**——runtime_handler 经 zun.conf `container_runtime=kata` 传入
  （driver.py:74-77），QEMU 因 virtiofs（Manila RWX 前提）+ Cinder 块热插成熟 + overlayfs
  snapshotter 三条胜出；**Firecracker 排除**（无 virtiofs、热插受限、要 devmapper）；
  Cloud Hypervisor 留作阶段 4 密度/启动优化候选（与 capsule 预热池一起评估）。runc 仅
  阶段 0 PoC 用（默认值即是），阶段 2 前切 kata 并回归 e2e。provider 生成模板永不设
  runtime 字段，pod.spec.runtimeClassName 按不支持字段 errdefs 拒绝——运行时选择权在平台。
  计算节点硬件前提：/dev/kvm（VM 需嵌套虚拟化）。
- **⚠️ 与 kubelet 共节点约束**（2026-08-06，计算节点同时是 k8s worker）：kubelet 与
  zun-compute 不可共用同一 containerd CRI 端点——CRI 协议无归属概念且 CRI 插件钉死
  k8s.io 单 namespace（containerd pkg/cri/constants），kubelet 清理循环会把非己管
  sandbox 当孤儿删除，capsule 被秒杀；CNI conf 目录也会冲突。containerd namespace
  隔离只对原生 API 客户端有效，救不了 CRI 客户端。且节点不能摘出 k8s
  （ovn-controller/CSI 均为 pod 形态）。
  **定案（2026-08-06 用户执行）：运行时按守护进程分家——kubelet 换 CRI-O，containerd
  整实例连同默认 socket 让给 Zun**。开发节点 04-06 已切换完成（cri-o 1.36.3，containerd
  缓存已清空专属 Zun），socket fork 补丁与第二 containerd 实例均不再需要；备选方案
  （kubelet 保留 containerd 的环境）= 第二 containerd 实例 + fork socket 配置项，降为
  候选。生产 container1/2 上线前按同款 CRI-O 分家改造。zun.conf `container_runtime`
  需匹配 containerd handler 实名（如 kata-qemu）。
- ClusterIP/kube-proxy/service env 注入：**不实现**——Service 数据面 = Octavia OVN LB，
  普通 ClusterIP 在租户 DNS 故意 NXDOMAIN（kubetron DESIGN §6.4），K8s Service CIDR 与
  数据面无关。

---

### 7.4 规模：chassis 预算与 kubetron 的关键差异（2026-08-07 实测）

kubetron 之所以要分片 + OVN-IC，是因为**每台跑租户 pod 的 K8s worker 都是一个 OVN
chassis**，而 OVN 的 chassis 预算约 500。

**B2' 不继承这个约束**：capsule 的 port 在 zun-compute 上，**只有 zun-compute 是
chassis**；虚拟节点是纯逻辑的，在 OVN 里不存在。
实测（3 计算节点 + 3 虚拟节点）：chassis 恰为 incus-node-04/05/06 三个，
三个虚拟节点占用 **0**。

| | kubetron（B1） | kubezun（B2'） |
|---|---|---|
| chassis = | 每个跑租户 pod 的 worker | 只有 zun-compute 节点 |
| 随什么增长 | worker 数（≈ 租户负载量） | 物理计算节点数 |
| 500 墙何时到 | 500 worker | 500 台计算节点 |

**租户增长与 chassis 增长解耦**。推论：
- **OVN 不需要装在 K8s 节点上**——租户 pod 不在那里，VK 进程也只走管理网调 API
  （且 §7 要求它不得接入租户网络）。实验室里 node-04/05/06 兼任两角是凑合，非架构要求。
- **zun-compute 可独立于 K8s 集群扩展**，K8s 侧可以只是不大的控制面集群。

⚠️ B2' 的规模压力换到了**另一根轴：Zun API/DB 的恒定轮询负载**（每租户约 0.275 次
list/秒，与是否活跃无关）。⚠️ **不可用 devstack 评估**（同机跑满全套 OpenStack）。
先修轮询（复用同步结果、空闲退避、给 fork 加变更通道/ETag）再谈容量与分片；
zun-compute 已按 host 天然分片，zun-api 无状态可扩容，真正的"分片 Zun"只指拆 DB。

#### 7.4.1 ⚠️ 第二根 OVN 轴：逻辑流。这一根 kubetron 没有，我们独有（2026-08-13）

上面比的是 **chassis**，那根轴我们**更好**。但 OVN 还有一根轴——**ACL / Port_Group /
Address_Set 这类逻辑流对象**——在这一根上方向**正好相反**：

| 轴 | kubetron（B1） | kubezun（B2'） |
|---|---|---|
| chassis | 每个跑租户 pod 的 worker 都是 chassis，500 墙 | 只有 zun-compute 是 chassis ✅ **我们更好** |
| **逻辑流 / 安全组** | **零** | **全部** ❌ **我们更差** |

**依据**：`grep -rln "NetworkPolicy\|SecurityGroup" /root/kubetron` → **零个文件**。
kubetron 根本不把 NetworkPolicy 翻进 Neutron——**B1 的策略由 Cilium 在 eBPF 里执行，
完全不进 OVN**。只有 B2' 走"NetworkPolicy → Neutron 安全组 → OVN 逻辑流"这条路（§7.7）。

⇒ **不要把 kubetron 的分片经验直接搬过来**。它加 OVN-IC 分片是为了 chassis；
我们要分片是为了**它从来没有过的那根轴**，两者的墙在不同位置、由不同东西推着涨。

**而 K8s 负载在这根轴上比纯 VM 早到墙**（机制上成立，⚠️ 但**没有量过**，见 §7.7.7）：

- 安全组**对象**增删 → **全云** northd 全量重算（`en-sync-sb.c:164-175` → `:503-528`，
  连 nova 与 Octavia 的 port group 一起重建）。而我们**一条策略一个安全组**（§7.7.3）。
- 地址组有增量路径（`ovn-controller.c:4380-4399`），**port group 没有**（`:4416-4470`）
  ——这正是 §7.7.1 选 `remote_address_group_id` 的理由。
- pod churn 远高于 VM churn；策略数随应用数长，而 VM 项目的安全组通常又少又静态。

#### 7.4.2 ⭐ OVN 的分片单位是 **region**，AZ 分不了（2026-08-13 源码确认）

**AZ 在 OVN 里只是 Chassis 行上 `ovn-cms-options` 里的一个字符串**：

```python
# neutron/common/ovn/utils.py:911-923
opt_key = constants.CMS_OPT_AVAILABILITY_ZONES + '='   # "availability-zones="
```

它唯一的用途是过滤**网关端口**的调度候选（`ovn_client.py:1777-1783`）。
**同一个 region 的所有 AZ 共用同一套 OVN NB/SB。**

⇒ **要给 OVN 分片，只能加 region。** 推论有三条，都影响形态：

1. **region 数不由地理决定，由 OVN 容量决定。** 在 `节点数 = regions × K × AZs × archs`
   里，`regions` 是被 OVN 推着涨的那一项，不是配置选项。
2. **`topology.kubernetes.io/region` 标签从"将来可能需要"变成"必然需要"**（§3.1）——
   多 region 不是可选项，是逻辑流轴的必然结果。⚠️ 晚做就是同名 AZ 的卷静默错配。
3. **§7.7.7 那两个没测的数，是 region 数量规划的前置依据**——不知道一个 OVN 能扛多少
   策略，就不知道该切几个 region。它们从"增量 2 的门槛"升级为"**容量规划的输入**"。

⚠️ 这条边界和 §2 说的"region 是凭据硬边界"（一份 `Credentials` 只解析一个 region 的端点）
**是同一条线**——不用维护两套划分。

### 7.5 Service = Octavia，K8s Service CIDR 无关（2026-08-07 实测定案）

**capsule 只有一张网卡，在租户网络上。** 实测：
- capsule → 同租户另一 capsule 的 podIP：**可达**
- capsule → 该 Service 的 ClusterIP（254.51.24.88）：**不可达**

原因：ClusterIP 属 K8s service CIDR，OVN 不认识它；而 kube-proxy 在 worker 上编的规则，
capsule 的流量一次也不会经过。**kubetron 靠双挂（eth0=Cilium + 第二张 OVN 网卡）开的那个
互通口子，capsule 结构上做不到**——无法挂 Cilium 网卡。

**故：每个 Service 都要有一个 Octavia LB，不只是 `type=LoadBalancer` 的。**
ClusterIP 与 LoadBalancer 的差别只在要不要对外暴露，而不是"有没有 LB"。
OVN provider 无 amphora（LB 即 NB 规则）使这在成本上成立。
Headless（`ClusterIP: None`）不要地址，不建 LB。

**实测 PoC（2026-08-07）**：在 t1-vip-subnet（192.168.200.0/24，独立 VIP 网 + 租户
router 前置）建 `provider=ovn` 的 LB → VIP 192.168.200.187 → listener TCP:80 →
pool `SOURCE_IP_PORT` → member = r-good 的 podIP + t1-subnet →
**从另一个 capsule 访问该 VIP 成功**。reconciler 的三处具体选择（ovn / SOURCE_IP_PORT /
member 带 subnet）在真环境原样成立。

⚠️ 由此，**Service 的可用地址是 VIP 而不是 ClusterIP**：租户读 ClusterIP 得到的是个
哪儿也去不了的地址。VIP 需经 `status.loadBalancer.ingress` 回写 + 租户 DNS 解析，
DNS 编排因此是必需项而非可选项。

### 7.5a Service 与 Ingress 是两类东西，成本模型不同（2026-08-07 定案）

K8s 里它们本就是两个 kind，在这套架构下差别更硬：

| | Service | Ingress |
|---|---|---|
| Octavia provider | **ovn**（L4） | **amphora / incus**（L7）——⚠️ **绝不能用 ovn**，它是 L4-only，拒绝一切 L7 对象 |
| 数据面形态 | OVN northbound 规则，**无实例** | **真实实例**（amphora 是 VM；incus driver 是容器化 L7 worker） |
| 成本 | 近乎为零 | 有实实在在的资源成本 |
| 供给策略 | **每个 Service 都建**（§7.5：不建则 pod 之间按名字/ClusterIP 都不通） | **按需/计费能力**，不默认给 |

"每个 Service 都建 LB"之所以成立，前提正是 OVN provider 无 amphora。这条不能想当然
推广到 Ingress。

### 7.5b 两类证书，归属正好相反（2026-08-07 定案）

| | ① kubelet 服务证书 | ② Ingress TLS 终止证书 |
|---|---|---|
| 用途 | apiserver 拨号 kubezun 的 kubelet API（logs/exec） | 租户自己服务的 HTTPS |
| 谁签发 | **平台**——租户不知道 OpenStack/VK 存在，且 apiserver 必须信任签发者 | **租户自己**：在自己命名空间装 cert-manager / smallstep 等，这就是 K8s 的做法 |
| 谁轮换 | 平台 | **租户**。不续期就该过期，kubezun 不替他续 |
| kubezun 的责任 | 签发接入 + **热加载**（已实现，§见 TODO） | **只做传播**，不碰签发 |

⚠️ **②"完全不管"会制造假健康**：Octavia 做 L7 终止时证书不是从 K8s Secret 直接读的——
要先镜像进 Barbican，再由 listener 的 `default_tls_container_ref` 引用。若不传播续期：
租户 cert-manager 续期成功、`kubectl get secret` 一切正常，**而 Octavia 仍送已过期的证书**，
租户从自己能看到的任何地方都查不出原因。

**分工定案：签发与轮换是租户的事，传播是我们的事。** 机械动作三步：
watch 租户 TLS Secret → 内容变了重新镜像 → 更新 listener 引用。

**实现直接抄 kubetron**（`pkg/ingress/barbican.go:43-44`）：Barbican secret 名字里带
**证书内容的 sha256 前缀**，续期→内容变→哈希变→新名字→新 ref，reconcile 自然跟上，
既不用比对有效期，也不用知道是谁签的。

⚠️ **范围仅限 Ingress**：Service（L4）根本不终止 TLS，流量直通 pod，证书在租户容器里，
我们看不见也不需要看见。

### 7.5c ⚠️ Designate 的 zone 名**全局唯一**（2026-08-07 实测）

实测：同一个 zone 名在第二个 project 建立时直接 `duplicate_zone` 被拒。
**跨 project 不隔离命名空间。**

**我们的方案成立，但依赖一个前提，必须写下来：**

zone 名是 `<上游 namespace>.svc.cluster.local.`，而 kubezoo 的上游 namespace **带租户前缀**
（实测：`111111-default`、`111111-kube-system`…）。所以：

- 租户 111111 → `111111-default.svc.cluster.local.`
- 租户 222222 → `222222-default.svc.cluster.local.`

天然不同。实测两者在各自 project 下并存成功。

⚠️ **关键：`svc.cluster.local.` 这个 zone 本身从不创建**（实测确认不存在）。我们只建它的
下一级，父域不被任何租户占有——否则第一个建它的租户就会把其余所有租户挡在门外。

**由此产生的硬约束（改动前必读）：**

1. **zone 名必须包含租户前缀。** 若将来改成用租户可见的 namespace（`default`）命名，
   **第二个租户就建不出 zone**。租户前缀是这套命名唯一的隔离来源。
2. **不得创建 `svc.cluster.local.` 或 `cluster.local.` 作为 zone。**
3. ⚠️ **多集群共用一个 Designate 时会撞车**：两个平台各有租户 111111 → 同名 zone。
   需在名字里加集群标识（kubetron 用 `device_owner` tag 加 cluster id 解同类问题，
   `pkg/neutron/clusterid_test.go`）。**当前未处理**，单集群下不暴露。

### 7.6a VIP port 与生命周期（2026-08-07 实测，含一次自我更正）

**OVN provider 会建 VIP port**（`helper.py:3040 create_vip_port`，命名
`ovn-lb-vip-<lb_id>`，并设 `device_id=lb-<id>` 防止被 Nova 挂载）。
新建 Service 立刻验证：port 存在。

⚠️ **但本轮早先建的三个 LB，其 `vip_port_id` 全部指向不存在的 port**（按 id、按 IP、
原始 API 三种方式复核一致），而 LB 处于 ACTIVE 且 VIP 流量可达。
**原因未确定**；最可能是创建过程中一度进入 ERROR，provider 在错误路径上删掉了 VIP port
（`helper.py:1570-1578` "Deleting the VIP port ... since LB went into ERROR state"），
其后 LB 恢复 ACTIVE 但 `vip_port_id` 成为悬空引用。本 reconciler 的激进重试与此吻合。

> 📌 **自我更正**：我曾据这三个样本把"VIP 没有 port"写成 OVN provider 的**性质**。
> 它不是性质，是**故障症状**。三个样本、未查证机制就下普遍结论，是这一轮最该避免的错误。

**由此得出的设计选择（仍然成立，但理由变了）：kubezun 自己预建 VIP port，
建 LB 时传 `vip_port_id`**（`loadbalancers.CreateOpts.VipPortID`，OVN provider 会消费它，
`helper.py:495-520`）。理由不再是"provider 不建 port"，而是：

- **不依赖 provider 的失败路径**：port 由我们持有，LB 进 ERROR 不会把它删掉，
  也就不会出现悬空引用；
- **DNS 与 FIP 都挂在这张 port 上**，而它的生命周期由我们与 Service 对齐；
- **VIP 地址稳定**：LB 重建不换地址（与 §14.2 保住 capsule IP 是同一模式）。

代价：每 Service 多管一张 port。⚠️ 存量 LB 需重建一次才能改用预建 port（VIP 会变）。

### 7.6 租户 DNS：**定案 = OVN 数据面 DNS，不需要 Designate**（2026-08-08 实测推翻前案）

⚠️ **本节推翻了 2026-08-07 的 Designate 方案。** 该方案已部署并验证可用，但存在一条
更简单且严格更优的路径，实测全通。

**机制**：ML2 的 dns 扩展把 port 的 `dns_name`/`dns_domain` 写进 **OVN NB 的 DNS 表**，
ovn-controller 在**每台 hypervisor 的数据面**直接应答。与 Designate 无关，
`external_dns_driver` 也不需要。

**实测结果**（capsule 内，`dns_nameservers = 8.8.8.8`）：
```
samels.111111-default.svc.cluster.local → 192.168.201.56   OVN 拦截并应答
samels                                  → 192.168.201.56   短名同样命中
github.com                              → 20.27.177.113    未命中则放行到真实解析器
wget http://samels/ 与 http://samels.111111-default.svc.cluster.local/ 均成功
```

⚠️ **OVN 拦截的是发往 DHCP 通告的那个 resolver 的查询**，未命中时**放行原包**继续送达
该 resolver。所以子网的 `dns_nameservers` 应直接填一个**真实可用的递归解析器**——
既得到租户内解析，又不牺牲公网解析，不需要额外组件。

**唯一的拓扑要求**：OVN DNS 按**逻辑交换机**作用域，所以 **VIP 子网必须与 pod 子网在
同一个 Neutron 网络**（同交换机、不同子网），否则记录落在别的交换机上、capsule 查不到。
⚠️ 实测该配置下**东西向流量正常**（`wget http://<VIP>/` 成功），kubetron 记录的
"VIP 独立子网、dst-MAC 坑"在此配置下未出现——它们的结论来自不同拓扑，勿直接套用。

**相对 Designate 方案的优势**（全部是消除而非增加）：

| | Designate 方案 | OVN 数据面 |
|---|---|---|
| 组件 | Designate ×5 服务 + BIND | **无** |
| zone 对象 | 每 namespace 一个，需开通/回收 | **不存在** |
| 全局名字唯一性 | ⚠️ 强制唯一，多集群撞车（§7.5c） | **不存在**——记录按交换机隔离 |
| 跨租户可见性 | 需 per-tenant view | **天生隔离**——只能查到自己交换机上的 |
| 解析器 | 需部署且租户网可达 | **无**——每台 hypervisor 本地应答 |
| 单点 | 有 | 无 |
| 记录生命周期 | 随 port（同） | 随 port（同） |

**kubezun 侧代码零改动**：仍是在 VIP port 上设 `dns_name`/`dns_domain`（已实现）。

⚠️ 环境中的 Designate 与 BIND 可以停用，但**先留着**——若将来有对外权威 DNS 需求
（把租户服务发布到公网域名）仍会用到，那是另一件事。

### 7.7 NetworkPolicy（2026-08-11 定案）

**今天的状态是最坏的一种：apiserver 收下、存起来、`kubectl get netpol` 看得见，
没有任何东西执行它。**全仓库零处处理 NetworkPolicy 或安全组，而租户的 N 个命名空间
落进**同一个 project、同一张 OVN 网、同一组安全组**——租户建第二个命名空间最常见的
动机恰恰是隔离（prod/staging）。**虚假隔离比没有隔离危险：没有隔离时人会自己小心。**

#### 7.7.1 映射：谁承载什么

OVN schema 里 ACL 只能挂在两处（`ovn-nb.ovsschema`：`Logical_Switch.acls`、
`Port_Group.acls`），**`Address_Set` 没有 `acls` 列**。所以两者不可互换：
**Address_Set 是"被匹配的东西"，Port_Group 是"规则的挂载点"。**

| NetworkPolicy 的部分 | 落到 | 经 Neutron 的什么 |
|---|---|---|
| subject（这条策略管哪些 pod） | **Port_Group 成员** | 端口的 `security_groups` 列表 |
| peer（from/to 的 selector） | **Address_Set** | `remote_address_group_id` → 地址组 |
| 端口/协议/方向 | ACL 匹配 | 安全组规则字段 |
| namespace | **不落任何底层对象** | 只活在我们的地址组命名键里 |

⚠️ **peer 必须用 `remote_address_group_id`，不能用 `remote_group_id`。**后者解析成
那个安全组**自己的 port-group 地址集**（Neutron `common/ovn/acl.py:226-233`），等于
把 peer 变动压到 Port_Group 那条轴上——而那条轴在 ovn-controller 里**没有增量路径**
（见 7.7.3）。

⚠️ **allow-all 必须是两个安全组，不是一个。**Neutron 的安全组**没有方向**（方向在
规则上，`acl.py:59-68`），而 NetworkPolicy 的隔离是**按方向**翻转的。一个合并的
allow-all 组在 `policyTypes: [Ingress]` 时被摘掉，会**连出向一起杀掉**。两个组各自
按方向引用计数。

Neutron 的基线本来就是拒绝：开了 port_security 的端口一律加入全云的
`neutron_pg_drop`（`ovn_client.py:678-682`），drop 在 1001、allow 在 1002。所以
"未被任何策略选中的 pod 保持全开"要靠**主动挂上那两个 allow-all 组**来表达，
不是靠什么都不做。空的安全组列表是被尊重的（默认组只在该属性**未设置**时注入，
`securitygroups_db.py:1238-1250`）。

#### 7.7.2 对象模型（每租户）

| 对象 | 数量 | 随什么增长 |
|---|---|---|
| `knp-allow-ingress` / `knp-allow-egress` | 2 个安全组、约 4 条规则 | **不增长** |
| 策略安全组 | **每条 NetworkPolicy 一个** | 策略数（**不是** pod 数，**不是**策略组合数——见 §7.7.4a） |
| 地址组 | 每个 (namespace, peer-selector) 一个 | 不同 peer selector 数 |
| Neutron port | 每 capsule 一个 | pod 数（**今天已经在付**） |

**一条策略一个安全组，端口的安全组列表承载"这个 pod 被哪几条策略选中"。**
安全组规则是白名单，所以挂多个组 = 规则取并集，正好是 K8s "多条策略叠加"的语义。这是
ovn-kubernetes 的镜像——他们把 subject 放 port group、peer 放 address set；我们把
subject 放**安全组成员（端口属性）**、peer 放地址组。两者都做到"每个被选中的 pod
O(1) 条规则"。

⚠️ **配额不是约束。**devstack 出厂的 `secgroups=10 / rules=100` 是运维要调的默认值，
按 K8s pod 规模部署时放开几个数量级。**要论证的是增长率，不是绝对上限。**
（开通清单需新增：按租户规模调 `secgroup_rules` 配额。）

#### 7.7.3 代价模型：**哪个组件付钱**

这是整节最重要的表，所有设计取舍都从它推出来。

| 事件 | 落在哪条轴 | 增量处理 | 是不是新增开销 |
|---|---|---|---|
| pod 增删 | Port_Group 成员 + 地址组 delta | port group **无**；地址组**有** | port group 那笔**今天已在付**（端口必然加入某个组）；地址组是新增但便宜 |
| **pod 标签变动**导致换规则集 | Port_Group 成员 | **无**（`ovn-controller.c:4416-4470` 三种情况全走重新解析） | **新增，设计上要避开** |
| 策略增删**规则** | ACL 规则 | —— | 新增，northd 不全量重算 |
| **建/删安全组对象** | **全云 northd 全量重算** | —— | **最贵，要控制频率** |

- **northd 从不展开集合**：`northd/northd.c:7737` 把 NB 的 match 字符串原样抄进 SB
  逻辑流，`$addrset` / `@portgroup` 保持符号形式。**逻辑流条数与集合大小无关。**
- **但安全组对象增删会引发全云重算**：`northd/en-sync-sb.c:164-175`，只要有任何
  Port_Group 新增或删除就返回 `EN_UNHANDLED`，触发 `:503-528` 遍历**云里每一个**
  port group 重建地址集——**连 nova 和 Octavia 的一起**。规则增删不触发。
- **ovn-controller 两条轴不对称**：地址集成员变动有专门的 delta 路径
  （`ovn-controller.c:4380-4399`），Port_Group 成员变动**没有**（`:4416-4470`）。
- ⚠️ **SB 的 Address_Set 和 Port_Group 没有任何监视条件**（受控表清单
  `ovn-controller.c:422-439` 里没有它们）——**每台 chassis 都收全云所有租户的**
  地址集和端口组，包括一个 capsule 都没有的纯 nova 计算节点。**没有per-租户爆炸半径。**

**推论（设计约束）**：
1. **不要每策略/每命名空间建一个安全组** —— 那是全云 northd 那条轴。
2. **不要让 pod 标签变动频繁改写端口的安全组列表** —— 那是无增量路径那条轴。
3. **把 peer churn 全部赶进地址组** —— 唯一两端（Neutron driver 与 ovn-controller）
   都做增量的轴。

#### 7.7.4 明确拒绝的（不做近似实现）

近似的隔离和虚假的隔离是一回事，而那正是本节要修的东西。

| 拒绝 | 为什么不是"还没做" |
|---|---|
| **ANP / BANP** | 语义就是分层的 Pass/Deny。Neutron **没有 tier**（OVN driver 里零处 `tier`），只有两个固定优先级 `ALLOW=1002 / DROP=1001`（`common/ovn/constants.py:114-115`），安全组规则也**没有 deny 动作**（`acl.py:178-190`）。**没有地方能落。** |
| **`ipBlock.except`** | Neutron 匹配语法只生成 `==`（`acl.py:86-91`），无取反。可展开成补集，但每个 except 每个协议族最多约 32 条规则。**绝不能静默丢掉 except——那把"收窄"变成"放宽"，是 fail-open 方向。** |
| **命名端口（首版）** | 解析是**每 pod** 的，而规则属于组不属于目标。⚠️ **不要抄 ovn-kubernetes**：`gress_policy.go:118-122` 只读 `IntVal`，字符串端口得 0，随后 `getL4Match` 返回**裸 `tcp`**——一条本该只放行某端口的规则变成放行整个协议。**它在端口处理上不是正确性参照。** |

⚠️ **拒绝落在哪里：kubezun 不在准入路径上，它自己拒绝不了任何东西**，NetworkPolicy
也没有 status 字段可写。明确拒绝只能由 **Kyverno/VAP 或 kubezoo 网关**做（阶段 2 协作项）。
kubezun 单独能做的最强动作是**在 NetworkPolicy 对象上发 Warning Event**，
`kubectl describe netpol` 看得见。

#### 7.7.4a 规则集的两条非显然规则（2026-08-11 实现时定）

- **只写被隔离方向的规则。**一条策略隔离 ingress、同时列了 egress 规则,
  **它对 egress 毫无约束**——K8s 对"未隔离方向"的规则视为无效。照写会把一个
  租户特意留开的方向变成"只允许列出的那些",**收窄了它没要求收窄的东西**。
- **一条策略一个安全组,pod 携带选中它的那几个组。**安全组规则是白名单,所以
  **挂多个组 = 规则取并集**,而 K8s 的语义正好也是"多条策略选中同一个 pod 时允许项
  取并集"——两者精确对应。⚠️ **组名取自策略身份,不取自规则内容。**内容哈希看着更
  整洁,但在唯一要紧的那个维度上更差:编辑一条策略会**另造一个组**并让旧的变孤儿,
  而一个 pod 被新的策略组合选中会再造一个——**组数于是跟着"策略组合数"走,是指数级**。
  按策略命名则恒等于策略数,编辑只改组内规则,pod 归属变化**只是一次端口更新**。
- ⚠️ **"这条规则在不在有效方向上"必须逐策略判断,不能按 pod 汇总。**K8s 忽略策略
  未在 `policyTypes` 里声明的方向上的规则(`policyTypes: [Ingress]` 下的 `egress:`
  段完全无效)。按 pod 汇总时,这些死规则会在**另一条**策略隔离了该方向的那一刻
  **复活**——那既不是 API 说的,也不是租户写的。
- ⚠️ **地址组名也必须哈希**:选择器键里带标签表达式,可能超过 Neutron 的 255 字符
  上限,而截断后的名字**会被两个不同的选择器共用**——那等于让一条策略的 peer 替
  另一条回答。
- ⚠️ **v4 前缀不能做 v6 规则的 remote**(反之亦然),Neutron 直接拒;**而在写一组
  规则的中途被拒,会让 pod 停在"半条策略"上**。展开时按族过滤。

#### 7.7.4b 经过 Service 的流量,源地址保留(2026-08-11 实测)

`podSelector` 类的规则匹配**源 IP**,所以有一个必须验的问题:client → ClusterIP →
server 这条路上 Octavia 做了 DNAT,**server 看到的还是不是客户端 pod 的地址?**
若它同时 SNAT,一条"只允许 role=client"的策略会把**经过 Service 的合法流量全挡掉**,
而这是最常见的真实路径。

**实测:保留。**同一条策略下,直连和走 VIP 行为完全一致(client 通、stranger 挡)。
OVN provider 的 LB 只做 DNAT,不改源地址。

⚠️ **测的时候先撞上另一件事**:Service 的 LB 建出来了却**没有 pool 也没有 member**,
原因是 RBAC 只给了 `services/status` 的写权限,而 LB id 和 VIP 地址是记在
**Service 的注解**上的(注解在主资源上)。**照原清单部署,任何新建 Service 都对账不完**。
已在 `deploy/tenant-vk.yaml` 补 `services` 的 `update`/`patch`。

#### 7.7.5 Zun fork 侧需要的一刀（小，已做：fork `cb5a57da`）

驱动侧**已经**在读 `capsule.security_groups` 并传给 port（`cri/driver.py:323-330`），
而 `network/neutron.py:110-113` 的**创建分支**本来就会把 `security_groups` 写进 port。
**唯一缺的是 capsule API 不收这个字段**（`schemas/capsules.py` 零处提及）——补上
约十几行，端口就在**创建时**带着安全组，**没有 fail-open 窗口**。

⚠️ 否决了"kubezun 自建 port 再把 UUID 交给 Zun"（`nets: [{port: uuid}]` 这条路
技术上可行）：它要改 §7 的"Zun 原生 port"定案，还要我们接管 port 的生命周期
（`preserve_on_delete=True` 后 Zun 不再删它）——**为了省一个我们本来就在维护的
fork，买进一个定案变更和一份新的生命周期责任。**

#### 7.7.5a ⚠️ 迁移必须整租户一次切,不能逐个 pod（2026-08-11 实测）

**今天租户的东西向连通性,靠的是"所有 pod 在同一个 `default` 安全组里"。**
`default` 唯一的 ingress 规则是 **`remote_group_id` = 它自己**——即"只接受本组成员"。

于是把一个 pod 换成我们的两个 allow-all 组,它**立刻失去 DNS**:不是因为它自己的
规则不够宽(我们的 ingress 是 `0.0.0.0/0`,是 `default` 的**超集**),而是因为
**CoreDNS 仍在 `default` 里,于是丢弃这个已经不是同组成员的来访者**。
⚠️ **失败发生在接收方**,所以怎么放宽发送方的规则都没用——二分实测:
`default`+我们的任一组 → 通;**只有我们的两个组 → 不通**;而**两端都换成新组 → 通**。

**推论:转换是全有或全无的。**部分切换会在"已切"与"未切"之间造成断流,而断的
方向取决于谁还在 `default` 里,极难排查。

**安全迁移顺序**(每租户):
1. 给该租户**所有** capsule 的 port **追加**两个 allow-all 组(保留 `default`)——
   中间态是"更宽",不断流;
2. 全部追加完毕后,**再从所有 port 摘掉 `default`**;
3. 此后隔离翻转才有意义(`default` 还在时,它的同组 ingress 会让摘掉
   allow-ingress 也隔离不掉)。

⚠️ 新建的 capsule 从第 1 步开始就必须带上这两个组,否则它会成为"未切"的一员。

#### 7.7.5c ⚠️ 不变式:一个租户的 peer 同步只能有一个所有者

**同一租户的多个 VK 进程,必须服务同一份 namespace 集合。**

地址组名 `AddressGroupName(selectorKey)` **只哈希选择器,不含分片身份**——这是有意的:
同一个 peer selector 在全租户范围内应当解析出**同一个集合**。于是两个进程若
`ServesNamespace` 是**不同子集**,会各自算出**部分集合**,然后**每 10 分钟的全量重算
互相覆写一次,永远**。改成"把分片身份放进组名"更糟:那会让同一个选择器裂成两个组,
而每条策略的规则只引用其中一个,**结果是永久看不见另一半 peer**。

**旧模型满足它是因为"每租户一个进程"**;⚠️ 实验床上曾留着一份
`kubezun@111111-arm64.service`,用 `--namespaces 111111-default`(子集),而主进程用
`--namespace-selector kubezoo.io/tenant=111111`(全部)——**启动它就会撞出上面那个
定时互相覆写**。已删除;新增节点一律用 `--node`,不要另起进程。

**⚠️ 2026-08-13 共享节点形态下,这条不变式的满足方式变了,而且更脆:**

它现在依赖**一个租户的全部 namespace 恒定属于同一个分片**。所以:

- **归属粒度必须是租户,不能是 namespace。** 按 namespace 分配会把同租户的
  `<tid>-default` 与 `<tid>-kube-system` 分到不同进程,**当场违反本不变式**。
  ⚠️ 这一条正是我们**不能整段照抄 kubetron** 的地方:它的 `NamespaceShard()`
  就是按 namespace 解析的(`claim_webhook.go:81`)——对它成立是因为它的分片轴是 AZ、
  且它没有"跨 namespace 求并集"的地址组;对我们不成立。
- **归属判定只能有一套。** ⚠️ kubetron 有两套(claim 创建时打标、Service reconciler
  每次读 ConfigMap),同一 namespace 可能出现"claim 归 A、Service 归 B"——那**就是**
  双所有者。我们的归属只存一处(Tenant CRD,与 project id 同处,§2.1/§4.6.3)。
- ⚠️ **迁移仍然必须"先停旧、再起新",不能重叠。** 声明式分配(§2.1)没有消除这个要求,
  只是把它的单位从"全体租户"缩小到"一个租户"——可以逐个做、可以回滚、出问题只影响一个。
  ~~"K 一次定死"~~ 那条限制随哈希取模一起作废。

#### 7.7.5d ⚠️ 一个分片**一个**进程,不能靠多副本做高可用(2026-08-12 查证)

7.7.5c 说的是"两个进程服务不同 namespace 子集"会坏。这里说的是**服务同一份 namespace
的两个进程**——也就是把 kubezun 做成 `Deployment` 且 `replicas: 2` 的那种形态。
结论:**同样不行,而且坏的地方不在直觉指的那一处**。

> ⚠️ **2026-08-13 起读作"一个分片一个进程"**。共享节点形态把进程数从"每租户一个"改成
> `regions × K`(§2),但本节结论一字不改:**分片内也只能有一个写者**,要 HA 就上
> Lease 锁的 active/passive,不是 `replicas: 2`。而且爆炸半径变大了——现在一个进程
> 倒下影响 1/K 租户而非 1 个,**这是提高 K 的理由,不是上多副本的理由**。

**清理不会打架。** 三个清扫器在多副本下都是收敛的,因为它们是**纯函数**:输入(capsule
列表、pod 列表、策略列表)相同就得出相同结论,两个副本只会同时做同一个决定。

| 清扫器 | 判据 | 并发结果 |
|---|---|---|
| capsule 孤儿(`orphans.go`) | pod UID 不在 K8s | 同判;并发删,后到的拿 404 → `Warn` 一行 |
| 地址组 GC(`netpol.Sweep`,30 min) | 组不被任何策略引用 | 同判 |
| LB GC(`service/gc.go`) | 名字前缀解不出活 Service | 同判 |
| Zun 端口回收(`manager.py`) | `binding:host_id == self.host` | **计算节点之间根本不重叠**,天然无竞争 |

**打架的是创建,形状全是 check-then-act。** 两个副本同时"查——没有——建":

- `provider.go:214-221` `CreatePod`:`ListManaged` 没查到就建。**这个检查本来就是为了防重**
  ——注释写着 Zun 不拒绝同名 capsule,所以指望 API 报错是不行的。两个副本同时错过,
  就是一个 pod 两个 capsule,都在跑都在计费。
- `service/reconciler.go:225-232` `ensureLoadBalancer`:`GetLoadBalancerByName` 没查到就建。
  两个副本 → **一个 Service 两个同名 Octavia LB**。
- 地址组 `SyncAddresses`(`neutron.go:468`)是读-diff-写,并发写靠 400 当共识,
  同 namespace 集合下两副本算出同一个 `want`,**收敛**;只有 informer 缓存偏斜的瞬间会抖,
  下一次 pod 事件或 10 分钟全量重算抹平。

**然后清理去收拾创建的烂摊子,收拾得不对。** 这是真正的代价:

- 重复 capsule 会被孤儿清扫按"留最新的"处理(`orphans.go:127-139`),而被删的那个
  **可能正是另一个副本写进 pod status 的那个 IP** → `podIP == capsule OVN IP` 不变式破了,
  pod 指向一个已删除的 capsule。
- 重复 LB **连收都收不掉**:`parseLBName` 判的是"名字解出的 Service 还活着吗",
  重复 LB 的名字是**合法的**,所以 GC 认为它天经地义。而 `lbIDAnnotation` 是
  last-writer-wins,输的那个 LB 从此无人引用、无人回收、一直计费。

**没有 leader election**:全仓库零处 `leaderelection`,`go.sum` 里也没有这个依赖。
所以"多副本"目前不是"能力弱",是"**没有任何东西在阻止它双写**"。

**当前形态是对的**:`deploy/kubezun@.service`,systemd `Restart=always`,一个进程带多个
虚拟节点(重复 `--node`,共享 informer 和凭据)。这等于 active/passive,代价是重启期间有个
空窗。真要 HA,**方向是 active/passive 的 leader election(Lease 锁),不是 `replicas: 2`**
——上面每一条都要求"同一时刻只有一个写者",而不是"多个写者协调好"。

⚠️ **2026-08-13 后单元的实例名从租户变成分片**(`kubezun@<region>-<shard>`),
形态本身不变。

#### 7.7.5b 开通与运维清单

**RBAC 两项,都是实测才发现清单里没有的:**

- `networkpolicies` **只读**(get/list/watch)。⚠️ 缺了它的症状是**沉默的**:
  watch 拿到 403 只是一行日志、**不是启动失败**,于是"执行开关开着、一条策略都看不见、
  什么都不执行",看起来一切正常。
- `services` 的 **update/patch**(不只是 `services/status`)。LB 的 id 和地址记在
  **Service 的注解**上,注解在主资源上。⚠️ 缺了它,**任何新建 Service 都对账不完**:
  LB 建出来,pool 和 member 永远为空,而症状是一条"地址未就绪"的 Warning,
  **不是权限报错**。

**转换命令**(`deploy/kubezun@.service` 注释里也有一份):

```
kubezun --convert-network-policy=attach            # 只报告，不写
kubezun --convert-network-policy=attach --convert-confirm
kubezun --convert-network-policy=detach --convert-confirm
# 两阶段全租户跑完，才打开 --enforce-network-policy
```

只需要 OpenStack 凭据,**不需要 kubeconfig**——手工跑的那件事不该是准备工作最多的。

**开通清单新增**:按租户规模调 `secgroup_rules` 配额(devstack 默认 100 远不够)。

#### 7.7.6 分两步交付（增量 1 已做，增量 0 未做）

**增量 0（先做，零底层对象）**：准入 webhook 拒掉表达不了的东西（ANP/BANP、
`except`、命名端口）。**它把今天的静默 fail-open 变成明确拒绝，一个 OVN 对象都不造**，
不依赖任何未测量的规模性质，可独立发布。

**增量 1**：隔离翻转 + 每租户两个常量 allow-all 安全组 + 地址组承载 peer，
限定在可表达子集（数字端口、podSelector/namespaceSelector 由我们算成**一个**地址组、
普通 `ipBlock` CIDR）。

#### 7.7.7 ⚠️ 没有人给过数字

**OVN 自己不压测我们要造的那类对象**：`tests/perf-northd.at:197-215` 的 200HV×200port
规模测试建的全是逻辑交换机/路由器/端口，**零 ACL、零 Port_Group、零 Address_Set**；
`ovn-architecture.7.xml:2361` 只说"单个 OVN 控制面总有上限"。流传的 1M ACL / 500 节点
之类都来自提交信息描述的外部 ovn-heater 跑分，**不在任何可复现的代码里**。

**增量 2 之前必须自己压两个数**（实验床 `ovn-appctl stopwatch/show` 即可读）：
① northd 在**建删安全组**时的重算耗时；② 计算节点 ovn-controller 在**改端口安全组
列表**时的 `lflow_run` 耗时。**这两个数决定这套设计能不能撑过第一批租户。**

⭐ **2026-08-13 用途升级：它们同时是 region 数量规划的前置输入**（§7.4.2）。
OVN 的分片单位是 region，而**不知道一个 OVN 控制面能扛多少策略，就不知道该切几个
region**——于是 `节点数 = regions × K × AZs × archs` 里的第一项无从估算。
这两个数从"增量 2 的门槛"变成"**容量规划的输入**"，优先级随之上升。

---

## 8. 存储与配置

### 8.1 ConfigMap/Secret：真相源在 K8s，Barbican 不做主存储

- 租户 `kubectl create secret` + workload 引用是"完整 k8s 体验"的一部分；kubezoo 前缀
  隔离、RBAC、审计已在 K8s 侧闭环。Barbican 做主存储 = 双真相源 + 重建引用语义，否决。
- Zun 侧 Barbican 集成为零（全仓 grep 无命中），任何 Barbican 方案都是自建，成本自知。
- **Barbican 的两个正确位置**：
  1. **etcd KMS 加密后端**（建议直接做）：cloud-provider-openstack 现成 barbican-kms-plugin
     （kubetron CLAUDE.md:33 已列 CPO 为复用来源）——Secret API 语义留在 K8s，落盘密钥归
     OpenStack，对租户透明；
  2. **（P3，fork 项）Secret 进 capsule 的传输通道**：现方案 Secret 明文经 Zun API 落
     Zun MySQL（capsule spec 持久化）。介意则 fork：capsule spec 只带 Barbican ref，
     zun-compute 在 RunPodSandbox 时服务端拉取、挂 tmpfs，DB 永远只有引用。provider 把
     K8s Secret 同步成租户 project 下的 Barbican secret（生命周期跟随 pod）。
- provider 读取方式：**按对象 GET**，不用集群级 informer（(VK) nodeutil 默认 informer 是
  集群级 LIST/WATCH，namespace 级 Role 会 403，controller.go:329-346）。
- **语义声明**（租户文档）：无 kubelet 即无 ConfigMap 挂载热更新——env/文件启动后不可变，
  行为等同 K8s 原生 env 注入；改配置需滚动重启。Fargate 类产品标准约定。
- SA token：MVP 默认 Kyverno 强制 automountServiceAccountToken=false（capsule 不可变，
  bound token 无法轮换）；需要访问 apiserver 的负载可显式开启，创建时注入一次性 TokenRequest
  token 并文档声明 TTL；长期轮换通道等 fork 的文件刷新机制（ExecSync 落地后评估）。

### 8.2 卷

- **Cinder 是 capsule 唯一卷提供者**（(Zun) capsules.py:418-430 注释明示"cinder is the
  only volume provider... re-visited if Manila is introduced"）。
- **pod 内共享**：✅ capsule 多容器同 Kata VM，emptyDir 类共享成立——sidecar 模式完整可用。
- **跨 pod 共享文件**：hostPath 类**不存在**（无共享宿主机）。Cinder multiattach 有
  （(Zun) volume/driver.py:193-205）但是块级——非集群 FS 多写即损坏，仅"一写多读"可用。
  RWX 真解 = Manila/virtiofs，fork P3（CPO pkg/csi/manila 可参考）。
- **跨 pod Unix socket / fd 传递**：**物理不可能**——socket 是内核对象，capsule 各有
  guest kernel，同物理机也隔离。替代 = 网络通信（同租户 OVN LS 一跳直达 + headless
  Service/租户 DNS 发现）。

---

## 9. DaemonSet —— 已废弃（2026-08-13）

**定案：B2' 不支持租户 DaemonSet，与 B1 一致维持 `tenant-deny-daemonset.yaml`。**

放弃的理由不是做不到（模板 mutate 那套机制是通的），而是它**依附于每租户虚拟节点**，
而后者已被 §1.2 放弃。DS 在这里的价值本来就薄——旧版本节自己第一句就写着
*"DS 的机制价值（per-host agent）在逻辑节点上趋近于零"*，保留的只是 chart 兼容。
而 AWS Fargate 明确不支持 DaemonSet 且仍是成功产品，这佐证它不是必需品。

**连带消失的三样**（不要再当成"还没做"重提）：

| 旧机制 | 现状 |
|---|---|
| mutate 作用于 `DaemonSet.spec.template` | 不需要——不再为租户 DS 注入任何东西 |
| 每 AZ/分区自动扇出一份 | 不需要——节点不再是租户的分区 |
| DS 扇出进计费模型（旧 §3.5 唯一的非控制面成本） | 该成本项消失 |

**⚠️ 两件事没有跟着消失，别一起删掉**：

1. **系统 DS 排除仍然必需**（§4 第 5 条）。平台自己的 DS（kube-proxy/CNI/监控等，
   带 `operator: Exists` 全容忍）仍会试图落到虚拟节点上，仍要靠
   `nodeAffinity: type NotIn (virtual-kubelet)` 挡住。**租户 DS 废弃 ≠ 系统 DS 不会来**。
2. **平台自用的"每节点一份"需求**（旧文第 ③ 条：租户 DNS capsule、日志聚合器）——
   这些是平台侧的部署选择，不经租户 DS 通道，不受本条影响。

**租户文档措辞**：与 B1 完全一致——"命名空间内可运行 Deployment/StatefulSet/Job 等全部
工作负载；DaemonSet 不适用于 serverless 算力"。替代方案同 B1：sidecar / 网络推送（§8.2）。

---

## 10. Zun fork 工作清单

上游 2021 起纯维护、微版本停滞——rebase 成本低，fork 可行性已确认。
**维护边界**：docker driver、kuryr_network、Container API 划为不维护区；主干 =
capsule + CRI + zun-cni（正是以下各刀要动的地方）。

| 优先级 | 项 | 说明 |
|---|---|---|
| **门槛** | ExecSync + liveness 重启 | §6；stub 现成（api_pb2_grpc.py:100-103），在 zun-compute 落实执行 + restart 语义 |
| **门槛** | logs | CRI 驱动补 log_directory/log_path（cri/driver.py:89-94,181-192 现在不落日志文件）+ 新增 GET /capsules/{id}/logs（capsules.py:111-113 _custom_actions 为空）。capsule 容器 TYPE_CAPSULE_CONTAINER 无法借道 Container API（db/sqlalchemy/api.py:150-152） |
| P1 | capsule 模板收 `securityGroups` | §7.7.5；驱动已在读 `capsule.security_groups`（`cri/driver.py:323-330`）、`neutron.py:110-113` 创建分支已会写进 port，**只差 API schema 不收**（`schemas/capsules.py` 零处）。补上即可让端口在创建时就带安全组，无 fail-open 窗口 |
| P2 | Barbican secret ref 注入 | §8.1；与上面两刀同动 cri/driver.py sandbox 路径 |
| P3 | Manila/RWX（virtiofs） | §8.2 |
| ✅ 已做 | 架构上报 + capsule 架构约束 | §3.6；`driver.py` 上报 architecture/trait，capsule 模板加 `architecture` → `trait:COMPUTE_ARCH_*=required` |
| **P1** | 同 owner capsule 反亲和（**平台默认启用**） | §4.5；⚠️ 不是锦上添花——实测 8/8 capsule 堆在一台，三个计算节点两个全空。Zun 无 weigher（只有 `filters/`），filter 后不排序取首个可 claim 的主机（`filter_scheduler.py:75-105`）。两侧都要动：Zun 补 weigher + 反亲和实现，kubezun 补 owner 标签 |
| 候选 | nets/固定 IP 传递核实与补齐 | §5 待实测后定 |
| 顺手 | linux_net.py ovs-vsctl → ovsdb | 上游自己的 TODO（linux_net.py:48）；规模下省每次 fork 进程 |

exec（kubectl exec）第一期砍掉返回 errdefs 明确错误。

---

## 11. kubezun 重写清单

现有 745 行基于已归档 node-cli v0.1.2 + go 1.12 + k8s 1.14（go.mod:3、main.go），实质是
**参考骨架的重写**。唯一整体保留资产：状态映射（zun.go:447-536，修三处 bug，§5）。
config.go:20-56 loadConfig 是死代码，弃。

| 优先级 | 模块 | 工作量 | 章节 |
|---|---|---|---|
| P0 | 依赖栈升级（go/k8s 1.36 对齐 kubezoo）+ 迁移 nodeutil.NewNode，重写 main/provider 骨架 | 大 | §1.1 |
| P0 | provider namespace 白名单 + defer recover + nil-map 修复 | 小 | §4/§5 |
| P0 | 多租户凭据层（appcred per-tenant，废弃 AuthOptionsFromEnv 单凭据，zun.go:41-75） | 中 | §2 |
| P1 | Pod→capsule 转换重写（uid 命名/nets/limits/errdefs 显式拒绝） | 中 | §5 |
| P1 | podIP==OVN IP 回填 + port ACTIVE Ready + NotifyPods + 异步 DeletePod | 中 | §5 |
| P1 | 节点真实上报（~~配额镜像~~ **静态大额** capacity/conditions/InternalIP/标签污点全集） | 中 | §3 |
| P1 | 状态映射三处修复 | 小 | §5 |
| P2 | ConfigMap/Secret/emptyDir 合成（按对象 GET）+ SA token 策略 | 中 | §8 |
| P2 | logs/exec 通路（对接 fork 的 /capsules/{id}/logs；exec 返回 errdefs） | 中 | §10 |
| P2 | 孤儿 capsule 治理（不可伪造标记） | 小 | §5 |
| P3 | capsule 预热池（kubetron NetworkPortPool 水位模型作蓝本，"预绑 host_id"换"预建 capsule"；对标 kata 冷启动数十秒 → 秒级） | 大 | §7 |

**共享节点形态新增（2026-08-13，§1.2 定案的落地面）**：

| 优先级 | 模块 | 工作量 | 章节 |
|---|---|---|---|
| **P0** | **凭据按 namespace 解析**：`zunClient` 单值 → `namespace → project → Secret → clients` 解析器。⚠️ 影响 8 个子系统构造点（capsules/块存储/共享存储/netpol/Octavia/Neutron/KeyManager/Subnets，`main.go` 19 处引用）；namespace/授权侧无需改动（`--namespace-selector` 已是多租户） | 大 | §4.6 |
| **P0** | **project 绑定三态校验**（无记录→写入 / 一致→放行 / **不一致→拒绝启动**）。⚠️ 必须在同步循环之前；⚠️ 校验对象是 token 里的 project id，**不是 Secret 哈希**（否则禁掉轮换） | 小 | §4.6.3 |
| **P1** | 节点补 `topology.kubernetes.io/region` + PV nodeAffinity 加一条 MatchExpression。⚠️ **必须先于第二个 region 上线**，否则同名 AZ 静默错配 | 小 | §3.1 |
| P1 | 节点身份改造：名字/标签/污点去租户化（`knaas.io/serverless` 取代 `knaas.io/tenant`），加 `knaas.io/shard` | 小 | §3.1 |
| P2 | 分片装配：**声明式**归属（Tenant CRD 上一个字段），⚠️ 粒度必须是**租户**不是 namespace，且**归属判定只能有一套** | 中 | §2.1/§7.7.5c |
| **P1** | **informer 收窄**：`scmFactory` 八类对象仍是全集群缓存（实测 2 租户时已 6 倍过取）。⚠️ **现存缺陷，不是新形态才有的**；⚠️ `allPods` 只能收窄到**分片**不能到单租户 | 中 | §2.2 |

平台侧配套（不在本仓库）：

- Tenant CRD 开通控制器扩展：appcred 开通 + **写平台命名空间 Secret** + **记录 project id**
  （绑定真相源，§4.6.3）+ Kyverno 实例 + ResourceQuota。⚠️ 不再包含"节点 spec / 每租户 VK
  Deployment"——节点与进程都不再是租户资产。
- kubezoo：`NodePoolFor(tenantID) = tenantID` 改造（⚠️ 该函数注释写明**三处必须一致**）；
  节点从租户视图移除。
- ~~tenant-deny-daemonset 分档~~ → **两档统一 deny**（§9 废弃）。
- Kyverno/VAP 策略集（§4）、kubetron DNS 分发通道改造、M8 编排层独立部署形态。
- **重绑运维流程**（§4.6.4）：先在旧绑定下清空并核验归零，再改绑定。⚠️ 顺序反了会造成
  永久失联的 capsule。

---

## 12. 路线图

| 阶段 | 内容 | 验收 |
|---|---|---|
| **0 PoC**（先于一切编码） | 手工验证既定路线：租户网建 capsule + Octavia OVN LB + 租户 DNS | capsule 内 curl Service VIP 通、DNS 解析到 VIP；顺带实测 nets 传递、preserve_on_delete、per-container restart 保 IP（§14 三项待定一并清掉） |
| **1 MVP 单租户** | P0 全部 + P1 转换/状态/节点上报 | 1.36 集群上 Deployment pod 经逻辑节点建 capsule，状态/删除全链路正确；带 limits 不 panic；不支持字段明确报错；EndpointSlice 出现 capsule OVN IP 且 kubetron LB 收编成功 |
| **2 多租户** | Tenant CRD/appcred/Kyverno+VAP/placement/kubezoo 视图接入 | 渗透用例：两租户互相无法经 nodeName/binding/nodeSelector 到对方资源；A 的 capsule 在 A 的 project 走 A 的网络；kube-system 控制器 pod 实测过 Kyverno；租户 `get no` **为空** |
| **3 探针** | fork ExecSync/liveness 重启 + 容器内探针 + 系统 DS 排除 | liveness 失败触发重启；readiness 结果回写 pod Ready；系统 DS 不落虚拟节点、真实节点无 Pending 残留。⚠️ **租户 DS 已从本阶段移除**（§9 废弃） |
| **3.5 共享节点形态**（2026-08-13 新增） | 凭据按 namespace 解析（§4.6）+ project 绑定校验 + 节点补 region 标签 + kubezoo `NodePoolFor` 改造 | 一个进程服务两个租户，各自 capsule 落各自 project（**判据必须能分辨"落对了"和"两边都没建"**）；改 Secret 换 project → **拒绝启动**而非静默切换；两个 region 同名 AZ 的 PV 不互相错配 |
| **4 生产化** | fork logs / Barbican KMS+ref / SA token / 预热池 / 心跳调优 / 计费 | kubectl logs 可用；规模实测（`/root/kwok-scale-lab`，§3.5 未测项）；单分片 VK 崩溃只影响该分片（故障注入）；⚠️ 心跳调优**上限 50s**，见 §3.5 那堵墙 |

---

## 13. 已否决方案（不要重新提议）

| 方案 | 否决理由 |
|---|---|
| kubezoo 直接注册 Node（无 VK） | Node 是契约非记录；kubezoo 是无状态翻译器，履约即在网关内重写一个 VK+provider，且破坏其定位与每租户隔离原则（§1.1） |
| 剧场节点（视图层伪造 Node，pod 实跑 B1 池） | DS 立刻穿帮（DS controller 按上游真实 Node 扇出）；节点级调度语义全假；kubezoo 翻译面深度膨胀。仅可作过渡期产品单独评估，不通向 DS |
| B1.5 影子 pod 路线（VK 后端=K8s worker） | 双倍 etcd 对象 + 镜像状态机，买到的只是"B1 算力换节点皮"，算力经济学与 B1 相同；投入不如砸向有真差异的 Zun 线（2026-08-06 定案） |
| kubetron NetworkPortClaim 对接 capsule | 时序问题在 Zun 内联路径不存在；强套造 host_id 双主冲突（§7） |
| NetworkPolicy 的 peer 用 `remote_group_id` | 它解析成安全组**自己的** port-group 地址集（Neutron `acl.py:226-233`），把 peer churn 压到 ovn-controller **没有增量路径**的那条轴上（`ovn-controller.c:4416-4470`）。用 `remote_address_group_id`（§7.7.1） |
| 每策略／每命名空间建一个安全组 | 安全组**对象**增删触发**全云** northd 全量重算（`en-sync-sb.c:164-175`→`:503-528`，连 nova 和 Octavia 的 port group 一起重建）。安全组随**不同规则集**增长，不随策略数（§7.7.3） |
| kubezun 自建 port 再交 UUID 给 Zun（为挂安全组） | 技术可行（`nets: [{port: uuid}]`），但要改 §7"Zun 原生 port"定案 + 接管 port 生命周期，只为省一个我们本来就在维护的 fork 里十几行（§7.7.5） |
| 直接写 OVN NB / 依赖 northd 调优旋钮 | 我们只经 Neutron API 到 OVN。SB relay、`--n-threads`、`ovn-monitor-all` 都不归我们，属于 OpenStack 控制面运维（§7.7.3） |
| 静默近似 `ipBlock.except` / 命名端口 | 丢掉 `except` 把"收窄"变成"放宽"；命名端口解析不了就退化成裸协议——**都是 fail-open 方向**，正是本项要修的病（§7.7.4） |
| Barbican 作为 ConfigMap/Secret 主存储 | 双真相源；破坏 K8s 体验；重建引用/权限语义（§8.1） |
| provider 远程探测 OVN IP（探针方案 B） | 要求管理面进程挂进重叠 CIDR 租户网，违反 kubetron 双挂纪律（§6） |
| ~~单进程多租户 VK~~ | ⚠️ **2026-08-13 已翻案，不再是否决项**——该行原本就写明"凭据外置+informer 白名单+per-node 身份三件事完成后可作为成本优化重评"，三条现已满足（§2 有逐条对照）。⚠️ 原理由里"全集群 secret 缓存集中"在我们的实现上**从来不成立**（`ObjectReader` 是按对象 GET，`pkg/vknode` 就是为绕开 VK 库默认 informer 而写）。剩下的凭据集中与 panic 爆炸半径由 K 旋钮定价（§2），不再是有或无 |
| **每租户虚拟节点**（2026-08-13 新增否决） | 节点数 ∝ 租户数，而**空闲租户在 K8s 侧是全价**：1 Node（实测 4,614 B）+ 1 Lease（707 B）+ 每 10s 一写 + 一个 66–84 MB 进程。买到的只有 `get no` 非空与 DS，而"租户零节点"本就不是差异（kaaas §2.2）。**判定信号**：这一刀让四个正在讨论的补丁同时失效（注 nodeName / 中和 NLC / 拉长 lease / 惰性节点），它们全在给同一个前提打补丁。见 §1.2 |
| **租户 DaemonSet**（2026-08-13 新增否决） | 依附于每租户虚拟节点，随之废弃。机制价值在逻辑节点上本就趋近于零（旧 §9 自陈），Fargate 不支持 DS 亦是成功产品。⚠️ **系统 DS 排除仍然必需**——租户 DS 废弃不等于系统 DS 不会来（§9） |
| 在 K8s 侧解决 capsule 的物理反亲和 | 两层都堵死：kubezoo 早已丢弃 `spec.Affinity` 与 `spec.TopologySpreadConstraints`（`placement.go:167-168`）；且**K8s 看不见物理机**——一个逻辑节点代表该 AZ 的全部 Zun 计算节点。只有 Zun 调度器同时看得见副本与硬件（§4.5） |
| 把 appcred Secret 放租户命名空间、靠 kubezoo 视图过滤 | Secret **不需要被看见就能被使用**：pod spec 引用即可，而该路径不经 kubezoo（`provider/files.go:35` 由 kubezun 用自己的凭据按名 GET）。要挡就得挡**引用**，即覆盖 volumes/projected/envFrom/secretKeyRef/imagePullSecrets 每一条且随上游演进维护——封闭白名单形状。放平台命名空间是结构性的：pod 只能挂自己命名空间的 Secret（§4.6.2） |
| Zun admin 凭据 | 现成跨租户读/删洞（§2） |
| VK 默认 virtual-kubelet.io/provider 污点 | 被通用 chart 全容忍规则误踩；用 `knaas.io/serverless`（§3.1，2026-08-13 前为 `knaas.io/tenant`，随节点去租户化改名） |

## 14. 待定项

1. ~~**nets 字段可传递性**~~ —— **已实测结案（2026-08-06，PoC）**：请求体 schema 直接
   引用 capsule_template（schemas/capsules.py:17-26），**字符串与对象两种形态过同一处
   校验**，"对象模板绕过"的设想被证伪。fork 分支 `feat/capsule-nets-schema` 给
   capsule_template 补 `"nets": nets`（复用 Container API 同一定义）后，两种形态均
   通过，capsule 正确落在指定租户网络 → **provider 可直接用 gophercloud 原生
   `capsules.CreateOpts`，无需自组 body**。附带：availabilityZone 本就在白名单且被
   消费——§3.4 按 AZ 节点的 capsule 落位无需 fork。
2. ~~**liveness 重启保 IP**~~ —— **已实测结案（2026-08-06，PoC-4）：机制可行，但
   port 生命周期必须由 provider 自己管，不能依赖 Zun。**
   实测链路：provider 预建 Neutron port → capsule 用 `nets:[{"port": <id>}]` 起（此路径
   `preserve_on_delete=True`，common/utils.py:528）→ 删除 capsule 后 **port 与 IP 均保留**
   → 清空 port 的 `device_id`/`device_owner` → 用同一 port 重建 capsule →
   **IP 完全一致（192.168.100.166）**。
   ⚠️ **Zun 的自动 unbind 不可用**：`_delete_neutron_ports` → `delete_or_unbind_ports`
   → `_unbind_port`（neutron.py:154-187）本应清 device_id，实测先报
   `Forbidden: rule:update_port:device_id and rule:update_port:device_owner`（其 admin
   客户端权限不足），换 admin 凭据后 device_id 仍未清空。
   **定案：kubezun provider 用租户 appcred 自建/自清/自复用 port**（建 port → 传
   `nets:[{"port"}]` → 删 capsule 后清 device_id → 重建复用）。好处：IP 稳定性不依赖
   Zun 的 admin 权限配置，且与 §2"每租户 appcred、严禁 admin 凭据"一致；代价是
   provider 要为 port 的孤儿回收负责（capsule 删除后 port 需按 pod 生命周期显式删除）。
3. **VK 进程的部署形态：宿主机 systemd 还是集群内 Deployment**（2026-08-07 提出）。
   自然形态是 Deployment——凭据变 Secret、证书变 Secret、kubeconfig 变投影 token，
   `deploy/tenant-vk.yaml` 的 SA/RBAC 原样可用。**未决的是地址**：
   apiserver 按节点上报的地址拨 kubelet API，而 pod 的地址每次重新调度都变。
   两条路：① 每次启动重新签证书（真 kubelet 的 bootstrap 就是这么做的）；
   ② 上报一个稳定名字作 `InternalDNS` 地址、证书覆盖该名字——但这取决于控制面
   能否解析集群 DNS（托管控制面不一定能）。
   两者都不难，但差别足够大，值得**明确选一个而不是默认滑进去**。
   在此之前，已验证并支持的形态是宿主机 systemd（`deploy/kubezun@.service`）。
   ⚠️ 若走 hostNetwork 规避地址问题：计算节点的 kubelet 已占 10250，需换端口。
4. ~~**Service→Octavia 由谁编排**~~ —— **已定案（2026-08-07，用户决策）：kubezun 自建，
   与 kubetron 共存，各管各的。** 理由是两者是不同形态的产品：kubetron 的数据面半边
   （claim/pool/binding、webhook cni-args 注入）服务的是"K8s pod + Multus/ovs-cni"，
   capsule 走 Zun 原生 port 结构性不需要；而其 Service reconciler 的
   `memberEndpoint`（`members.go:100-147`）强制 member 带 kubetron claim 注解并从
   `NetworkPortClaim.Status.Subnet.ID` 取子网，capsule 无 claim。

   **共存安全性已查证**：
   - **Neutron port 不会被误清**——kubetron 的孤儿 GC 按 `device_owner` tag 过滤
     （`kubetron` / `kubetron:<clusterid>`，`pkg/neutron/clusterid_test.go` 明示这是
     为多实例共享一个 Neutron 而设计），Zun 的 port 是 `compute:zun`（consts.py:80），
     不同 owner，列都列不到。
   - **kubetron 不会去动 kubezun 的 pod**——没有它认识的注解就不进它的处理路径。

   ⚠️ **唯一边界条件**：`memberEndpoint` 对无注解 pod 是 **return error 而非 skip**，
   且 `BuildMembers` 直接向上抛（`members.go:54-57`）。所以**同一个 Service 同时选中
   两种 pod** 时，kubetron 对该 Service 的 reconcile 整体失败，连它自己的 member 也停止
   更新（`BatchUpdatePoolMembers` 是全集合 PUT，不执行 = pool 冻结在上一状态，比清空
   安全但不再跟随 readiness，且只在 kubetron 日志可见）。
   按租户模型不该发生（B1/B2' 不同租户不同 namespace）；**会撞上的是迁移场景**
   （一个租户从 B1 迁到 B2'，或同时用两档）。记录备查，不为它设计。

   **自建的工作量清单**（kubetron 那半边不是薄封装，重写要连这些一起）：
   Service→LB/listener/pool 生命周期、**幂等全量 PUT**（`BatchUpdatePoolMembers`
   是全集合语义，少放一个 member 就清空整个 pool）、LB GC、双栈（一个 pool 绑一个族）、
   Ingress 复用。可**照抄 kubetron 的实现**（同一作者、同一 gophercloud v2 栈），
   subnet 改为取自 capsule 地址记录的 `subnet_id`（`pkg/zun/capsule.go:59`），
   不再需要 claim。

5. **kube-system 控制器 pod 过 Kyverno**：resourceFilters/excludeGroups 核查——决定
   validate 边界的实际覆盖。
6. **租户业务标签写自己节点**：MVP 禁；有 pinning 需求再经 VAP 白名单放开。
5. **InternalIP 展示值**：是否在 kubezoo 层改写为中性值。
6. **SA token 长期轮换通道**：fork ExecSync 落地后评估文件刷新机制。
7. **单进程多节点 informer 共享**的具体实现形态（pod watch fieldSelector 合并粒度）。
8. **PVC 供给流程**：租户 PVC → 租户 project 内 Cinder 卷的 provisioner 是谁——
   候选 = CPO cinder-csi 作 provision-only（⚠️ 其 cloud.conf 静态多 cloud 凭据模型需换
   per-tenant appcred，kubetron CLAUDE.md 已记录同一问题）或 provider 直管。capsule 侧
   挂载走 Zun 原生 Cinder 路径不变（§8.2），本项只关乎 PV 对象与卷的生命周期归属。
