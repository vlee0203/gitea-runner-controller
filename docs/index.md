---
title: 文档
slug: docs
weight: 10
---

# gitea-runner-controller 文档

把 Gitea Actions 的 job 跑成 Kubernetes 里**一次性 Pod** 的 controller（类 GitHub ARC）。
workflow 以 **GitHub Actions 标准**为准：`runs-on: ubuntu-latest`、`uses: actions/checkout@v4`、
`docker/build-push-action@v6` 这类写法原样可用。

## 章节

| 页面 | 讲什么 | 什么时候看 |
| --- | --- | --- |
| [部署总览](installation/overview.zh-cn.md) | 三种作用域形态、前置条件、逐步部署、部署后校验 | 第一次部署 / 换形态 |
| [内网（不出网）部署](installation/air-gapped.zh-cn.md) | 要提前搬哪四样东西、镜像地址改哪几处、证书与收口 | 部署到不能出网的集群 |
| [写 workflow](usage/overview.zh-cn.md) | 标准写法示例、池与 `runs-on` 的匹配规则 | 写/改 CI |
| [在 workflow 里构建镜像](usage/image-building.zh-cn.md) | `docker build/push` 与官方 docker action 怎么被接住、构建日志、缓存策略与边界 | CI 里要出镜像 |
| [架构与机制](advanced/architecture.zh-cn.md) | 先领取 + 无状态 facade、一次性 Pod、接管与 drain | 想改代码 / 排查深层问题 |
| [安全模型](advanced/security.zh-cn.md) | 作用域隔离、平台不持管理 token、注册令牌与 runner 身份、**刻意不做**的那些事 | 安全评审 |
| [兼容矩阵](advanced/compatibility.zh-cn.md) | 哪些 GitHub 能力可用、哪些是 Gitea 文档差异、哪些本平台做不到及原因 | 迁移既有 workflow |
| [平台公共 action](advanced/actions-mirror.zh-cn.md) | 本地镜像了哪些 action、怎么再加一个、谁有权改 | 用/维护 `uses:` 的 action 源 |
| [开发](advanced/development.zh-cn.md) | 代码结构、本地构建与测试、CI（GitHub / Gitea 两套 workflow） | 改代码 / 发版 |
| [排查](help/troubleshooting.zh-cn.md) | 症状 → 原因（静默失败、被驱逐、卡住、改了没生效…） | 出问题 |

## 相关

- 仓库根 [README](https://github.com/vlee0203/gitea-runner-controller/blob/main/README.md)：一句话定位、镜像、构建方式（速查）。
- `deploy/`：`namespace/all.yaml`（用户级）、`cluster/all.yaml`（集群级）、`cache.yaml`（可选：共享 `actions/cache`，与镜像构建层缓存无关）。
- `container/docker.sh`：本平台的 `docker` 替身（把标准 docker 命令转给同 Pod 的 kaniko 容器）。
