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

**kubezun = KAaaS 平台的第二条算力产品线（B2'）：每租户逻辑虚拟节点 + serverless 容器算力
（pod = Zun capsule，Kata 隔离，租户零 worker 节点）。**

它与 B1 的关系是**体验档位共存**，不是替代：

| | **B1（现状，kubezoo+kubetron）** | **B2' KNaaS（kubezun + Zun fork）** |
|---|---|---|
| 租户体验 | "能跑 pod 的命名空间"：workload/Service/DNS 完整；`kubectl get no` 为空；DaemonSet 拒绝（`tenant-deny-daemonset.yaml` 维持现状） | 完整集群幻觉：逻辑节点可见、DS 扇出、配额即容量、AZ 拓扑语义 |
| 算力 | 平台共享 kata 节点池，K8s 记账 | Zun capsule，**归属租户 OpenStack project**（kaaas 文档 §2.4 翻案条件成立：容器与 Nova VM 共用 OpenStack 配额/调度/计费） |
| 网络 | kubetron 全量（port 接入 + 编排） | Zun 原生 port + **kubezun 自建编排**（§7、§14.4） |
| 探针/logs/exec | 原生 kubelet 白拿 | Zun fork：容器内探针 + logs + ExecSync（§6，已实现） |

两档可同租户混用：B1 pod 与 B2' capsule 都满足 podIP==OVN IP 不变式，同一个
EndpointSlice / 同一个 Octavia LB 后面可同时站两种后端，租户升级 KNaaS 时 Service 流量
无缝过渡。

**产品叙事**：围绕"算力归属 OpenStack project + 完整节点语义"，不是"租户不买节点"
（B1 同样做到后者，不构成差异）。B1 的空节点列表就让它空着——这是两档之间诚实的产品边界，
也是升级 B2' 的理由。

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
- 分层红利：provider 接口之上的一切投资（kubezoo 视图、placement、Kyverno 策略、DS 机制、
  kubetron 编排）只与 K8s API 对象交互，对后端零感知。换后端的爆炸半径被严格限制在
  provider 实现 + 探针/logs 通道一层。

---

## 2. 分层架构与组件边界

```
租户 kubectl
  │
【视图层】kubezoo-gateway/controller/contract
  证书 OU/SA token 识别租户、ns/name/group 三前缀改写、impersonate <tid>-admin。
  不伪造 Node（三处豁免已删并实测，kaaas 文档 §7.1）；VK 节点名带 <tid>- 前缀即
  自然进入租户视图。硬依赖理由：K8s RBAC 无法按对象名过滤 list 结果。
  │
【准入层】Kyverno / VAP / MAP
  写路径守门：特权屏蔽、pod-security（替代可绕过的 PSA，kaaas §8.2.1）、placement
  注入、nodeName 禁写。碰不到读路径（kaaas §8.0）。
  │
【调度层】上游原生 kube-scheduler
  靠 kubezoo.io/pool=<tid> 标签 + tenant taint 把租户 pod 钉到其逻辑节点。
  真 Node 对象 + 真调度器 = 调度体验不是模拟出来的。
  │
【算力层】B1: kata 真实节点池 + 原生 kubelet（主路）
         B2': kubezun 逻辑虚拟节点 → Zun capsule（本文档）
  │
【数据面】OVN/Octavia（B1 由 kubetron 编排，B2' 由 kubezun 自建编排，两者共存互不干涉）
  Service = Octavia OVN LB（member = OVN IP，EndpointSlice 驱动）、租户 DNS zone、
  VIP 独立子网 + tenant router。K8s Service CIDR 与数据面无关。
  │
【身份/配额】Keystone application credential + Neutron RBAC + K8s ResourceQuota
  namespace 只是解析域，租户边界 = OpenStack project（kubetron DESIGN §4.2）。
```

**kubezun 自身的部署形态**：每租户一个独立 VK Deployment（跑在管理节点上），管理该租户的
全部逻辑节点。理由（多租户审查定案）：

- (VK) nodeutil 默认为节点建**无过滤全集群** Secrets/ConfigMaps/Services informer
  （node/nodeutil/controller.go:329-346）——共享进程 = 进程内存里 N 份全量集群 secret，
  一次 RCE 全集群沦陷；
- 每租户独立进程使 Keystone 凭据、panic 爆炸半径、缓存全部按租户隔离；per-node :10250 +
  独立证书 + `WebhookAuth(nodeName)` 自然成立（避开 (VK) auth.go:167-181 授权属性为
  nodes/<nodeName> 而 PodHandler 路由不含 node 的打穿问题）；
- 同租户进程内多节点共享 informer（pod watch 按 nodeName fieldSelector 合并），把单节点
  边际内存压到接近零。

**凭据纪律**：每租户一份 Keystone application credential（unrestricted=false、限定 role、
设 expires_at），存放于 VK 自己的 namespace，租户不可见。**严禁 admin 凭据**——Zun admin
context 强制 all_projects=True（(Zun) api/utils.py:70-71）+ DB 查询不加 project 过滤
（db/sqlalchemy/api.py:111-118）+ 按名跨项目查找（同文件 215-228），是现成的跨租户读/删洞。
客户端构造直接复用 (kubetron) pkg/neutron/provider.go 的 `NewClientFromAppCred`（gophercloud v2）。

---

## 3. 逻辑节点规格

**本质：逻辑节点不是机器的化身，是"配额分区 + 调度目标 + 节点语义 API"的呈现物。**
背后没有宿主机，有的是租户的 Keystone project、OVN 网络和一份配额。

### 3.1 对象模型（租户经 kubezoo 视图所见）

```yaml
apiVersion: v1
kind: Node
metadata:
  name: node-az1                      # 上游实名 <tid>-node-az1
  labels:
    kubezoo.io/pool: <tid>            # placement 钉子（(kubezoo) convert/placement.go:131）
    type: virtual-kubelet             # 系统 DS 排除锚点（(VK) controller.go:296-302 默认标签）
    topology.kubernetes.io/zone: az1  # 真实语义：capsule 落该 AZ 的 Zun 资源池
    node-role.kubernetes.io/serverless: ""   # ROLES 列显示 serverless
    kubernetes.io/os: linux           # ⚠️ well-known 三件套必须齐——大量标准 chart 默认
    kubernetes.io/arch: amd64         #    nodeSelector {kubernetes.io/os: linux}，缺失则
    kubernetes.io/hostname: node-az1  #    helm install 全部 Pending 且极难排查
    node.kubernetes.io/instance-type: knaas.serverless
spec:
  taints:
  - key: knaas.io/tenant              # ⚠️ 不用 virtual-kubelet.io/provider 默认污点——
    value: <tid>                      #    会被通用 chart 的全容忍规则误踩
    effect: NoSchedule
status:
  capacity:                           # 动态镜像租户配额（§3.2），非硬编码
    cpu: "64"
    memory: 256Gi
    pods: "200"
  addresses:
  - type: InternalIP
    address: <VK 实例 IP>             # logs/exec 经 apiserver 回连 :10250 的前提
  daemonEndpoints:
    kubeletEndpoint: { port: 10250 }
  conditions:
  - type: Ready                       # = VK 存活 ∧ Zun API 可达 ∧ 租户网络就绪（§3.3）
  nodeInfo:
    kubeletVersion: v1.36.3-knaas.1   # ⚠️ semver 兼容格式——operator 会解析它做特性门控
    containerRuntimeVersion: zun://kata-3.x   # 诚实声明，不伪装 containerd
    operatingSystem: linux
```

污点在 `describe no` 对租户如实展示、不在 kubezoo 层隐藏：租户 pod 的 toleration 由
placement 自动注入（§4），裸 manifest 照常调度；看得见污点能解释行为，也兼容自带
tolerations 的 chart。

### 3.2 容量 = 配额镜像（本节最重要的决定）

capacity 实时镜像租户 ResourceQuota（K8s ResourceQuota 是唯一记账闸门——Zun quota 对
capsule 结构性不记账：count_usage 按 container_type 只数 TYPE_CONTAINER，(Zun)
objects/container.py:374 + quota.py:569-582，capsule 创建路径无检查）。

依据 kaaas 文档 §2.3 教训：**静态容量把失败从调度期 Pending 位移到 ContainerCreating
卡死**。镜像配额后，超卖在调度期得到清晰 Pending + 事件；租户升级套餐 → 控制器改
ResourceQuota → VK 同步抬 capacity，"扩容节点"零秒完成。现有硬编码 cpu=20/mem=100Gi
（zun.go:66-68,539-545）废弃。

### 3.3 conditions / addresses

- Ready 是真实健康信号：Zun API 失联、租户 Neutron 网络异常都打 NotReady，让租户在节点层
  看到平台侧故障。现有静态恒 Ready + OutOfDisk（zun.go:255-299）废弃。
- 没有 DiskPressure/PIDPressure 等机器态 condition（没有机器）。
- InternalIP 现返回 nil（zun.go:303-305）必须修——它是 logs/exec 回连断裂的根因之一。
  它暴露管理网地址，介意可在 kubezoo 翻译层改写展示值，apiserver 用上游真值（待定项 §14）。

### 3.4 数量模型（节点数是产品旋钮，不是机器数）

| 形态 | 节点数 | 场景 |
|---|---|---|
| 默认 | 1 | 绝大多数租户；DS = 每租户一份 |
| 按 AZ | 每 AZ 一个 | zone 标签映射真实 AZ：topologySpreadConstraints、DS per-AZ 扇出、AZ 容灾**全部免费复活**——K8s 拓扑机制原样工作 |
| **按架构** | **每架构一个** | 混合 x86/ARM 计算池；见 §3.6 |
| 按分区 | 每资源池一个 | 隔离"生产/批处理"配额：每节点镜像不同配额分区 |

生命周期：Tenant CRD 声明式创建/销毁；缩节点走标准 drain（capsule 无宿主机绑定，迁移即重建）。

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
| etcd 对象 | ~10KB | 1 Node + 1 Lease |
| apiserver 写 QPS | ~0.1–0.2/s | lease 心跳 + status 更新，控制面主要持续成本 |
| VK 进程内存 | ~10–50MB | 共享 informer 后接近零 |
| **DS 扇出** | **真实算力** | 唯一非控制面成本：每个 DS 在新节点多一份 capsule，**进计费模型** |
| 监控基数 | 中 | node 系列指标/告警累积 |

**战略成本**：全租户逻辑节点共享上游集群的节点规模预算（单集群实用上限几千）。对策：
① 心跳间隔放宽到 30–60s——逻辑节点不会像物理机那样突然宕机，其健康即 VK 进程健康，
这是虚拟节点独有红利，写 QPS 直降 3–6 倍；② 按套餐限节点数；③ 规模墙的出路 =
kubezoo M8 分片 + 多上游集群（M8 优先级据此重估）。空节点在 OpenStack 侧零成本
（不建 capsule 不建 port）。

---

## 4. 调度与安全边界

**层级：provider 硬校验是安全边界，准入策略是第二层，mutate 只是便利。**

1. **provider namespace 白名单（唯一不可绕过的授权边界，必需项）**：(VK) PodController 只按
   spec.nodeName 过滤（node/nodeutil/client.go:53-58），该字段创建者可直接写死绕过调度器。
   因此 CreatePod/GetPod/GetPodStatus/GetContainerLogs 等所有入口先校验
   `pod.Namespace ∈ 本租户命名空间集`，不匹配返回 errdefs.NotFound。
2. **Kyverno validate（deny，failurePolicy=Fail）—— 不是兜底，是防 DoS 的必需层**：
   禁租户写 spec.nodeName；禁 nodeSelector/toleration 指向非本租户节点；RBAC 收回租户
   对 pods/binding 子资源的 create（第二条逃逸路径）。上线前实测 kube-system 控制器 SA
   创建的 pod 确实经过策略（核查 resourceFilters/excludeGroups）。
   **实测依据（2026-08-07 阶段 2 渗透）**：租户 A 用 `spec.nodeName` 直写或用
   `nodeSelector: kubezoo.io/pool=B` + B 的 toleration，**K8s 调度层都挡不住——pod
   确实被绑到 B 的节点上**；provider 白名单让它停在 ProviderFailed，B 的 OpenStack
   project 里零 capsule（执行面安全）。但 **被拒的 pod 仍计入 B 节点的 Allocated
   resources**（实测一个 limits=4CPU/8Gi 的攻击 pod 占掉 B 节点 12%），因此 A 可以用
   大 limits 的垃圾 pod 耗尽 B 的可调度容量，让 B 自己的 pod 报 Insufficient cpu。
   → **执行面靠 provider 白名单，容量面必须靠准入层拦截**，两层缺一不可。
3. **VAP 保护 Node 写面**：nodes/status 只许 VK 自己的凭据写（前缀归属使租户"拥有"自己的
   虚拟节点名，必须闸住）；受保护标签/污点前缀（kubezoo.io/、knaas.io/、node-role、
   topology.*）只许平台写。租户业务标签写入 MVP 先禁、按需求开（待定项）。
4. **placement mutate（便利层）**：复用 (kubezoo) convert/placement.go:118-155 机制——剥
   nodeName、注入 pool nodeSelector + tenant toleration。⚠️ 对 DaemonSet 必须作用于
   **spec.template**（§9）。
5. **系统 DS 排除**：给 kube-proxy/CNI 等 operator:Exists 全容忍 DS 注入
   `requiredDuringScheduling nodeAffinity: type NotIn (virtual-kubelet)`（AKS virtual node
   同款）。托管集群无权改时靠第 1 条兜底。provider 入口 defer recover，防漏网 pod 打挂进程。

**租户调度语义三档**（写入租户文档）：

- ✅ 原样工作：nodeSelector/nodeAffinity（按其节点标签）、topologySpread 按 zone（真容灾
  语义）、pod 亲和以 zone 为 topologyKey、PriorityClass 抢占、Pending 事件。
- ⚠️ 语义重解释：hostname 反亲和 = 分布到不同**逻辑节点**（配额分区/AZ），非不同物理机；
  同一逻辑节点内副本的物理分布由 Zun/Nova 决定——物理 HA 平台侧兜底（Zun 调度层对同
  owner capsule 软反亲和，fork 候选项 §10）；"节点资源压力" = 配额余量。
- ⛔ 禁止：spec.nodeName 直写；改受保护标签/污点。

---

### 4.4 同一命名空间多个虚拟节点（混合架构的必然结果）

在此之前每租户恰好一个节点，以下三处缺陷永不触发；一旦第二个节点出现（按架构、
按 AZ 都会）就会立刻咬人。三处都已修复并有单测：

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

### 7.6a ⚠️ OVN provider 的 VIP **没有 Neutron port**（2026-08-07 实测，影响两个功能）

三个 kubezun 建的 LB，`vip_port_id` 全部指向**不存在的 port**：按 id 查 `PortNotFound`，
按 IP 查 0 个 port（原始 API 复核，排除 CLI 干扰）。而 LB 本身 ACTIVE、VIP 流量实测可达。
原因是 amphora-less：地址活在 OVN northbound 规则里，不需要一张 port 承载。

**这推翻了两个功能共同的隐含假设**（"`lb.VipPortID` 能解析成一个真实 Neutron port"）：

| 功能 | 影响 |
|---|---|
| **FIP 绑定** | FIP 挂在 port 上 → 这条路在 OVN provider 下**不可用**。`type=LoadBalancer` + 要公网时需另寻机制（候选：给 VIP 所在子网做 SNAT/DNAT、或改用有 amphora 的 provider、或在 router 上做 FIP） |
| **DNS 命名** | 记录靠 port 的 `dns_name` 发布 → 同样不可用。**Neutron 自动集成这条路对 Service VIP 失效**，但**对 capsule port 仍然成立**（capsule 有真实 port） |

**已做的处理**：两处都从"静默不生效"改为**显式失败/告警**——FIP 直接报错，
DNS 记一条 warning 说明该 Service 只能靠地址访问。⚠️ **两条替代路径尚未确定**。

> 教训：FIP 与 DNS 都是先写代码后验证，两次都建立在同一个未验证的假设上。
> 涉及外部系统对象模型的功能，应先用一次真实调用确认对象存在。

### 7.6 租户 DNS：方案空间与当前阻塞（2026-08-07 勘察）

§7.5 定案后 DNS 从配套变成**必需**：ClusterIP Service 的可用地址只存在于
`knaas.io/address` 注解里，应用不读注解，它们用名字。

**一条结构性约束框定了方案空间**：VK 进程按 §7 不得接入租户网络，所以能应答 DNS 的
只有两类东西——**数据面（OVN 自己）**，或**租户网内的一个 capsule**。
（kubetron 的做法是渲染 zone 进 ConfigMap **由 kubelet 挂载**给 CoreDNS，我们没有 kubelet。）

| 方案 | 优势 | 代价 / 阻塞 |
|---|---|---|
| **A. OVN 内建 DNS**（Neutron dns-integration）⭐ | **零工作负载、零单点**：ovn-controller 在每台 hypervisor 的数据面直接应答；kubezun 只需给 Service 的 VIP port 和 capsule port 设 `dns_name` | ⚠️ **当前被阻塞**：实测 `extension_drivers = port_security,qos`，**DNS 扩展驱动未加载**，`dns_domain`/`dns_name` 静默不生效（API 广播了扩展但 ML2 不存字段）。需改 ML2 配置并重启 neutron-server——**部署级变更，需平台决策**。名字形态（`<svc>.<ns>.svc.cluster.local`）应可经 `dns-domain-ports` 扩展按 port 表达，**未验证** |
| B. CoreDNS 跑成 capsule | 不改 Neutron | **capsule 不可变**：zone 变更要重建 → 每次 Service 变动都有 DNS 中断窗口。预建 port 可保住 IP（§14.2），但保不住可用性 |
| C. 平台级 resolver（跨租户，按 view 隔离），kubezun 经 API 推记录 | 无每租户负载、无中断、不改 Neutron | 新增一个平台组件；租户隔离要靠 view/ACL 保证 |

**建议 A，C 作为 Neutron 变更不被接受时的退路。** A 结构上最正确（在数据面、零负载），
且它要的配置变更是绝大多数启用 DNS 的 OpenStack 部署的标准做法。

⚠️ 无论哪条，都还有一个未定项：**capsule 的 resolv.conf 来自子网的 `dns_nameservers`**
（Zun CRI 驱动读取并注入 sandbox，实测当前是 8.8.8.8）。租户内解析生效需要把它指向
对应方案的 resolver，并保留外部解析的转发路径。

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

## 9. DaemonSet

**价值定位**（写入产品文档）：DS 的机制价值（per-host agent）在逻辑节点上趋近于零；保留
的是 ① **chart 级生态兼容**——helm install 带 DS 的标准技术栈装得上、跑得绿（对比 EKS
Fargate 直接不支持的差异化）；② **多节点租户的分区扇出**——每 AZ/分区自动一份、随节点
增删跟随，Deployment 表达不了"跟随节点数"；③ **平台自用**——租户 DNS capsule、日志聚合
器打成 DS，生命周期自动跟随节点。

**机制**：
- ⚠️ **mutate 必须作用于 DaemonSet.spec.template，不是 Pod**：DS controller 按 template
  对全集群节点算 eligibility（1.17+ DS pod 走默认调度器，controller 只注 metadata.name
  nodeAffinity）。template 无 toleration → 根本不为虚拟节点建 pod；Pod 级注入会与 DS 注入
  的 nodeAffinity 矛盾，造成真实节点上永久 Pending。存量 DS 需 mutateExisting 或触发模板
  更新重算。
- 配套 Pod 级 validate 双保险 + 系统 DS 排除（§4 第 5 条）。
- B2' 放行 DS 同时堵上 kaaas §7.3 的洞（实测租户 DS 曾落到平台每个节点）：模板注入 pool
  selector 后 DS 只扇出到该租户节点；B1 租户维持 tenant-deny-daemonset.yaml。
- **红线三条**（租户文档显式划掉并给替代）：hostPath/hostNetwork/privileged 不可用；
  DS pod 观测不到同节点邻居 pod（无共享宿主机）；socket 共享不可能。替代 = sidecar /
  网络推送（§8.2）。DS pod 语义上 = "每逻辑节点恰好一份的普通 pod"。

---

## 10. Zun fork 工作清单

上游 2021 起纯维护、微版本停滞——rebase 成本低，fork 可行性已确认。
**维护边界**：docker driver、kuryr_network、Container API 划为不维护区；主干 =
capsule + CRI + zun-cni（正是以下各刀要动的地方）。

| 优先级 | 项 | 说明 |
|---|---|---|
| **门槛** | ExecSync + liveness 重启 | §6；stub 现成（api_pb2_grpc.py:100-103），在 zun-compute 落实执行 + restart 语义 |
| **门槛** | logs | CRI 驱动补 log_directory/log_path（cri/driver.py:89-94,181-192 现在不落日志文件）+ 新增 GET /capsules/{id}/logs（capsules.py:111-113 _custom_actions 为空）。capsule 容器 TYPE_CAPSULE_CONTAINER 无法借道 Container API（db/sqlalchemy/api.py:150-152） |
| P2 | Barbican secret ref 注入 | §8.1；与上面两刀同动 cri/driver.py sandbox 路径 |
| P3 | Manila/RWX（virtiofs） | §8.2 |
| ✅ 已做 | 架构上报 + capsule 架构约束 | §3.6；`driver.py` 上报 architecture/trait，capsule 模板加 `architecture` → `trait:COMPUTE_ARCH_*=required` |
| 候选 | 同 owner capsule 软反亲和 | §4 物理 HA 兜底 |
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
| P1 | 节点真实上报（配额镜像 capacity/conditions/InternalIP/标签污点全集） | 中 | §3 |
| P1 | 状态映射三处修复 | 小 | §5 |
| P2 | ConfigMap/Secret/emptyDir 合成（按对象 GET）+ SA token 策略 | 中 | §8 |
| P2 | logs/exec 通路（对接 fork 的 /capsules/{id}/logs；exec 返回 errdefs） | 中 | §10 |
| P2 | 孤儿 capsule 治理（不可伪造标记） | 小 | §5 |
| P3 | capsule 预热池（kubetron NetworkPortPool 水位模型作蓝本，"预绑 host_id"换"预建 capsule"；对标 kata 冷启动数十秒 → 秒级） | 大 | §7 |

平台侧配套（不在本仓库）：Tenant CRD 开通控制器扩展（节点 spec/VK Deployment/appcred/
Kyverno 实例/ResourceQuota）、Kyverno/VAP 策略集（§4）、tenant-deny-daemonset 分档、
kubetron DNS 分发通道改造、M8 编排层独立部署形态。

---

## 12. 路线图

| 阶段 | 内容 | 验收 |
|---|---|---|
| **0 PoC**（先于一切编码） | 手工验证既定路线：租户网建 capsule + Octavia OVN LB + 租户 DNS | capsule 内 curl Service VIP 通、DNS 解析到 VIP；顺带实测 nets 传递、preserve_on_delete、per-container restart 保 IP（§14 三项待定一并清掉） |
| **1 MVP 单租户** | P0 全部 + P1 转换/状态/节点上报 | 1.36 集群上 Deployment pod 经逻辑节点建 capsule，状态/删除全链路正确；带 limits 不 panic；不支持字段明确报错；EndpointSlice 出现 capsule OVN IP 且 kubetron LB 收编成功 |
| **2 多租户** | Tenant CRD/每租户 VK/appcred/Kyverno+VAP/placement/kubezoo 视图接入 | 渗透用例：两租户互相无法经 nodeName/binding/nodeSelector 到对方节点；A 的 capsule 在 A 的 project 走 A 的网络；kube-system 控制器 pod 实测过 Kyverno；租户 `get no` 只见自己节点 |
| **3 DS + 探针** | DS 模板 mutate + 系统 DS 排除 + fork ExecSync/HM readiness | 去特权 fluent-bit DS 在租户节点起一份 capsule；系统 DS 不落虚拟节点；真实节点无 Pending 残留；liveness 失败触发重启；HM 摘除未就绪 member |
| **4 生产化** | fork logs / Barbican KMS+ref / SA token / 预热池 / 心跳调优 / 计费 | kubectl logs 可用；50 租户规模 apiserver watch 数与 Zun QPS 达标；单租户 VK 崩溃不影响他租户（故障注入）；DS 扇出进计费 |

---

## 13. 已否决方案（不要重新提议）

| 方案 | 否决理由 |
|---|---|
| kubezoo 直接注册 Node（无 VK） | Node 是契约非记录；kubezoo 是无状态翻译器，履约即在网关内重写一个 VK+provider，且破坏其定位与每租户隔离原则（§1.1） |
| 剧场节点（视图层伪造 Node，pod 实跑 B1 池） | DS 立刻穿帮（DS controller 按上游真实 Node 扇出）；节点级调度语义全假；kubezoo 翻译面深度膨胀。仅可作过渡期产品单独评估，不通向 DS |
| B1.5 影子 pod 路线（VK 后端=K8s worker） | 双倍 etcd 对象 + 镜像状态机，买到的只是"B1 算力换节点皮"，算力经济学与 B1 相同；投入不如砸向有真差异的 Zun 线（2026-08-06 定案） |
| kubetron NetworkPortClaim 对接 capsule | 时序问题在 Zun 内联路径不存在；强套造 host_id 双主冲突（§7） |
| Barbican 作为 ConfigMap/Secret 主存储 | 双真相源；破坏 K8s 体验；重建引用/权限语义（§8.1） |
| provider 远程探测 OVN IP（探针方案 B） | 要求管理面进程挂进重叠 CIDR 租户网，违反 kubetron 双挂纪律（§6） |
| 单进程多租户 VK | 全集群 secret 缓存集中 + 凭据集中 + panic 全租户爆炸半径（§2）。凭据外置+informer 白名单+per-node 身份三件事完成后可作为成本优化重评 |
| Zun admin 凭据 | 现成跨租户读/删洞（§2） |
| VK 默认 virtual-kubelet.io/provider 污点 | 被通用 chart 全容忍规则误踩；用 knaas.io/tenant（§3.1） |

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
