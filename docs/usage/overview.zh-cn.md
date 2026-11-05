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

## 默认 env

模板和顶层 `env` 会注入 runner 容器和 kaniko 容器：

```yaml
env:
  PIP_INDEX_URL: http://kkrepo.<ns>.svc:8080/repository/pypi-group/simple
  PIP_TRUSTED_HOST: kkrepo.<ns>.svc
  NPM_CONFIG_REGISTRY: http://kkrepo.<ns>.svc:8080/repository/npm-group/
  YARN_REGISTRY: http://kkrepo.<ns>.svc:8080/repository/npm-group/
  YARN_NPM_REGISTRY_SERVER: http://kkrepo.<ns>.svc:8080/repository/npm-group/
  GOPROXY: http://kkrepo.<ns>.svc:8080/repository/go-group/
```

这些是**默认值**，workflow 里的 `env:` 可以覆盖。它们不是 Secret，直接写在模板 ConfigMap 里。

kkrepo 用集群内 Service 地址时，`.svc` 已在 `NO_PROXY` 中，不会走平台 HTTP 代理。

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
