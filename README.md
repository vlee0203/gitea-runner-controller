# gitea-runner-controller

把 **Gitea Actions** 的 job 跑成 Kubernetes 里**一次性 Pod** 的 controller（类 GitHub ARC）。

workflow 以 **GitHub Actions 标准**为准：`runs-on: ubuntu-latest`、`uses: actions/checkout@v4`
这类写法原样可用，不需要为本平台做任何改动。

```text
Gitea queued jobs ──list/webhook──▶ controller ──create──▶ runner Pod（一次性）
       ▲                                                       │
       └──────────────── runner 直接上报状态/日志 ──────────────┘
```

## 设计

- **controller 是唯一常驻组件**。它对比 Gitea 的 waiting jobs 和 Kubernetes 里的 runner Pod，
  按模板补足并发；不持有 job，不做 task 交接。
- **所有 runner 从模板派生**。模板定义 labels、镜像、资源、并发上限、超时、是否带 kaniko，
  以及所有 job 共用的包代理（`buildEnv`）和基础镜像代理（`registryMaps`）。
- **每个 runner 预注册为 ephemeral**，拿到自己的 uuid/token 后直接连 Gitea，跑一个 job 退出。
- Gitea 的 waiting job 列表就是队列；webhook 只负责触发即时 reconcile，定时 reconcile 兜底。
- 没有 worker、facade、assignment Secret、精确 job↔Pod 预绑定。

## 文档

| 章节 | 内容 |
| --- | --- |
| [安装](docs/installation/overview.zh-cn.md) | Gitea API token、system webhook、模板、部署与验证 |
| [安装 · 内网部署](docs/installation/air-gapped.zh-cn.md) | 不能出网的集群：镜像/action 怎么搬进去 |
| [用法](docs/usage/overview.zh-cn.md) | 模板/labels、包代理/镜像代理、构建镜像 |
| [用法 · 构建镜像](docs/usage/image-building.zh-cn.md) | `docker login/build/push`、kaniko、crane |
| [进阶 · 架构](docs/advanced/architecture.zh-cn.md) | reconcile 循环、runner 生命周期、并发 |
| [进阶 · 安全](docs/advanced/security.zh-cn.md) | 站点级 API token、per-runner 身份、边界 |
| [进阶 · 兼容矩阵](docs/advanced/compatibility.zh-cn.md) | 哪些可用、哪些做不到及原因 |
| [进阶 · 平台公共 action](docs/advanced/actions-mirror.zh-cn.md) | 本地镜像了哪些 action、怎么再加 |
| [进阶 · 开发](docs/advanced/development.zh-cn.md) | 代码结构、测试、CI |
| [帮助 · 排查](docs/help/troubleshooting.zh-cn.md) | 症状 → 原因 |

在线文档：<https://vlee0203.github.io/gitea-runner-controller/>

## 快速开始

```bash
# 1. Gitea 侧：创建 admin Actions API token，并配置 system webhook：
#    URL    http://gitea-runner-controller.<namespace>.svc:8080/webhook
#    事件   Workflow jobs
#    Secret 与 WEBHOOK_SECRET 一致

# 2. 填写 deploy/all.yaml 里的 GITEA_API_TOKEN / WEBHOOK_SECRET 和模板 ConfigMap
kubectl -n <ns> apply -f deploy/all.yaml

# 3. 在 Gitea 仓库里跑一个最小 workflow
```

```yaml
name: smoke
on: push
jobs:
  t:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: echo ok
```

构建镜像的 workflow 不需要额外的 `runs-on` label：每个 runner Pod 都带官方 kaniko
旁路容器，`docker build` / `docker/build-push-action` 原样可用。

## 镜像

| 镜像 | 内容 |
| --- | --- |
| `ghcr.io/vlee0203/gitea-runner-controller/controller:latest` | controller（`container/controller.Dockerfile`，distroless） |
| `ghcr.io/vlee0203/gitea-runner-controller/runner:latest` | 执行环境（工具链 + stock gitea-runner + `docker` 替身 + crane） |

> 部署模板默认用 `:latest`；CI 同时产出不可变的 `sha-<短 commit sha>`。
> **线上滚动更新不要用 `latest`**：把模板里的 runner image 和 controller image 一起钉到同一次构建的 sha。

## 能力速查

- ✅ `checkout` / `run` / `if` / `needs`+outputs / `matrix` / artifacts / `actions/cache` / secrets / vars /
  `timeout-minutes` / `continue-on-error` / `concurrency` / 可复用 workflow / `workflow_dispatch`
- ✅ 在 workflow 里构建并推送镜像：`docker login|build|push` 与官方 docker action（内部用 kaniko）；
  纯搬运 `docker pull|tag|push` 也支持（内部用 crane）
- ✅ 模板级包代理：pip/npm/yarn/pnpm/go 等源同时覆盖 `run:` 步骤和 Dockerfile 构建；基础镜像可经
  `registryMaps` 走 kkrepo Docker 代理
- ❌ 需要真 docker daemon 的写法：`container:` / `services:` / `uses: docker://` / `docker run`
- ❌ 多平台构建（需 binfmt/QEMU，本集群安全基线不允许特权）、`windows-*` / `macos-*`

细节与原因见[兼容矩阵](docs/advanced/compatibility.zh-cn.md)。
