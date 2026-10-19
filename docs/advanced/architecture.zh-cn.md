---
title: 架构与机制
slug: advanced/architecture
weight: 40
---

# 架构与机制

## 为什么是自研

| 现成方案 | 为什么不用 |
| --- | --- |
| Gitea 官方 runner 的 `kubernetes://` 模式 | 需要 k8s ≥ 1.30（靠 WebSocket `v5.channel.k8s.io` exec 进 job pod）；低版本上 Pod 建得起来但 exec 阶段被杀 |
| GARM + garm-provider-k8s | 支持 GitHub 与 Gitea（Gitea ≥ 1.24），也能用 Mercedes-Benz 的 k8s provider/operator 按需起 Pod；但它是独立的 runner 管理器，需要在 Gitea 侧持有管理凭据（建/删 runner、管池）并引入自己的 webhook/provider 控制面，与本项目"不持管理 token、只持注册令牌与 runner 身份"的模型不符 |
| Gitea Enterprise ARC | 商业版，靠 Enterprise 侧的 JIT/scale-set 协议 |

根因：**Gitea OSS 没有 JIT configuration API**——服务端拿不到"把指定 job 绑给指定 runner"的能力，
而 Gitea 自己的调度是"按 label 拉取"（`models/actions/task.go:CreateTaskForRunner`：按 label 子集筛
`status=Waiting` 的 job，谁先拉到归谁）。

## 核心机制：先领取 + facade 交接

既然服务端不能"指派"，那就在**客户端**把它做实：

1. **worker 先原子领取**：worker 本身是一条注册在册的 Gitea runner（声明池的 label）。有空位时调
   `FetchTask`，**原子领走一个具体 task**——Gitea 侧该 task 的归属当场确定，别的 worker 抢不走。
2. **交接给一次性环境**：controller 为该 task 建一个一次性 executor Pod；Pod 内的 **stock
   `gitea-runner`** 通过 controller 的 **facade** 拿到"这个已被领取的 task"并执行。
3. **回报**：Pod 内 runner 的 `UpdateTask`/`UpdateLog` 打到 facade，facade 用**领取者身份**转发给 Gitea。

### 为什么是 facade，而不是让 Pod 直连 Gitea

task 已经被 **worker 身份**领取了。Pod 内的 runner 若直连 Gitea：

- 它自己 `FetchTask` 拿不到这个 task（已被领取的不在 Waiting 里）；
- 用 worker 凭据直连，则 Gitea 只看到"一个 runner 跑了所有 job"，而且**把长期凭据下发到了 Pod**。

facade 让 stock runner **一行都不改**（`--instance` 指向 facade），长期凭据留在 controller；每个 Pod
只拿到一张**绑定本次 assignment 的 capability**（一次性、随 Pod 一起消失）。facade 还要反向代理非
RunnerService/Ping 的路径到真实 Gitea——因为 runner 会把 `--instance` 当成 job 的 server URL，
否则 `actions/checkout` 会去 facade 拉代码而 404。

## 组件

```
Gitea ◀─(worker 凭注册身份领取/回报)─ controller ──建 Pod──▶ executor Pod
                                                 │                 │
                                                 └── facade ◀──────┘（stock gitea-runner）
```

- **worker**：每个池一个，= 一条 Gitea runner 注册 + 一个领活循环 + 本地槽位（`slots`）。
- **facade**：每个 controller 实例一个（所有池共用），从 assignment Secret 解析预领取的 task，并把回报交给对应池的领取者身份。
- **executor Pod**：一次性，跑完即删；命名 `grc-<repo>-<job>-<taskid>`，按 label 定位。
- **构建型 job**：Pod 里多一个官方 kaniko 容器（见[在 workflow 里构建镜像](../usage/image-building.zh-cn.md)）。

## 状态与恢复

两类状态，来源不同：

- **worker 的注册身份**（uuid + token）：存 k8s Secret，重启复用（否则每次重启都在 Gitea 里堆一条新记录）。
  注册顺序是**先证明 Secret 可写、再注册**——注册会产生一条只有 API 能删的记录，先注册后落盘失败会留僵尸。
- **in-flight assignment**：以 k8s 为状态源 —— 每条 assignment 一个 Secret（task、poolKey、capability
  哈希、lastSeen），不引数据库，也不把 assignment 只存在进程内存里。

controller 启动时 **Recover**：列出上一次留下的 executor Pod；assignment Secret 还在且池匹配的**接管**
（`Adopt`），assignment/pool 已不存在的才当孤儿删除。facade 无状态，所以接管不要求原进程还活着。

Pod 起不来（`StartError` / `ImagePullBackOff` / 配置错）也会被判定为失败：这类状态会让 Pod **永远停在
Pending**，不处理就会占着槽位、并卡住 controller 的 drain 与滚动更新。

## 滚动更新与 drain

- **facade 无状态**：`capability → assignment` 每次从 assignment Secret 解析（`K8sResolver`），所以
  **任何实例都能服务任何 job**；executor Pod 经 Service 访问 facade，不需要粘在创建它的实例上。
- **滚动更新**：Deployment 用 `RollingUpdate`（maxSurge 1 / maxUnavailable 1）+ readinessProbe
  （TCP 8080）。新实例只有**拿到 Lease** 后才会启动 facade / 变 Ready；老实例收到 SIGTERM 后
  `ReleaseOnCancel: true` 立即释放租约，新实例可以马上接管并开始领新 task，**不必等老实例 drain 完**。
  并发仍由池的 `slots` 控制；副本保持 1 只是部署上的简化。
- **drain（老实例收尾）**：老实例停止领新 task，但已领的 job 用独立于 leader context 的 job context
  继续跑；最多等 `SHUTDOWN_TIMEOUT`（默认 30m，须 ≤ `terminationGracePeriodSeconds`）。期间每 15s
  检查：某 job 连续 `DRAIN_SILENCE`（默认 10m）没有和 facade 通信 → 判卡死，单独取消（报失败 + 删
  Pod）；还在上报的 job 继续等。到总超时则取消剩余 job。这段 drain 只决定老实例何时退出，不影响新
  实例服务与领取。

`controller` 是本仓库唯一的可执行文件（`gitea-runner-controller controller`）。
