# kubezun 工作区

本目录是「kubezun」项目的工作区。**kubezun = KAaaS 平台的 B2' KNaaS 算力线**：
virtual-kubelet provider，把租户的逻辑虚拟节点落到 OpenStack Zun capsule（Kata 隔离，
租户零 worker 节点），与 B1（kubezoo+kubetron，pod 跑平台 kata 池）体验档位共存。

**⭐ 真相源（改代码前先读；会话压缩后先读这两份恢复状态，勿凭记忆续做）**：
- [`docs/DESIGN.md`](docs/DESIGN.md) —— 14 章设计定案。**§13 已否决方案清单：不要重新提议**。
- [`TODO.md`](TODO.md) —— 任务分解看板，**开发进度唯一真相源**，做一项勾一项补 commit hash。
- 两份不一致时：先改 DESIGN，再改 TODO。姊妹文档：
  `/root/kubezoo-gateway/docs/kaaas-platform-architecture-cn.md`（平台全貌）、
  `/root/kubetron/DESIGN-refactor.md`（OVN 数据面）。

## 核心定调速记（详见 DESIGN，此处仅防失忆）

- **Node 是契约非记录**：心跳/执行/回写/kubelet API 四义务 → VK 库履约，Zun 是执行后端。
- **租户可见文本只有一条禁令：不得点名后端**（`capsule`/`zun`）。其余照实说——
  `Restarting`/`Paused`/`Created` 本来就是容器世界的通用状态名（Docker 就这套），
  租户看得懂。⚠️ 2026-08-08 在这上面返工三次，根子是把规则写成了**封闭白名单**
  （"只能用 kubelet 的词表"），于是不断拿准确性换合规：`Paused` 被映射成
  `ContainerStatusUnknown`（明明是已知状态）、`Restarting` 被并进
  `ContainerCreating`（丢掉了"重启"与"首次创建"的区分）。**规则错的时候测试通过是坏消息**
  ——那个白名单测试每次都是绿的。写约束时先问它禁的是什么，而不是它许可什么。
- **推理不能代替核实**：同一天里，"节点在抖"（实为 RBAC 缺失）、"端口属于 Octavia 项目"
  （实为租户自己的）、"K8s 推断不出架构"（实为我们谎报了没硬件的节点）、
  "Zun 区分 Error/Dead"（CRI 路径根本不产生 Dead）四条都是先下结论、后被证伪。
  ⚠️ 代价不只是返工：其中两条差点作为"要求对方配合"发给 kubezoo 负责人。
  **能自己查的别问别人，能自己修的别写进协作请求。**
- **podIP == capsule OVN IP 不变式**：守住它是必要条件但**不充分**——kubetron 的
  Service reconciler 还要 member 的 subnet ID，而它只从 NetworkPortClaim 取
  （`members.go:100-147`，capsule 无 claim 必报错）。缺口只有这一个字段，见 DESIGN §14.6。
- **provider namespace 白名单是唯一不可绕过的授权边界**（PodController 只按 spec.nodeName
  过滤，创建者可写死）；Kyverno/VAP 是第二层。
- **每租户独立 VK 进程 + application credential；严禁 Zun admin 凭据**（admin context 跨租户）。
- 探针：readiness → Octavia HM；liveness → Zun fork ExecSync + 重启。VK 库不执行探针。
- 网络：**Zun 原生 port**（内联建绑，无时序问题），不接 kubetron NetworkPortClaim。
- DS：**mutate 作用于 DaemonSet.spec.template 而非 Pod**；系统 DS 用
  `nodeAffinity: type NotIn (virtual-kubelet)` 排除。
- 节点 capacity = ResourceQuota 镜像（静态容量把失败位移到 ContainerCreating）；
  well-known 标签三件套（os/arch/hostname）缺失 = 标准 chart 全 Pending。

## 参考项目（全部本地克隆，只读；行为/API 不确定时直接查源码，不要猜；已有实现直接 import/照抄，别重复造轮子）

| 路径 | 内容 | 关键查证点 |
|---|---|---|
| `/root/gophercloud` | **gophercloud v2 官方仓库**（module `github.com/gophercloud/gophercloud/v2`，go 1.25；kubetron 同款 v2，全调用传 `context.Context`） | **现成 Zun capsule 包 `openstack/container/v1/capsules/`**（Create/Get/List/Delete + `microversions.go` + `template.go`）——直接 import，别手写 REST。⚠️ 两个坑：① `CreateOpts.ToCapsuleCreateMap` 恒把模板转**字符串**发送（`requests.go`），服务端字符串路径必走 schema 校验 → 顶层 `nets` 被拦（DESIGN §14.1，定案=fork 补 schema；过渡=`ServiceClient.Post` 自组 body 发对象模板）；② `Template.Parse` 接受 JSON/YAML（`template.go`）。其他直取包：`openstack/loadbalancer/v2/`（Octavia LB/listener/pool/member/monitor）、`openstack/networking/v2/`（ports/subnets/networks/SG）、`openstack/identity/v3/applicationcredentials/`（appcred 开通）、`openstack/keymanager/`（Barbican，阶段 4）、`openstack/config/`（clouds.yaml 加载） |
| `/root/k8s-zun-provider/virtual-kubelet` | VK 上游库（唯一成熟的 kubelet 契约库，作为库引入无锁定） | `node/nodeutil/provider.go:21-43,59`（Provider 接口 + NewProviderFunc——重写的接口面）；`node/nodeutil/controller.go`（NewNode:289 无单例限制、296-302 默认标签 type=virtual-kubelet、⚠️329-346 默认**全集群** informer，namespace 级 Role 会 403 → ConfigMap/Secret 按对象 GET）；`node/nodeutil/client.go:53-58`（PodController 按 spec.nodeName 过滤——白名单边界的依据）；`node/podcontroller.go:79-90`（PodNotifier/NotifyPods）、`:635-700`（deleteDanglingPods 会删非己管 capsule，需归属标记）；`node/sync.go:99-120`（无 NotifyPods 时回退全量轮询）；`node/api/`（kubelet API server + `auth.go:167-181` WebhookAuth 授权属性 nodes/<nodeName>）。⚠️ 全库零处执行探针 |
| `/root/k8s-zun-provider/openstack/zun` | **Zun fork 本体（可直接改并 push）**：remote = `github.com/fivetime/openstack-zun`，**单分支 master**，上游基点 e79265e8，我们的改动 29 个提交在其上。上游 2021 起纯维护，rebase 便宜。⚠️ **直接在 master 上做，不要再开 feature 分支**（2026-08-08 定：六条分支合回时其中两条已被主线重做取代，只换来冲突；这是一条线性工作且只整体部署）。⚠️ **实验环境全部 `/opt/stack/zun` 都是手改的工作树**（HEAD=上游基点 + 未提交改动），所以"在跑什么"只能算哈希、不能看 git；2026-08-08 核过一次：计算节点 04/05/06 与 master 逐字节相同，node-01（控制面，`container_driver=docker`）的 `cri/driver.py` 是旧的但**在那台上是死代码**——查证运行时行为要上计算节点，别查控制面；维护边界：capsule+CRI+zun-cni 主干，docker driver/kuryr_network/Container API 不维护区 | `zun/container/cri/driver.py`（:74-83 runtime 选择、:89-94 sandbox 无 DNS/log 配置、:96-120 **port 内联 create+bind**、:163-178 privileged 恒 False + memory=cgroup 硬限、:397-405 execute_* NotImplementedError——fork ExecSync 落点）；`zun/api/controllers/v1/capsules.py`（:50-59 字符串才校验、:111-113 `_custom_actions` 空=无 logs/exec 端点、:221-223 顶层 nets 消费、:418-430 cinder-only 卷）；`zun/api/controllers/v1/schemas/parameter_types.py:523-549`（capsule_spec additionalProperties:**True** / template 顶层 **False**；availabilityZone 在白名单——AZ 落位免 fork）；`zun/conf/container_driver.py:32`（capsule_driver 默认 cri）；`zun/network/linux_net.py:36-43`（br-int `external-ids:iface-id`——与 ovs-cni 同契约，OVN 视角 capsule port ≡ kubetron pod port）；`zun/volume/driver.py:193-205`（Cinder multiattach，块级）；`zun/cni/`（zun-cni-daemon，CRI 路径不需要 kuryr-libnetwork）；⚠️ `zun/api/utils.py:70-71` + `zun/db/sqlalchemy/api.py:111-118,215-228`（admin context 跨租户——严禁 admin 凭据的依据）；`api_pb2_grpc.py:100-103`（ExecSync stub） |
| `/root/k8s-zun-provider/openstack/ovn-octavia-provider` | 官方 OVN provider（**stable/2026.1，与开发环境同版本；已装进 node-01 devstack venv 并与自研 incus provider 共存**，`default_provider_driver` 仍是 incus，建 LB 时显式 `--provider ovn`） | **B2' 的 Service 数据面选型**：amphora-less，LB 直接翻成 OVN NB `Load_Balancer`（ovn-controller 分布式 DNAT），纯 L4 正合 capsule 的 ClusterIP 语义（不需要 L7）。能力边界查证：`ovn_octavia_provider/driver.py`（`UnsupportedOptionError`：HM 类型、`lb_algorithm` 仅 SOURCE_IP_PORT、session persistence 仅 SOURCE_IP、无 L7/TLS）；`common/constants.py`（`SUPPORTED_HEALTH_MONITOR_TYPES` 白名单——**DESIGN §6 readiness 是否需降级 tcp 的直接依据**）；`helper.py`（NB 翻译、`ip_port_mappings`、`ls_lb_add`/`lr_lb_add` 的租户隔离） |
| `/root/k8s-zun-provider/openstack/placement` | OpenStack Placement 本体 | **Zun 的真实资源把关处**（不是 FilterScheduler）：`zun/scheduler/client/query.py:61` 查 `get_allocation_candidates`，`filter_scheduler.py:101` 做 `claim_resources`。⚠️ Zun 默认启用的 filter 只有 AvailabilityZone/Compute/Runtime 三个，**CPUFilter/RamFilter 均未启用**（`zun/conf/scheduler.py:65-69`），所以 `compute_node.cpus=0` 不影响调度——VCPU 库存由 `update_provider_tree` 经 `psutil.cpu_count()` 独立上报（driver.py:226 起） |
| `/root/k8s-zun-provider/containerd` | containerd 源码 | CRI 行为权威：`pkg/cri/`（CRI 插件，**namespace 硬编码 k8s.io**，是 kubelet 与 Zun 不能共用一个 containerd 的根因）、`snapshots/devmapper/`（thin-pool 恢复——僵死 snapshot 拖垮启动的那次排障落点）、`core/runtime/v2/`（shim 管理） |
| `/root/k8s-zun-provider/cri-o` | CRI-O 源码 | 计算节点 kubelet 侧的运行时（与 Zun 的 containerd 分家，DESIGN §7）。查 CRI 语义差异、CNI 处理、sandbox 生命周期时对照用 |
| `/root/k8s-zun-provider/openstack/neutron` | Neutron 本体（含 ML2/OVN mech driver） | **端口绑定/OVN 行为权威**，PoC 排障主力：`plugins/ml2/drivers/ovn/mech_driver/mech_driver.py:1155-1200`（bind_port 拒绝路径——**:1195 "Refusing to bind port to dead agent" 就是僵尸 chassis 记录导致 capsule 绑定失败的出处**）；`plugins/ml2/drivers/ovn/agent/neutron_agent.py:96-102,187-230`（agent alive 判定 = `nb_global.nb_cfg - chassis.nb_cfg <= 1`，排查 dead agent 先看这里）；`plugins/ml2/drivers/ovn/mech_driver/ovsdb/`（NB/SB 同步、Port_Binding）；南北向/bridge-mappings 与 external LSP 逻辑亦在此树 |
| `/root/k8s-zun-provider/openstack/neutron-lib` | Neutron API 权威定义 + api-ref | **不要猜字段/端点**：`neutron_lib/api/definitions/`（portbindings、portbindings_extended 双绑、port_security、securitygroup、provider_net）；`api-ref/source/v2/`（ports/networks/subnets 请求响应契约）。kubetron 亦以此为准 |
| `/root/k8s-zun-provider/openstack/zun-ui` | Zun 的 Horizon 面板(官方 zun-ui) | **判断"改 Zun 会不会打断 Horizon"的唯一依据**。⚠️ 它**只用 Container API,且终端只有 attach**:`zun_ui/api/client.py:259`(`containers.attach(id)` → `GET /v1/containers/{id}/attach`,返回 websocket URL)、`static/dashboard/container/containers/details/console.controller.js:42`(判 `container.interactive`,否则显示"interactive mode needs to be enabled when this container was created")、`static/cloud-shell/cloud-shell.controller.js:57`(建容器写死 `interactive: true`)。**capsule 在 UI 里没有任何终端入口**。⇒ 我们的计算节点 `container_driver = cri` 而 `CriDriver` 只继承 `BaseDriver+CapsuleDriver`(**不是 `ContainerDriver`**),`_get_driver` 直接拒:"This host serves capsules only"——**zun-ui 的容器终端在 capsule-only 节点上本就不可用**,与我们改 exec 无关。要让它可用 = 让 CriDriver 实现整个 ContainerDriver(create/start/stop/reboot/pause/commit/get_archive…),即"不维护区"。替代方案:capsule + `exec -it`(2026-08-11 已实测可用,终端体验等价) |
| `/root/k8s-zun-provider/openstack/zun-tempest-plugin` | Zun 官方 API 契约测试 | **fork 回归验证工具**（我们改了 capsule schema / driver 加载 / CRI 版本，需证明 API 契约未破）：`zun_tempest_plugin/tests/tempest/api/test_capsules.py`（capsule 生命周期契约——最贴近 kubezun 用法）、`test_services.py`、`test_containers.py`（Container API，capsule-only 主机上预期不适用）。阶段 1 前跑一轮 capsule 用例做基线 |
| `/root/k8s-zun-provider/openstack/os-brick` | Cinder 卷连接库（Zun volume driver 依赖） | 卷挂载行为查证（DESIGN §8.2 / §14.8 PVC 供给）：连接器实现（iSCSI/RBD/NVMe）、multiattach 语义、`initiator/connector.py`。⚠️ Zun 挂卷失败时先看它而非 Cinder API |
| `/root/k8s-zun-provider/openstack/manila` | Manila 本体（共享文件系统） | Zun fork 的 **RWX/P3 项**规格来源（DESIGN §8.2：capsule 跨 pod 共享文件需 Manila+virtiofs，Zun 现仅 Cinder，capsules.py:418-430 明示）；开发环境 devstack 已启 Manila（LVM/NFS 后端），可直接实验 |
| `/root/k8s-zun-provider/openstack/os-vif` | os-vif 官方源码（Zun CNI 的 VIF 插拔实现来源） | **VIF 插拔行为权威**：`vif_plug_ovs/ovs.py:81-90`（`[os_vif_ovs] ovsdb_connection` / `ovsdb_interface`——**容器化 OVS 环境必须设 `unix:/run/openvswitch/db.sock`，默认 tcp:127.0.0.1:6640 连不上**，PoC 已踩）；`vif_plug_ovs/ovsdb/ovsdb_lib.py`（native/vsctl 两种后端）；`vif_plug_ovs/linux_net.py`（veth/tap 创建、`external_ids:iface-id` 写入——与 kubetron 的 ovs-cni 同契约）。Zun 侧对接见 `zun/network/os_vif_util.py`（vif_type→translator 映射，`binding_failed` 时报 "No 'zun.vif_translators' driver found"） |
| `/root/kubetron` | K8s↔Neutron 薄编排层（B1 数据面；**B2' 复用其编排半边**） | **直接抄/复用**：`pkg/neutron/provider.go`（`NewClientFromAppCred`，gophercloud v2 + 租户 appcred——凭据层照抄）；`pkg/service/`（Service→Octavia OVN LB reconciler，**EndpointSlice 驱动** `members.go:25,107`；⚠️ **非零改动**——`memberEndpoint`
（`members.go:100-147`）强制 claim 注解 + 从 NetworkPortClaim 取 member subnet，
capsule 无 claim 必报错，缺口仅此一字段，DESIGN §14.6）；`pkg/controller/dns_controller.go`（租户 DNS zone 渲染，分发通道要改）；`tests/osapi.py`（setup-tenant/cleanup-tenant/appcred-for-project 现成 op——**阶段 0 PoC 直接用**）。**不复用**：claim/pool/binding 控制器、webhook cni-args 注入、探针改写（Zun 路径结构性不需要，DESIGN §7/§13）。拓扑经验：VIP 独立子网 + tenant router 前置（DESIGN-refactor §5.3 dst-MAC 坑） |
| `/root/kubezoo-gateway` `/root/kubezoo-controller` `/root/kubezoo-contract` | 租户视图三件套（ByteDance KubeZoo fork，已移植 k8s 1.36.3） | `kubezoo-gateway/pkg/convert/placement.go:118-155`（PlacementTransformer：剥 nodeName、注 pool selector+toleration——DS 需扩展到 spec.template）；`kubezoo-contract/pkg/common/clusterscope.go`（租户集群域资源白名单；:167-178 nodes/proxy 撤走的原因）；`kubezoo-contract/config/policy/tenant-deny-daemonset.yaml`（B1 维持、B2' 放行改造对象）；`kubezoo-gateway/docs/kaaas-platform-architecture-cn.md`（§2 B2 否决/§2.3 静态容量教训/§2.4 翻案条件/§7 隔离实测/§8 策略分工）。Tenant CRD 开通控制器扩展落点大概率 kubezoo-controller |
| `/root/k8s-zun-provider/kata-containers` | Kata Containers 官方源码（main 分支；**查证行为时先 `git checkout 3.31.0`**——生产/开发全部节点部署的是 3.31.0 kata-static 包） | 行为查证权威，不要猜：`src/runtime/`（Go shim v2 本体——runtime_handler/ConfigPath 解析、annotations；⚠️ 生产 container1/2 的 shim 打过补丁，有 `.stock.bak` 备份，行为异常时先 diff）；`src/runtime/virtcontainers/`（qemu/clh/fc 三个 hypervisor driver——**Cinder 块热插、virtiofs（Manila P3 前提）的能力边界在这查**）；`src/agent/`（guest 内 agent——CRI ExecSync 进 VM 后的执行路径，fork ExecSync 项的下半场）；`tools/packaging/`（kata-static 打包/kata-deploy——生产 /opt/kata 包的来源）；`src/runtime-rs`+`src/dragonball`（rust 运行时，未用，备查） |
| `/root/cloud-provider-openstack` | K8s 官方 OpenStack cloud provider（**go 1.26 + gophercloud v2.12.0，与 kubezun 目标栈同基线，可 drop-in**；Apache 2.0） | **阶段 4 直用**：`pkg/kms/barbican/` + `cmd/barbican-kms-plugin/`（etcd KMS 加密后端，DESIGN §8.1 现成实现）。**查证/参考**：`pkg/client/`（AuthOpts→ServiceClient 构造范式）；`pkg/csi/cinder/`（PVC→Cinder 供给流程参考，DESIGN §14.8；⚠️ 其 `cloud.conf` 静态多 cloud 凭据模型需换 per-tenant appcred——kubetron 已记录同一问题）；`pkg/csi/manila/`（RWX share 挂载编排——Zun fork Manila/virtiofs 项的规格来源，§8.2）；`pkg/openstack/loadbalancer.go`（Octavia batch member reconcile `buildBatchUpdateMemberOpts`——kubetron service reconciler 的上游范式，一般不直接用）；`cmd/k8s-keystone-auth/`（Keystone↔K8s authn/authz webhook——若未来租户身份统一到 Keystone 的候选，现阶段 kubezoo 走证书/SA token，仅备查）；`cmd/octavia-ingress-controller/`（L7 ingress 参考——⚠️ OVN provider 无 L7，需 amphora，阶段 4 后再议） |
| `/root/kubernetes` | kubernetes/kubernetes 源码树 | K8s 机制权威：DS controller eligibility 计算、默认调度器、EndpointSlice、TokenRequest、VAP/MAP、well-known 标签常量（`staging/src/k8s.io/api/core/v1/well_known_labels.go`） |
| `/root/kyverno` | Kyverno 源码 | 阶段 2/3 策略写法查证：mutate existing、DaemonSet template 注入、resourceFilters/excludeGroups（kube-system pod 是否过策略，DESIGN §14.3） |

## 周边参考（/root 下其余本地克隆；价值分层，按需查阅）

**与 kubezun 直接相关：**

| 路径 | 内容 | 用途 |
|---|---|---|
| `/root/kwok` + `/root/kwok-scale-lab` | KWOK（无 kubelet 假节点）+ 用户自建规模实验室（loadgen/topology.yaml） | **§3.5 节点规模预算实测的现成工具**：模拟千级 Node 验证 lease QPS/watch 扇出/调度器曲线，阶段 4 心跳调优与"多少逻辑节点碰墙"的答案在这做 |
| `/root/capsule` | clastix Capsule 全家桶（capsule + **capsule-proxy** + headlamp-plugin） | Tenant CRD/配额模型的同类设计参考；⚠️ capsule-proxy 是"租户 Node 可见性过滤"的**另一种实现**（代理层按标签过滤 list），与 kubezoo 前缀方案对照查证用，勿混搭 |
| `/root/gatekeeper` | OPA Gatekeeper | 策略引擎对照件：Kyverno 写不动的场景（CEL/Rego 表达力对比）备查；平台已定 Kyverno+VAP 主路（kaaas §8） |

**平台规模 track（阶段 4 / kubezoo M8 配套，用户自有实验线）：**

| 路径 | 内容 | 用途 |
|---|---|---|
| `/root/kubebrain` + `/root/kubebrain-client` + `/root/tikv` + `/root/pd` + `/root/kine` | ByteDance KubeBrain（apiserver 存储换 TiKV/badger）+ kine（etcd shim）；本机已有 build/soak 日志（kb-*.log、poc50/ 多 apiserver 实验） | 上游集群 etcd 天花板的出路——逻辑节点/租户数逼近单集群极限时的存储层扩展路线 |
| `/root/kubesluice` | KubeSluice：语义感知的 kube-apiserver 前置 LB | 多 apiserver/M8 分片形态的流量治理配套 |

**架构对照（DESIGN §1.1/§13 已论证过的替代路线，勿重提为主路）：**

- `/root/kamaji`、`/root/vcluster`：per-tenant 独立控制面路线（集群即服务）——与共享控制面
  + kubezoo 网关是不同产品形态，仅当网关路线碰死墙时重评。
- `/root/cilium`：集群主 CNI（B1/kubetron 域的隔离结论来源）；B2' capsule 完全绕开 CNI，
  与 kubezun 无直接交集。
- `/root/katalyst-api`、`/root/katalyst-core`：ByteDance QoS/混部——B1 kata 池的资源治理
  候选，与 B2' 无关。
- `/root/etcd`、`/root/external-snapshotter`、`/root/ingress-nginx`：通用组件源码，
  按需查证（snapshotter 对应 PVC 快照，§14.8 之后的事）。

## 环境注记

- OpenStack 控制面：**待用户提供**（标准 `OS_*` 环境变量，参考 kubetron
  `tests/osapi.py` 的认证模式；admin 仅用于开通，capsule 操作一律租户 appcred）。
- 本机 k3s：127.0.0.1:6443 存活，1.36 族（`/root/k3s.yaml`），可作阶段 1 MVP 开发集群；
  ⚠️ 当前 kubeconfig 证书不匹配（x509 unknown authority），用前先修
  （取 `/var/lib/rancher/k3s/server/cred` 侧新 kubeconfig 或重签）。
- **开发环境（PoC/阶段 1-3 主战场，10.32.32.x，2026-08-06 勘察）**：
  - incus-node-01（.130，64C/125G）= devstack 控制面（stable/2026.1）：Keystone/Neutron
    ML2-OVN geneve/Nova-incus/Cinder/Glance/Barbican/Manila(LVM-NFS)/Octavia/Gnocchi；
    openrc：`source /opt/stack/devstack/openrc admin admin`（ADMIN_PASSWORD=password）；
    **无 Zun——由我们部署 fork 至此**（无 kubelet，默认 containerd socket 无冲突）。
  - ⚠️ Octavia provider = 自研 **incus** driver（"OVN L4 frontend with ALL_ACTIVE
    containerized L7 workers"，默认 provider）——非社区 ovn-provider，HM 能力按实测，
    DESIGN §6"仅 TCP/UDP-CONNECT"的限制可能不适用。
  - incus-node-02/03（.131/.132）= nova-incus 计算（lxd hypervisor）。
  - incus-node-04/05/06（.133-.135，16C/31G）= k8s worker + **kubezun 计算节点**，
    运行时已分家（2026-08-06）：kubelet→**CRI-O 1.36.3**，**containerd 整实例连默认
    socket 专属 Zun**（缓存已清空、已启用；CNI conf 目录已指 `/etc/cni/zun-net.d`，
    内含临时 bridge 配置，zun-cni 部署时替换）；**kata 3.31.0 三 VMM 已部署并冒烟通过**
    （2026-08-06，镜像生产形态：/opt/kata 整包 + /etc/kata-containers 三 toml +
    /etc/containerd/conf.d/kata.toml drop-in + kata-fc-thinpool.service loopback 池，
    9/9 冒烟 guest=6.18.28）；/dev/kvm 可用；
    **容器化 OVS/OVN chassis**（openstack ns 的 DaemonSet pod，宿主机无 ovs-vsctl）
    接入 devstack OVN；CNI=Cilium。
  - k8s 控制面 = 10.224.18.50:6443（托管形态）；**KubeZoo 已在跑**（111111/222222/333333
    租户命名空间）。kubectl 在 node-01 上直连可用。全部 6 台均为 OVN chassis。
  - SSH root（凭据同 container1/2，用户会话提供，勿写入仓库）。
- **生产环境计算节点（阶段 4 目标形态）**：container1（10.32.16.34）/ container2（10.32.16.35），16C/30G，
  Ubuntu 26.04，containerd 2.3.1，kata 3.31.0（handler 实名 kata-qemu/kata-clh/kata-fc，
  fc 档 devmapper），/dev/kvm 可用，SSH root（凭据用户会话提供，勿写入仓库）。
  ⚠️ 两台是 k8s-hosted OpenStack（apiserver 10.224.17.1:6443）的在役计算节点：
  nova-compute-incus、ovn-controller、Multus/ovs-cni、rook-ceph CSI 均以 pod 形态在跑
  ——OVN chassis 已就位（容器化），但 **kubelet 共存 → Zun 必须走专用第二 containerd
  实例 + fork socket 配置项**（DESIGN §7 共节点约束，TODO F 门槛项）。
- 阶段 0 PoC 不依赖 Kata：网络/LB/DNS/restart 保 IP 验证走 runc 同样成立，Kata 只影响
  隔离性，PoC 通过后再补。
- 那 745 行旧代码（zun.go/types.go/config.go，基于已归档 node-cli + go1.12）已按 DESIGN §11
  重写完毕，只有状态映射迁移保留了下来；master 上就是重写后的代码，旧骨架不复存在。
- **两个仓库都只有一条 master 分支，直接在 master 上开发**（2026-08-08 定，kubezun 与
  Zun fork 同时收拢）。之前 kubezun 的工作挂在 `feat/rewrite-provider` 上挂了 72 个提交、
  Zun fork 散成六条，结果是 master 这个词在两个仓库里都不再指代任何有意义的东西，
  "哪份是权威"每次都要重新查。开分支前先想清楚它要跟谁并行——没有并行就不要开。
