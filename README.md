# gitea-runner-controller

把 **Gitea Actions** 的 job 跑成 Kubernetes 里**一次性 Pod** 的 controller（类 GitHub ARC）。

workflow 以 **GitHub Actions 标准**为准：`runs-on: ubuntu-latest`、`uses: actions/checkout@v4`、
`docker/build-push-action@v6` 这类写法**原样可用**，不需要为本平台做任何改动。

```
Gitea ──领取/回报──▶ controller ──建 Pod──▶ executor Pod（一次性，跑完即删）
                        │                        │
                        └── facade ◀─────────────┘（stock gitea-runner）
```

为什么自建：Gitea OSS 没有 JIT configuration API（服务端无法"把指定 job 绑给指定 runner"），官方 runner 的
`kubernetes://` 模式又要求 k8s ≥ 1.30。

## 文档

| 章节 | 内容 |
| --- | --- |
| [安装](docs/installation/overview.zh-cn.md) | 三种作用域形态、前置条件、部署步骤与校验、升级与回滚 |
| [安装 · 内网部署](docs/installation/air-gapped.zh-cn.md) | 不能出网的集群：镜像/action 怎么搬进去、证书与收口 |
| [用法](docs/usage/overview.zh-cn.md) | 标准 workflow 写法、池与 `runs-on` 的匹配规则、能力对照 |
| [用法 · 构建镜像](docs/usage/image-building.zh-cn.md) | `docker login/build/push` 与官方 docker action 怎么被接住、构建日志、缓存策略与边界 |
| [进阶 · 架构](docs/advanced/architecture.zh-cn.md) | 先领取 + 无状态 facade、一次性 Pod、接管与 drain |
| [进阶 · 安全](docs/advanced/security.zh-cn.md) | 作用域隔离、平台不持管理 token、注册令牌与 runner 身份、刻意不做的事 |
| [进阶 · 兼容矩阵](docs/advanced/compatibility.zh-cn.md) | 哪些可用、哪些是 Gitea 文档差异、哪些本平台做不到及原因 |
| [进阶 · 平台公共 action](docs/advanced/actions-mirror.zh-cn.md) | 本地镜像了哪些 action、怎么再加、谁有权改 |
| [进阶 · 开发](docs/advanced/development.zh-cn.md) | 代码结构、本地构建与测试、CI |
| [帮助 · 排查](docs/help/troubleshooting.zh-cn.md) | 症状 → 原因 |

在线文档：<https://vlee0203.github.io/gitea-runner-controller/>

## 快速开始

```bash
# 1. 注册令牌放进 Secret（controller 经 k8s API 读，不落盘、不随 Pod 分发）
kubectl -n <ns> create secret generic gitea-runner-controller-registration --from-literal=token=<TOKEN>

# 2. 按需改清单里的 GITEA_INSTANCE_URL / CONTROLLER_URL / pools.yaml，然后应用
kubectl apply -f deploy/namespace/all.yaml     # 用户级；集群级用 deploy/cluster/all.yaml
kubectl apply -f deploy/cache.yaml             # 可选：共享 actions/cache（与镜像构建层缓存无关）
```

跑一个最小 workflow 验收：

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

## 镜像

| 镜像 | 内容 |
| --- | --- |
| `ghcr.io/vlee0203/gitea-runner-controller/controller:latest` | controller（`container/controller.Dockerfile`，distroless） |
| `ghcr.io/vlee0203/gitea-runner-controller/runner:ubuntu-latest` | 执行环境（工具链 + stock gitea-runner + `docker` 替身） |

> 部署请钉不可变的 `sha-<短 commit sha>` 标签；`latest` / `ubuntu-latest` 会被下次构建覆盖，只适合临时试用。

## 能力速查

- ✅ `checkout` / `run` / `if` / `needs`+outputs / `matrix` / artifacts / `actions/cache` / secrets / vars /
  `timeout-minutes` / `continue-on-error` / `concurrency` / 可复用 workflow / `workflow_dispatch`
- ✅ 在 workflow 里构建并推送镜像：`docker login|build|push` 与官方 docker action（内部用 kaniko）；
  构建日志只显示步骤、RUN 输出与 `[cache]` 状态
- ✅ 镜像构建层缓存：默认关闭，workflow 显式写 `cache-from/cache-to: type=registry` 时启用
- ❌ 需要真 docker daemon 的写法：`container:` / `services:` / `uses: docker://` / `docker run`
- ❌ 多平台构建（需 binfmt/QEMU，本集群安全基线不允许特权）、`windows-*` / `macos-*`

细节与原因见[兼容矩阵](docs/advanced/compatibility.zh-cn.md)。

## 状态

Lease 选主 + 无状态 facade + 滚动更新 drain；已在 Gitea 1.27.3 / 28.1.0 与 k8s 1.28 上端到端实测，
包括用本平台构建本仓库自己的两个镜像（自举）。
