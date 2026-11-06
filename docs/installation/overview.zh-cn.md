---
title: 安装
slug: installation/overview
weight: 10
---

# 安装

## 前置条件

1. Kubernetes 1.28+，命名空间 PSA baseline；不需要 Docker daemon。
2. Gitea 1.24+，并确认：
   - 支持 `workflow_job` webhook；
   - 支持 Actions job 列表 API；
   - 有可用的 admin Actions API token。
3. 一个站点级 Gitea API token，至少能：
   - 读 `/admin/actions/jobs`（状态用 `queued` / `in_progress` / `completed`，不是内部的 `waiting`）；
   - 创建 runner registration token；
   - 删除自己注册的 runner 记录。
4. 一个 system webhook secret。

USW 实例（Gitea 28.1.0）已验证上述能力。

## 1. Gitea 侧

### 1.1 创建 API token

用 Gitea 管理员账号创建一个 token，权限选择能管理 Actions runner / 读取 Actions 的 scope。
token 直接写在 controller Deployment 的 env 里，不进入 job Pod。

### 1.2 创建 system webhook

在 Gitea 管理后台创建 **system webhook**：

```text
URL:    http://gitea-runner-controller.<namespace>.svc:8080/webhook
Secret: 与 controller 的 WEBHOOK_SECRET 一致
事件:   勾选 Workflow jobs
```

`workflow_job` 事件是 controller 的即时触发器；即使漏投，30s 的定时 reconcile 也会补上。

使用 Sealos 模板时，`webhook_secret` 由模板自动生成（`defaults.webhook_secret`），不需要手工生成。
部署后从 controller Deployment 的 env 读出，填入 Gitea system webhook：

```bash
kubectl -n <namespace> get deploy <app-name> \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="WEBHOOK_SECRET")].value}'
```

使用 `deploy/all.yaml` 裸清单时，仍需手工填写 `WEBHOOK_SECRET`。

## 2. Kubernetes 侧

```bash
kubectl apply -f deploy/all.yaml
```

需要替换：

- Deployment 里的 `GITEA_INSTANCE_URL`；
- Deployment 里的 `GITEA_API_TOKEN`（直接 env）；
- Deployment 里的 `WEBHOOK_SECRET`（直接 env）；
- ConfigMap 里的模板 image、kkrepo 地址、资源、并发。

只有这一种部署形态：controller 和 runner Pod 都在同一个命名空间内。

## 3. 模板

`templates.yaml` 的结构：

```yaml
buildEnv:
  PIP_INDEX_URL: http://kkrepo.<ns>.svc:8080/repository/pypi-group/simple
  PIP_TRUSTED_HOST: kkrepo.<ns>.svc
  NPM_CONFIG_REGISTRY: http://kkrepo.<ns>.svc:8080/repository/npm-group/
  YARN_REGISTRY: http://kkrepo.<ns>.svc:8080/repository/npm-group/
  YARN_NPM_REGISTRY_SERVER: http://kkrepo.<ns>.svc:8080/repository/npm-group/
  GOPROXY: http://kkrepo.<ns>.svc:8080/repository/go-group/

registryMaps:
  # Docker Hub uses --registry-mirror; other registries use --registry-map.
  docker.io: https://docker.<domain>
  ghcr.io: https://docker.<domain>
skipDefaultRegistryFallback: false

templates:
  - name: standard
    labels: [self-hosted, linux, x64, ubuntu-latest]
    image: ghcr.io/vlee0203/gitea-runner-controller/runner:latest
    maxConcurrent: 2
    timeout: 1h
    resources:
      cpu: '2'
      memory: 4Gi
      ephemeralStorage: 10Gi

```

- `env` 只导出到 runner；`buildEnv` 同时导出到 runner 并注入每个 Dockerfile 阶段。构建注入只写
  `ARG`，值通过 `--build-arg` 传入，因此不会以 `ENV` 形式留在最终镜像里。
- `registryMaps` 只影响 kaniko 拉基础镜像。目标可以是 kkrepo Docker group 的专用地址，也可以带
  路径前缀；kaniko 没有通配映射，每新增一个上游 registry 需要加一行。
- `docker.io` 会被规范成 `index.docker.io`，配置里写 `docker.io` 即可。HTTP 目标会自动追加
  `--insecure-registry`；HTTPS 自签由 `EXECUTOR_CA_PATH` 注入的 CA 覆盖。
- `skipDefaultRegistryFallback: true` 时，mapped registry 没有镜像就直接失败，不静默回源公网。
- 每个 runner Pod 都带 kaniko sidecar；标准 workflow 的 `docker build` 原样可用。
- `maxConcurrent` 是该模板的并发上限，超出的 job 留在 Gitea waiting 队列。

## 4. 验证

1. controller 日志出现模板数量和 `waiting=... created=...`。
2. 在 Gitea 仓库跑最小 workflow：

```yaml
name: smoke
on: push
jobs:
  t:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: pip config list && npm config get registry && go env GOPROXY
```

3. `kubectl get pods` 应看到一个一次性 runner Pod，跑完即删。
4. `kubectl get pods` 不应残留大量离线 runner；controller 会清理自己注册但没跑成的 runner。

## 5. 依赖清单（必须写进交付文档）

- Gitea 版本：支持 `workflow_job` webhook 和 Actions job 列表 API（USW 28.1.0 已验证）。
- Gitea 侧：system webhook + Workflow jobs 事件 + webhook secret。
- Gitea 侧：admin Actions API token，作用域与 controller 的站点级身份一致。
- Kubernetes：允许 controller 创建/删除 Pod 和 Lease。
- 如果使用自签 Gitea / kkrepo：把 CA 挂到 controller，配置 `EXECUTOR_CA_PATH`；controller 自己
  （Gitea API/注册）、runner 和 kaniko 都使用它。kaniko 拉基础镜像走的就是这份 CA；Dockerfile
  内 `RUN` 的 HTTPS 包代理不继承 kaniko 进程环境，包代理建议用集群内 HTTP Service。
- 可选：`buildEnv` 统一定义 pip/npm/yarn/pnpm/go 的包代理，同时作用于 `run:` 和 Dockerfile 构建。
- 可选：`registryMaps` 把基础镜像拉取重定向到 kkrepo Docker group；每个上游 registry 一条。
- 如果 runner 镜像在私有仓库：提供 `IMAGE_PULL_SECRET`。

## 6. 升级与回滚

新架构与旧架构**不并存**。升级步骤：

1. 停掉旧 controller；
2. 等在跑的旧 job 结束，或取消；
3. 删除旧 controller、旧 worker 身份、旧 assignment Secret；
4. 部署新 controller；
5. 新 controller 从 waiting jobs 重新拉起 runner。

回滚就是恢复旧 Deployment 和旧镜像；新架构不保留旧代码路径，不做双写。
