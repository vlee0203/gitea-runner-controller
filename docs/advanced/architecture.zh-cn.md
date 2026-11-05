---
title: 架构与机制
slug: advanced/architecture
weight: 40
---

# 架构与机制

## 一句话

**Gitea 的 queued jobs 是队列，Kubernetes 里的 runner Pod 是容量，controller 只做对账。**

```text
Gitea waiting jobs ──list/webhook──▶ controller ──create──▶ runner Pod（一次性）
       ▲                                                       │
       └──────────────── runner 直接上报状态/日志 ──────────────┘
```

## 为什么这么设计

Gitea OSS 没有 JIT configuration API，但 1.24+ 提供了 `workflow_job` webhook 和 Actions job 列表 API。
因此不需要再自己注册成 worker、领取 task、再通过 facade 交接。controller 只需要：

1. 知道有哪些 waiting job；
2. 知道当前有哪些 runner Pod；
3. 在 `active < maxConcurrent` 时注册一个 ephemeral runner 并创建 Pod。

runner 直接连 Gitea，成功/失败/日志都由 runner 自己上报。controller 不再替 runner 上报任务状态。

## 组件

- **controller**：唯一常驻组件。Lease 选主、reconcile、webhook、runner 注册/Pod 创建/清理。
- **runner Pod**：一次性，stock `gitea-runner` host 模式；从模板派生；`--once`。
- **runner 模板**：labels、image、resources、maxConcurrent、timeout、build、env。

## reconcile 循环

每 `RECONCILE_INTERVAL`（默认 30s）或收到 `workflow_job` webhook 时：

```text
list queued jobs       (Gitea job API status=queued)
list in-progress jobs  (status=in_progress)
list managed runner Pods

清理 Succeeded/Failed Pod + 对应 runner 记录
删除长时间没有对应 in-progress job 的空 runner Pod
  （避免“注册了但永远没领到活”的 ephemeral runner 残留）

对每个 waiting job：
  按 job 的 labels 选择最具体的模板
  统计每个模板期望 runner 数，封顶 maxConcurrent

对每个模板：
  active < desired 时：
    取注册令牌
    预注册 ephemeral runner（拿到 id/uuid/token）
    创建 runner Pod（.runner 走 env + init）
```

因为 waiting jobs 来自 Gitea，controller 重启不会丢任务；webhook 只是降低延迟，漏投由定时
reconcile 补上。

## runner 生命周期

### 创建

1. controller 通过 admin API 获取注册令牌；
2. 通过 RunnerService `Register` 注册 runner，`ephemeral=true`，获取 id/uuid/token；
3. 把 `.runner`、可选 `config.yaml`、可选 CA 通过 env 交给 init container；
4. 创建 Pod；Pod 里 init container 把 `.runner` 复制到 `/data`；
5. runner 容器执行 `gitea-runner daemon --once`，直接连 Gitea。

### 成功/失败/超时

- 成功/失败：runner 自己上报给 Gitea。
- workflow 的 `timeout-minutes` / runner timeout：runner 自己取消并上报。
- Pod 的 `activeDeadlineSeconds` 是模板级兜底；Pod 被杀后 controller 释放容量。
- runner 静默死亡：Gitea 服务端 `ZOMBIE_TASK_TIMEOUT`（默认 10m）收尾；controller 只负责删 Pod、
  删自己注册的 runner 记录、释放槽位。

### 取消

- 用户取消 run/job：Gitea 把任务置为 `Cancelling`，runner 收尾并上报；
- runner 还活着时，controller 不需要额外操作；
- runner 失联时，Gitea 用 `TaskReportTimeout` / zombie timeout 收敛；
- 公开 API 只有 run 级取消，没有单 job 取消 API。

### 清理

- Pod 进入 Succeeded/Failed：删除 Pod，删除 controller 预注册的 runner 记录。
- 空 runner（注册后没有 running job）超过 `IDLE_RUNNER_TIMEOUT`（默认 15m）：删除。
- Gitea 也会清理“已经跑完任务的 ephemeral runner”；controller 的删除是兜底，并覆盖“没领到任务”
  的 runner。

## 并发

- `maxConcurrent` 是每个模板的硬上限；超出的 job 留在 Gitea waiting 队列。
- active 包括 Pending/Running 的 Pod。
- 多个 controller 副本通过 Lease 选主，只有 leader 创建 Pod。

## 模板匹配

Gitea 的匹配方向是：**job 的 `runs-on` labels 必须全部出现在 runner 的 labels 里**。所以模板 labels
是 job labels 的超集；多个模板都覆盖时，选 labels 最少（最通用）的模板。例如：

```yaml
standard: [self-hosted, linux, x64, ubuntu-latest, ubuntu-24.04]
```

- `runs-on: ubuntu-latest` → `standard`
- `runs-on: [self-hosted, linux, x64, ubuntu-latest]` → `standard`

## 构建

每个 runner Pod 都带官方 kaniko 旁路容器，runner 容器里的 `docker` 替身把构建请求交给它。
这样标准 workflow 的 `docker build` / `docker/build-push-action` 不需要任何平台专用 label，
保持“GitHub Actions 写法原样可用”。

代价是每个 Pod 多一个空闲的 kaniko 容器；这是为了不解析 workflow 内容、也不要求用户改
`runs-on` 所做的取舍。

## 与旧架构的关系

旧版的 worker、facade、assignment Secret、精确 job↔Pod 绑定已全部删除。新架构不提供兼容模式；
升级是替换，不是新旧并存。
