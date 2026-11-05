---
title: 在 workflow 里构建镜像
slug: usage/image-building
weight: 31
---

# 在 workflow 里构建并推送镜像

每个 runner Pod 都带官方 kaniko 旁路容器，**不需要额外的 `runs-on` label**。

**按 GitHub 写就行**，本平台不需要对 workflow 做任何改动：

```yaml
- uses: actions/checkout@v4
- run: docker login $REGISTRY -u '${{ secrets.REG_USER }}' -p '${{ secrets.REG_PASS }}'
- run: docker build -t $IMAGE:$TAG -t $IMAGE:latest .
- run: docker push $IMAGE:$TAG
```

只做镜像搬运（不构建）也按标准写法：

```yaml
- run: docker login $SRC_REGISTRY -u '${{ secrets.SRC_USER }}' -p '${{ secrets.SRC_PASS }}'
- run: docker login $DST_REGISTRY -u '${{ secrets.DST_USER }}' -p '${{ secrets.DST_PASS }}'
- run: |
    docker pull $SRC_IMAGE:$TAG
    docker tag $SRC_IMAGE:$TAG $DST_IMAGE:$TAG
    docker push $DST_IMAGE:$TAG
```

`pull` 在 job 里只校验引用并记录来源，`push` 时由 runner 内置的 `crane` 直接从源 registry 复制到
目标 registry（层不落本地）；`docker login` 会合并多个 registry 的凭据，因此源和目标可以是两个
不同的仓库。要搬全部 tag：`docker pull --all-tags $SRC_IMAGE` + `docker push --all-tags $DST_IMAGE`。
内网 HTTP registry 与构建时一样，引用写成 `http://host/...`（替身会去掉协议并给 crane 加 `--insecure`）。

`docker/login-action` 单独用在纯搬运 job 里也可以，它只调用 `docker login`，不会触发 kaniko。
但 Docker 凭据是 **job 级**的：login 和 build/push 必须放在同一个 job；放到不同 job 时，build job
需要自己再登录一次，否则 push 会 401。

官方 docker action 也照原样可用（已实测跑通）：

```yaml
- uses: docker/setup-qemu-action@v3      # 跳过并告警（本平台不做多平台模拟，见下）
- uses: docker/setup-buildx-action@v3    # 虚拟 builder（真正构建仍由 kaniko 完成）
- uses: docker/login-action@v3
- uses: docker/metadata-action@v5
- uses: docker/build-push-action@v6
  with:
    context: .
    file: ./Dockerfile
    platforms: linux/amd64
    push: true
    tags: ...
```

## 平台是怎么接住的

本平台没有任何 docker daemon（部署集群的命名空间是 PSA `baseline`，不许特权容器；docker / podman /
buildah 都要挂载文件系统，没有 `CAP_SYS_ADMIN` 干不了）。唯一能在这种限制下构建镜像的是 **kaniko**，
而 kaniko 官方要求**作为它自己的镜像运行**（它会清空自己容器的根文件系统来解包基础镜像）。

所以流程是：

1. 每个 runner Pod 都带 kaniko sidecar；controller 不解析 workflow 内容，也不需要用户加 label；
2. 该 job 的 Pod 里**多一个容器**，跑**官方 kaniko 镜像**（待命；无特权、无 daemon、无 socket；
   仍然是"每 job 一个一次性 Pod"）；
3. runner 容器里的 **`docker` 替身**（`container/docker.sh`）把标准 docker 命令转成对那个容器的请求
   （共享目录 `/shared/req/<id>/{args,go,log,rc,done}`），并把 kaniko 的日志流回 job 日志、以它的退出码结束；
4. runner 容器和 kaniko 容器的 `HOME` 都是 `/data`，`actions/checkout` 的工作区即构建上下文，两边都看得见。

替身为了配合官方 action 还提供：裸 `docker buildx`（它们用它判断 buildx 可用性）、`buildx inspect`
（返回其逐行解析的表格）、`context inspect`（JSON）、`version/info --format`、`image inspect`/`images`、
`pull`（校验并记录来源，push 时搬运）、`tag`（记录映射）、`run`（**只**跳过 binfmt/QEMU 注册调用，其它明确报错）、
`--metadata-file`/`--iidfile`（用 kaniko 的 `--digest-file` 写 action 读的 digest 元数据）。

## 构建日志

job 日志只显示：

- `>>> [<elapsed>] RUN/COPY/…`：Dockerfile 指令头；`<elapsed>` 是 kaniko 的耗时（秒数或时间戳），可直接看出每步耗时
- RUN 命令的原始输出（`go build` 这类成功时本身无输出）
- `[cache] hit` / `[cache] miss: RUN …`
- 告警与错误

kaniko 的 `Retrieving image manifest`、`Unpacking rootfs`、`Taking snapshot` 等内部流水被过滤掉。

## 代理

平台侧（在 controller 上设 `HTTP_PROXY/HTTPS_PROXY/NO_PROXY`，平台注入每个 Pod）见[部署](../installation/overview.zh-cn.md)。

**workflow 也可以自带代理**（`env:`，或仓库 secret/variable 里给）——两条路径都通：

- 它作用在**步骤**上（含 JS action；Node ≥ 20 还需要 `NODE_USE_ENV_PROXY=1` 才会用它）。
- 构建部分：替身会把本次步骤看到的代理设置**一并转交给 kaniko 容器**，所以拉基础镜像、推镜像也走它。
  转交时 `NO_PROXY` 取「workflow 那份 + 平台那份（`GRC_PLATFORM_NO_PROXY`）」的**并集**——即使 workflow
  里漏写集群内地址，也不会把 Gitea / kkrepo 的流量送进代理。

实测（workflow 里把 `HTTPS_PROXY` 指到一个死端口）：

```
NO_PROXY=localhost,127.0.0.1,::1,kubernetes.default.svc,.svc,.svc.cluster.local,<gitea>.<ns>,...   ← 平台注入
[docker] level=info msg="build submitted to the kaniko sidecar"
error building image: Get "https://docker.gitea.com/v2/": proxyconnect tcp: dial tcp 127.0.0.1:9: connect: connection refused
```

同一次运行里 `actions/checkout@v4` 照常成功——集群内地址在 `NO_PROXY` 里，不受用户代理影响。

## 已知边界

| 边界 | 说明 |
| --- | --- |
| **多平台构建**（`platforms: linux/amd64,linux/arm64`） | 做不到：需要 binfmt/QEMU 模拟，而注册模拟器要 `docker run --privileged`，安全基线不允许。平台会**明确报错并说明原因**（不静默忽略） |
| `docker pull` + `docker tag` + `docker push`（纯搬运） | 支持：`pull` 校验引用并记录，`push` 由 runner 内置的 `crane` 直接 registry-to-registry copy；镜像层不落本地，也不需要 docker daemon |
| `docker build` + 单独 `docker push` | 能跑，但 kaniko 没有本地镜像库，`push` 会**按同样参数重新构建一次**再推（多个 tag 会一次推完，不会推多个 tag 就构建多遍） |
| `docker run` / `docker compose` / `services:` / `uses: docker://` | 需要真 daemon，明确报错 |
| 构建层缓存 | 平台默认**关闭**。workflow 写 `cache-from/cache-to: type=registry,ref=<仓库>` 时映射到 kaniko `--cache-repo`；`type=gha`/`type=local` 不支持（告警并禁用缓存）；`no-cache: true` 可关闭。这是 OCI 层缓存，与 `actions/cache` 的 cache-server 是两套 |

显式开启层缓存（`<image>/cache` 只是约定，换成任意可推的仓库也行）：

```yaml
- uses: docker/build-push-action@v6
  with:
    cache-from: type=registry,ref=<registry>/<owner>/<repo>/<image>/cache:latest
    cache-to: type=registry,ref=<registry>/<owner>/<repo>/<image>/cache:latest,mode=max
```

层缓存只复用**未变化**的层；源码每变一次，`COPY . .` 和其后的 `RUN go build` 必然重新执行，收益主要在
依赖下载这类稳定层。首次构建是填充缓存（`[cache] miss`），之后才可能命中。

## 两个会伪装成"别的问题"的坑

- **`ephemeralStorage` 不足 → 构建中途被驱逐**：kaniko 解包基础镜像+打快照需要 GB 级临时空间；
  模板里没写、命名空间有 LimitRange 默认值（常见 100Mi）时，表现为 job 跑到一半被 **cancelled**，
  事件里写着 `Container kaniko exceeded its local ephemeral storage limit`。模板里显式给大即可。
- **模板镜像 tag 可变 + 默认拉取策略 → 改了镜像没生效**：`runner:latest` 这类可变 tag 下，
  `IfNotPresent` 会让 Pod 一直用节点上缓存的旧镜像（症状：新加的能力"看不见"）。平台的 executor Pod
  已强制 `PullAlways`；模板里的 `latest` 因此能生效，线上要可复现则把模板镜像和 controller 镜像
  钉到同一次 CI 的 sha。
