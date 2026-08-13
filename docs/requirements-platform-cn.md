# KNaaS 共享节点形态:平台侧(kubezoo)配合需求

2026-08-13。背景:kubezun 已切换为共享虚拟节点形态(DESIGN §1.2 定案、§4.6 落地面),
kubezun 侧工程已完成并实验床验收(一个进程带两个租户,capsule 各落各 project)。
**正式切换卡在下面几项 kubezoo 侧改动上**——本文档是完整需求,每项带证据与验收判据,
kubezun 侧不再有未查证的依赖。

约定:R1–R3 必须与 kubezun 的形态切换**同一窗口上线**(它们互为前提);R4、R5 独立。

---

## R1 placement 注入按租户档位分叉(核心项)

**现状**:`place()`(kubezoo-gateway `pkg/convert/placement.go:130-168`)对每个租户 pod
无条件注入:

- `nodeSelector: {kubezoo.io/pool: <tid>}`(整体替换)
- tolerations 整体替换为三条:pool 污点 + not-ready/unreachable 各 300s
- `schedulerName: default-scheduler`;`Affinity`/`TopologySpreadConstraints` 置 nil

**要求**:按租户档位分叉。KNaaS 档(判定方式见 R4)注入改为:

```yaml
nodeSelector:
  node.kubernetes.io/instance-type: knaas.serverless   # 共享节点标识(kubezun pkg/node 已上报)
tolerations:
- {key: knaas.io/serverless, operator: Equal, value: "true", effect: NoSchedule}
- {key: node.kubernetes.io/not-ready,    operator: Exists, effect: NoExecute}   # ⚠️ 无 tolerationSeconds
- {key: node.kubernetes.io/unreachable,  operator: Exists, effect: NoExecute}   # ⚠️ 无 tolerationSeconds
```

B1 档维持现状不动。

**⚠️ not-ready/unreachable 必须是无限期,不是 300s**——这不是偏好,是查证过的语义:
虚拟节点 NotReady = VK 进程不可用,capsule 在 Zun 里**照常运行**(探针与 liveness 重启由
zun-compute 自身周期任务执行,`zun/compute/manager.py:1393`,不经控制面)。300s 后驱逐的
净效果是:删掉活着的负载 → 替补 pod 落回**同一个**不可用节点 → Pending。纯损失。
驱逐换不来迁移,因为 selector 钉死在同一类节点上。

**验收判据**(⚠️ 要能分辨"对了"和"没生效"):

1. KNaaS 租户建一个裸 Deployment → pod 落在 `knaas-*` 共享节点上 1/1 Running(正向);
2. 同一 pod 的 tolerations 里 not-ready/unreachable **没有** tolerationSeconds 字段;
3. B1 租户的 pod 注入结果与改造前逐字段相同(回归);
4. 停掉该分片的 VK 进程 10 分钟 → KNaaS 租户的 pod **不被驱逐**(条件 NotReady 可以出现,
   pod 对象必须还在)。

---

## R2 `NodePoolFor` 解耦(三处同动)

**现状**:`NodePoolFor(tenantID) = tenantID`(kubezoo-contract `pkg/util/placement.go:57`)。
其注释写明**三处必须一致**:Kyverno 策略 / kubezoo 注入 / 手工打标——改它必须三处同动,
这是该函数自己立的规矩。

**要求**:KNaaS 档位不再使用 pool 概念(R1 的 selector 已换轴)。`NodePoolFor` 与
`kubezoo.io/pool` 相关的注入、策略、测试(`TestPlacementMatchesThePolicy` 读策略比对)
对 KNaaS 档位全部旁路;B1 档位不动。

**验收判据**:kubezoo-contract 的策略一致性测试对两个档位分别断言;KNaaS pod 上
**没有** `kubezoo.io/pool` selector。

---

## R3 节点对租户不可见(大概率免费,需确认)

**现状**:VK 节点名带 `<tid>-` 前缀即自然进入租户视图(DESIGN §2 记载的机制);
租户对 nodes 只读(kubezoo-contract `clusterscope.go:59`)。

**要求**:共享节点名为 `knaas-<region>-<shard>-<az>-<arch>`,不匹配任何租户前缀,
**应当自动不可见**。需要 kubezoo 侧确认:没有任何豁免/例外路径把非前缀节点放进租户视图。

**验收判据**:KNaaS 租户 `kubectl get no` 返回**空**;`kubectl get po -o wide` 的 NODE 列
照常显示节点名(pod 上的 spec.nodeName 不在隐藏范围,与 B1 现状一致即可)。

---

## R4 租户档位标记(一个标签,不改 CRD 不写代码逻辑)

**现状**:2026-08-08 已定(kubezun TODO P 段):开通控制器在 kubezun 侧
(Keystone admin 凭据不进 kubezoo 进程),kubezoo 只打标签。

**要求**:Tenant 对象上一个标签 `knaas.io/compute: capsule` 标记 KNaaS 档;
R1 的分叉判定读它(或读它派生到 namespace 上的等价物,实现自选,但**判定来源只能有一处**)。

**验收判据**:给一个租户加/去这个标签,R1 的注入行为随之切换;无标签租户 = B1,行为不变。

---

## R5 NetworkPolicy 不可表达特性的准入拒绝(独立项,早于形态切换也可做)

**现状**:kubezun 对 `ipBlock.except`/命名端口/SCTP 只能**拒绝执行并记日志**
(fail-closed:相应流量保持 deny),但 NetworkPolicy 没有 status 可写,租户看不到原因
(DESIGN §7.7.4;kubezun 不在准入路径上)。

**要求**:在网关或 Kyverno 层对 KNaaS 租户的 NetworkPolicy 拒绝这三类字段,
错误信息说明"该平台不支持 X;省略它的后果是相应流量保持拒绝"。

**验收判据**:KNaaS 租户 apply 带 `ipBlock.except` 的策略 → 创建被拒且错误信息点名字段;
B1 租户(Cilium 全支持)不受影响。

---

## kubezun 侧已就绪、供对接时直接使用的接口

| 物件 | 值 |
|---|---|
| 共享节点标签 | `node.kubernetes.io/instance-type: knaas.serverless`、`knaas.io/shard`、`topology.kubernetes.io/{region,zone}`、`kubernetes.io/arch` |
| 共享节点污点 | `knaas.io/serverless=true:NoSchedule` |
| 进程形态 | `kubezun --shard <s> --platform-namespace <ns> --namespace-selector '<tenant-label> in (…)'` |
| 每租户绑定 | Secret `<platform-ns>/<tenant>`,注解 `knaas.io/{project-id,region,network-id,vip-subnet-id,vip-network-id}` |
| namespace→租户 | `kubezoo.io/tenant` 标签(现有机制,我们把它当授权边界依赖——**继续保证网关写入且拒改**) |
