---
title: 安装 · 内网部署
slug: installation/air-gapped
weight: 11
---

# 内网部署

内网与公网部署的差别只有镜像/action 来源和 TLS 信任；controller 架构不变。

## 1. 镜像

需要搬进去：

- `ghcr.io/vlee0203/gitea-runner-controller/controller:<tag>`
- `ghcr.io/vlee0203/gitea-runner-controller/runner:<tag>`
- `ghcr.io/osscontainertools/kaniko:<tag>`（每个 runner Pod 的构建 sidecar）
- 运行 workflow 时用到的业务镜像

把 `deploy/*/all.yaml` 里的 controller image、模板 image、`KANIKO_IMAGE` 改成内网仓库地址。
线上滚动更新时 controller image 和模板 runner image 钉到同一次 CI 的 sha。

## 2. Actions

`uses:` 的 action 由 runner 从 Gitea 获取。内网 Gitea：

```bash
kubectl -n <gitea-ns> set env sts/<gitea> \
  GITEA__actions__DEFAULT_ACTIONS_URL=self
```

然后用仓库里的 `scripts/mirror-actions.sh` 导入常用 action。Gitea 的注册/建组织要关闭，
防止同名 owner 抢注投毒。

## 3. TLS

Gitea / kkrepo 用自签或企业 CA 时：

1. 把 CA 挂到 controller（例如 ConfigMap/Secret 挂到 `/etc/grc-ca`）；
2. 设置 `EXECUTOR_CA_PATH` 指向该挂载点；
3. controller 自己会读取并信任该 CA（追加到系统根证书，不替换），用于列 job、注册 runner；
4. controller 还会把 CA 注入每个 runner Pod，并注入 `SSL_CERT_FILE`、`NODE_EXTRA_CA_CERTS`、
   `REQUESTS_CA_BUNDLE` 等变量。

不需要再单独设置 `SSL_CERT_DIR`；`EXECUTOR_CA_PATH` 同时覆盖 controller 和 runner Pod。

## 4. 网络

- runner Pod 需要访问 Gitea（HTTPS）和 kkrepo（集群内 Service 或 HTTPS）。
- 不需要访问 controller；runner 直连 Gitea。
- 如果平台有 HTTP 代理，kkrepo 的 Service DNS（`.svc`）已在 `NO_PROXY` 中；ingress 域名要显式加入
  `NO_PROXY`。
- Gitea 的 system webhook 需要能访问 controller Service（集群内即可）。

## 5. 验证

```bash
kubectl -n <ns> logs deploy/gitea-runner-controller
```

应看到模板数量、`waiting=... created=...`。跑一个最小 workflow，确认 runner Pod 创建、job 成功、
Pod 被删除。

## 6. 依赖清单

- Gitea 支持 `workflow_job` webhook 和 Actions job 列表 API；
- system webhook + Workflow jobs 事件 + secret；
- 站点级 admin Actions API token；
- 内网 CA（如有）同时给 controller 和 runner Pod；
- 内网镜像仓库及 imagePullSecret（如需要）。
