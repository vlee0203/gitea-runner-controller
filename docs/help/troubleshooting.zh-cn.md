---
title: 排查
slug: help/troubleshooting
weight: 60
---

# 排查

| 症状 | 原因 | 处理 |
| --- | --- | --- |
| job 一直 waiting，没有 runner Pod | 没有模板匹配 `runs-on` labels；或 controller 无法列 waiting jobs | 看 controller 日志的 `labels=... match no template` / API 错误；检查模板 labels 和 API token 权限 |
| 有 runner Pod，但一直不领 job | runner 标签不匹配；或 runner 没启动 | `kubectl logs <pod> -c runner`；检查 `.runner` 的 labels 和 Gitea runner 记录 |
| Pod 创建失败，Gitea 里留下 runner | 创建 Pod 失败后 controller 会按 runner ID 回滚 | 看 controller 日志 `rollback runner`；确认 API token 有 runner 管理权限 |
| Gitea Runners 里堆离线条目 | 空 runner（注册后没领到任务），或手动删除的 Pod | controller 在 `IDLE_RUNNER_TIMEOUT` 后按自己的 runner ID 删除；其余按作用域手工删 |
| job 超时/失败但 Gitea 仍显示 running | runner 失联，Gitea 等 `ZOMBIE_TASK_TIMEOUT`（默认 10m） | 等 Gitea 清理；需要更快就调小 `[actions] ZOMBIE_TASK_TIMEOUT` |
| 取消后 Pod 还在 | runner 收尾中；或 runner 失联 | 等 Gitea/runner 收敛；controller 只负责清理终态 Pod |
| `pull`/`push` 报 `crane is missing` | 模板 image 不是当前 runner 镜像 | 使用带 crane 的 runner 镜像 |
| 包管理器仍走公网/代理 | 模板 env 没写，或 kkrepo 是 ingress 域名但没进 `NO_PROXY` | 检查模板 env；service DNS 用 `.svc`，ingress 地址加 `NO_PROXY` |
| webhook 没触发但定时能跑 | webhook secret/事件配置不对 | 检查 Gitea system webhook 的 Workflow jobs 事件和 signature secret |
| 模板更新不生效 | controller 只在启动时读 templates.yaml | 更新 ConfigMap 后重启 controller |
