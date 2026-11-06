---
title: 用法
slug: usage/overview
weight: 20
---

# 用法

## 模板与 labels

一个模板 = **labels + image + resources + maxConcurrent + timeout + build + env**。
controller 对每个 waiting job 按 `runs-on` 的 labels 选择模板：

- Gitea 的匹配方向：job 的每一个 label 都必须出现在模板 labels 里（模板是超集）；
- 多个模板都覆盖时，选择 labels 最少的（最通用的）模板；
- 没有模板匹配时，job 留在 Gitea waiting 队列，controller 日志会说明。

标准 GitHub 写法零改动：

```yaml
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: echo ok
```

模板声明：

```yaml
templates:
  - name: standard
    labels: [self-hosted, linux, x64, ubuntu-latest]
    image: ghcr.io/vlee0203/gitea-runner-controller/runner:latest
    maxConcurrent: 2
    timeout: 1h
```

## 包代理与镜像代理

`buildEnv` 同时导出到 runner（`run:` 步骤）并注入 Dockerfile 的每个构建阶段：

```yaml
buildEnv:
  PIP_INDEX_URL: http://kkrepo.<ns>.svc:8080/repository/pypi-group/simple
  PIP_TRUSTED_HOST: kkrepo.<ns>.svc
  NPM_CONFIG_REGISTRY: http://kkrepo.<ns>.svc:8080/repository/npm-group/
  YARN_REGISTRY: http://kkrepo.<ns>.svc:8080/repository/npm-group/
  YARN_NPM_REGISTRY_SERVER: http://kkrepo.<ns>.svc:8080/repository/npm-group/
  GOPROXY: http://kkrepo.<ns>.svc:8080/repository/go-group/
```

构建注入只写 `ARG`，值通过 `--build-arg` 传入；不会以 `ENV` 形式留在最终镜像里。用户
Dockerfile 后面自己写的 `ENV` 或显式 `--build-arg` 优先。workflow 的 `env:` 覆盖也会被 shim
读取，因此构建和 `run:` 步骤使用同一份生效值。

`registryMaps` 让 kaniko 的基础镜像拉取走 kkrepo：

```yaml
registryMaps:
  docker.io: https://docker.<domain>
  ghcr.io: https://docker.<domain>
```

- Docker Hub 自动用 `--registry-mirror`（内部 key 是 `index.docker.io`），其它 registry 用
  `--registry-map`；目标可以带 path 前缀。
- kaniko 没有通配映射，每新增一个上游 registry 需要加一行。
- HTTP 目标自动加 `--insecure-registry`；HTTPS 自签由 `EXECUTOR_CA_PATH` 的 CA 覆盖。
- kkrepo 用集群内 Service 地址时，`.svc` 已在 `NO_PROXY` 中，不会走平台 HTTP 代理。
- 这些值不是 Secret，直接写在模板 ConfigMap 里；不要把带用户名密码的 URL 放进来。
- 如果目标是 kkrepo Docker group：group 按成员顺序解析，且只有一个路径命名空间；两个上游存在同名
  image 时由成员顺序决定。需要严格区分来源时，把该上游单独映射到它的 proxy 地址，不要经过 group。
- Cleanup policy 要选具体的 proxy 仓库，不能选 group；group 不拥有内容。

## 构建镜像

每个 runner Pod 都带官方 kaniko 旁路容器；标准写法原样可用，不需要额外的 `runs-on` label：

```yaml
runs-on: ubuntu-latest
steps:
  - uses: actions/checkout@v4
  - run: docker build -t example/app:test .
```

kaniko 容器空闲时几乎不消耗 CPU；这是为了保持 workflow 零改动所做的取舍。

## 并发

`maxConcurrent` 是每个模板的硬上限；超出部分留在 Gitea 队列。Pod 是 pending 还是 running 都计入
并发，避免超卖。

## 取消与超时

- `timeout-minutes` 由 runner 执行；
- 模板的 `timeout` 作为 Pod `activeDeadlineSeconds` 兜底；
- 用户取消由 Gitea 通知 runner 收尾；
- runner 失联时，Gitea 的 zombie/endless timeout 最终收敛任务。
