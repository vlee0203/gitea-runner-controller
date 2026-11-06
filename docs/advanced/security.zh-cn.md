---
title: 安全模型
slug: advanced/security
weight: 41
---

# 安全模型

新架构只有一个 controller，固定为**站点级**（site scope）。不再有 user-level / cluster-level 双形态，
也没有 scope 开关。

## 凭据

| 凭据 | 位置 | 用途 |
| --- | --- | --- |
| Gitea admin Actions API token | controller Deployment 的 env（直接值） | 列 queued/in-progress jobs、创建注册令牌、删除自己注册的 runner |
| Webhook secret | controller Deployment 的 env（直接值） | 校验 system webhook 的 HMAC |
| runner uuid/token | runner Pod 的 env，init 写入 `/data/.runner` | runner 直接连 Gitea；随 Pod 销毁 |
| 注册令牌 | 不落盘 | controller 在创建 runner 时通过 API 获取，只用于一次 Register |

**executor Pod 只拿到 per-runner 身份，不拿 admin API token，也不拿注册令牌。**

## 为什么必须是站点级

Gitea 的 runner 注册和 API 查询是两套权限：

- runner 注册令牌有 admin/user/org/repo 作用域；
- 查 job 列表的 REST API 需要用户 API token，作用域必须覆盖同样的仓库。

站点级 controller 要看到全站 waiting jobs，因此 API token 必须是 admin 作用域。这不是可选项；
如果不想持有 admin token，只能每个 repo 部署一个 controller，或者放弃 API 对账、只靠 webhook
（Gitea webhook 失败不自动重试，会漏 job）。

## 一次性 runner

- 每个 Pod 预注册为 `ephemeral=true`；
- runner 跑一个 job 后退出（`--once`）；
- 成功跑完的 ephemeral runner 由 Gitea cleanup 删除；
- controller 对自己预注册但没领到任务的 runner 按 ID 删除，避免残留离线记录。

## Pod 边界

- runner 容器不挂 Docker socket；
- 构建型模板额外运行官方 kaniko 容器，非特权；
- 构建/运行资源由模板的 resources 决定；
- 可选 `EXECUTOR_CA_PATH` 让 controller 自己（Gitea API/注册）和所有 runner Pod 信任内网 CA；CA 追加到系统根证书，不替换；
- 可选 proxy 由 controller 注入，集群内 Gitea/kkrepo/cache 地址自动进入 NO_PROXY。

## 明确不做

- 不做 user-level 形态；
- 不做精确 job↔Pod 预绑定；
- 不做 runner 巡检删除（只删 controller 自己注册的 runner ID）；
- 不把 admin API token 或注册令牌下发到 job Pod；
- 不把包管理器源配置做成 Secret；它们是非敏感默认值，直接写模板 `buildEnv`。`buildEnv` 只以
  `ARG` 注入构建阶段，避免把内部地址以 `ENV` 写进最终镜像；带凭据的 URL 仍不允许放进去。
