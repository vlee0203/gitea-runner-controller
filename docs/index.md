# gitea-runner-controller

把 Gitea Actions 的 job 跑成 Kubernetes 里一次性 Pod 的 controller。

```text
Gitea queued jobs ──list/webhook──▶ controller ──create──▶ runner Pod（一次性）
       ▲                                                       │
       └──────────────── runner 直接上报状态/日志 ──────────────┘
```

- controller 对比 Gitea waiting jobs 与 Kubernetes runner Pod，按模板补足并发。
- runner 从模板派生，预注册为 ephemeral，跑一个 job 后退出。
- 没有 worker、facade、assignment Secret；Gitea 的 waiting jobs 就是队列。

## 文档

| 章节 | 内容 | 适合谁 |
| --- | --- | --- |
| [安装](installation/overview.zh-cn.md) | Gitea API token、system webhook、模板、部署与验证 | 首次部署 |
| [安装 · 内网部署](installation/air-gapped.zh-cn.md) | 不能出网的集群：镜像/action 怎么搬进去 | 内网交付 |
| [用法](usage/overview.zh-cn.md) | 模板/labels、默认 env、构建镜像 | workflow 作者 |
| [用法 · 构建镜像](usage/image-building.zh-cn.md) | `docker login/build/push`、kaniko、crane | workflow 作者 |
| [进阶 · 架构](advanced/architecture.zh-cn.md) | reconcile 循环、runner 生命周期、并发 | 想改代码 / 排查深层问题 |
| [进阶 · 安全](advanced/security.zh-cn.md) | 站点级 API token、per-runner 身份、边界 | 安全评审 |
| [进阶 · 兼容矩阵](advanced/compatibility.zh-cn.md) | 哪些可用、哪些做不到及原因 | 选型 / 评审 |
| [进阶 · 平台公共 action](advanced/actions-mirror.zh-cn.md) | 本地镜像了哪些 action、怎么再加 | 平台维护者 |
| [进阶 · 开发](advanced/development.zh-cn.md) | 代码结构、测试、CI | 贡献者 |
| [帮助 · 排查](help/troubleshooting.zh-cn.md) | 症状 → 原因 | 值班 / 运维 |
