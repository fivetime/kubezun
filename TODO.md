# kubezun TODO — 任务分解看板

本文件是 [`docs/DESIGN.md`](docs/DESIGN.md) 的任务分解，**开发进度唯一真相源**。
DESIGN 回答"为什么这样设计"，本文件回答"还剩什么没做"。

## 使用约定（与 kubetron/kaaas 同一套）

- **做完一项勾掉**，并在条目末尾补 commit 短 hash。
- **实现中发现设计写错了或最终采用别的方案：先改 `docs/DESIGN.md`，再回来改本条目。**
  两份文档不允许长期不一致。
- **会话压缩后先读本文件 + `docs/DESIGN.md` 恢复状态，勿凭记忆续做。**
- ⚠️ 标记的是**容易踩或容易忘**的点，不是可选项。
- 条目后的 `§N` 指 DESIGN 章节。

## 阶段总览

| 阶段 | 内容 | 门槛 |
|---|---|---|
| **0** | PoC：OpenStack 侧手工验证既定路线 | 不通则清掉待定项前不动代码 |
| **1** | MVP 单租户：新骨架重写 | provider 主链路成立 |
| **2** | 多租户：凭据/策略/视图接入 | 渗透用例过，才能对外 |
| **3** | DS + 探针（依赖 Zun fork 门槛项） | KNaaS 差异化体验成立 |
| **4** | 生产化 | 规模/计费/可运维 |
| **F** | Zun fork（独立工作流，与 1–4 并行推进） | 门槛两项卡阶段 3 |
| **P** | 平台侧配套（代码不在本仓库，进度在此跟踪） | — |

---

## 阶段 0：PoC（先于一切编码）（§12）

纯 OpenStack 侧操作，目标：验证既定技术路线 + 清掉三个实测型待定项（§14.1/2）。

- [ ] 环境就绪确认（**PoC 在开发环境 10.32.32.x 做**，勘察记录见 CLAUDE.md 环境注记）：
      devstack@incus-node-01(.130) 已有 Keystone/Neutron ML2-OVN/Octavia/Barbican/Manila，
      stable/2026.1；⚠️ Octavia provider 是自研 **incus** driver（OVN L4 + L7 workers），
      非社区 ovn-provider——HM 能力矩阵按实测为准，httpGet readiness 或可不降级（§6 修订点）
- [x] 计算节点运行时分家（2026-08-06）：133-135 kubelet 换 **CRI-O 1.36.3**（用户执行），
      containerd 整实例连默认 socket 让给 Zun（缓存已清空各释放 ~5G、服务已启用）——
      socket fork 与第二 containerd 均免除（DESIGN §7 定案更新）
- [x] kata 三 VMM 部署到 133-135 的 Zun containerd（2026-08-06）：镜像生产形态
      （/opt/kata 3.31.0 整包流式复制 + 三 toml + conf.d/kata.toml drop-in +
      kata-fc-thinpool loopback 池 + CNI 目录切 /etc/cni/zun-net.d）；
      冒烟 9/9：三节点 × qemu/clh/fc 均起真 microVM（guest 6.18.28）
- [x] **部署 Zun fork 完成**（2026-08-06）：node-01 zun-api（:9517，Keystone container
      服务 + 三 interface endpoint，zun 库已 upgrade，devstack@zun-api）；133-135
      zun-compute + zun-cni-daemon（/opt/stack/venv 独立 venv，container_driver=cri、
      container_runtime=kata-qemu，zun-cni 入 /opt/cni/bin，CNI conf 已换 10-zun-cni.conf，
      daemon 听 127.0.0.1:9036）；三台 service state=up、零错误零重启。
      期间产出 fork 分支 `feat/cri-only-compute`（4 commit，已 push）：capsule-only 主机
      支持（container_driver=cri 入口点 + 类型校验放宽 + 启动对账/周期状态同步分流）；
      另装 PyMySQL（Zun requirements 不含 DB 驱动）
- [x] **capsule 状态同步已补齐并实测（2026-08-07，fork commit `9bdc6fe0`）**：
      实现 `CriDriver.update_containers_states`——按 CRI 实况刷新每个容器，
      再汇总到 capsule（**全部容器 Running 才算 Running**）。
      故障注入实测：`crictl stop` 掉某 pod 的容器后——
      修复前 Zun 报 Running / K8s 报 1/1 Running（**假健康，自愈链完全断裂**）；
      修复后 Zun 报 Stopped / K8s 报 0/1 Completed / **ReplicaSet 自动重建副本
      1/1 Running**。这是整条自愈链路（容器死→Zun 察觉→provider 回写→K8s 重建）
      的首次贯通
- [ ] kubelet 侧配 system-reserved 预留 capsule 资源份额
- [x] Octavia provider 选型定案：**官方 ovn provider（amphora-less 纯 L4）承担 B2' 的
      Service 数据面**——capsule 只需 ClusterIP 的 L4 语义，不需要 L7；已与自研 incus
      provider 共存部署，能力矩阵与 HM 行为均实测（见阶段 0 条目）
- [ ] 手工开租户：Keystone project + application credential（unrestricted=false）+ 租户网络/子网
- [ ] 经 Zun API 在租户网建 capsule；验证 OVN LS 上出现 port、br-int 上
      `external-ids:iface-id` 正确（§7）
- [x] **nets 字段实测完成**（2026-08-06）：结论 = **必须 fork**，"对象模板绕过 schema"的
      设想被证伪（请求体 schema 直接引用 capsule_template，两种形态同一处校验）。
      补丁 `feat/capsule-nets-schema` 落地后**字符串模板也通过** → gophercloud 原生
      `capsules.CreateOpts` 可直接用，provider 无需自组 body。DESIGN §14.1 已回写
- [x] **capsule 端到端跑通**（2026-08-06，node-04）：kata-qemu microVM（guest 6.18.28，
      qemu+virtiofsd 进程实证）、Neutron port `vif_type=ovs / status=ACTIVE`、
      br-int 上 tap 口 `external_ids:iface-id`=port UUID、**capsule 内 IP/MAC == Neutron
      port（podIP==OVN IP 不变式数据面成立）**、OVN LSP up + 已绑 chassis、
      **东西向 capsule 互 ping 0% 丢包**
- [x] CRI ExecSync 底层可用性验证：crictl exec 进 capsule 正常执行命令
      → F 工作流"Zun 补 ExecSync"只差 Zun 侧接线，底层无障碍
- [x] **南北向打通**（2026-08-06，用户修复）：public 网络子网从 devstack 出厂
      172.24.4.0/24 换成 br-ex 实际桥接网段 10.128.32.128/27（gw .129，pool .136-.158）；
      验证 = 物理段 ping 路由器外部地址通且 ARP 解析到 Neutron MAC（网关 chassis
      incus-node-03 经 br-ex→ovn-ext 应答），OVN 侧默认路由/SNAT 重写正确。
      **capsule 实测出网成功**（10.128.32.129 与 8.8.8.8 均 0% 丢包）。
      注：capsule ping 自己的网关 192.168.100.1 不通属 OVN LRP 的 ICMP 行为，
      不影响 DNS/LB/服务访问，不追
- [x] **Octavia LB 实测全通（2026-08-06，官方 ovn provider）**：装
      ovn-octavia-provider stable/2026.1 与自研 incus provider **共存**
      （两者都列入 `enabled_provider_drivers`，`default_provider_driver` 仍 incus，
      建 LB 时显式 `--provider ovn`，不影响既有环境）。LB/listener/pool 全
      ACTIVE+ONLINE，member = capsule OVN IP（192.168.100.232/.117），
      OVN NB `vips` 映射正确，**capsule 内 curl VIP 返回 nginx 页（3/3）**。
      VIP 在独立 t1-vip-net（kubetron 拓扑），未踩同子网 dst-MAC 坑
      ⚠️ 自研 incus provider 那条线仍受阻（L7 worker 的 Nova 实例
      octavia-incus-<lb>-0/1 已 ACTIVE，但 provider 判
      "Worker unknown at 10.91.0.104 was not ready after 600s"）——与 Zun 无关，
      留给用户；其 L7 能力（HTTP HM）恢复后可作对比补测
- [x] **租户 DNS 打通（2026-08-06，PoC-4）**：原先 capsule 继承宿主 resolv.conf；
      fork 分支 `feat/capsule-dns` 收集 subnet `dns_nameservers` → 填
      `PodSandboxConfig.dns_config`（CRI v1 field 4）→ 实测 capsule 内
      `/etc/resolv.conf` = Neutron 下发的 nameserver，nslookup 成功。
      ⚠️ 同函数里 `log_directory` 是相邻 field 3 → F 的 logs 那刀同落点，可一并做
- [x] **重启保 IP 实测结案（2026-08-06，PoC-4）**：预建 port + `nets:[{"port"}]`
      （preserve_on_delete=True）→ 删 capsule 后 port 与 IP 保留 → 清 device_id →
      同 port 重建 → **IP 完全一致（192.168.100.166）**。
      ⚠️ Zun 自动 unbind 不可用（`_unbind_port` 先报 Forbidden
      `rule:update_port:device_id`，换 admin 凭据后 device_id 仍未清）
      → **定案：provider 用租户 appcred 自建/自清/自复用 port**（DESIGN §14.2 已回写）
- [x] **Octavia HM 摘除实测通过（2026-08-06）——探针方案 A 的 readiness 半边成立（§6）**：
      HM TCP 建成后 OVN NB 生成 `Load_Balancer_Health_Check`
      （failure_count=3/interval=5/success_count=2/timeout=3）+ `ip_port_mappings`
      指向两个 capsule；**故障注入：停一个后端容器，15s 内该 member→ERROR，
      健康 member 保持 ONLINE，从第三个 capsule 访问 VIP 5/5 成功**。
      能力矩阵（源码 `ovn_octavia_provider/common/constants.py:106-113` + 实测）：
      协议 TCP/UDP/SCTP、算法**仅** SOURCE_IP_PORT、**HM 仅 TCP 与 UDP-CONNECT
      （HTTP 型被 provider 明确拒绝）** → **httpGet readiness 必须降级为 tcp**。
      DESIGN §6 已回写为实测定案
- [x] PoC 结论汇总回写 DESIGN（2026-08-06：§14.1 nets、§14.2 保 IP、§6 探针
      readiness、§7 DNS 均已从"待定/假设"转为实测定案）
- [ ] （新增，F 候选）Zun `_unbind_port` 为何不清 device_id：先 Forbidden
      （策略 `update_port:device_id`/`device_owner`），换 admin 凭据后仍未生效。
      provider 自管 port 后此项不阻塞，但 fork 修好可减少 provider 侧清理逻辑

---

## 阶段 1：MVP 单租户（§11 P0/P1，§12）

### P0 骨架 ✅ 全完（2026-08-07，分支 `feat/rewrite-provider`，2 commit）

- [x] go.mod 重建：go 1.26 + k8s.io/* v0.36.3（对齐 kubezoo）+ VK v1.13.0 +
      gophercloud/v2 v2.13.0；删除 node-cli。踩坑：pbr 式旧代码需移出构建
      （`legacy/*.go.txt`）、client-go/apiserver 要与 api 同版本、genproto 老版本
      造成 ambiguous import 需升级
- [x] main.go 重写：nodeutil.NewNode + 每租户配置（namespaces/tenant/zone/
      network-id/capacity/internal-ip），节点对象在 `pkg/node/node.go`：
      well-known 三件套 + pool label + zone + role + instance-type +
      `knaas.io/tenant` 污点 + kubelet 端点 + semver 版本串
- [x] provider 骨架 `pkg/provider/provider.go`：实现 nodeutil.Provider 全部方法；
      未实现能力（logs/exec/attach/port-forward/stats/metrics）返回指明原因的错误
- [x] ⚠️ **namespace 白名单**：7 个入口全覆盖，返回 NotFound（与"存在但为空"
      不可区分，防租户探测）；**已有测试逐入口验证**
- [x] defer recover（CreatePod/DeletePod）+ 资源转换不再依赖未初始化 map
      （测试 `TestBuildTemplateSurvivesPodWithoutResources` 守卫）
- [x] 凭据层 `pkg/zun/client.go`：**只接受 application credential**（缺少
      OS_APPLICATION_CREDENTIAL_SECRET 直接拒绝启动），废弃密码/admin 路径

### P1 主链路

- [x] Pod→capsule 转换重写（`pkg/zun/template.go`）：capsule 名 = `kubezun-<podUID>`
      （测试守卫跨 namespace 不碰撞）
- [x] nets 显式传租户网络（`TemplateOptions.NetworkID`；`PortID` 优先，为保 IP 铺路）
- [x] 资源映射：K8s **limits** → Zun（测试 `...MapsLimitsNotRequests` 守卫）
- [x] ⚠️ 不可支持字段显式拒绝（hostNetwork/hostPID/hostIPC/hostPath/projected/
      privileged/**探针**），错误信息点名字段与原因；测试逐项覆盖
- [x] **podIP 回填 capsule OVN IP**（`sync.go`，同时填 PodIPs/HostIP）
- [x] **节点级健康：Ready 反映 Zun 可达性（2026-08-07，`pkg/provider/nodehealth.go`）**——
      消除节点级假健康。此前节点恒 Ready，Zun 挂掉时调度器仍把 pod 送来、
      永远卡 ContainerCreating。现实现 NodeProvider：连续 3 次探测失败才转
      NotReady（单次失败多为抖动，据此翻转会驱逐节点上全部 pod），
      Ready condition 直接携带失败原因。
      **实测（停 devstack@zun-api）**：两租户节点均转 NotReady 并显示
      "zun is unreachable: ...connection refused"；新 pod 停在 **Pending**
      （而非假装在创建）；**运行中的 pod 不受影响**；zun-api 恢复后
      两节点自动回到 Ready、Pending 的 pod 随即 1/1 Running
- [x] Ready 判定 = capsule Running ∧ 有地址（`zun.PodConditions(status, ready, t)`，
      未就绪时 Reason=NetworkNotReady）
- [x] NotifyPods 异步状态推送（`provider.NotifyPods` + 5s `syncLoop`）；
      **状态指纹去抖**——稳态零通知（测试守卫）
- [x] DeletePod 异步化（不等 capsule 消失，终态由状态轮询回报）
- [x] 状态映射三处修复 + 单元测试守卫（Stopped→Terminated / 失败 exit code≠0 /
      startTime 保留；另加 Ready 需数据面、PodPhase 全表、PodIP/PortIDs）
- [ ] 节点真实上报：capacity 镜像 ResourceQuota（⚠️ kaaas §2.3 教训：静态容量把失败位移到
      ContainerCreating；弃硬编码 zun.go:66-68,539-545）；conditions 真实化（弃静态恒 Ready
      + OutOfDisk，zun.go:255-299）；InternalIP=VK 实例 IP（现 nil，zun.go:303-305）（§3）
- [ ] ⚠️ 节点标签污点全集：well-known 三件套（kubernetes.io/os·arch·hostname——缺失则
      标准 chart 全部 Pending 且极难排查）+ kubezoo.io/pool + zone + node-role +
      knaas.io/tenant 污点（⚠️ 不用 virtual-kubelet.io/provider 默认污点）（§3.1）
- [ ] 状态映射三处修复：Stopped→Running 误映射（zun.go:456）、exit code 恒 0（zun.go:468）、
      startTime 被覆盖（zun.go:325）（§5）
- [ ] 孤儿 capsule 治理：不可伪造租户标记；deleteDanglingPods 只清本 VK 管理的 capsule（§5）
- [x] **MVP 验收 e2e 通过（2026-08-07，租户 111111 在 10.224.18.50 集群）**：
      虚拟节点 `111111-node-az1` 注册 Ready（角色 serverless、v1.36.3-knaas.1、
      运行时 zun://kata、容量 32C/64Gi/100pods、well-known 标签与 tenant 污点齐全）；
      Deployment(replicas=2) → **pod 1/1 Running**，各持 OVN IP（192.168.100.x）；
      **EndpointSlice 收录两个 capsule OVN IP 且 ready=true**（podIP==OVN IP 不变式
      端到端成立 → kubetron service reconciler 可零改动收编）；删除 Deployment →
      pod 全部清除。部署物：systemd `kubezun@111111`（appcred 走环境变量）+
      ClusterRole/SA kubeconfig
      期间修掉 8 个只有真实环境才暴露的问题（见 commit `aa2eb82`/`267c215`）：
      gophercloud 用 application-container 构造 microversion header（Zun 只认
      container，406）、gophercloud Capsule 类型与真实响应不符（restart_policy 对象、
      时间戳非 RFC3339）、microversion ≥1.32 字段改名 name/labels、
      capsule 模板 restartPolicy 用 K8s 拼写、K8s zone ≠ Zun AZ（AZ filter 滤掉全部
      主机）、Zun 忽略容器名需按序匹配、删除需 force=True（否则 409）、
      终态须先回写 K8s 才能删 pod 对象
- [x] **系统 DS 排除已落地并验证（2026-08-07，DESIGN §4.5）**：给 cilium /
      cilium-envoy / cilium-node-init 注入 `type NotIn (virtual-kubelet)`
      nodeAffinity（虚拟节点自带该标签）。⚠️ cilium-envoy 已有 nodeAffinity
      （排除 cilium.io/no-schedule），必须**追加到同一 matchExpressions**
      而非 merge 覆盖。结果：DS DESIRED 4→3、虚拟节点上零 pod、
      **租户 pod 只带自身 toleration 即可调度并 1/1 Running**。
      ⚠️ 残留的 `node.cilium.io/agent-not-ready` 污点需手工清一次
      （nodeAffinity 只挡新 pod，而能清除该污点的 agent 已不再被调度上去）。
      步骤与判据见 `deploy/exclude-virtual-nodes.md`；托管/operator 管理的集群
      需改从 operator 配置或用 mutating 策略下发，否则会被 reconcile 回滚
- [ ] 平台侧其余待办：
      ⚠️ 租户 pod 需 `automountServiceAccountToken: false`（否则被 SA token
      projected volume 拒绝，错误信息已点名该设置）——应由 Kyverno 默认注入
      ⚠️ 计算节点需装 `numactl`（缺失则 Zun 报 cpus=0，一切调度失败）
- [x] **孤儿 capsule 对账完成并实测（2026-08-07，`pkg/provider/orphans.go`）**：
      2 分钟周期扫描，实测 8 个 capsule → 精确剩 2 个（对应 2 个存活 pod），
      期间 pod 始终 1/1 Running 未受影响。
      清理两类：① provider 停机期间删除的 pod 留下的 capsule；
      ② ⚠️ **重复 capsule**——实测发现 **Zun 不强制 capsule 名唯一**，VK 重试
      CreatePod 时会重复创建（同名 capsule 出现 3 份，各自计费占资源）。
      已双向修复：CreatePod 先查已存在（幂等）+ 对账保留最新、删除其余。
      **权威模型**：K8s 决定"该不该存在"（声明式意图），Zun 决定"实际状态"
      （运行时事实）；判定不确定时一律不删（误删=毁掉运行中负载不可恢复，
      漏删=多占一个周期配额）——四重保护（无 pod cache 不跑 / lister 报错跳过 /
      capsule 年龄 <5min 不动 / UID 匹配即保留）各有测试守卫

---

## 阶段 2：多租户（§12）

- [x] **第二租户上线，双虚拟节点并存（2026-08-07）**：租户 222222 的 Keystone
      project/appcred、OVN 网络（192.168.110.0/24 + VIP 210.0/24 + router 上联）、
      K8s SA/ClusterRoleBinding、systemd `kubezun@222222`；
      `111111-node-az1` 与 `222222-node-az1` 同时 Ready
- [x] **跨租户渗透验收通过（2026-08-07）**：租户 A 尝试三条路径打到 B 的节点——
      ① `spec.nodeName` 直写 ② `pods/binding` ③ `nodeSelector`+B 的 toleration。
      结果：**①③ 在 K8s 层都成功绑到了 B 的节点**（调度层挡不住，符合预期），
      但 provider 白名单让其停在 ProviderFailed，**B 的 OpenStack project 零 capsule**；
      ② 被 apiserver 直接拒（Conflict）。执行面隔离成立
- [x] **Kyverno 1.18.2 部署 + 租户策略落地并实测（2026-08-07，
      `deploy/kyverno-tenant-policies.yaml`）**，DoS 向量已封堵：
      ① `spec.nodeName` 直写 → **被拒**（DoS 主路，因其绕过调度器直接绑定计入容量）；
      ② 指向他人 pool 的 nodeSelector/toleration → **被 mutate 改写成自己的**，
      pod 落到自己节点（⚠️ Kyverno mutate 先于 validate，所以 refuse-foreign-pool
      规则实际不触发——矫正比拒绝更友好，规则保留作 mutate 失效时的兜底）；
      ③ `automountServiceAccountToken=false` 自动注入 → **裸 Deployment
      （无 nodeSelector/toleration/automount）直接 1/1 Running**，租户不再需要
      写任何 KNaaS 专属样板；
      ④ **DaemonSet 模板注入成功：DESIRED=1、pod 1/1 Running 在租户虚拟节点上**
      （注入必须落在 spec.template——DS controller 在建 pod 前按模板算节点资格）
      策略按 namespace 上的 `knaas.io/tenant` 标签选取，租户 ns 需打此标签
- [x] **混合架构支持（2026-08-07，kubezun `69dd17f` + zun fork `f548d825`）**：
      `--arch` 一个值同时决定节点 `kubernetes.io/arch` 标签与 capsule 的
      `architecture` 字段，后者经 Zun 转成 `trait:COMPUTE_ARCH_*=required` 交 Placement，
      不匹配的宿主机在**调度阶段**被排除。Zun 侧 CRI 驱动补上 architecture/trait 上报
      （此前只有 docker 驱动有）。实测：amd64 capsule 落 node-04 Running；arm64 得到
      "There are not enough hosts available." 从未被放置；sparc 被 API schema 拒（400）。
      详见 DESIGN §3.6
- [x] **同租户多节点暴露的四个缺陷全部修复并有单测（2026-08-07，DESIGN §4.4）**——
      在此之前每租户恰好一个节点，四处永不触发：
      ① 孤儿清理删掉兄弟节点运行中的 capsule（VK informer 按 nodeName 过滤，
      看不见兄弟的 pod）→ capsule 打 `knaas.io/node-name`，无标签的旧 capsule 一律不动；
      ② **同名重建 pod 永远拿不到 capsule**（VK `createOrUpdatePod` 先问 `GetPod` 再比
      spec，我们保留的已删除记录被当活 pod 返回而 spec 完全相同 → 判"无事可做"）——
      StatefulSet 每次重启都复用 pod 名，这是常态；
      ③ 状态同步按 name 匹配，旧 capsule 的健康和 IP 会套到新 pod 上；
      ④ Placement 拒绝的 capsule 显示成 Creating（kubectl 优先显示容器 waiting reason）
      → 改报 `CapsuleUnschedulable` + Zun 原因
- [x] **kubelet API 上线：TLS + WebhookAuth（2026-08-07，`c661fbc`）**。此前端点
      **根本没起**（nodeutil 只在有 TLS config 时才起 HTTP server），logs/exec 无路由，
      唯一迹象是日志里一条 warning。
      ⚠️ **证书是每进程一张，不是每节点一张**：apiserver 按 `node.Status.Addresses` +
      `daemonEndpoints` 拨号（`kubelet_client.go:188-215`），**节点名不进 TLS，端口也不进**，
      租户所有节点同一进程同一 IP → 一张证书全覆盖。每节点独立的是**授权器**
      （`nodes/<nodeName>` 的 SAR），这才是每节点各自监听地址的理由。
      **有证书但无 `--client-ca-file` 直接拒绝启动**——delegating authenticator 无 CA 时
      根本不做 mTLS，端点后面每条路由都是读/进入租户容器。
      实测：匿名 curl 在握手阶段就被拒；`kubectl logs` 经 apiserver 打通到 provider，
      返回我们自己的 not-implemented（即 Zun fork 的 logs 端点，非本路径问题）。
      **坑**：SA 缺 `system:auth-delegator` 时每个请求返回 500 且报 subjectaccessreviews
      被拒，看起来像策略问题其实是缺绑定
- [x] **部署物 `deploy/tenant-vk.yaml` + `deploy/serving-cert.md`（2026-08-07）**：
      per-tenant SA、ClusterRole/Binding、auth-delegator 绑定；证书用集群自带的
      `kubernetes.io/kubelet-serving` 签发器（**控制面零配置**，apiserver 本来就信任），
      CSR subject 必须 `O=system:nodes,CN=system:node:<name>`（签发器硬性要求，
      但 apiserver 只校验 SAN 不看 CN，故写哪个节点名无所谓）
- [ ] 证书轮换：`kubelet-serving` 证书短期有效，当前无轮换机制 →
      过期即 logs/exec 中断（节点本身不受影响，证书只服务 kubelet API）
- [ ] 每租户 VK 部署形态从手写 systemd unit 收敛为 manifest/chart；
      appcred 明文进 unit 文件需换成 Secret 挂载
- [x] **同租户单进程多节点：共享 informer（2026-08-07，`a743545`，`pkg/vknode`）**。
      `nodeutil.NewNode` 每节点自建 informer factory 且无注入钩子，故降到 `node` 包
      自行组装控制器。pod informer 一份带全部节点的 pod，每个节点的 PodController 用
      **`PodEventFilterFunc`**（VK 为此场景预留）只认自己 nodeName 的 pod——单测摘掉
      过滤器即失败（每个节点会去接管所有 pod）。
      实测：租户 111111 两节点跑在一个进程里，apiserver 连接数与单节点进程持平；
      arch-x86→amd64 节点 Running、arch-arm→arm64 节点 CapsuleUnschedulable，互不串台。
      **顺带收窄了权限面**：informer 在只服务一个 namespace 时按 namespace 作用域
      （此前 pod watch 无论如何都是全集群）；也让孤儿清理**能看见兄弟节点的 pod**了
      （节点名标签守卫保留为主防线）。
      CLI：`--nodename` 等仍描述单节点（存量部署不变），额外节点用可重复的
      `--node name=<n>[,arch=][,zone=][,zun-az=][,listen=]`；同名或同端口直接拒绝
- [x] **ConfigMap/Secret 进 capsule（2026-08-07，kubezun `a1812c4` + zun fork `50aca0f2`）**：
      ① **env 本来就通**（VK 在 CreatePod 前调 `PopulateEnvironmentVariables` 解析
      `envFrom`/`valueFrom`）——实测 configMapRef 与 secretKeyRef 都正确落到 capsule；
      ② **文件挂载此前是静默丢弃**：`Validate` 只拦 hostPath/projected，configMap/secret
      卷直接穿过去，capsule `mounts: None`，而 pod 显示 **1/1 Running**——租户以为
      `/etc/appcfg` 存在。已实测复现并修复。
      实现：Zun 的 `Local` 卷驱动本来就能写 base64 内容并 bind-mount（Container API 一直在用），
      只有 capsule 路径拒收——fork 侧给 capsule 卷加 `file: {contents}` 源
      （schema + `capsules.py` + ⚠️ `common/utils.py:429` 还有第二处硬编码 cinder-only 检查）。
      provider 侧按 key 拆成一卷一文件（Zun Local 一卷只写一个文件），
      subPath 挂到 mountPath 本身，两容器共享的卷只声明一次。
      **实测**：容器内 `/etc/appcfg/{GREETING,app.conf}` 存在且内容正确。
      ⚠️ **是快照不是投影**：ConfigMap 事后修改不会到达运行中的 capsule
      （真 kubelet 会刷新）。其余卷类型现在**按名字明确拒绝**，不再穿透
      ⚠️ 原"按对象 GET 避免 403"的顾虑已由共享 informer 的 namespace 作用域解决（见上）
- [ ] SA token：Kyverno 默认强制 automountServiceAccountToken=false；显式开启者创建时
      注入一次性 TokenRequest token + TTL 文档（§8.1）
- [ ] Kyverno validate 策略集：禁 spec.nodeName、禁指向外租户节点的 nodeSelector/toleration、
      RBAC 收回 pods/binding create（§4.2）
- [ ] ⚠️ 核查 kube-system 控制器 SA 创建的 pod 实测经过 Kyverno
      （resourceFilters/excludeGroups）→ 回写 DESIGN §14.3
- [ ] VAP：nodes/status 仅 VK 凭据可写；受保护标签/污点前缀仅平台可写（§4.3）
- [ ] placement 注入对接：(kubezoo) convert/placement.go 机制扩展至租户 pod 注入
      pool selector + tenant toleration（§4.4）
- [ ] kubezoo 视图验证：`<tid>-` 前缀节点在租户 `kubectl get no` 正确显示/翻译
- [ ] **渗透验收**：两租户互相无法经 nodeName/binding/nodeSelector 到对方节点；A 的
      capsule 在 A 的 project、走 A 的网络；租户 `get no` 只见自己节点（§12）

---

## 阶段 3：DaemonSet + 探针（§6/§9，依赖 F 门槛两项）

- [ ] ⚠️ DS 策略改造：**mutate 作用于 DaemonSet.spec.template，不是 Pod**（Pod 级注入与
      DS controller 的 nodeAffinity 矛盾 → 真实节点永久 Pending）；存量 DS 需
      mutateExisting 或触发模板更新（§9）
- [ ] 配套 Pod 级 validate 双保险（落虚拟节点的 pod 带正确 toleration/selector）（§9）
- [ ] 系统 DS 排除：kube-proxy/CNI 等注入 `nodeAffinity: type NotIn (virtual-kubelet)`（§4.5）
- [ ] B1/B2' 分档：tenant-deny-daemonset.yaml 对 B2' 租户放行（堵 kaaas §7.3 洞的同时放行）
- [ ] readiness 链路：kubetron LB reconciler 的 HM 配置对 capsule member 生效
      （httpGet 降级 tcp 的策略与文档）（§6）
- [ ] liveness 链路：对接 fork ExecSync 探针 + 重启；探针结果回流 capsule status →
      provider 映射 pod Ready（§6）
- [ ] 红线文档交付租户：hostPath/hostNetwork/privileged 不可用、DS 观测不到邻居 pod、
      socket 共享不可能 + 替代路径（sidecar/网络推送/headless 发现）（§8.2/§9）
- [ ] **验收**：去特权 fluent-bit DS 在租户节点起一份 capsule；系统 DS 不落虚拟节点；
      真实节点无 Pending 残留；liveness 失败触发重启；HM 摘除未就绪 member（§12）

---

## 阶段 4：生产化（§12）

- [ ] Zun capsule 容器名 minLength=2，K8s 允许单字符容器名 → 租户写 `name: c` 得到一个
      看不懂的 400。fork 侧放宽 schema，或 provider 侧改写并在状态里映射回原名
- [ ] kubectl logs 通路：对接 fork 的 GET /capsules/{id}/logs；exec 返回 errdefs 明确错误（§10）
- [ ] Barbican KMS：barbican-kms-plugin（CPO 现成）做 etcd 加密后端（§8.1）
- [ ] Barbican secret ref 注入（对接 fork，替代 Secret 明文过 Zun DB）（§8.1）
- [ ] capsule 预热池：kubetron NetworkPortPool 水位模型作蓝本，"预绑 host_id"换"预建
      capsule"，对标 kata 冷启动数十秒 → 秒级（§11 P3）
- [ ] 心跳调优：lease 间隔放宽 30–60s（逻辑节点独有红利，写 QPS 降 3–6 倍）（§3.5）
- [ ] DS 扇出进计费模型；节点数按套餐限额（§3.5）
- [ ] **验收**：50 租户规模 apiserver watch 数与 Zun QPS 达标；单租户 VK 崩溃不影响他租户
      （故障注入）；kubectl logs 可用（§12）

---

## F：Zun fork 工作流（独立推进，§10）

fork 仓库已就位：`/root/k8s-zun-provider/openstack/zun` = `github.com/fivetime/openstack-zun`
（master 基点 e79265e8，与 origin 同步）。**每项功能开 feature 分支，勿直接踩 master。**
维护边界先立：docker driver / kuryr_network / Container API 划为不维护区；
主干 = capsule + CRI + zun-cni。

- [ ] **（门槛，卡阶段 1）CriDriver.update_containers_states**：capsule 状态同步（现无实现
      → 状态不刷新，kubezun GetPodStatus 会读到陈旧值）；积木已有
      （cri/driver.py `_show_container` / `_populate_container_state`），
      钩子已在 `feat/cri-only-compute` 的 sync_container_state 留好
- [x] **探针 step1：传递链打通（2026-08-07）**——capsule 模板 container schema 加
      `livenessProbe/readinessProbe/startupProbe`（core/v1.Probe 形状，fork
      `feat/capsule-probes` commit `e40cf2bc`）+ 创建路径存进 `healthcheck.k8s_probes`
      （复用现有 JSON 列，免 DB migration）；provider 不再拒绝探针而是原样传递
      （kubezun commit `5cd5e1f`）。**实测**：带 exec + httpGet 探针的 Deployment
      1/1 Running，探针定义（命令/路径/端口/全部阈值）已落 Zun 数据库
      ⚠️ 仍拒绝两类无法忠实执行的形式：无 handler 的探针、指向容器自身以外
      host 的 httpGet/tcpSocket（探针在容器内执行，否则会静默探错对象）
- [x] **探针 step2 完成并实测（2026-08-07）**：
      **step2a 改写**（kubezun `4574b43`，移植 kubetron `pkg/webhook/probes.go` 经验）：
      httpGet/tcpSocket/grpc → `sh -c` exec against 127.0.0.1；命名端口按容器声明解析
      （未声明则拒绝，否则会探到空）；curl→wget / nc→curl-telnet /
      grpc_health_probe→nc 三级 fallback；镜像无工具时**显式失败而非静默健康**；
      时间字段（period/threshold/timeout）原样保留；exec 探针不改。
      ⚠️ 改写只作用于发给 Zun 的模板，**K8s 里的 pod spec 保持用户原样**
      **step2b 执行**（fork `4a9d9fd1`）：CRI `ExecSync` + 周期执行 + 阈值计数 +
      liveness 连续失败达 failureThreshold 后重启容器（**不动 sandbox，保住 capsule IP**）+
      startup 探针门控前两者 + readiness 结果落 `healthcheck.k8s_probe_state`。
      探针跑不起来一律判失败（否则会掩盖它本要发现的故障）
      **实测**：readiness 指向 404 路径的 pod → DB 里存的是改写后的 curl 命令 →
      prober 按周期执行 → curl 退出码 22 → `{"ready": false, "readiness_failures": 1}`
      ⚠️ 部署踩坑：在 133 上装 grpcio-tools 升级了 protobuf/grpcio，生成的 pb2 与
      134/135 的 runtime 不兼容（gencode 7.35.1 vs runtime 6.33.5）→ 三台需版本对齐
- [x] **探针 step3 代码完成（2026-08-07，kubezun `fcb45ef`）**：provider 读
      `healthcheck.k8s_probe_state.ready`，Ready 现在要求三件事同时成立——
      capsule Running ∧ 有地址 ∧ **容器自称在服务**；
      语义（各有测试守卫）：无 readiness 探针 → 一 Running 即 Ready（同 K8s）；
      **已声明但尚未应答 → 不 Ready**（不能把流量送给从未检查过的容器）；
      探针失败 → 不 Ready（这正是脑裂场景）；容器非 Running → 不 Ready
      （陈旧探针结果不得比容器活得久）；capsule 需全部容器 Ready
      **端到端验证通过（2026-08-07，重启三节点后）**：探针成功的 pod
      **1/1 Running 且 EndpointSlice ready=true**，完整链路闭合：
      应用探针 → Zun prober 执行 → 容器 readiness → pod Ready condition →
      EndpointSlice → （kubetron/Octavia member）
      期间修掉三个只有真实环境才暴露的 bug（fork commit
      `fd1f5fd9`/`5e8b2099`/`e4692f50`）：
      ① **单次运行时查询失败即判容器 Deleted** → capsule 转 Stopped → pod 被判终止 →
      控制器重建 → 下轮又改回 Running，**pod 无限 churn**。修：单次查不到不作判据
      （容器创建中与运行时恢复中长得一样），真正消失由 capsule 删除与孤儿对账兜底
      ② **readiness 仅在变化时写入 + 首次比较默认 True** → 首次即通过的探针什么都不写，
      读侧把"缺值"当作"从未应答"而永久不 Ready。修：每次检查都记录
      ③ **state 是 healthcheck 内部 dict 的引用** → 保存时拿自己和自己比较，
      永远"无变化"，从不落库。修：先拷贝再修改
      ⚠️ 环境教训：多轮 `force delete` + `crictl rmp -f` + `pkill -9 qemu` 在 kata 上
      累积僵死 sandbox（node-04 到 63 个），最终 containerd 卡死无法恢复
      （延长 systemd 超时、重建 devmapper 池、禁用 devmapper 均无效），**只有重启解决**。
      重启后 containerd 恢复正常，陈旧 capsule 由孤儿对账自动清理（69 → 3）
- [x] **对照验证：readiness 正确区分健康与不健康（2026-08-07）**——同一 Service 下
      `192.168.100.82 ready=true`（探针探 `/` 成功）与 `192.168.100.52 ready=false`
      （探针探 `/gone` 得 404），两者同时在 EndpointSlice 中且判定相反
- [x] **capsule 卡 Creating 的超时处理（kubezun `f0c6c9b` + `c217dc2`）**：实测发现一个
      pod 卡 Creating **97 分钟**——capsule 记录建了但**没有任何 compute 节点见过它**
      （创建 RPC 在 zun-compute 重启窗口丢失，Zun 侧无重试机制；同配置的新 pod
      2 分钟即 Running，证明与探针配置无关）。provider 现按 **capsule 自身
      `created_at`** 判定超过 10 分钟仍 Creating 即标记 pod 失败、交控制器重建。
      ⚠️ 时间基准是关键：最初用 `pod.Status.StartTime` 无效——它由 provider 设置，
      **进程重启即重置**，卡了 90 分钟的 pod 会被当成刚创建。
      **实测：卡 97 分钟的 pod 被判失败并成功重建**
- [ ] （原设计）Zun 侧 prober 执行——ExecSync + 周期执行 +
      阈值计数 + liveness 失败重启 + readiness 结果回流。
      ⚠️ **实测定案（DESIGN §6.0）：所有探针类型都必须在容器内执行**——
      宿主机网络与 kata sandbox netns **均不可达 capsule IP**（实测 ping/curl 全失败；
      kata 的 netns 里只有 tap 设备，网络栈在 VM guest 内），
      而容器内 `wget 127.0.0.1` / `nc -z 127.0.0.1` 实测可行。
      故 httpGet/tcpSocket/grpc 需注入**静态** helper（distroless 无 curl/nc）——
      **kubetron `cmd/probe/main.go` 现成可复用**（get/tcp/install 自安装）
- [ ] **distroless 镜像的探针 helper 注入（未做，2026-08-07 复核）**。现状不是假健康：
      无 curl/wget 的镜像探针会明确报 "image has no curl or wget"，非 distroless 镜像
      的探针链路已完整可用并实测通过，故非紧急项。
      ⚠️ **更正此前判断：不需要 Zun 支持 emptyDir**——Zun 现有 `local` volume driver
      即可把文件送进 capsule。真正未决的是传输方式，两条路各有代价：
      ① **宿主机预置**（每台计算节点铺一份，随 zun-cni 或独立 DaemonSet）：最省事，
      但把"探针能否运行"绑死在宿主机镜像版本上，与租户自服务模型冲突；
      ② **建 capsule 时由 provider 推下去**：干净，但每 pod 多一次数据面往返，且
      **helper 是以租户身份注入进租户容器的可执行文件**——这条安全边界要先想清楚。
      先定②的安全模型再动手；kubetron 已解同一问题，实现可直接借鉴
- [x] **exec 端点上线（2026-08-07，zun fork `181ae7ce` + kubezun `7b59f42`）**：
      ExecSync 辅助函数探针早已在用，但**外部没有入口**——capsule 只能通过自己的探针
      被观察。新增 `POST /capsules/{id}/execute` + `capsule:execute` policy，
      provider 接上 `RunInContainer`。
      与 logs 同样按 `capsule.host` 定向（容器不记录 host）。
      ⚠️ **踩到的坑**：ExecSync 超时原用 `CONF.default_timeout`（10 分钟），
      远大于 RPC 回复超时（60s）→ 任何长命令（`sh` 无 stdin 即可）都在 60 秒后
      变成 messaging 层的 500，且不说明命令还在跑。新增 conf `cri_exec_timeout`
      （默认 30s，**必须低于 rpc_response_timeout**），现在 30 秒明确报
      "Command did not finish within 30 seconds"。
      实测：`kubectl exec -- echo`、带空格的 `sh -c`、非零退出码回传均正确；
      `-t`（要终端）明确拒绝——Zun 一次性返回，没有流可附着
- [x] **liveness 失败重启真正生效（2026-08-07，zun fork `3f00613f` + kubezun `e7a0b6b`）**。
      ⚠️ **此前是最坏的一类假健康**：探针正确判失败并调用了重启，但 CRI 容器**只能启动
      一次**——`StopContainer` + `StartContainer` 返回 `container is in CONTAINER_EXITED
      state`，而该错误被降级成 warning，于是 pod 永远 1/1 Running 而应用已死。
      修法：重启改为**替换容器**（Remove 旧的 → 在同一 sandbox 内 Create+Start 新的）。
      ⚠️ **Remove 必须先于 Create**：运行时会为容器名保留记录，先建会撞
      `failed to reserve container name ... is reserved for <old>`（实测踩过）。
      **sandbox 保留 → capsule 地址不变**（实测两次重启后 podIP 仍是 192.168.100.243，
      守住 podIP==OVN IP 不变式；否则每次重启全部客户端都要重新发现服务）。
      每次以递增的 attempt 号创建（crictl 里 ATTEMPT=2），重启失败改报 error 而非 warning。
      provider 侧回传 RestartCount（此前恒 0），并**加入状态指纹**——否则两次轮询之间被
      替换的容器回来仍是 Running+ready，指纹相同，重启永远不会写进 pod。
      实测 `kubectl get pod` RESTARTS 0→1→2
- [x] **探针按声明的周期执行（2026-08-07，zun fork `055e3afd`）**：此前探针搭在
      容器状态同步上，**所有探针都按同步间隔（默认 60s）跑**，`periodSeconds` 完全被
      忽略，而且从外部无从察觉自己写的数字没被兑现。
      改为独立周期任务 + 每个探针记录下次到期时间；新增 conf `probe_check_interval`
      （默认 2s，**是 periodSeconds 的分辨率而非周期本身**，声明小于它的按它执行）。
      **`initialDelaySeconds` 一并实现**（此前也被忽略）——从容器 started_at 起算，
      否则慢启动应用一启动就被探，靠 failureThreshold 兜底才不至于立刻进重启循环。
      单个容器探针异常不再中断本轮其余容器的探针。
      实测：`periodSeconds:5` + `failureThreshold:2` 的 liveness 现在**约 10 秒**内
      重启容器（此前 60 秒以上）；前 40 秒不健康 + `initialDelaySeconds:60` 的容器不被杀
- [x] **⚠️ 周期任务必须按 host 过滤（2026-08-07，zun fork `f387eb47`）**——探针提频后
      暴露：`check_probes` 和 `sync_container_state` 都用 `objects.Capsule.list(ctx)`
      列**全集群** capsule，而**每台计算节点都跑这两个任务**。不持有该 capsule 的节点
      去 exec 一个本地运行时不认识的 container_id → 必失败 → 谁最后写谁说了算。
      现象：健康容器的 ready 按任务频率来回抖动（实测 3 节点下约 2/3 概率误报失败）。
      状态同步也一样——**节点在反复"修正"自己看不见的容器的状态**，
      这正是当初"单次查询无响应不得判定容器消失"那条补丁在遮掩的根因。
      两处改为 `list_by_host`（Container 路径一直是这么做的）。
      实测：r-good 连续 19 次 ready 稳定，此前交替
- [x] 探针 exec 截止时间 = `timeoutSeconds` + `probe_exec_overhead`（默认 2s，新配置项）：
      租户的超时约束的是**他们的命令**，外层 exec 截止若等于内层超时则两者赛跑，
      shell 启动没有余量。实测 kata sandbox 内一次 exec 固有开销仅约 80–105ms，
      故本项不是抖动的成因（成因是上一条），但语义上必须分开
- [x] **logs 全链路打通（2026-08-07，zun fork `4432216e` + kubezun `b3a0210`）**：
      sandbox 加 `log_directory`、container 加 `log_path`（此前运行时**直接丢弃**输出，
      既无流可 attach 也无文件），新增 `GET /capsules/{id}/logs` 按 CRI 日志格式解析
      （P/F 标记合并被运行时截断的长行），新增 `capsule:logs` policy。
      新增 conf `cri_log_root`（默认 /var/log/zun/capsules，⚠️ 无自动清理）。
      ⚠️ **踩到的关键坑**：`container.host` 为 None（只有 capsule 记录 host），
      RPC 走共享 topic → **任意计算节点应答**，而只有真正跑 capsule 那台有文件 →
      端点按节点数比例随机返回空。修为按 `capsule.host` 定向。
      实测：修前 3 次里约 1 次有内容，修后 20/20 全对。
      ⚠️ **第二个坑**：Zun 丢弃模板给的容器名、自己生成
      （`capsule-<uuid>-phi-12`），按名字查必 404 → provider 改为**按位置**解析成
      container uuid（与状态映射同一不变式）。
      实测 `kubectl logs` / `--tail` / `--timestamps` 全部正确；
      `-f` 明确拒绝（Zun 一次性返回全量，轮询会在每个边界重复行）
- [ ] Barbican secret ref：sandbox 创建时服务端拉取挂 tmpfs，DB 只存引用（§8.1）
- [ ] （P3）Manila/RWX：virtiofs 透传（参考 CPO pkg/csi/manila）（§8.2）
- [ ] （候选）同 owner capsule 软反亲和——逻辑节点内物理 HA 兜底（§4）
- [ ] （候选，已降级）CRI socket 可配置：cri/driver.py:44-45 硬编码改 conf 选项——
      2026-08-06 运行时分家（kubelet→CRI-O）后默认 socket 即 Zun 专属，仅当某环境
      kubelet 必须保留 containerd 时才需要（DESIGN §7）
- [ ] （候选）nets/固定 IP 按 PoC 结论补齐
- [ ] （顺手）linux_net.py ovs-vsctl → ovsdb（上游自己的 TODO，linux_net.py:48）

## P：平台侧配套（代码不在本仓库）

- [ ] Tenant CRD 开通控制器扩展：节点 spec / VK Deployment / appcred / Kyverno 实例 /
      ResourceQuota（落点大概率 kubezoo-controller）（§11）
- [ ] kubetron 租户 DNS 分发通道改造：DNS 跑租户网内 capsule、控制器直推 zone
      （无 kubelet 挂 ConfigMap）（§7）
- [ ] kubetron M8 顺带：Service/DNS 编排半边可独立部署（kubezun-only 形态只拉编排层）（§7）
- [ ] kubezoo 层：InternalIP 展示值是否改写（§14.5）

## 待定项镜像（详见 DESIGN §14，清掉一项这里勾一项）

- [ ] §14.1 nets 可传递性（阶段 0 实测）
- [ ] §14.2 liveness 重启保 IP（阶段 0 实测）
- [ ] §14.3 kube-system pod 过 Kyverno（阶段 2 核查）
- [ ] §14.4 租户业务标签写自己节点（MVP 禁，有需求再议）
- [ ] §14.5 InternalIP 展示值改写
- [ ] §14.6 SA token 长期轮换通道（等 F/ExecSync 落地）
- [ ] §14.7 单进程多节点 informer 共享形态（阶段 2 定）
- [ ] §14.8 PVC 供给流程（cinder-csi provision-only vs provider 直管；阶段 2 有状态负载需求时定）
