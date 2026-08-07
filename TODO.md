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
- [ ] ⚠️ **capsule 状态同步缺口**：CriDriver 无 `update_containers_states`，capsule 状态
      停留在最后一次操作时的记录（fork 已留钩子，见 manager.py sync_container_state）。
      kubezun 的 GetPodStatus 依赖 Zun 侧状态真实性 → 阶段 1 前必须补
      （_show_container/_populate_container_state 是现成积木）→ 归入 F 工作流
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
- [ ] 孤儿 capsule 治理：provider 停机期间删除的 pod 会留下 capsule（实测残留 6 个）
      → ListManaged 已能按 owner label 识别，补一个启动时对账即可（§5）

---

## 阶段 2：多租户（§12）

- [ ] 每租户 VK Deployment 部署物（manifest/chart）：per-tenant SA、per-node :10250 +
      证书 + WebhookAuth(nodeName)（§2）
- [ ] 同租户单进程多节点：共享 informer（pod watch 按 nodeName fieldSelector 合并）
      （§2/§14.7）
- [ ] ⚠️ ConfigMap/Secret **按对象 GET**（不用集群级 informer——namespace 级 Role 下
      nodeutil 默认 informer 会 403，(VK) controller.go:329-346）；env/文件合成进 capsule
      （DESIGN 优先级 P2，租户真实负载需要则此阶段落地）（§8.1）
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
- [ ] **（门槛，卡阶段 3）ExecSync + liveness 重启**：stub 现成
      （(Zun) api_pb2_grpc.py:100-103），zun-compute 落实 capsule 内执行 + restart 语义
- [ ] **（门槛，卡阶段 4 logs）logs**：CRI 补 log_directory/log_path
      （cri/driver.py:89-94,181-192）+ 新增 GET /capsules/{id}/logs
      （capsules.py:111-113 _custom_actions 为空）
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
