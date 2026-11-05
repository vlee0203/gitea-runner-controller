---
title: 开发
slug: advanced/development
weight: 50
---

# 开发

## 代码结构

```text
cmd/gitea-runner-controller/   进程入口、leader election、webhook server
internal/config/               controller env + templates.yaml 解析与校验
internal/controller/           reconcile：waiting jobs ↔ runner Pods
internal/giteaapi/             Gitea admin Actions REST API
internal/giteaclient/          RunnerService.Register
internal/executor/             runner Pod + env/init 文件
internal/webhook/              workflow_job webhook 校验
container/                     controller/runner 镜像、docker 替身
deploy/                        namespace / cluster 清单
```

没有 worker、facade、state、scope、lifecycle 包。

## 构建与测试

```bash
GOWORK=off go test ./...
GOWORK=off go vet ./...
GOWORK=off go build ./cmd/gitea-runner-controller
```

测试覆盖：

- config：模板解析、模板匹配、env 合并、proxy NO_PROXY；
- controller：并发上限、模板选择、终态清理、空 runner 清理、Pod 创建失败回滚；
- executor：`.runner` env + init 落盘、runner/kaniko 双容器、HOME=/data、CA 注入；
- giteaapi：job 列表分页、注册令牌、删除 runner；
- webhook：HMAC、事件过滤。

## CI 镜像

CI 产出：

- `ghcr.io/vlee0203/gitea-runner-controller/controller:latest` + `:sha-<commit>`
- `ghcr.io/vlee0203/gitea-runner-controller/runner:latest` + `:sha-<commit>`

线上滚动更新时把模板里的 runner image 和 controller image 一起钉到同一次构建的 sha。
