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
| **3** | ~~DS +~~ 探针（依赖 Zun fork 门槛项） | ⚠️ **租户 DS 已废弃**（DESIGN §9）——本阶段只剩探针 |
| **3.5** | **共享节点形态**（2026-08-13 新增，DESIGN §1.2 定案的落地面） | 一个进程服务多租户且各落各 project |
| **4** | 生产化 | 规模/计费/可运维 |
| **F** | Zun fork（独立工作流，与 1–4 并行推进） | 门槛两项卡阶段 3 |
| **P** | 平台侧配套（代码不在本仓库，进度在此跟踪） | — |

> ⚠️ **2026-08-13 形态变更**：放弃每租户虚拟节点与 DaemonSet，节点变成平台对象
> （`regions × K × AZs × archs`，与租户数无关）。**DESIGN §1.2 是定案，§4.6 是落地面。**
> 本文件里凡涉及"每租户一个节点/一个 VK 进程/租户看得见节点/租户 DS"的条目，
> 已就地标注或改写；**已完成的历史条目保留原文并加注**，不要按它们的字面意思继续做。

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
- [x] **证书热加载（2026-08-07，实测）**：此前启动时读一次，**轮换后不重载** →
      证书过期而替换件就躺在同一个文件里，表现是 logs/exec 中断而节点仍 Ready，
      静默且难查。改为每次握手从内存取、每分钟重读文件；读失败保留上一张可用证书
      （轮换写文件不是原子的，为一瞬间的半截文件拒绝所有连接更糟）。
      实测：不重启进程换证书，序列号 `925AAE…` → `19608C…`，`kubectl exec` 仍正常。
      ⚠️ 仍需外部机制**签发**新证书（本项只解决"签发了但不生效"）
- [x] **心跳可调（2026-08-07）**：新增 `--lease-duration`（默认 40s 即真 kubelet 的值）。
      DESIGN §3.5 论证可放宽到 30–60s——**虚拟节点的健康就是进程在不在**，
      不像物理机会在两次心跳之间悄悄失联。代价是进程死后调度器还会往上放多久 pod
- [x] **appcred 移出 unit 文件（2026-08-07）**：unit 文件是 **world-readable**（实测
      644，`nobody` 可读到 appcred）。改为 `EnvironmentFile=/etc/kubezun/<T>/openrc`，
      0600 文件 + 0700 目录，与 tls.key / client-ca / kubeconfig 同一目录。
      两个租户已迁移，节点保持 Ready。模板见 `deploy/kubezun@.service`，
      流程见 `deploy/README.md`
- [ ] **部署形态：systemd vs 集群内 Deployment —— 需先决策再实现**（DESIGN §14.3）。
      未决点是**地址**：apiserver 按节点上报地址拨 kubelet API，pod 地址每次重调度就变
      → 要么每次启动重签证书（真 kubelet 的 bootstrap 路线），要么上报稳定的
      `InternalDNS` 名字（取决于控制面能否解析集群 DNS）。
      ⚠️ hostNetwork 规避不通：计算节点 kubelet 已占 10250
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
- [ ] Kyverno validate 策略集：禁 spec.nodeName、RBAC 收回 pods/binding create（§4.2）。
      ⚠️ **"禁指向外租户节点的 nodeSelector/toleration"随节点去租户化失效**——没有"外租户
      节点"这回事了。但**容量面的攻击没有消失、受害面还扩大了**：任一租户可以用大 limits
      的垃圾 pod 耗尽整个分片共享节点的可调度容量（旧实测：一个 limits=4CPU/8Gi 的 pod
      占掉 12%）。⇒ **这一层要改成按 namespace 的 ResourceQuota 拦截**（DESIGN §4 第 2 条）
- [ ] ⚠️ 核查 kube-system 控制器 SA 创建的 pod 实测经过 Kyverno
      （resourceFilters/excludeGroups）→ 回写 DESIGN §14.3
- [ ] VAP：nodes/status 仅 VK 凭据可写；受保护标签/污点前缀仅平台可写（§4.3）。
      ⚠️ 理由变了：租户已看不到节点，防的是平台内部误写与其他控制器
- [ ] placement 注入对接：(kubezoo) convert/placement.go 机制扩展至租户 pod 注入
      nodeSelector + toleration（§4.4）。⚠️ **`NodePoolFor(tenantID) = tenantID` 必须改**
      ——池不再等于租户；该函数注释写明**三处必须一致**（Kyverno 策略 / kubezoo 注入 /
      手工打标），改它要三处同动
- [~] ~~kubezoo 视图验证：`<tid>-` 前缀节点在租户 `kubectl get no` 正确显示/翻译~~ ——
      **反过来了**：节点是平台对象、不带 `<tid>-` 前缀，验收判据改为**租户 `get no` 为空**
      （与 B1 一致）
- [ ] **渗透验收**：两租户互相无法经 nodeName/binding/nodeSelector 触及对方**资源**；A 的
      capsule 在 A 的 project、走 A 的网络；租户 `get no` **为空**（§12）。
      ⚠️ 判据要能分辨"隔离住了"和"两边都没建成"——旧版靠"B 的 project 里零 capsule"，
      这个判据在"什么都没发生"时同样成立，需要配一个**正向对照**（A 自己的 capsule 建成了）

---

## 阶段 3：探针（§6，依赖 F 门槛两项）

> ⚠️ **2026-08-13：本阶段的 DaemonSet 部分整体废弃**（DESIGN §9）。下面三条已完成的 DS
> 条目**保留为历史记录**，不要按字面继续做；未完成的两条已划掉。
> ⚠️ **但"系统 DS 排除"不在废弃之列**——租户 DS 废弃 ≠ 平台自己的 DS 不会来。

- [~] ~~DS 策略改造：mutate 作用于 DaemonSet.spec.template~~ —— **废弃**（§9）：不再为租户
      DS 注入任何东西
- [~] ~~配套 Pod 级 validate 双保险~~ —— **废弃**（§9）
- [~] ~~B1/B2' 分档：tenant-deny-daemonset.yaml 对 B2' 租户放行~~ —— **废弃**：
      **两档统一 deny**，B2' 不再放行租户 DS
- [x] **系统 DS 排除已验证生效（2026-08-07）**：cilium/cilium-envoy/cilium-node-init/
      konnectivity-agent/kube-multus-ds/ovs-cni/ovn-chassis **全部 3/3 只落三台真实
      worker**，虚拟节点上零系统 pod。
      ⚠️ **这条继续有效且必须保留**——它防的是平台 DS，与租户 DS 废弃无关（DESIGN §9）
- [x] ~~**租户 DS 实测（2026-08-07）**~~（历史记录）：`DESIRED=2`（每虚拟节点一份），
      amd64 节点上 1/1 Running 拿到 OVN IP，arm64 节点上 `CapsuleUnschedulable`——
      扇出与失败均正确。⚠️ **当时由此得出的供给规则"不要为没有对应宿主机的架构创建虚拟
      节点"仍然成立**，但理由变了：现在没有租户 DS 会去反复重建那个必然失败的 pod，
      剩下的是"注册一个没有任何宿主机能满足的架构标签 → 落到它上面的 pod 永远
      `CapsuleUnschedulable`"（DESIGN §3.6 启动期校验 `--arch` 那条）
- [x] **readiness → EndpointSlice 实测打通（2026-08-07）**——**这才是流量摘除的真实通道**
      （kubetron 的 LB reconciler 是 EndpointSlice 驱动的，`pkg/service/members.go:25,107`）：
      同一个 Service 选中 r-good/r-bad 两个 pod，EndpointSlice 里
      `r-good ready=True serving=True`、`r-bad ready=False serving=False`，
      **两条 address 都是 OVN IP**（192.168.100.82 / .144）——正是 kubetron 需要的输入，
      podIP==OVN IP 不变式在此兑现。
      **动态转换实测**：用 `kubectl exec` 在容器内补上探针要的路径 → **5 秒内**
      `ready` 翻成 True 进入 EndpointSlice（探针 periodSeconds=5，
      得益于本轮的探针周期修复；此前光探针一项就要 60s+）
- [ ] **kubezun 自建 Service→Octavia reconciler**（定案 2026-08-07，DESIGN §14.4：
      与 kubetron 共存各管各的，不复用其 Service 半边）。
      **共存安全性已查证**：kubetron 孤儿 GC 按 `device_owner` tag 过滤
      （`kubetron`/`kubetron:<clusterid>`），Zun port 是 `compute:zun`（consts.py:80），
      不会误清；无 kubetron 注解的 pod 不进其处理路径。
      ⚠️ 边界：`memberEndpoint` 对无注解 pod 是 **error 非 skip**（`members.go:54-57`），
      **同一 Service 混选两种 pod** 会让 kubetron 对该 Service 整体 reconcile 失败
      （pool 冻结不清空）——迁移场景会撞上，记录备查
      实现要点（照抄 kubetron 同名实现，subnet 改取 capsule 地址的 `subnet_id`，
      `pkg/zun/capsule.go:59`）：LB/listener/pool 生命周期、**幂等全量 PUT**
      （`BatchUpdatePoolMembers` 全集合语义，少放一个就清空 pool）、LB GC、
      双栈（一 pool 一族）、Ingress
- [x] **租户 DNS 方案验证通过（2026-08-07，实测于 node-01，`deploy/designate.md`）**：
      Designate 已部署（api/central/worker/producer/mdns + BIND9 后端），ML2 DNS 驱动已打开。
      **实测链路**：带 `dns_name`+`dns_domain` 的 port → Designate 自动生成
      `rsvc.111111-default.svc.cluster.local. A 192.168.100.47`；**删 port → 记录自动清理**。
      后者是选这条路的核心理由——本轮 FIP/LB/capsule 的泄漏全是生命周期 bug，
      记录的生死跟着 port 走就不会漏。
      ⚠️ **四个静默失败的坑**（详见 `deploy/designate.md`）：
      ① `[DEFAULT] dns_domain` 保持默认 `openstacklocal` → 扩展直接 return，
      port 的 DNS 字段接受但不存储、无任何报错；
      ② `subnet_dns_publish_fixed_ip` **继承自** `dns_domain_ports`，两个都列 → 逻辑跑两遍，
      建 port 报 `NeutronDbObjectDuplicateEntry ... PortDNS`。只列前者即可；
      ③ `neutron.conf` 里**本来就有一个注释掉的 `[designate]` 段**，追加在文件末尾的配置
      被 oslo.config 忽略 → 报 `EndpointNotFound`，看起来像 catalog 问题其实是配置没生效；
      ④ 子网需 `--dns-publish-fixed-ip`（capsule 用的是 fixed IP，默认只发布 FIP）。
      另：Designate 的 `[keystone_authtoken]` 需 `interface = public`（本环境只发布 public endpoint）
- [x] **Service DNS 端到端可达（2026-08-07 实测）**：capsule 内三种写法全通——
      `http://rsvc/`（短名）、`http://rsvc.111111-default/`、
      `http://rsvc.111111-default.svc.cluster.local/`；**公网域名同时正常**
      （BIND 递归有效，解决了"指权威 DNS 会断公网"的顾虑）。
      ⚠️ **中途发现 Zun 只设 `dns_config.servers` 不设 `searches`** → 短名 NXDOMAIN，
      表现像 Service 不存在。已补：kubezun 按 kubelet 规则组出三条 search 域，
      经 capsule **annotations** 传给 Zun（避免加 DB 字段）。顺序有单测保护——
      自己 namespace 必须第一，否则别的 namespace 同名 Service 会先应答
- [x] **LB GC（2026-08-07 实测）**：按租户前缀认自己的 LB，永不碰他人或 kubetron 的；
      所有检查 fail-closed。**造真孤儿验证**：停进程 → 删 Service → 重启 → 扫描清理成功
- [x] **FIP 全路径实测（2026-08-07）**：`internal=false` → 分配并回写 EXTERNAL-IP；
      改回 `internal=true` → **归还**，地址回落私网 VIP；删 Service → **不泄漏**
      （FIP 计数 4→3，LB 一并删除）。
      ⚠️ 前置条件：**VIP 子网必须接在带外网网关的 router 上**，否则报
      `ExternalGatewayForFloatingIPNotFound`（本环境原先只接了 pod 子网，已补接）
- [x] **✅ DNS 定案：租户自己的 CoreDNS，平台不发布任何名字（2026-08-08，与 kubezoo 协同）**。
      ⚠️ **推翻了本轮全部前案**（Designate、OVN 数据面 DNS、在 VIP port 上设 dns_name）。
      **推翻的理由**：从租户视角看，Service 在 `default` 命名空间，应用问的是
      `rsvc.default.svc.cluster.local`；而平台侧存的是 `111111-default`。
      实测租户 CoreDNS：租户视角的名字有应答，我发布的上游名字 **NXDOMAIN**——
      **我发布的名字没有任何租户应用会去问**。
      更关键：`<svc>.default.svc.cluster.local` 每个租户都要指向不同地址，
      **任何全局 DNS 命名空间都做不到**（Designate 的 zone 全局唯一在此从假设变成现实）。
      **per-tenant 解析器不是优化，是唯一可行的架构**，而 kubezoo 已经建好了。
- [x] **kubezun 侧改为读 `pod.Spec.DNSConfig`（2026-08-08，实测）**：网关注入的就是租户视角的
      search + nameserver，两者一起拿到，顺带解决"谁告诉 capsule 用哪个 resolver"。
      Zun 侧加 `knaas.io/dns-nameservers` 注解，pod 指定的覆盖子网默认值。
      ⚠️ **保留按 namespace 拼装作兜底**——resolver 未 serving 时网关 fail-open 不注入，
      那时 dnsConfig 为空，没有兜底则短名完全不可解析。
      **实测（租户视角建 pod）**：capsule 内 `search default.svc.cluster.local svc.cluster.local
      cluster.local` / `nameserver 254.51.215.104`，与网关注入完全一致
- [x] **~~VIP := ClusterIP~~ 已否决（2026-08-08，用户否）**：做法是在每个租户网络上建
      同一个 service-CIDR 子网，让 VIP 落在上游分配的 ClusterIP 上。技术上实测可行
      （capsule 直接 `wget http://254.51.24.88/` 成功），但**把同一段地址注册进每个
      租户的网络就等于在 Service 层把租户之间打通了**，OVN 的多租户隔离随之失效。
      → 改由下面的注解契约解决：地址由数据面决定，网关照抄，不要求两边地址相同
- [x] **✅ Service 地址契约 `kubezoo.io/cluster-ip`（2026-08-08，kubezun `a0335a8`，
      kubezoo `1e017f81` 已上线）**：kubezun 把 LB 的真实地址写进 Service 注解，
      kubezoo 在租户视角把 `spec.clusterIP` 换成它，上游值原样不动。
      **键用网关的域名而不是我们的**——将来换一种数据面也填同一个键，
      这里不该假设背后是负载均衡器。
      写入时机在 `loadbalancers.Create` 返回后、**不等 provisioning 完成**
      （VIP 那时已定），把租户看到不可达地址的窗口从几十秒压到 **2 秒**；
      注解被抹掉能在 45 秒内自愈。reconcile 失败时在 Service 上发
      `AddressNotReady` 事件——否则租户只看到一个不通的地址而原因只在我们日志里。
      **kubezoo 侧七项验证全过**：租户见 VIP / 上游未动 / CoreDNS 答 VIP /
      headless 不被覆盖 / apply 幂等 / 租户写注解被剥 / 租户指定地址被拒
- [x] **⚠️ 修复：LB 被反复删了重建，租户 ClusterIP 一直在漂（2026-08-08，`7b877db`，
      理由更正 `8caeaa4`）**：`ensureLoadBalancer` 读 LB 的 `vip_port_id`，
      404 就判定"provider 建失败留下了坏 LB"，于是删掉重建。
      **但那个端口本来就会消失**——三个 LB 全部 ACTIVE / ONLINE、member ONLINE，
      而它们记录的 `vip_port_id` 连 admin 都查不到；OVN provider 把地址放在数据面，
      不依赖一个要长期存在的 port。于是每轮 reconcile 都把三个 LB 删光重建，
      **每次重建换一个地址**——正是上面那个契约要保住的东西。
      日志上写着"负载均衡器没有地址端口"，实际坏它的就是这个进程自己。
      当初写这段时"实验室同时测到三个"的依据，就是这个 bug 自己造出来的。
      **修法是删掉，不加替代检查**。实测：三个 VIP 连续 3 分钟四次采样不变，
      `rebuilding` 触发 0 次。
      ⚠️ 中途我把原因误判为"端口属于 Octavia 项目所以租户看不见"，
      那个 project id 其实是租户自己的——已更正
- [ ] **租户 CoreDNS 必须跑成 capsule**（kubezoo 侧已移除落点豁免，2026-08-08）：
      只有跑在池上它的 pod 才拿到 OVN 地址，才能当 Octavia member；
      跑在平台 worker 上（Cilium 地址）则 capsule 既够不到它、它也不能作为 member。
      ✅ 前置条件已实测：capsule → apiserver 走租户 router 通（返回 401 = 到达且无凭据）
      **⚠️ 2026-08-08 复查，这条被三件事同时挡住，缺一不可**：
      1. **CoreDNS 还在 Cilium 节点上**（240.24.0.x / incus-node-05,06）。
         Kyverno 三条策略选的是 `knaas.io/tenant`，而 `111111-kube-system`
         只有 kubezoo 的 `kubezoo.io/tenant` → 策略不匹配 → 不注入放置
      2. **kubezun 没有服务 `111111-kube-system`**（`--namespaces` 是静态单值），
         所以不给 kube-dns 建 LB，注解不出现，kubezoo 只能报上游那个不可达地址
         → 见下面的 selector 改造
      3. ~~**CoreDNS 镜像是 distroless**~~ **✅ 已解决（2026-08-08）**：见下面的探针 helper。
         原文保留备查：（`registry.k8s.io/coredns/coredns:v1.13.1`，
         实测 `/bin/sh` 直接 `CreateContainerError`），而它的 readinessProbe 是
         `httpGet :8181/ready`，我们改写成容器内执行 → 无可执行文件 → 永不 Ready
         → **永远不能成为 Octavia member** → 见下面的探针 helper
- [ ] 环境中的 Designate/BIND 可停用；**建议先留着**——将来若需对外权威 DNS
      （把租户服务发布到公网域名）仍会用到，那是另一件事
- [ ] Octavia health monitor 作为**第二层**（LB 侧自检）是否需要：EndpointSlice 已能
      按 readiness 摘除 member，HM 是冗余保护而非必需；若启用需查 OVN provider 的
      `SUPPORTED_HEALTH_MONITOR_TYPES` 白名单（DESIGN §6）
- [ ] liveness 链路：对接 fork ExecSync 探针 + 重启；探针结果回流 capsule status →
      provider 映射 pod Ready（§6）
- [x] **租户红线文档 `docs/tenant-guide.md`（2026-08-07）**：不可用项（host* / privileged /
      spec.nodeName / emptyDir 等卷 / logs -f / exec -it / attach / port-forward / top）、
      语义差异（ConfigMap 是快照不是投影、探针在容器内跑且 distroless 无 curl 会失败、
      SA token 默认关）、以及"该怎么写"（设 limits、需要架构就写 nodeSelector、
      VM 冷启动慢要设 initialDelaySeconds）。每条都有本轮实测依据
- [x] **✅ 命名空间作用域改为标签 selector（2026-08-08 完成，`41ac354`+`a04e267`）**：
      `--namespace-selector kubezoo.io/tenant=<id>`；Secret/ConfigMap 改成按需读、零缓存
      （上游 PodController 只用 `.Lister()` 从不用 `.Informer()`，所以自己实现 lister 即可）；
      pod 改成每节点一个 informer 按 `spec.nodeName` 选（`orphans.go` 的注释本来就这么写，
      实现终于对上了）。**实测服务 4 个命名空间，`111111-kube-system` 自动纳入，kube-dns 有 LB 了**。
      ⚠️ **部署时连撞三个问题，全部已修，值得记住**：
      ① RBAC 缺 `namespaces: list/watch` → 进程阻塞在同步 → 节点停心跳 →
         **`TaintManagerEviction` 把 pod 全驱逐、ReplicaSet 重建**。设计错在
         "让一条 watch 决定节点死活"；改成有界等待 + 失败只告警（`373f04e`）。
         **节点该说"我在但不收新 pod"，不该说"我没了"**——后者会毁掉正在跑的东西
      ② Service informer 变全集群后 reconciler 不查命名空间 → **用租户凭据、在租户 project 里
         给全集群的 Service 建了 19 个 LB**，含平台的 hubble/kyverno/kubetron、
         `default/kubernetes`、以及**另一个租户的 kube-dns**。加命名空间检查 + GC 回收
         （`7896008`），实测 19→4 自动清干净。GC 在集合为空时整轮跳过——
         "空"是"还不知道"不是"没有"
      ③ 见下面的地址占用
- [ ] ~~命名空间作用域改造~~（原条目，已完成，保留描述备查）：
      现在是 `--namespaces 111111-default` 静态单值，代价有三——
      `<tid>-kube-system` 进不来（kube-dns 因此没有 LB）；租户新建命名空间那里
      **永远 Pending 且静默**（`authorize` 故意不区分"无权"和"空"）；
      而填两个以上会让 informer **退化成全集群 watch**
      （`vknode.go:99` 只有 `len==1` 才加 `WithNamespace`），
      于是这个租户的进程把**所有租户的 Secret 缓存进内存**。
      改用 `kubezoo.io/tenant=<id>`：kubezoo 强制写、租户改不动
      （`convert/namespace.go:53` 拒绝改成别人的值，`proxy/apply.go` 防剥离），
      而且它**能在 watch 层表达**（前缀不能，field selector 不支持前缀）。
      我们代码里也就不用抄"定长 6 + 第 7 位是 dash"那段算术——那是 kubezoo 的概念。
      ⚠️ pod/Secret 的 informer 要随这个集合动态建，**不能先加命名空间再说**，
      否则就是拿跨租户的 Secret 缓存换一个 DNS
- [x] **运行时任务没了的 capsule 现在删得掉了(2026-08-11,fork `fcbc5da4`)**:
      根子是**停止和删除写在同一个 try 里**——shim 没了时 stop 会等一个没人应答的任务、
      `DEADLINE_EXCEEDED`,然后把删除一起带走。于是**永远删不掉**:每次重试都在等同一个
      不存在的 shim,记录还在、资源账还在,而没有任何东西看起来占着它。
      **删除才是目的,停止只是礼貌** —— 现在两者分开尝试,停不掉就记日志继续删
      (CRI 本来就规定 `RemovePodSandbox` 要强制终止里面还在跑的东西)。
      **实测**:那批 2026-08-06 起删不掉的,`DELETE` 从 500 变 204;租户的 capsule 从
      29 个降到 5 个,且 5 个全部有 pod 在跑。
      ⚠️ 修好这步才露出下一步:并发删 13 个时 Neutron 拒连(`Remote end closed
      connection`)——那是负载不是逻辑,放慢重试即全部通过。
      ⚠️ **纠正:孤儿 capsule 治理早就做了**(`pkg/provider/orphans.go`,每 2 分钟一轮)。
      积压不是因为它不存在,而是两件事叠加,现在都没了:① 13 次删除里 9 次撞上上面
      那个删不掉的 bug;② **192 次/天被"这个 capsule 没有节点名,不敢动"挡住**——那批
      是加 `knaas.io/node-name` 标签之前建的历史遗留,已手工清空,新建的一律带标签。
      **它分辨归属靠六道闸,每道都 fail-closed**:pod 标签必须能解出 key(⚠️ **租户
      直接用 Zun API 建的 capsule 没有这些标签,整个跳过**)、只看 capsule 不看原生
      容器、`node-name` 必须等于本节点、命名空间必须是本节点服务的、按 **pod UID**
      而不是名字匹配、5 分钟宽限期
- [ ] ⚠️ **实验床的 systemd unit 是手改的,和仓库模板已经分叉(2026-08-11 发现)**:
      `/etc/systemd/system/kubezun@111111.service` 不是从 `deploy/kubezun@.service`
      生成的实例,而是一份独立维护的文件(21 个参数 vs 模板的一套)。我这次为了开
      NetworkPolicy **直接 sed 改了它** —— 那是错的:改动没进仓库、下次照模板部署就没了,
      而且"实验床在跑什么"从此只能上机看、不能读仓库。
      **和 `/opt/stack/zun` 是同一个形状**(CLAUDE.md 已经为 Zun 记过这条教训)。
      要么让实例文件由模板生成,要么把两者的差异明确记下来
- [x] **NetworkPolicy 已实现并端到端实测(2026-08-11)**——原"静默 fail-open"已消除。
      设计见 DESIGN §7.7;实现 `pkg/netpol`(翻译/Neutron/规则集/reconciler/控制器/迁移)。
      **实验床端到端**:租户 144 个 capsule 端口两阶段转换(attach 只加不减→连通性零影响;
      detach 摘 default→**仍全通**),开启执行后写一条
      "server 只收 role=client:8080":**client 通、stranger 被挡**;删掉策略两者恢复。
      端口最终形态 `[knp-rules-<hash>, knp-allow-egress, knp-deny-all]`。
      ⚠️ **默认关闭**(`--enforce-network-policy`),且开启前必须先跑两阶段转换——
      逐个 pod 切会断流(§7.7.5a)。
      ⚠️ **RBAC 需新增 `networkpolicies` 只读**(本次实测才发现清单里没有)。
      **GC 已做**(`cd25996`,半小时一轮、按策略身份识别、一趟收敛)。
      **仍未做**:`ipBlock.except`/命名端口/ANP 的**准入拒绝**——kubezun 不在准入路径上,
      NetworkPolicy 也没有 status 可写,只能记日志;真正的拒绝要 Kyverno 或 kubezoo
      网关(§7.7.4),是跨团队协作项。
- [x] **（P2 → 已做）外部评审 (b):Neutron 写全部挪出 pod 热路径（2026-08-14 实现并实测）**：
      策略 worker(`SyncPolicyPeers`)现在拥有一条策略的**全部** Neutron 写(地址组 +
      内容 + rules 组),写成功后才记入 `ruleGroups` 缓存;pod 路径(`policyGroupsFor`)
      纯缓存读,未命中返回 `ErrPolicyPending` → controller 立即催策略队列 + pod 退避重入,
      **绝不内联回退**(回退路径就是冷启动时的热路径复辟)。策略删除 → `ForgetPolicy`
      (残留缓存会把 sweep 将删的组 id 发给新 pod)。`ReportRefusals` 随写路径搬到策略
      worker,顺带消掉每 pod 一条的重复告警。
      **测试的妙处**:夹具的 Neutron 本来就是 nil——**nil 即零调用证明**,旧代码在这两个
      场景直接 panic,天然红。实验床回归:重启后 keeper(无标签)仍被挡、新建
      `role=client` pod 经冷缓存路径放行——执行语义双向不变。
      原 verdict 记录:
      ——已验证属实:`reconciler.go:155,162,272` 每个 pod 事件付 O(选中策略数) 次 Neutron
      往返,且 pod 队列无合并窗口(`controller.go:147,220` 直接 Add,策略队列才有
      AddAfter)。改法:策略 worker 维护 policy→groupID 表,pod 路径只读 ID,塌缩成
      一次 ports.Get + 至多一次 Update。⚠️ 两个边界:缓存未命中(pod 先于策略 worker)
      → 重入队退避,不回退到内联 Ensure;策略删除后 groupID 失效 → Update 拿 400 时
      fail-closed 重试,等 sweep/worker 收敛
      ✅ **采纳(文档)**:secgroup_rules 配额按 **Σ(peers × ports × families)** 估,
      不按策略规则条数——比 DESIGN 现在"放开几个数量级"更具体,并入 §7.7.5b 运维清单
      ⛔ **否决 (a) 删 deny-all 锚**——评审说"Zun 模板路径没验之前别动",验了:拦路虎
      在**我们自己**的序列化层且今天就在生效:`template.go:281`
      `json:"securityGroups,omitempty"`,空列表整个被丢 → 字段缺失 → 端口拿 project
      default SG → **全隔离 pod 静默全开**。这正是历史上"empty vs absent"那次事故的
      机制,锚就是当时的修复。删锚省的是 1/3 的 PG 重建成本(两个 baseline 组照样在),
      换来的是"链条上任何一层把空集折叠成缺省"永久成为静默 fail-open——
      封闭白名单形状的脆弱性,不换
- [x] ⭐ **压 OVN 那两个数 —— 已测并经分辨性补测更正(2026-08-14)**：
      ⚠️ 第一版结论错了一半,被"观察到全云更新了吗"一问抓住:耗时平坦 ≠ 增量——
      `inc-engine/show-stats` 证实 **SG 建删/端口组变更每次都是全量 lflow 重算**,
      平坦只因空 PG 贡献零 lflow(SB 2219→2219)。**伸缩变量 = 全云 lflow 总量**:
      2.2k 条 →20ms,外推 100k 条 → ~1s/策略事件——这才是 region 容量规划的数,
      §7.4.2 的切分逻辑被加强而非解除。controller 侧 ≤1ms 但 delta 仅 6 条 lflow,
      增量与否分辨不出,保守假设。**唯一无保留证实的押注:address group 轴真免费**
      (引擎未触发)。全表进 DESIGN §7.7.7。原条目:（DESIGN §7.7.7，实验床 `ovn-appctl stopwatch/show`）：
      ① northd 在**建删安全组**时的重算耗时；② 计算节点 ovn-controller 在**改端口安全组
      列表**时的 `lflow_run` 耗时。
      ⚠️ **2026-08-13 用途升级**：它们不只是"增量 2 的门槛"，而是 **region 数量规划的
      前置输入**（§7.4.2）——OVN 的分片单位是 region，不知道一个 OVN 控制面能扛多少策略，
      就不知道该切几个 region，于是 `节点数 = regions × K × AZs × archs` 的第一项无从估算。
      ⚠️ **这根轴 kubetron 没有**：`grep -rln "NetworkPolicy\|SecurityGroup" /root/kubetron`
      → 零个文件，B1 的策略由 Cilium 在 eBPF 执行、完全不进 OVN。**所以不能拿它的分片
      经验套用**——它加 OVN-IC 是为了 chassis，我们要加 region 是为了它没有的那根轴
      ⚠️ **不可用 devstack 评估**（同机跑满全套 OpenStack，同 §7.4 的告诫）
- [ ] **⚠️ arm64 虚拟节点没有硬件却照样上报容量（2026-08-08 发现）**：
      CoreDNS 先被调度到 `111111-node-arm64`，全部 `CapsuleUnschedulable`（Placement 拒绝）。
      这正是"节点上报的 capacity 是承诺不是库存"的实例——虚拟节点按配额镜像容量，
      与该架构背后有没有主机无关。暂时用 `nodeSelector: kubernetes.io/arch=amd64` 绕开。
      真修法待定：某架构无可用主机时该不该上报可调度容量
- [x] **✅ 已修（2026-08-08，fork 侧补齐 CRI security_context 传递；走的是路线 ①）**：
      runAsUser/runAsGroup/fsGroup/readOnlyRootFilesystem/allowPrivilegeEscalation/
      capabilities/seccompProfile 全部落到运行时。
      ⚠️ 它存在 `healthcheck` 列里,不是 capsule 的 annotations——API 会用生成名覆盖
      capsule 容器的名字,按名字做键永远匹配不上,于是每个容器都以 root 跑且根文件系统
      可写,**静默地**。实测 `id` 回 `uid=1001 gid=2001 groups=2001,3001`。
      原始记录（保留备查）：**⚠️ securityContext 大部分字段被静默丢弃（2026-08-08 发现）**：
      Zun 的 CRI 驱动只传一个字段——`LinuxContainerSecurityContext(privileged=...)`
      （`driver.py:262`），而我们的 `unsupported()` 也只拦 `privileged`。于是租户写的
      这些**全部无声消失**：
      | 租户写的 | capsule 里实际 |
      |---|---|
      | `runAsNonRoot` / `runAsUser` | 用镜像自带用户（镜像默认 root 就是 root） |
      | `readOnlyRootFilesystem: true` | 根文件系统可写 |
      | `capabilities.drop: [ALL]` | 没有丢弃 |
      | `seccompProfile: RuntimeDefault` | 无 seccomp |
      撞的正是 `template.go:38` 自己写下的原则（"静默丢弃比失败更糟：pod 会以更弱的
      隔离运行"）。而 PodSecurity `restricted` **强制**租户写这些字段——等于我们要求
      他们写、然后忽略。
      缓解：capsule 是 Kata VM 且 `privileged` 恒 False，逃逸落在 guest 内核里而不是
      宿主机。但"要求非 root 却以 root 运行"本身就是错的。
      **两条路**：① fork 补齐 CRI 的 security_context 传递（run_as_user/readonly_rootfs/
      capabilities/seccomp，CRI 都有对应字段）；② 暂时按 `unsupported()` 明确拒绝。
      倾向 ①——②会让符合 PodSecurity restricted 的 pod 全部无法运行
- [ ] **Pod 失败原因用 K8s 词汇而不是后端词汇（2026-08-08 发现，小改）**：
      现在写的是 `CapsuleMissing` / `CapsuleStuckCreating`，租户没听说过 capsule。
      K8s 自己有：`ContainerStatusUnknown`（kubelet 判断不出容器结局时用的）、
      `FailedCreatePodSandBox`（sandbox 始终没建起来）。
      惯例是 **Reason 用标准词、Message 放细节**，capsule UUID 留在我们日志里
- [ ] **验收**：~~去特权 fluent-bit DS 在租户节点起一份 capsule~~（DS 已废弃）；
      系统 DS 不落虚拟节点、真实节点无 Pending 残留；liveness 失败触发重启；
      readiness 结果回写 pod Ready 并驱动 EndpointSlice；HM 摘除未就绪 member（§12）

---

## I：Ingress（按需计费能力，DESIGN §7.5a/§7.5b）

⚠️ **与 Service 是两类东西**：Ingress 走 L7，Octavia provider 必须是 amphora 或 incus
（**绝不能 ovn**，它是 L4-only 拒绝一切 L7 对象），意味着**真实实例成本** →
按需/计费开通，不像 Service 那样默认给。

- [x] **✅ 抄 kubetron `pkg/ingress/` 完成（2026-08-08，`8abc1b6`+`684ee25`+`9353aca`）**：
      `l7.go`/`barbican.go`/`tuning.go` 基本平移；`reconciler.go` 剥掉了 kubetron 的
      `NSConfig`/`ResolveNamespaceConfig`（每 namespace ConfigMap）和 `webhook.NamespaceShard`
      （分片）——kubezun 的作用域是**服务命名空间集**。teardown 不用 finalizer，
      改走 pkg/service 的**名字派生 + 孤儿 sweep**（一套恢复模型盖两种 LB）；FIP 归属从
      Ingress 注解移到 **FIP 的 description**（sweep 没有对象也能判 delete/detach）。
      ⚠️ **2026-08-13 复核：剥掉 `NamespaceShard` 的决定在共享节点形态下依然成立**，
      别修回去——进程服务的命名空间集**就是**分片，没有第二层分片要表达。
      `BuildMembers` 与 Service 复用同一个（member 取 capsule 地址与 subnet）。
      ⚠️ **Service sweep 的 `parseLBName` 必须拒绝 ing 名字**：两种名字共享租户前缀，
      按第一个下划线切会把 "ing" 读成 namespace、找不到同名 Service，然后把每个
      Ingress LB 当孤儿删掉。
      **实测（租户视角）**：建 Ingress → 拿到 ADDRESS `192.168.200.174`；Octavia 侧
      LB ACTIVE、listener :80、两条 l7policy（position 1/2，按最长路径排序）、pool member
      `192.168.100.228:80 ONLINE`——**控制面全部正确**。
      ⚠️ **首次部署撞了两个**：① `Ingresses().List(nil)` 在 lister 里 panic，每个
      EndpointSlice 事件都崩 → 改 `labels.Everything()`；② 网关给 cluster-scoped 名字
      **加租户前缀**，IngressClass 也不例外——租户写 `knaas` 这边读到 `111111-knaas`，
      只匹配裸名会把每个租户 Ingress 判成"非我的"而走进 teardown；而那条 teardown 的
      Barbican 清理是无条件调用的，租户没有 key-manager 权限时 403 无限重试卡死整个队列。
      已修：`Ours` 认前缀 + 无 LB 时的 Barbican 清理改 best-effort
- [x] **L7 数据面 503 已解决（2026-08-10）——两个独立成因，都不在 incus provider**
      （此前"断点在 provider"的判断是错的：provider 侧对象翻译一直正常）：
      1. **capsule 端口只在项目 default 安全组里，入站规则只有"同组来源"**。
         同租户 pod 互访、以及 L4 的 ovn provider Service 都不受影响（DNAT 后源仍是同组
         capsule），但 L7 的 worker 是**另一组的独立实例**，报文全被丢在 capsule 端口上。
         `traceroute` 打到 `192.168.200.1` 后即止。补一条"放行 VIP 子网 → capsule"
         的入站规则后 `/web` 立刻 200。⚠️ **实验床目前是手工加的规则，见下一项**
      2. **`/` 路径的 l7policy 一条规则都没有**（Octavia 按规则匹配，无规则=永不匹配），
         而 drift 比较里埋了同一个假设（把无规则读回成 `/`），于是自我掩盖：
         reconcile 永远判"无漂移"，症状只剩 503，Octavia 侧一切看起来健康（member ONLINE）。
         已修 `ba1d36a`。**实测**：`/` → ROOT-BACKEND 200，`/web` → WEB-PATH 200
- [x] **capsule 拿不到集群 DNS（2026-08-10 修，`00d1947`）**——症状是"租户 DNS 全挂"，
      但 CoreDNS 一直在跑、可达、记录也对，**只是没有任何 capsule 问过它**：
      pod 绝大多数不带 `dnsConfig`（`dnsPolicy` 默认 ClusterFirst，本该由 kubelet 补全），
      我们不补 → capsule 用了 Neutron 子网的解析器（公网 DNS）→ 集群内名字全 NXDOMAIN。
      搜索域同向错误：用的是**存储态命名空间** `111111-default`，而租户的 CoreDNS 透过
      网关看世界、只认 `default`。⚠️ 解析器地址必须取 Service 的 `kubezoo.io/cluster-ip`
      注解（= Octavia VIP），**不是 `spec.clusterIP`**——后者是网关给租户看的虚地址，
      租户网络上无人路由，实测从 capsule 问它必超时
- [ ] **把"放行 L7 worker → capsule"做进开通流程**（当前是实验床手工规则，重建租户即丢）：
      capsule 端口用项目 default SG，只认同组来源；Ingress 的 L7 worker 在 VIP 子网上，
      必须显式放行。**按 VIP 子网 CIDR 加一条规则，不要每 pod 一个 SG**——用户已明确
      OVN 下安全组数量的代价（TODO 行 512 同一约束）。落点大概率是 provisioner 建租户
      网络时一并写入，与 §14.6 的 member subnet 归在同一处
- [ ] **实验床的 SNAT 豁免是手工 OVN 改动，重建租户即丢**（2026-08-10）：
      为让 capsule 访问 VIP 而删掉的 router SNAT，代价是**租户所有出网都没了**——
      CoreDNS 连不上 apiserver（kubeconfig 指向 `10.224.18.51:6443`），
      `kubernetes` 插件永不就绪 → `/ready` 503 → 探针如实上报 → Deployment 挂在
      `0/2 available` 好几个小时，而 DNS 链路本身**哪里都没错**。
      现用 OVN `NAT.exempted_ext_ips` 恢复 SNAT 并豁免东西向。⚠️ **豁免集必须同时含
      pod 子网和 VIP 子网**：OVN 先做 LB 的 DNAT，比对豁免时目的地已经是成员的 pod IP
      不是 VIP；只豁免 VIP 子网实测的结果是出网恢复、L7 Ingress 正常（worker 是真实例
      不发夹）、而**所有 L4 Service VIP 全黑**。正解仍是 docs/bootstrap.md 的地址域
      （天然覆盖两个子网，不用推理 NAT 次序），但 Neutron 不允许把已存在的子网并入
      subnet pool，所以实验床只能补 OVN
- [x] **Zun 拒绝单字符容器名**（K8s 合法、Zun schema `minLength:2` 且 pattern 自带第二字符
      要求）→ 容器名为 `c` 的 pod 直接 ProviderFailed。已修 Zun fork `6c2d7404`
- [x] **TLS / host 规则 / FIP 端到端实测通过（2026-08-10）**：
      自签证书 → `kubernetes.io/tls` Secret → `spec.tls` → Octavia
      **TERMINATED_HTTPS listener :443**；从 capsule `--resolve` 访问
      `https://web.knaas.test/` → ROOT-BACKEND 200、`/web/` → WEB-PATH 200，
      握手证书正是我们签的（`subject: CN=web.knaas.test`）；**host 不匹配返回 503
      而不是误路由**。FIP：`octavia.ingress.kubernetes.io/internal=false` → 分配
      `10.128.32.139` 并回写 ADDRESS，**从租户网络外**访问 HTTPS/HTTP 均 200；
      改回 `true` → FIP 被**删除**（不是仅解绑）、ADDRESS 回落 VIP，无计费泄漏。
      归属标记确实写在 FIP description 上（⚠️ `openstack floating ip list` 不返回
      description 列，只有 `show` 才看得到，别据此以为没写）
- [x] **每 Ingress 选 provider（照 kubetron，`58038c3`）**：注解 `knaas.io/ingress-provider`，
      不填即部署默认（`-ingress-provider`）。**改已有 Ingress 的 provider 直接拒绝**——
      Octavia 没有"迁移 provider"这个操作，硬来要么让 Ingress 继续跑在旧 provider 上而
      一切显示健康，要么静默重建、把租户已经公布出去的 VIP/FIP 弄丢。另外两道在
      **创建任何东西之前**就挡下：`ovn` 按名拒（L4-only，它是租户最可能顺手写的那个，
      因为所有 Service 都用它）；本部署没装的 provider 直接报错并列出装了哪些。
      实测三条报错文案 + amphora 与 incus 同租户并存（LB 建在 amphora 上、Octavia 一路
      驱动到起 amphora 虚机，**本实验床 nova-incus 起不来那台虚机**，断点在计算层不在我们）
- [ ] **实验床 amphora provider 起不来**：`ComputeWaitTimeoutException`，amphora 镜像在
      nova-incus（lxd hypervisor）上引导不起来。不影响 kubezun，但 L7 只剩 incus 一种
      可选，**provider 并存只验到了控制面**
- [x] **两个被 provider 测试顺带挖出来的缺陷（`58038c3`，与 provider 无关）**：
      ① `SetPoolMembers` 无条件 `BatchUpdateMembers`——成员没变也是一次写，Octavia 把
      pool 和 LB 推进 PENDING_UPDATE，而那正是它拒绝其他一切改动（"immutable"）的状态。
      实测改前每 ~30 秒写一次、几乎从不 ACTIVE，改后两分钟零写入。⚠️ pkg/service 共用
      同一函数，**Service 侧一直在同样空转**；
      ② 给运行中的 Ingress 加 TLS 会把 :80 listener 连同它那份 l7policy 遗留下来，
      **再也不被 reconcile**，于是继续按 TLS 到达那一刻的配置服务一个 Ingress 已经不再
      声明的端口。它能藏住正是因为"80 端口有响应"。现在切换协议时删掉不要的那个
      listener（policy 随之级联删除）。⚠️ **kubetron 同源同病**（`pkg/ingress/l7.go:362`）
- [ ] **开通必须给租户 `creator` 角色，否则 TLS Ingress 全线不可用**（2026-08-10 实测）：
      Barbican 建/列 secret 需要 `creator`，租户 appcred 原本只有 `member`+`reader`
      → reconcile 卡在 `listing Barbican secrets ...: 403`，listener :443 永远建不出来。
      ⚠️ **给用户加角色不会改已签发的 appcred**——appcred 的角色在签发时固化，必须
      重发一张；且 appcred 只能由**该用户自己**签（admin 签出来的是 admin 项目的，
      正是设计明令禁止的东西）。实验床已重发（`841ad224…`，roles=creator,member,reader）
- [x] **TLS 续期已实测（2026-08-10）**：`ensureTLS` + `barbican.go` 的内容哈希命名
      （续期→哈希变→新 ref→reconcile 自然跟上，不比对有效期、不关心谁签的）；
      `spec.tls` 存在时自动建 TERMINATED_HTTPS listener，陈旧 bundle 在 listener 指向
      新 ref **之后**才删（顺序反了会把两种 provider 卡在 PENDING_UPDATE）。
      ⚠️ 首次签发已实测（见上一项）；**换证书这条路径还没跑过**——把 Secret 里的
      证书换掉，确认哈希变 → 新 Barbican ref → listener 指过去 → 旧 bundle 才被删

---

## 本轮补齐的功能面（2026-08-10）

- [x] **ServiceAccount token（`1ddf9f3` + Zun `cc36dca3`）**：以前直接拒绝带 token 卷的
      pod，理由是"capsule 无法刷新绑定 token"——那是事实，但不是拒绝的理由，是该去做
      刷新的理由。现在三件套(token/ca.crt/namespace)随 capsule 带进去，token 经
      TokenRequest 铸造并**绑定到 pod**（pod 没了 token 立刻失效，不是长期 SA Secret）。
      续期走 Zun 新端点**原地改写文件卷**，⚠️ **不能用 exec**：distroless 镜像没有 shell，
      实测报 `the file ls was not found`——最该照顾的镜像恰恰是它够不到的那批。
      `namespace` 文件写租户视角的名字（存储态会让网关再加一次前缀，问的是
      `<tid>-<tid>-default`，与 DNS 搜索域同一个错）。**实测**：三个文件都在、token 认证
      通过、用它列 pod 得到 403（认证通过、被正确拒绝）
- [ ] ⚠️ **租户 `kube-root-ca.crt` 与网关证书不同源(kubezoo 侧)**：ConfigMap 里是上游集群
      CA(`CN=kubernetes` 自签)，而 capsule 实际访问的 10.224.18.51 出示的是 **KubeZoo 自己的
      CA**(`O=KubeZoo`)。租户按投影进去的 CA 校验必失败(`-k` 才通)。我们这边无法凭空造出
      正确的 CA——发给租户的那个 ConfigMap 必须装它签的东西
- [x] **`kubectl top` 端到端通(2026-08-11,`9c26a86`)**:metrics-server 0.8.1 抓虚拟节点
      曾全 `<unknown>`,原因是我们 kubelet API 用了 `RequireAndVerifyClientCert`——
      **比真 kubelet 严**,在 TLS 握手就拒掉只带 bearer token 的 metrics-server。真 kubelet
      用 `RequestClientCert` 正是为此。改后节点/pod 指标都出真值。⚠️ 并不降低安全:
      每个请求仍走 authn(x509 或 TokenReview)+ authz(`nodes/<name>` SAR),两者都没有
      的调用方在 HTTP 层被 401
- [x] **`kubectl top` / HPA 的数据面（`53204a9` + Zun `c6f85263`）**：以前 capsule 是黑盒。
      Zun 侧新增 capsule stats（CRI `ListContainerStats`，按容器给），kubezun 侧同时实现
      `/stats/summary` **和** `/metrics/resource`——只做前者的话现代 metrics-server 抓不到，
      `kubectl top` 仍是空的。⚠️ CPU 记的是累计计数器，速率要两次读数之差，而**容器重启会
      把计数器清零**，按名字记会算出一个天文数字的速率——正是让 HPA 给空闲负载扩容的那种
      输入，所以游标同时带容器 id。实测：节点/每 pod 的 CPU 与内存都出真实值
- [x] **重启不再摘流量（`b586179`）**：进程重启后内存里 pod 表是空的 → VK 对每个 pod 调
      CreatePod → 发现 capsule 已在 → **旧代码把状态重置成 Pending/not-ready**，
      EndpointSlice 随即清空、两个 reconciler 各写一次空成员集，租户流量断掉；几秒后
      sync 循环再补回来，所以一直没人注意到。现在是**认领**：沿用 API server 上已有的状态。
      实测 Ready 的 lastTransitionTime 重启前后完全一致
- [x] **节点启动前置检查（`53204a9`）**：AZ 不存在的节点照样注册、照样收 pod，然后每个 pod
      被 Zun 逐个拒成 "no valid host"——读起来像集群满了而不是配置写错。⚠️ 这个检查第一版
      **自己把能工作的节点拒了**（读了 `name` 字段而响应里是 `availability_zone`，得到一串
      空名字），现在只在**确实读到了名字**时才拒绝。架构无法核实：Zun 的 hosts API 是
      admin-only，而我们持租户凭据——拼写错能挡，"拼对了但没这硬件"归开通管

## 存储（2026-08-11，emptyDir + PVC RWO/RWX 全部端到端实测）

- [x] **emptyDir（Zun `84dd67d4` + kubezun `b7639ad`）**：新卷种 `emptydir`,容器间共享、
      随 capsule 删除;`medium: Memory` → 宿主机 tmpfs(内核强制 sizeLimit,实测 64Mi
      在 guest 里正好 65536KB);目录 0777(同 kubelet——capsule 不知道镜像用什么 uid)。
      ⚠️ 放行清单散在四处,少一处一个错法:schema、`utils.capsule_get_volume_spec`、
      stevedore entry point、`[volume] driver_list`
- [x] **PVC 由 kubezun 自己 provision(§14.8 落定,`c21b50b`)**:不部署 CSI——集群级
      CSI 控制器要持全租户云凭据,正是本平台要避免的;kubezun 已持租户 appcred、已在跑
      同形状的 reconciler。accessModes 决定后端:RWO→Cinder,**RWX→只有 Manila**
      (multiattach 共享的是块设备不是文件系统,两个写者静默损坏 ext4,`KindFor` 直接拒)。
      PV 用 CSI 形状承载(driver 名 `cinder.knaas.io`/`manila.knaas.io`),将来真 CSI 接手
      不用改数据。建了存储但写 PV 失败 → 删存储(唯一会漏成账单的方向)
- [x] **RWO 端到端(Cinder/ceph)**:PVC→卷→Bound→pod 挂载写入→删 pod 重建→数据在。
      ⚠️ **fsGroup 是两半**,缺任何一半症状一样(挂上了、属主对、每次写都 Permission
      denied):fork 挂载后 chown :fsGroup + setgid(`93ba565b`),**且** fsGroup 必须进
      CRI 的 supplemental_groups——进程不在那个组里,属主改了也没用。
      ⚠️ 计算节点要 ceph-common + /etc/ceph 配置(`which rbd` 失败即此);已装 04/05/06
- [x] **RWX 端到端(Manila NFS,fork `684580a3`)**:PVC RWX→share→两 pod 同挂,
      reader 实时读到 writer 写入。**节点自授权模型**:attach 时节点用**租户请求上下文**
      给自己的 /32 授权(永不授权子网),detach 时最后一个挂载走了才 revoke;
      Manila 用 keystone session 直发 4 个 REST,不装 manilaclient。
      ⚠️ 两节点同时 grant 是常态不是边角(RWX 本来就是多处同挂),Manila 一次只应用一条
      规则、期间拒 400——必须重试,否则 pod 输在竞态上
- [x] **RWX 安全边界:已落成控制(2026-08-11,fork `8e4b33d6`)**——原分析不变,
      变的是它从**文档变成了代码**。信任单元是节点不是 capsule:share 挂在节点上,
      文件服务器按客户端地址授权。两条控制,**都默认拒绝**:
      ① **`[volume] host_dedicated_to_capsules`(默认 false)**:节点不自称"这里没有
      别的租户负载"就**根本不挂 share**。⚠️ **开通/部署流程必须为纯 KNaaS 节点
      设置它**,否则 RWX 全线不可用(04/05/06 已设)。
      ② **授权集宽于单机即拒挂**:我们发的每条授权都是 `/32`、最后一个挂载走了就撤销,
      那就是宿主挂载 share 的**全部**隔离;一条子网规则就把它换成零,而 capsule
      看不出区别。非 `ip` 类型规则同样拒绝。
      **为什么拒绝不是告警**:被保护的性质从 capsule 内部**不可观测**——邻居也能读的
      share 和私有的 share 长得一模一样,直到被读走。**拒绝对能修的人可见,暴露对谁
      都不可见。**
      **实测四步**:未声明→拒绝且点名要设的配置;声明后→正常挂载读写;手工加
      `10.32.32.0/24`→再次拒绝并说明宽在哪;删掉该规则→恢复,授权表回到一条 `/32`。
      ⚠️ **这不让共节点形态变安全,是让它不可用**,直到有**凭据属于 share 而非属于
      节点**的后端(CephFS+cephx)或 **guest 内挂载**(§8.2 P3:客户端身份=capsule
      OVN port IP,授权单元与租户边界重合;代价是拓扑反转 + guest 内核 NFS client +
      Zun→Kata direct-volume 通道)
- [x] **失败原因不再半路丢失(kubezun `pkg/zun/status.go`)**:Error/Dead 分支读的是
      `status_detail`,而后端把解释放在 **`status_reason`**(同文件的 waiting 分支
      读的就是它)。于是一条点名了"要改哪个配置、在哪台机上改"的完整拒绝,到租户手里
      只剩 `exitCode 1 / Error`。**失败路径上偏偏取了那个通常为空的字段。**已加回归测试
- [x] **与 nova-incus 的 Manila 实现对比后硬化（fork `447a3a59`）**——抄了四处:
      ①挂载带 `nosuid,nodev`(租户控制 share 全部内容且挂在宿主机上);②mount 加 60s
      超时(NFS 服务端不可达时裸 mount 卡几分钟,zun-compute 是单进程 greenthread——
      又是心跳饿死那个形状);③已有挂载校验 source==export(旧挂载静默顶替=给 capsule
      一个它没要的文件系统);④**per-share 锁修真竞态**:两 capsule 同时释放最后两个
      挂载,各自 umount 后都看到对方的还在 → 双双跳过 revoke → 授权永久泄漏。
      实测:同时删两 pod → 三节点 0 挂载、授权清单 0 条。
      **保留的差异**:授权留在 driver、用租户请求 token——nova 在上层用服务凭据授权,
      分层更净但要一份近 admin 的 Manila 服务用户;Zun 无 conductor 层、cinder 路径
      本来就在节点上用请求上下文,且租户 token 意味着节点只能操作它正在放置的租户的
      share,权限严格更小。
      **记账未抄**:nova-incus 的 share journal(挂载 crash 恢复——本次部署的 NameError
      窗口恰好泄漏了一个它专治的孤儿挂载,已手工清)、CephFS secretfile(per-share
      凭据的现成模板,留给"有凭据后端"生产化项)
- [x] **provision 竞态泄漏(`fcf2803`,用户问"改好了吗"复查时抓到)**:informer 缓存
      滞后 → 同一 claim 两次 provision 各建一个卷;PV 只有一个赢家,输家在 AlreadyExists
      分支直接返回,**它刚建的卷无人记录**——winner 的 PV 记的是 winner 的 ID,sweep
      永远认不出这个孤儿,漏成账单。现在输家在收养 winner 的 PV 前先删掉自己建的存储。
      实测:新 claim 恰好 1 个卷,删除后归零
- [x] **孤儿节点资源清扫(2026-08-11,fork `22faac5a`)**——卷走了但节点上的东西留下:
      ①NameError 窗口漏过一个 share 挂载;②dbpod 删除后 /dev/rbd0 仍 map 在 node-04
      (Cinder 侧 attachment 已摘、卷 available,但异步删除撞 watcher 失败回滚)。
      **不是美观问题**:映射着的 rbd 镜像持有 Ceph watcher,Ceph 拒绝删除被 watch 的
      镜像——表现为"卷可用但删不掉,且没有任何东西看起来占着它"。而一旦 volmap 行
      没了,**再没有任何东西会回来找这些残留**:引用它们的记录正是被删掉的那条。
      实现:周期任务(`[compute] reclaim_node_resources_interval`,默认 600s)比对
      `/proc/mounts` + `rbd showmapped` 与 volmap 表,无主的 unmount/unmap。
      ⚠️ **rbd 侧靠镜像名里的 Cinder 卷 id 反查**——volmap 自己的 uuid 根本没传到设备上。
      两种形状都实际发生过并已实测清掉
- [x] **跨节点 RWX 实测通过(2026-08-11)**:`rwx-far` 落 node-06 → 授权集自动变两条
      `/32`,它读到 node-04 上 4 个 pod 写的全部内容;删掉它 → 收缩回一条,**留下的正是
      仍有 pod 的那个节点**,其余 pod 读写不受影响。
      ⚠️ **散开的决定权不在 K8s**:pod 落哪个虚拟节点只决定约束(arch/AZ),capsule 开在
      该 AZ 的哪台计算节点由 **Zun 调度器**定,K8s 看不见。Zun 在容量充裕时**堆叠而非
      分散**——4 个 RWX pod 全落 node-04,只能临时 `disabled=1` 禁用 node-04 才逼出跨节点。
      这与租户有几个虚拟节点无关
- [x] **StorageClass 目录(`283a743`,方案来自用户对照 CPO 的分析)**:SC=目录项,
      `provisioner` 定服务(`cinder.knaas.io`/`manila.knaas.io`),`parameters.type`/
      `parameters.share_type` 定档位;租户 `get sc` 一眼知道选什么,档位可见可计费。
      类型钉死服务:块设备类 + RWX **拒绝并说明**,不静默给出类没承诺的东西;
      旧 `knaas` 类保留按 accessModes 推断的老行为。网关前缀照 IngressClass 的教训处理
      (`111111-ceph-nvme` → 上游 `ceph-nvme`)。
      **实测**:`ceph-nvme` 类 → `ceph-nvme-test` 类型的卷;`nfs-share` 类 → `incus-nfs`
      share;badmix 拒绝文案正确。
      ⚠️ **否决了"只跑 CPO controllerplugin"**:external-provisioner 按 provisioner 名
      全集群认领,一个实例=一份跨全租户建卷的凭据(CPO cloud.conf 静态模型,kubetron
      记过同一问题);每租户一套则 provisioner 名裂开、共享目录不复存在。provision 半边
      留自研(三百行已测),缺的是目录不是机器
- [x] **SC 目录已经 kubezoo 发布**(用户打标签发布,租户 `get sc` 看到
      ceph/ceph-nvme/nfs-share 三条,名字干净无前缀)。生产环境照此按 control1 的
      8 个 public volume type 各建一条
- [x] **legacy 类已废(`b43d24e`)**:`<tenant>-knaas` 字符串匹配删除,目录是唯一入口。
      已绑定的两个老 PVC 祖父化(绑定/挂载/回收都不再查类);legacy 名字的新 claim
      停 Pending。⚠️ **纠错**:kubezoo 对 storageClassName **原样透传**(用户 grep 坐实,
      无任何前缀逻辑)——我此前从 IngressClass 的前缀行为错误外推;PVC 里的 `111111-`
      是当初测试时字面写进去的,不是网关加的。带前缀查找保留为容错,不再是依据
- [x] **卷扩容 Cinder + Manila 全部实现并端到端实测(2026-08-12,kubezun `c68311f` + fork `4cd39aa3`)**:
      ⚠️ 起点是**声明了但零实现**:`allowVolumeExpansion: true` 写在两个 Cinder 类上,
      而 `pkg/volume` 里扩容代码一行没有。**实测**:PVC 1Gi→2Gi 被 API 收下、90 秒后
      PVC 仍 1Gi、卷仍 1GiB、**一条 condition 都没有**(`Resizing` 由 resizer 设,本部署没有)。
      比"卡在 Resizing"更糟——**没有任何信号**。先撤回承诺,再实现,**实现完才重新声明**。
      **Manila**:extend share 即完,文件系统归文件服务器——pod 里 `df` 不做任何操作就变
      (1Gi→3Gi:share 1→3、`df` 974M→2.9G、数据在)。这就是 CPO 的 Manila node 侧
      只有三行 `Unimplemented` 的原因。
      **Cinder**:两步——extend 卷 + **os-brick `connector.extend_volume()` 重扫 +
      节点上 `resize2fs`**(实测 1Gi→3Gi:卷 1→3、**pod 里 `df` 973M→2.9G**、数据在)。
      借鉴来源见 CLAUDE.md:rescan 抄 nova-incus(os-brick 白送)、两段式与 microversion
      抄 CPO、fail-closed 尺寸校验抄 nova。
      ⚠️ **四个只有实测才撞得到的点**:
      ① **in-use 卷扩容要 microversion 3.42**,否则 Cinder 报 "status must be available",
      读起来像卷的状态问题而不是请求的版本问题;且要用**客户端副本**,别改共享的;
      ② **capsule 的卷挂在里面的容器上,不在 capsule 上**——按 capsule uuid 查一无所获;
      ③ **PVC status 必须等文件系统长完才写**:先写就等于宣布完成,reconciler 再也不回来,
      文件系统永远小。PV 容量可以立刻写(卷确实大了)。这条让重试自然发生;
      ④ ⚠️ **AZ 那次改动一直在把计算 AZ 送给存储服务**,而 OpenStack 有三个 AZ 名字空间
      (Nova/Cinder 都叫 `nova`,**Manila 是 `manila-zone-0`**)——**所有 RWX 供给从那次
      改动起就一直失败**,报 "No storage could be allocated",读起来像后端满了。
      已修:存储 AZ 只来自配置,绝不从计算 AZ 推
      ⚠️ **另有一处部署债**:四台的 `/opt/stack/zun` 各缺文件(控制面缺 3 个、每个计算
      节点缺 1 个),今天才因 `manila.py` 缺失撞出 zun-api 起不来。已全量同步 24 个改动文件
- [x] **AZ 拓扑 / WaitForFirstConsumer(2026-08-11,`21fc675`)**:卷不再在 PVC 创建时
      就开——**那时还没有任何东西决定 pod 去哪**,zone 只能从配置里猜。一个 AZ 时永远
      猜对;两个 AZ 时猜错**不可逆**:卷不能换 AZ,PVC 一直 Bound、pod 一直 Pending,
      两个对象都不说为什么。
      ⚠️ **WFFC 不是 CSI 的东西**(已查证):调度器的 volume binding 插件按
      StorageClass 的 `volumeBindingMode` 给**任何**认领该类的供给者打
      `volume.kubernetes.io/selected-node`(`binder.go:450`),CSI 只在"发布容量"
      那一处出现(`:984`,注释明写"要么不是 CSI driver")。**实测:调度器确实给我们
      这个非 CSI 供给者打了注解。**
      ⚠️ **它给的是虚拟节点,而 capsule 落哪台机器由 Zun 定**——这恰好够用:卷属于
      AZ 不属于主机,K8s 看不见的那一半正是对卷不重要的那一半。
      ⚠️ **两个 zone 名不是一个名字**:节点对 K8s 报 `az1`、向 OpenStack 要 `nova`;
      把前者交给 Cinder 等于要一个不存在的 AZ。这对名字**只在设置它们的启动参数里
      同时出现**,映射就从那里读。
      PV 还写了 zone 亲和——否则位置只决定一次就忘了,pod 删了重建可能被调到别的 zone,
      而卷跟不过去。
      ⚠️ **实测证明了决策,没证明映射**:实验床只有一个 Cinder AZ,"从节点解析出 nova"
      和"留空让 Cinder 自己选"产生同一个卷。单元测试区分了两者;日志行记录的是
      **问 Cinder 之前**我们决定的值。
      ⚠️ **`volumeBindingMode` 不可变**:`apply` 覆盖会失败,类必须删了重建
      (已 Bound 的 PVC/PV 不受影响——绑定不再读类;但当时正 Pending 的 claim 会失去
      它在等的类,所以要挑没有等待者的时候做)
- [ ] 开通清单新增:`KUBEZUN_VOLUME_TYPE`(必须映射到在跑的后端——默认类型指向没跑的
      lvmdriver-1 时卷直接 error)、`KUBEZUN_SHARE_TYPE`、计算节点 ceph-common+配置、
      nfs-common、RBAC pvc(读)/pv(写删)

## 双驱动：Container API 跑在 CRI 上（2026-08-11，定案见 fork `FORK.md` §4）

**为什么**：zun-ui / Horizon 那条"容器即虚机 + 终端"的产品线服务不懂 K8s 的用户，
不该因为我们把重心放在 capsule 上就消失。而 `container_driver = docker` 虽然零开发，
代价是两套镜像存储、两套 kata sandbox、VMM 各起各的、**资源账分裂**——CRI 的
`ListContainerStats` 看不见 moby 里的容器，计费就少一半数据。

- [x] **CriDriver 实现 ContainerDriver**（`b2f9af06` / `54beaf58` / `a5de1b94`）。
      在 CRI 上一个"容器"就是只有一个容器的 capsule——不是复用代码的技巧，是 CRI
      **没有 sandbox 之外的容器**这个概念。
      已实现并实测：create/delete/start/stop/show/list/attach/镜像 +
      reboot/update/stats/top + pause/unpause/带信号的 kill。
- [x] **契约测试**：按 **zun-ui 实际调用的 19 个方法**（`zun_ui/api/client.py`）打，
      不是 api-ref 的 28 个端点；凭据用租户 appcred（admin 令牌会让 `list` 失去意义）。
      23 项 4 失败，全部预期内（2 项做不到 + 2 项租户策略 403）。
- [x] **DockerDriver 未被破坏**：文件整个 fork 期间零改动，分派机制没动。
      ⚠️ 但共享代码上踩过一次（`b460f222`）：为 CRI 改 wsproxy 时把 TLS 证书来源从
      `CONF.docker.*` 挪成构造参数、又无条件下发运行时的子协议，**两处只有 docker
      那条路会疼**，而我们不跑 dockerd 所以永远测不出来。现按 URL 判别对端。
      **改共享代码时要问的不是"我这条路对不对"，而是"另一条路还在不在"。**

### 做不到的（写在这里，别再当成"还没做"重提）

- **`resize`（tty 尺寸）**：尺寸**已经**能改（流内第五通道，`exec -it` 实测好使）。
  够不到的是**从流外面改**——REST 是另一条连接，proxy 没有会话表把它和开着的流对上。
  要做 = 给 wsproxy 加会话跟踪，架构改动。现在明确报错说明原因，不返回 500。
- **`network_attach` / `network_detach`**：沙箱的网络创建时定死。kata 内部有
  `Sandbox.AddInterface`（`virtcontainers/sandbox.go:1245`），但 **shim 管理接口
  不暴露它**，CRI 也不会对运行中的 sandbox 重跑 CNI。

### 语义不等价（产品要知道）

- ⚠️ **pause 不释放任何东西**。freezer cgroup，而且冻结发生在 **guest 内部**——
  宿主机毫不知情，VMM 照旧持有整块 guest RAM，Placement claim 一动不动。
  **UI 文案不能让它读起来像"暂停就不计费"。**
- ⚠️ **stop/start 与 reboot 丢掉可写层**（docker 的 restart 保得住）。地址保住了，
  因为地址属于沙箱。marker 文件实测。

### 未决

- [ ] **zun-ui 要求创建时 `interactive=true` 才显示终端**
      （`console.controller.js:42`）。要么 UI 默认勾上，要么 `get_websocket_url`
      不依赖该标志。**产品决定，不是技术问题。**
- [ ] **UI 上隐藏 pause / 窗口大小同步**，或给出明确说明——否则用户点了没反应。
- [ ] **计费：ceilometer central pollster**（~250-300 行，全部在 ceilometer 侧，
      Zun 不用动）。参照 `ceilometer/load_balancer/octavia.py`。数据源是 capsule
      stats，CPU 本来就是累计值，正好对上 cumulative 样本类型。
- [ ] ⚠️ **契约测试有真实偶发性**：有一轮失败的检查后面两轮原样重跑全过，中间什么
      都没改。**单独一轮全绿不算证据**，查偶发之前先确认自己看的是不是同一个东西。

## 阶段 3.5：共享节点形态（2026-08-13 新增，DESIGN §1.2 定案 / §4.6 落地面）

**放弃每租户虚拟节点。** 节点从 `∝ 租户数` 变成 `regions × K × AZs × archs`，与租户数无关。
保住的是唯一那条真差异——算力归属 OpenStack project。租户体验退回与 B1 一致
（`get no` 为空、无 DS）。

⚠️ **卡在前面的决定**（没定就别开工，它改变实现）：
- **有没有客户真要节点可见性 / DaemonSet** —— 决定这刀能不能砍

✅ **"K 取多少"不再是开工前提**（2026-08-13）：分片归属改成**声明式**之后（§2.1，抄
kubetron），K 可以先取一个小值、以后逐个租户迁进新分片。~~"K 一次定死"~~ 那条限制随
哈希取模一起作废——它是哈希的限制，不是分片本身的。

- [x] **（P0）凭据按 namespace 解析 —— 已做并实验床验收（2026-08-13）**：
      `pkg/tenant.Resolver`（namespace → tenant → Binding → Session），provider 走
      `Capsules` 接口（`For`/`Each`/`TenantOf`），四个 controller 走
      `ReconcilerFor`/`EachReconciler` 接缝（每租户一个 Reconciler 实例，包内零改动）。
      **`--platform-namespace` 未设时一切走 Static 路径，行为与单租户完全一致。**
      ⚠️ 过程中抓住并修掉三个会静默出事的缺陷：
      ① Each 跳过坏凭据租户 + sync 把"listing 缺席"读成"capsule 没了"→ 一次凭据抖动
      判死整租户 pod（与 cff9f8b 同形状，高一层）——Each 现在**报告覆盖了谁**，sync 只判
      covered 租户；测试先绿后发现没测到（trackPod 盖掉 StartTime 落进宽限期），改直塞
      p.pods 后验证过会红。
      ② 每租户 netpol reconciler 必须用**每租户的** ServesNamespace——共享进程级检查会让
      A 的地址组灌进 B 的 pod IP（B 的 pod 被 A 的策略放行）。
      ③ 每租户配置不止凭据：network-id / vip-subnet-id 也是租户的，随 Secret 注解走
      （`knaas.io/network-id` 等），provider 加 `NetworkIDFor`。
      **实验床验收（一个进程带 111111+222222，同一节点）**：
      111111 凭据（project 4fb711f8）只见 111111 pod 的 capsule；222222 凭据
      （project b0f233fd）只见 222222 pod 的 capsule；两 pod 均 1/1 Running、IP 来自
      各自网段（192.168.100.x / 110.x）——正向对照与隔离同时成立。
      ⚠️ 验收时判据又踩一次 `OS_CLOUD=devstack-admin` 覆盖 openrc 的坑（本会话第二次）。
      ⚠️ 原条目正文（含影响面清单）：
- [~] ~~（原文）凭据按 namespace 解析~~：`zunClient` 从**单值**变成
      `namespace → project → Secret → clients` 的解析器。
      - ⚠️ 影响面已量过：`main.go` 里 19 处引用、8 个子系统构造点（capsules / 块存储 /
        共享存储 / netpol / Octavia / Neutron / KeyManager / Subnets）
      - ✅ **namespace/授权侧不用改**：`--namespace-selector` 是标签选择器，
        写成 `kubezoo.io/tenant in (a,b,c)` 现在就能服务多租户；`Serves()` 按集合判定
      - ⚠️ `authorize(namespace)` 从"允不允许"升级为**同时选凭据**——选错 = 拿 A 的凭据
        操作 B 的资源。`vknode/namespaces.go:105-109` 的注释早就点名了这个后果，
        只是当时防的是启动竞态，现在落在日常主路径上
      - **验收判据**：一个进程服务两个租户，各自 capsule 落各自 project。
        ⚠️ 判据必须能分辨"落对了"和"两边都没建成"——要有正向证据，不能只看"没串"
- [x] **（P0）project 绑定三态校验 —— 已实现（2026-08-13，与解析器同体）**：
      `Resolver.checkBinding` 三态（无记录→写入 Secret 注解 / 一致→放行 / 不一致→拒绝并
      每租户报一次）；校验对象是 **token 里的 project id + region**，不是 Secret 哈希——
      同 project 轮换 appcred 实测放行。两个最自然的错误写法都验证过测试会红：
      "不一致就覆盖"与"比对凭据材料"。记录失败不致命（下次启动再记，warn 一条）。
      实验床上首连即写入注解（需要 knaas-system 里 secrets 的 get+patch Role）。
      ⚠️ 原条目正文：
- [~] ~~（原文）project 绑定三态校验~~：
      ```
      无记录         → 写入（首次绑定）
      有记录且一致   → 正常
      有记录且不一致 → fail closed，拒绝启动 + 报警
      ```
      - ⚠️ **校验对象是 token 里的 project id，不是 Secret 哈希/版本**——
        否则会顺手禁掉凭据轮换（同 project 换 appcred 必须允许）
      - ⚠️ **"不一致就覆盖"是错的，而它恰好是随手会写出来的那一版**
      - ⚠️ **位置必须在同步循环之前**：晚一步，第一个周期就已经把所有 pod 判成 `Failed`
        （`sync.go:91-99`），ReplicaSet 立刻建替补
      - **为什么必须做**：kubezun 现在**从不记录也从不校验自己认到了哪个 project**
        （全仓零处引用）。凭据一换：旧 project 的 capsule 全部不可见 → 全部 pod 判 Failed
        → 重建；而旧 capsule **继续跑、继续计费、永远不会被回收**（孤儿清扫用新凭据 list，
        看不见它们）；Service 还会**复制一份 LB**（`ensureLoadBalancer` 得到 NotFound
        就 fall through 新建，旧的继续持有 VIP）
- [x] ⭐ **（P0）节点补 `topology.kubernetes.io/region` —— 已做并实测（2026-08-13）**：
      节点标签实测变为 `region=RegionOne` + `zone=az1`（⚠️ VK 会更新已注册节点的标签，
      不存在陈旧标签的升级隐患）；新建 PV 的 nodeAffinity 实测为
      `[{matchExpressions:[region In RegionOne, zone In az1]}]` —— **一个 term 两条要求**，
      且**消费它的 pod 1/1 Running**（后半句才是判据：亲和写对 ≠ 能用，写错的亲和会让 pod
      永远 Pending 而两个对象都不说话）。region 取自 `zunClient.Region()` 而非新开 flag——
      一份凭据只解析一个 region 的端点，region 是进程属性，flag 可能与它不一致。
      单测三条并逐一验证过会红：漏 region、拆成两个 term（变成 OR）、空 Placement 产生
      匹配一切的空 term。原条目正文：
- [~] ~~（P1 原文）节点补 `topology.kubernetes.io/region`~~ + PV nodeAffinity
      加一条 MatchExpression（§3.1）。⚠️ **必须先于第二个 region 上线**——今天 PV 只按
      zone 匹配（`volume/reconciler.go:632-634`），多 region 下 `r1/az1` 与 `r2/az1` 会撞，
      症状是 `reconciler.go:623-627` 注释描述的那个：**claim Bound、pod Pending、
      两个对象都不说为什么**。改动很小，但**晚做就是静默错配**。
      ⭐ **2026-08-13 上调理由**：多 region 不是"可能会有"，是**逻辑流轴的必然结果**——
      OVN 的分片单位就是 region（AZ 只是 Chassis 行上 `ovn-cms-options` 的一个字符串，
      `neutron/common/ovn/utils.py:911-923`，同 region 所有 AZ 共用一套 NB/SB），
      而 NetworkPolicy 正把我们推向那堵墙（§7.4.1/§7.4.2）
- [x] **（P0）绑定改成 `namespace → (project, region)` —— 已做（随解析器一并完成，
      de5b11c）**：`Resolver.checkBinding` 的 `want` 同时含 `ProjectAnnotation` 与
      `RegionAnnotation`（`resolver.go:230-233`），三态校验对两者都生效。
      Keystone 的 project 是全局的、可跨 region 有资源，而**卷与网络不跨 region**。
      ⚠️ 只记 project 的失败形态是"凭据对、region 错"：`Credentials.Region` 解析出另一个
      region 的端点 → 网络 ID 找不到、卷挂不上，而**两个字段单看都是对的**
- [x] **（P1）节点身份去租户化 —— 已做（2026-08-13）**：`--shard`（与 `--tenant` 互斥的
      两种部署形态）：设 shard 时节点带 `knaas.io/shard` 标签 + `knaas.io/serverless=true`
      污点、不打 pool；`--tenant` 形态**原样保留**——实验床还在跑每租户单元、kubezoo 还在
      注 pool selector，直接改身份会断现有部署。名字四坐标是约定不是代码强制
- [x] **（P1）容量改静态大额 —— 已做（2026-08-13）**：`--platform-namespace` 模式下
      capacity flags 未设（"0"）时默认 cpu=1000/mem=4Ti/pods=10000——零容量的节点谁都
      调度不上去。真把关 = ResourceQuota 准入 + Zun project 配额（§3.2）
- [ ] **（P1）容量改静态大额**（§3.2）：`capacity = 配额镜像` 作废——共享节点镜像不了任一
      租户的配额。把关落到 K8s ResourceQuota 准入 + Zun project 配额两道闸门
- [x] **（P1）informer 收窄 —— 组件与四个 controller 全部接线完成（2026-08-13/14）**：
      ⚠️ **棋盘卫生笔记**：下面的记录早就写着"已接线"，行首勾选符一直没跟着翻——
      2026-08-14 巡查连同 P0 绑定项一起补，两条都是完工漏打勾，不是遗留工作。
      `vknode.ScopedFactories`：每 served namespace 一个官方单 namespace factory，
      六类 namespaced 对象（services/slices/pods/policies/ingresses/claims），
      fan-out lister 实现标准接口（消费方零改动），handler 订阅覆盖后加入的 namespace，
      HasSynced 闩锁（onboard 不翻 false——第一版被自己测试抓过）。
      ⚠️ **草图方案已证伪并更正**："per-ns reflector 喂共享 Indexer"不行——Reflector 的
      Replace 契约是"这是全部"，每个 ns 一 list 就清掉别人的对象。独立 factory + 读侧
      聚合让"移除即清理"结构化成立，无需清理步骤。
      **已接线**：Service controller（`NewControllerFromSource`，实验床验证过，还顺带
      抓到跨租户 VIP 子网回退漏洞）。
      **已接线（2026-08-13）**：netpol / ingress / volume 三个 FromSource 构造器 + main
      条件装配（mt 分支从头不碰 `set.XInformer()` 访问器——调用即物化）。netpol 的
      handler 提取为 podHandler/policyHandler 供两个构造器共用，防双份漂移。
      PV/StorageClass 集群域保持 informer。实验床 mt 单元：零 panic、service 路径验证过；
      ✅ **claims 路径完整回归已做（2026-08-13，mt 单元实测）**：WFFC PVC + 消费 pod →
      Bound + 1/1 Running;判据全部带出处——PV 注解 `knaas.io/storage-kind=cinder`（证明是
      我们的 reconciler 经 scoped claims 供给）、卷名 `kubezun_111111_pvc_…` 且 111111
      凭据可见（project 归属）、PV nodeAffinity 单 term 含 region+zone、写入 take3 回读
      成功。删除闭环:PVC 删除 → PV Released（观察到），卷回收归周期清扫（与 LB GC 同
      恢复模型），异步完成。
      ⚠️ 过程中两次差点误判:① reader 撞上卷异步 detach 的 `volume-in-use`（竞态非缺陷，
      VK 退避重建）;② 无 fsGroup 时 Permission denied 起初当成 bug——**是标准 K8s 语义**
      （无 fsGroup 即无 chown），加 fsGroup 即通。
      ⚠️ **接线时的坑已查明**：svcRec 构造处无条件调 `set.XInformer().Lister()` 会**物化**
      集群级 informer（调用即创建，Start 就会启动它）——mt 分支必须从头就不碰那些访问器，
      否则收窄名存实亡。PV/StorageClass 是集群域资源，天然不收窄。
      （原条目正文含实测数字：）
      `vknode.go:143` 的 `scmFactory` 无任何过滤，八类对象全集群缓存
      （services / endpointslices / ingresses / networkpolicies / **allPods** /
      pvc / pv / storageclasses）。
      - **与 §2 自己的论证矛盾**：`ObjectReader` 特意不做 lister，注释写着"a cache wide
        enough to answer for every namespace the tenant may create is also wide enough to
        answer for another tenant's"——同一个担忧，ConfigMap/Secret 严防死守，
        这八类却照单全收
      - **实测（2026-08-13，仅 2 个租户）**：全集群 67 个 pod，属于 111111 的只有 11 个；
        Services 26 / 6。**两个租户时就已经 6 倍过取**
      - ⚠️ **分片形态下会从"浪费"变成"抵消"**：`K × 集群规模` 而非 `集群规模`。
        分片本来是为了省，这样反而更贵
      - **修法**：⚠️ **2026-08-13 查证：服务端标签过滤这条捷径不存在**——实测 pod/svc
        上没有任何租户标签（kubezoo 不打），而 informer 无法按"namespace 的标签"过滤
        对象。唯一路径 = **动态 per-namespace informer 工厂 + 聚合 lister**：
        `vknode.Namespaces.OnChange` 已有启停机制（per-node pod 工厂就是这么做的），
        缺的是跨 namespace 的聚合 Lister 实现。是一件完整的中型工程，勿顺手做。
        **实现草图（2026-08-13 定，下次直接照此开工）**：
        ① 每 kind 一个**共享 `cache.Indexer`**（`MetaNamespaceIndexFunc`），标准 lister
           （`corev1listers.NewServiceLister(indexer)` 等）直接包它——**消费方零改动**，
           这是整个方案成立的关键；
        ② 每 (namespace, kind) 一个 reflector（`cache.NewReflector` + 按 namespace 的
           ListWatch），全部喂同一个 indexer；
        ③ `Namespaces.OnChange` 启停 reflector；⚠️ **namespace 移除时必须清掉 indexer
           里它的对象**——reflector 停了不会自己清，残留对象 = 已退租租户的 pod 继续
           出现在 peer 集合里；
        ④ ⚠️ `HasSynced` 语义要自己定义：动态集合下"synced"= 当前已知的每个 namespace
           的首次 list 都完成。⚠️ 新 namespace 加入时不得把全局 HasSynced 翻回 false——
           那会让等它的控制器全部卡住；
        ⑤ 顺序：先 `services`+`slices`（Service controller 最吃扇出），跑通再推其余六类
      - ⚠️ **`allPods` 是例外，只能收窄到分片、不能到单租户**——它的注释写明
        "A policy's peers are pods wherever they run"
- [ ] **（P2）分片装配：声明式归属**（§2.1，抄 kubetron
      `pkg/webhook/claim_webhook.go:15-27` + `NamespaceShard()`）。
      ⚠️ **粒度必须是租户，不是 namespace**——按 namespace 分配会把同租户的
      `<tid>-default` 与 `<tid>-kube-system` 分到不同进程，**当场违反 §7.7.5c**。
      这一条正是**不能整段照抄 kubetron** 的地方：它的 `NamespaceShard()` 就是按 namespace
      解析的（对它成立，因为它的分片轴是 AZ 且没有跨 namespace 求并集的地址组）
      ⚠️ **归属判定只能有一套**——kubetron 有两套（claim 创建时打标 vs Service reconciler
      每次读 ConfigMap），同一 namespace 可能"claim 归 A、Service 归 B"，那**就是**双所有者。
      我们只存一处：Tenant CRD，与 project id 同处
      ⚠️ **迁移仍必须"先停旧、再起新"**，声明式只是把单位从"全体"缩到"一个租户"
- [ ] **（P2）systemd 单元实例名** 从租户改为分片（`kubezun@<region>-<shard>`）

## 阶段 4：生产化（§12）

- [x] Zun capsule 容器名 minLength=2，K8s 允许单字符容器名 → 租户写 `name: c` 得到一个
      看不懂的 400。已在 fork 侧放宽 schema（`6c2d7404`，minLength 与 pattern 都要改：
      原 pattern `^[a-zA-Z0-9][a-zA-Z0-9_.-]+$` 自带第二字符要求，只改 minLength 不够）
- [x] kubectl logs / exec 已通（板子上这条一直是过期的，2026-08-10 核实）：fork 侧
      `capsules.py` 的 `logs`/`execute` 端点在，kubezun 侧 `GetContainerLogs`/
      `RunInContainer` 对接完毕，**本轮排障全程在用**（ingtest 的 curl 都是 kubectl exec）
- [x] **logs `--follow` 已支持（2026-08-10，`ebcbb12`）**——不需要流式端点：
      运行时给每行都写了 RFC3339Nano 时间戳，那就是游标。始终向 Zun 要带时间戳的
      输出，记住最后一行的时间戳，下一轮只发它之后的，再按调用方意愿把时间戳去掉。
      **重复不是"不太可能"而是不可能**。实测：每 2 秒一行的 pod，连续输出、零重复行
- [x] **exec `-t`（交互终端）已支持**（2026-08-11，kubezun `3d83658` + fork `2de1c48d`）：
      走 Zun 原生模式。CRI 的 `Exec` 返回运行时自己流式服务器的 URL，那个服务器
      本来就说 `v4.channel.k8s.io`（五通道：stdin/stdout/stderr/error/resize）。
      ⚠️ 它监听 `127.0.0.1:随机端口` **且自身无认证**——URL 里的一次性 token 就是全部
      凭据，所以不暴露它：`zun-wsproxy` 跑在每台计算节点上，对外只给 token 认证的
      websocket。实测 `stty size` 随窗口变化（30 100 → 55 200）。
      ⚠️ **代理地址必须由承载节点自报**——API 主机配置里的那个 proxy 什么都不服务
- [ ] Barbican KMS：barbican-kms-plugin（CPO 现成）做 etcd 加密后端（§8.1）。
      ⚠️ **与 kubetron 的 Barbican 是两回事**（2026-08-07 查证）：kubetron 的
      `pkg/ingress/barbican.go` 是把 K8s TLS Secret 镜像成 PKCS12 供 Octavia 做
      **Ingress L7 TLS 终止**；本项是 etcd 静态加密的 KMS provider。同名不同物
- [ ] Barbican secret ref 注入（对接 fork，替代 Secret 明文过 Zun DB）（§8.1）
- [ ] capsule 预热池：kubetron NetworkPortPool 水位模型作蓝本，"预绑 host_id"换"预建
      capsule"，对标 kata 冷启动数十秒 → 秒级（§11 P3）
- [ ] 心跳调优：lease 间隔放宽——⚠️ **上限 50 秒不是 60**。`nodeMonitorGracePeriod` 是 KCM
      **全局** flag、默认 50s、无按节点粒度（`nodelifecycle/config/v1alpha1/defaults.go:46`），
      续约间隔超过它每个 VK 节点**永久 NotReady**；要更长得抬全局值，而它同时管着 B1 的
      真实 kata 池。**旧文写的"30–60s"横跨这堵墙**（§3.5）
- [~] ~~DS 扇出进计费模型；节点数按套餐限额~~ —— **两项均废弃**（§3.5/§9）：DS 没了，
      节点也不再是租户资产、无从按套餐限起
- [ ] **规模实测**（§3.5.1，⚠️ **2026-08-13 改了轴**）——旧写法是"量单集群虚拟节点天花板"，
      **那是量错了对象**：B2' 集群没有 worker，节点只有 `regions × K × AZs × archs`
      几十到几百个，根本碰不到墙；先碰到的是 pod 和租户。按这个顺序量：
      1. **单进程 pod 承载量**（**这个数直接定 K**）：`syncLoop`（`sync.go:32`）是周期性
         O(pods) 全扫，在多少 pod 时开始跟不上 `syncInterval`？
         ⚠️ **不需要 kwok**——这是我们自己进程的性能，实验床上灌 pod 就能量，
         比搭规模实验室便宜得多
      2. **单集群 pod / 租户容量**：etcd 对象数、watch 扇出、apiserver 负载
         （这一条用 `/root/kwok-scale-lab`）
      3. 节点数曲线仍要量，但**次要，且大概率富余很多**
- [ ] **验收**：规模实测有结论；**单分片 VK 崩溃只影响该分片**（故障注入，⚠️ 判据要能
      分辨"其他分片正常"和"故障注入没生效"）；kubectl logs 可用（§12）

---

## F：Zun fork 工作流（独立推进，§10）

fork 仓库已就位：`/root/k8s-zun-provider/openstack/zun` = `github.com/fivetime/openstack-zun`
（master 基点 e79265e8，与 origin 同步）。**直接在 master 上做，不要开 feature 分支**
（2026-08-08 定，见 CLAUDE.md：六条分支合回时两条已被主线重做取代，只换来冲突；
这是一条线性工作且只整体部署）。维护边界先立：docker driver / kuryr_network /
Container API 划为不维护区；主干 = capsule + CRI + zun-cni。

⚠️ **部署配对约束(2026-08-21 核实)**:另一工作线的 13 个提交带 3 个 alembic
migration(a1f5be2c7d90→b7c04e19af32→c3d17e5b8f42,flavor 限额/卷 IO/swap 改名),
**开发云两边都还没上**——DB 停在上游 head 3f2b36231bee(列级核过,无混合态,现状
无损)。**下次把 fork 主干整体同步到 /opt/stack/zun 时必须同窗跑
`/opt/stack/data/venv/bin/zun-db-manage upgrade`**:新 models 引用
container.swap/pids_limit 等不存在的列,新代码配旧库在 create/show 路径直接
OperationalError;反向(先升库后升代码)安全。注意 c3d17e5b8f42 会先清空
memory_swap 列再改名,不可逆(当前列还不存在,无实际损失)。

- [x] **容器退出码保真 —— 已修并 E2E 验证（2026-08-13，fork + kubezun 双侧）**：
      根因两层——CRI `ListContainers` 响应**本来就没有 exit_code 字段**（fork 只拿 state，
      EXITED→STOPPED 丢码），kubezun 只能按状态名猜（Stopped→0）。修法：fork 在 EXITED
      时补一次 `ContainerStatus` 调用，把码记进 `status_detail`（`exit:<code>`，容器再次
      RUNNING 时清除防止旧死状态套新生）；kubezun 解析它，reason 跟码走（≠0→Error），
      `PhaseOf` 让 Stopped capsule + 非零容器 = **PodFailed**。
      **E2E**：`exit 42` → `Failed/Error/exitCode:42`（修前 `Succeeded/Completed/0`）；
      正向对照 `exit 0` → `Succeeded/Completed/0` 不变。
      ⚠️ 状态调用失败时 detail 留空而非猜测——"没被告知"与"退出 0"保持可分辨，
      降级路径回落旧启发式。~~⚠️ 遗留小项：`startedAt` 仍 null~~——**已修（2026-08-21,三跳全查清）**:
      丢失点不止序列化端,是**三跳独立缺口**,修任何一跳都看不见效果:
      ① fork API view `_basic_keys` 没列 `started_at`(有值也被剥);② CRI 驱动只在
      EXITED 分支写(RUNNING 的容器 DB 里就是 NULL);③ kubezun 的
      `statusFingerprint` 不含时间戳(值到了也判"没变化"不推送——顺带发现
      terminated 只比 Reason 不比 exitCode,`exit 1→2` 同为 Error 也不传播,一起修)。
      fork `24b33540` + kubezun `9831439`,三租户实测 Running/Stopped 均出值
- [x] **反亲和 —— 已实现并实测（2026-08-14,fork + kubezun 双侧,平台默认启用）**：
      fork:weigher 种子框架(`scheduler/weights/`,排序永不过滤 → 软性是构造保证,
      配置删不掉)+ `OwnerAntiAffinityWeigher`(每台已有同 owner capsule 计 -1,
      每次调度一次 listing 缓存在 context 上);kubezun:capsule 打
      `knaas.io/owner-uid`(pod 的 controller ownerReference UID,单测钉住
      "副本同组、裸 pod 无组")。
      ⚠️ **首次部署静默无效,根因值得记**:`Container.list` 返回的行 labels 全空——
      capsule 的模板 labels 只在 capsule 行上,必须 `Capsule.list`。症状是"部署了、
      调度照旧",没有任何报错——判据(三副本分布)是唯一能看出它没生效的东西。
      **验收①(强)**:keeper 3 副本滚动后 **04/05/06 各一台**。
      **验收②(软性)——2026-08-21 补测完成,证据齐**(方案改为"副本数>主机数",
      不必停服务,且证据在清理前先落盘):5 副本 3 主机 → **5/5 全 Running、零
      NoValidHost**,超出的副本回落已用主机(04×3/06×2)。
      **验收③(weigher 直接观测,补测时白捡)**:计数 3/0/2 时串行扩容第 6 副本
      → **精确落在计数为 0 的 05**。⚠️ 顺带记一条固有行为:并发突发(5 副本一次
      建)时决策互相看不见(capsule 行还没带 host),散布可能不均(实测 3/0/2)——
      这是软排序的设计属性不是缺陷,串行/滚动创建则散布精确。
      ⚠️ 旧 capsule 无 owner 标签,滚动/重建后才参与散布——存量不迁移,自然换代。
      原条目正文:
- [~] ~~（原文）反亲和（平台默认启用，DESIGN §4.5）~~——⚠️ 这不是新能力，是**现行为主动反 HA**：
      实测 8/8 capsule 全落 `incus-node-04`，含 3 副本 StatefulSet（keeper-0/1/2）和
      2 副本 Deployment（coredns）；判据已排除"只有一台可用"——三个 `zun-compute` 全部
      `up`、同 AZ、无 disabled，node-05/06 的 Placement 用量 `0 0 0` 完全空着。
      根因：`scheduler/filter_scheduler.py:75-105` 在 `_get_filtered_hosts` 之后
      **不排序**（注释说 "sorted list" 但无任何 sort，是从 Nova 抄来的死话），
      取第一个 claim 成功的主机 → 副本堆到装满为止。
      **两侧都要动，缺一边静默无效**：
      - Zun 侧：先补 weigher 框架（`scheduler/loadables.py` 就是 Nova 那套通用加载器，
        `base_filters.py:58` 已在用；加 `base_weights.py` + `weights/` 是同模式再走一遍），
        再写按 owner 分组的反亲和权重。⚠️ **必须是软的**——`filters/` 是硬判定，返回空
        即 `NoValidHost`，单机实验床上第二个副本就调度失败
      - kubezun 侧：capsule 补 owner 标签（ownerReference 根 UID）。⚠️ **`pod-name` 不能用**
        ——keeper-0/1/2 名字互不相同，要的是它们共同的 owner；现有标签
        （`template.go:16-26`）里没有任何一个能分组
      - 验收判据：3 副本 StatefulSet 落在 ≥2 台计算节点上；**且**把可用计算节点缩到 1 台时
        仍能调度成功（证明是软的，不是把 HA 换成了调度失败）

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
- [ ] 同 owner capsule 反亲和——**不再是候选，已定为平台默认启用**（2026-08-13，DESIGN §4.5）。
      条目正文见本文件 F 段第一项：实测 8/8 capsule 堆在一台，两个计算节点全空
- [ ] （候选，已降级）CRI socket 可配置：cri/driver.py:44-45 硬编码改 conf 选项——
      2026-08-06 运行时分家（kubelet→CRI-O）后默认 socket 即 Zun 专属，仅当某环境
      kubelet 必须保留 containerd 时才需要（DESIGN §7）
- [ ] （候选）nets/固定 IP 按 PoC 结论补齐
- [ ] （顺手）linux_net.py ovs-vsctl → ovsdb（上游自己的 TODO，linux_net.py:48）

## R:rootfs 写放大防线(2026-08-21,四路核实后立项——背景:租户任意路径写入不得写爆宿主机)

- [ ] **P1 `/var/lib/containerd` 挪独立盘——核实后确认做不了原地挪,卡在虚拟化层**:
      三台开发节点(133-135)均为单 400G virtio 盘 VM,vda2 占满整盘、无 LVM、无备用
      设备;且共置比预想更糟——kubelet、CRI-O 存储(6-8G/台)、kata-fc thin-pool 的
      loop 文件全在同一个根 ext4 上。**要挪必须在 incus 虚拟化层给每台加一块虚拟盘**
      (要挪的数据 <1G 实块 + 20G 稀疏文件,挪本身是分钟级)——需要用户/平台侧供盘,
      加盘后顺带把 CRI-O 存储和 thin-pool 后备一起挪。
      ⚠️ 当前实际敞口:devmapper 路径(fc)有 20G 池顶;**overlayfs 路径(runc/
      kata-qemu/kata-clh,即现在所有 capsule)完全无界**,可直接写满根盘。
- [x] **P2a devmapper 档硬顶已生效(2026-08-21,用户升级 v2.3.4 + 配齐)**:
      三节点 containerd 升至 **v2.3.4**(带 erofs 丢层修复 #13876);devmapper
      插件三台全 ok,`base_image_size='8GB'` = fc 档 per-容器统一硬顶已上线。
      ⚠️ 顺带修掉一处 8-07 起的漂移:133 的 kata.toml 与它的 .bak 方向调换
      (好的躺在 .bak 里),devmapper 在 133 skip 了两周——50-kata.toml 把 fc 指向
      devmapper,等于 133 上 fc 档一直会创建失败,这次恢复后三台一致
      (md5 9bd05b,原文件留 .pre-restore)。**教训同"算哈希不看 git"**:
      插件状态(ok/skip)每台都要问,配置文件长得一样不等于生效状态一样。
- [ ] **P2b qemu/clh 档硬顶(现在所有 capsule 走的路,仍无界)——机制已核清**:
      ① **v2.3.4 就够做统一硬顶**:erofs snapshotter 的 `default_size` toml 项
      (v2.3.3 起就有,`blockMode: defaultSize > 0`)= 每容器定长块镜像,ENOSPC
      真硬顶;还差三步:装 erofs-utils(三台都没有 mkfs.erofs)→ 配
      `default_size` → qemu/clh 两 handler 切 snapshotter(已拉镜像会重新解包,
      切换窗口要留意)。内核模块齐(CONFIG_EROFS_FS=m + zstd)。
      per-snapshot **差异化**标签 `containerd.io/snapshot/max-size` 仍要等
      v2.4.0(f2b7791b,现只有 beta)——但那只服务"按套餐分档",防写爆不需要它。
      ② **CRI 协议对 Linux 没有 rootfs 大小字段**(`rootfs_size_in_bytes` 只在
      WindowsContainerResources),按租户差异化须走 fork 越界通道(同 socket 上
      snapshot 服务预建带标签的 snapshot,FORK.md §4.3.2 规矩内,但比 pause 重)。
      ③ 不等 ①② 的过渡:节点级统一上限(devmapper `base_image_size` 或 erofs
      `defaultSize`)+ 租户文档明说"大空间走 PVC"。

## O：租户开通清单（2026-08-13，给 222222 补齐时逐项踩出来的——开通控制器的规格）

> 背景:222222 名义上"已开通"（openrc + 单元 + 节点），但 **7 项缺失**让它的 coredns 留在
> B1、DNS 完全不可用、logs/exec 从未工作过。每一项都是实测踩到才发现的，这就是开通
> 控制器必须覆盖的清单（111111 当年手工做过、没记下来的部分）：

- [x] ① coredns Deployment 模板注 placement（pool selector + tenant toleration）——
      缺失 = coredns 落 B1 真实节点（Cilium IP），根本不在租户网里
- [x] ② 单元带 `--vip-subnet-id`/`--vip-network-id`（t2 的 VIP 网早建好了但从没接上）——
      缺失 = Service 全部无 LB，DNS 无从谈起
- [x] ③ 单元带 `--listen :1025X`（**每进程独立端口**）——缺失 = 两进程抢 :10250，
      输家的 kubelet API 静默死亡；且 **KubeletPort 上报要跟 listen 走**（发现 main 从未
      赋值，恒报 10250——已修，`kubeletPortOf`）
- [x] ④ TLS 三件套（tls.crt/tls.key/client-ca.crt，SAN=IP 可跨租户复用）——
      缺失 = kubelet API 不监听，logs/exec 对该租户从未工作，症状是 NotFound 不是拒绝
- [x] ⑤ `<sa>-auth-delegator` ClusterRoleBinding（system:auth-delegator）——
      缺失 = WebhookAuth 的 SubjectAccessReview 被拒
- [x] ⑥ 平台 Secret 注解（project/region/network-id/vip-*）——mt 形态的绑定
- [x] ⑦ ⭐ **东西向 SNAT 豁免**（本次最深的一坑，t1 修过但没记档）：Neutron 建的
      snat 行会把 pod→VIP 的东西向流量 SNAT 成外部 IP → LB 通路死。修法 =
      `address_set knaas_<t>_east_west = [pod子网, VIP子网]` + NAT 行
      `exempted_ext_ips` 指向它（生成 lr_out_snat priority-27 豁免流）。
      缺失 = Service/DNS 全部超时，而 NB 拓扑与工作租户**逐字段一致**——只有 SB 的
      lr_out_snat 流能看出差异。⚠️ 排障中三次判据坑：ovn-trace 的 ct_lb 默认不选后端
      （分辨不出 DNAT）、busybox nslookup 对 NXDOMAIN 返回非零、
      `port list -c security_group_ids` 列名无效静默为空
- [x] ⓪ **平台一次性:SG 缺省规则模板对齐 CNI 全开语义(2026-08-13 定案)**——
      default 组:CIDR 双向全开(⚠️ ingress 用显式 CIDR 不用 remote_group——LB
      hairpin/SNAT 后源地址可为 VIP/外部 IP,不在任何组里,remote_group 会拒,
      t2 DNS 即死于此);自建组:**只注入 egress 两条**,去掉 PARENT ingress
      (它是唯一带 remote_group 的注入,会留在 knp-allow-ingress 组里踩 §13 规模轴;
      自建组=显式白名单管理,与 K8s"写策略才隔离"同构;SG 纯叠加,default+空组仍全开)。
      ⚠️ 模板只对新建组生效,存量租户 default SG 要手工补 ingress 全开并清 remote_group
      规则。完整命令与注释已交用户笔记。kubezun 侧防御已验证:deny-all 无条件清空、
      baseline 删反方向、EnsureRuleSet 删外来规则(`neutron.go:273-279`)
- [x] ⑧ **333333 完整开通 —— 已做并验收（2026-08-13,首次全清单执行,零返工路径可复制）**：
      project `knaas-t3`(ac7ca689)/user+member/appcred;t3-net `192.168.120.0/24` +
      t3-vip `220.0/24`;t3-router 挂外网+双接口;**⑦ SNAT 豁免照 t2 定式**
      (删 VIP 子网 snat + `knaas_t3_east_west` 豁免集);单元照 222222 抄
      (`--listen :10252`,证书复用 SAN=IP);SA+两条 ClusterRoleBinding;
      knaas-system Secret+注解;coredns placement。
      **验收**:LB `ACTIVE ONLINE`;capsule 内 nslookup 由 `192.168.220.63`(自己的
      kube-dns VIP)答出;logs 可读(即③④⑤通);出网 OK。
      ⚠️ 一个执行坑:SA kubeconfig 手写 YAML 两次被格式咬
      (flow-style 里 URL 冒号/源文件格式与 grep 假设不符)——**用
      `kubectl config view -o jsonpath` 读值,写块式,起服务前先
      `kubectl --kubeconfig=... get` 自验**,这条并入开通控制器规格

## P：平台侧配套（代码不在本仓库）

> ⭐ **2026-08-13：需求已成文** → [`docs/requirements-platform-cn.md`](docs/requirements-platform-cn.md)
> ——R1 placement 分叉（含无限期 toleration 的语义论证）/ R2 NodePoolFor 三处同动 /
> R3 节点不可见确认 / R4 档位标签 / R5 NetworkPolicy 准入拒绝。每项带证据与
> 可分辨的验收判据；R1–R3 与 kubezun 形态切换同窗口，R4/R5 独立。
> **kubezoo 团队照它做即可，下面的旧条目为历史语境保留。**

- [ ] **开通控制器：落点定在我们这边，不在 kubezoo（2026-08-08 定）**。
      理由：建 Keystone project + appcred 要 **Keystone admin**，
      放进 kubezoo 就等于让所有租户的前门持有 OpenStack admin——
      它被攻破的爆炸半径现在止步于 K8s，加上这个就直通每个租户的 OpenStack 资源。
      次要理由：B1 租户根本不需要 OpenStack，他们代码里要为我们这档加分支；
      我们每改一次开通步骤要等他们发版。
      **分工**：Tenant 对象做触发器，kubezoo 只加一个标签
      （`knaas.io/compute: capsule`，不改 CRD、不改代码，B1 不打）；
      我们 watch Tenant，见到标签就做全套 project → appcred → 网络/子网/路由 →
      Secret → 拉起 VK → 状态写回 Tenant 注解。**凭据从头到尾不进 kubezoo 进程**。
      ✅ 顺带解决了开通的时序：CoreDNS 可以先建，Pending 到虚拟节点出现，自愈，无需协调
      ⚠️ 命名核对过：环境上的项目叫 `knaas-t1`/`knaas-t2`，不是 `knaas-<租户id>`
- [ ] Tenant CRD 其余扩展：Kyverno 实例 / ResourceQuota（§11）。
      ⚠️ **不再包含"节点 spec / 每租户 VK Deployment"**——节点与进程都不再是租户资产（§1.2）
- [ ] **（新）Tenant CRD 记录 project id**（§4.6.3）——绑定的真相源。
      落点选 Tenant CRD 而非每个 namespace 打注解：**后者会漂移**
      （有的 namespace 认 A、有的认 B）。⚠️ **不能挂 Node 上**：Node 不再是每租户一个
- [ ] **（新）appcred Secret 写平台命名空间**（§4.6.2），⛔ **不是租户命名空间**。
      ⚠️ "让 kubezoo 视图过滤掉"不成立——Secret **不需要被看见就能被使用**：
      pod spec 引用即可，而该路径不经 kubezoo（`provider/files.go:35` 由 kubezun 用
      自己的凭据按名 GET）。放平台命名空间是**结构性**的：pod 只能挂自己命名空间的 Secret
- [ ] **（新）重绑运维流程**（§4.6.4）：`① 旧绑定下清空工作负载 → ② 核验旧 project 归零
      → ③ 才改绑定`。⚠️ **顺序反了会造成永久失联**——`provider.go:316` 把 Delete 的
      NotFound 当成功吞掉，用新凭据删旧 capsule 会 404 → K8s 侧干净删除 →
      **旧 capsule 继续跑，而最后一份记录它存在过的东西刚被删掉**
- [ ] **（新）kubezoo：`NodePoolFor(tenantID) = tenantID` 改造** + 节点从租户视图移除。
      ⚠️ 该函数注释写明**三处必须一致**（Kyverno 策略 / kubezoo 注入 / 手工打标）
- [~] ~~tenant-deny-daemonset 分档~~ → **两档统一 deny**（§9 废弃）
- [ ] **（新，产品侧确认）第三档与 B2' 是不是同一个 OpenStack project？**（§1.2 / §4.6.5）
      是的话，**基于安全组的 NetworkPolicy 执行对双档租户本来就是建议性的**——
      租户经 Horizon 持有 project 凭据即可改掉 kubezun 建的安全组。
      ⚠️ 实测确认可达性不是理论问题：**capsule 能连到 Keystone 并拿到真实 API 响应**。
      要么接受并写进租户文档，要么给两档分不同 project。**不要假装严密**
- [ ] kubetron 租户 DNS 分发通道改造：DNS 跑租户网内 capsule、控制器直推 zone
      （无 kubelet 挂 ConfigMap）（§7）
- [ ] kubetron M8 顺带：Service/DNS 编排半边可独立部署（kubezun-only 形态只拉编排层）（§7）
- [~] ~~kubezoo 层：InternalIP 展示值是否改写（§14.5）~~ —— **不再需要**：
      节点对租户不可见，管理网地址不会外露

## 待定项镜像（详见 DESIGN §14，清掉一项这里勾一项）

- [ ] §14.1 nets 可传递性（阶段 0 实测）
- [ ] §14.2 liveness 重启保 IP（阶段 0 实测）
- [ ] §14.3 kube-system pod 过 Kyverno（阶段 2 核查）
- [~] ~~§14.4 租户业务标签写自己节点~~ —— **失效**：租户没有"自己的节点"了（§1.2）
- [~] ~~§14.5 InternalIP 展示值改写~~ —— **失效**：节点对租户不可见，管理网地址不外露
- [ ] §14.6 SA token 长期轮换通道（等 F/ExecSync 落地）
- [ ] §14.7 单进程多节点 informer 共享形态（阶段 2 定）。
      ⚠️ 共享节点形态下这项**从优化变成必需**——一个进程要带 `K × AZs × archs` 个节点
- [ ] §14.8 PVC 供给流程（cinder-csi provision-only vs provider 直管；阶段 2 有状态负载需求时定）

---

## 工程卫生（2026-08-08 一次性收拢）

- [x] **两个仓库都收成单条 master，直接在 master 上开发**（kubezun `c20ca4e`，
      Zun fork `b212c3e2`）。此前 kubezun 的工作挂在 `feat/rewrite-provider` 上
      **72 个提交**、Zun fork 散成六条，结果是 "master" 这个词在两个仓库里
      都不再指代任何有意义的东西，"哪份是权威"每次都要重新查。
      Zun fork 六条里有两条**不是没合、是已被主线重做取代**，用 `-s ours` 记账后删除，
      历史仍可达、内容不回流。开分支前先想清楚它要跟谁并行——没有并行就不要开
- [x] **Zun 配置收进 `deploy/zun/`**（`4bf83b1`）：此前只存在于那几台机器的 `/etc/zun`，
      环境重建一次就没了。凭据全是占位符。记下来的都是丢了要重踩的：
      `default_cpu/default_memory = 0`（否则 BestEffort pod 被静默限到 512MB 然后
      OOM，而 K8s 侧看不到任何解释）、`[os_vif_ovs] ovsdb_connection` 的 socket 路径
      （默认值在容器化 OVS 下无人监听，报成 `binding_failed` 看着像 Neutron 的问题）
- [x] **二进制可追溯**（`vcs.modified=false` + revision 对上 master）：
      此前跑的是从 `0c2886b4` 加一堆**未提交改动**编出来的，追溯不到任何提交。
      现在 `go version -m /usr/local/bin/kubezun | grep vcs.` 一条命令回答。
      ⚠️ Makefile 不会因源码变化重编（目标无依赖），改代码后要 `rm -f bin/virtual-kubelet`
- [x] **README 重写 + `docs/bootstrap.md`**（`749cb3d`）：README 原来还是上游那个
      归档项目的，把**上游 Zun** 写成前置条件、描述一个已不存在的 provider。
      bootstrap 补上了此前只能靠看机器才知道的东西：一台计算节点上的两个运行时
      （kubelet→CRI-O / containerd 整实例专属 Zun，containerd 的 CRI 插件硬编码
      `k8s.io` namespace 所以不能共用）、kata 三 handler 的 drop-in 与开机重建
      thinpool 的 unit、Keystone 注册、租户的两个子网、Kyverno 选哪个标签。
      ⚠️ 验证命令实测改过一次：`openstack appcontainer` 在环境上不存在
      （zunclient 插件没装），改成直接打 `/v1/services`
- [ ] **配置与代码之外还差一步**：部署仍是手改文件 / scp 二进制，
      两边一致是**巧合不是机制**。让部署从一个 ref 出发，机器的身份就是一个 commit id
