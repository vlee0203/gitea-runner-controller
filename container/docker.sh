#!/bin/sh
# docker —— 本平台的 docker CLI 替身：把标准 docker 命令转给同一个 Pod 里的 kaniko 容器。
#
# 为什么不自己构建：本平台的 Pod 跑在 PSA baseline 的命名空间里（不许特权容器），
# docker / podman / buildah 都要"挂载文件系统"，没有 CAP_SYS_ADMIN 干不了；能用的只有 kaniko。
# 而 kaniko 官方明确要求**作为它自己的镜像运行**（README: "kaniko is meant to be run as an
# image … We do not recommend running the kaniko executor binary in another image"）——
# 它会清空自己容器的根文件系统来解包基础镜像，挤在别的容器里会把别人的东西一起删掉。
#
# 所以：要构建镜像的 job，Pod 里会多一个 **官方 kaniko 镜像的容器**（无特权、无 daemon、
# 无 socket，只是待命），本替身通过共享目录把构建请求交给它，再把它的日志流回 job 日志。
# 共享目录与请求协议见 internal/executor（sidecar 侧）与本文件末尾（客户端侧）。
#
# 覆盖：login、build、buildx build、push、tag、version、info。
# 不覆盖（需要真 daemon，明确报错而不是静默失败）：run、compose、pull、exec、save、manifest …
set -eu

SHARED=${GRC_SHARED:-/shared}          # 与 kaniko 容器共享的目录（由 controller 挂载）
CREDS="$SHARED/kaniko/.docker/config.json"
SPEC=/tmp/.grc-last-build              # 最近一次 build 的参数，push 时复用
log() { echo "[docker] $*" >&2; }
die() { log "$*"; exit 1; }

# 注册表地址带协议（`https://gitea.corp.local`）是非常常见的写法 —— docker 自己会报
# invalid reference format，而 kaniko 更阴：它把 "https" 当成 registry 主机名，报
# `error checking push permissions … Get "https://https/v2/": EOF`（实测踩过）。
# 这里统一剥掉协议并说一声：镜像引用与凭据里的键都只写主机名。
strip_scheme() {
  case "$1" in
    *://*) log "镜像引用不该带协议，已去掉：$1 -> ${1#*://}"; printf '%s' "${1#*://}" ;;
    *)     printf '%s' "$1" ;;
  esac
}

# 从镜像引用里取出仓库路径（去掉 scheme/tag/digest），kaniko 的 --cache-repo 用
repo_of() {
  ref=${1#*://}
  ref=${ref%%@*}
  case "$ref" in
    */*) printf '%s/%s' "${ref%/*}" "$(printf '%s' "${ref##*/}" | cut -d: -f1)" ;;
    *:*) printf '%s' "${ref%%:*}" ;;
    *)   printf '%s' "$ref" ;;
  esac
}

# BuildKit 的缓存参数：只支持 type=registry,ref=<repo>（映射到 kaniko --cache-repo）。
# type=gha/local 在本平台无法实现；明确告警并禁用缓存，不静默换成别的缓存。
handle_cache_arg() {
  v=$1
  case "$v" in
    *type=registry*)
      case "$v" in
        *ref=*) r=${v#*ref=}; r=${r%%,*}; GRC_CACHE_REPO=$(repo_of "$r") ;;
        *)      GRC_CACHE_UNSUPPORTED=1; log "缓存参数缺少 ref，已忽略：$v" ;;
      esac ;;
    *) GRC_CACHE_UNSUPPORTED=1; log "缓存参数 $v 本平台不支持（仅 type=registry 可映射），本次不启用缓存" ;;
  esac
}

# 过滤 kaniko 的 info 内部流水：只留 Dockerfile 指令头、RUN 输出（不走 logrus）、
# 缓存命中情况和告警/错误。RUN 命令的 stdout/stderr 是直接继承的，没有 INFO 前缀。
filter_log() {
  awk '
    /^INFO\[[^]]*\] / {
      msg = $0
      sub(/^INFO\[[^]]*\] /, "", msg)
      if (msg == "Found cached layer") { print "[cache] hit"; next }
      if (msg ~ /^No cached layer found for cmd /) {
        sub(/^No cached layer found for cmd /, "", msg)
        print "[cache] miss: " msg
        next
      }
      if (msg ~ /^(RUN|COPY|ADD|WORKDIR|ARG|ENV|LABEL|FROM|ENTRYPOINT|CMD|HEALTHCHECK|USER|EXPOSE|VOLUME|SHELL|STOPSIGNAL|ONBUILD)( |$)/) {
        print ">>> " msg
      }
      next
    }
    { print }
  '
}

# 解析 build 的参数（$@ = build 之后的所有参数），结果放进 GRC_* 变量
parse_build() {
  GRC_TAGS=""; GRC_FILE=""; GRC_CTX=""; GRC_ARGS=""; GRC_TARGET=""; GRC_PLATFORM=""
  GRC_PUSH=0; GRC_IGNORED=""; GRC_IID=""; GRC_META=""; GRC_LABELS=""
  GRC_NOCACHE=0; GRC_CACHE_REPO=""; GRC_CACHE_UNSUPPORTED=0
  while [ $# -gt 0 ]; do
    case "$1" in
      -t|--tag)     GRC_TAGS="$GRC_TAGS $2"; shift 2 ;;
      -f|--file)    GRC_FILE=$2; shift 2 ;;
      --build-arg)  GRC_ARGS="$GRC_ARGS $2"; shift 2 ;;
      --target)     GRC_TARGET=$2; shift 2 ;;
      --platform)   GRC_PLATFORM=$2; shift 2 ;;
      # —— 等号形式：--flag=value ——
      --tag=*)         GRC_TAGS="$GRC_TAGS ${1#*=}"; shift ;;
      --file=*)        GRC_FILE=${1#*=}; shift ;;
      --build-arg=*)   GRC_ARGS="$GRC_ARGS ${1#*=}"; shift ;;
      --target=*)      GRC_TARGET=${1#*=}; shift ;;
      --platform=*)    GRC_PLATFORM=${1#*=}; shift ;;
      --iidfile=*)      GRC_IID=${1#*=}; shift ;;
      --metadata-file=*) GRC_META=${1#*=}; shift ;;
      --label=*)       GRC_LABELS="$GRC_LABELS ${1#*=}"; shift ;;
      --cache-from=*|--cache-to=*) handle_cache_arg "${1#*=}"; shift ;;
      --secret=*|--ssh=*|--output=*|--builder=*|--network=*|--shm-size=*|--progress=*)
                       GRC_IGNORED="$GRC_IGNORED ${1%%=*}"; shift ;;
      # 构建证明类参数（官方 build-push-action 默认会带 --attest）：kaniko 做不到，静默忽略。
      # 它们都带值，必须连值一起吃掉 —— 否则 `--attest type=provenance,…` 的值会被当成 context。
      --attest=*|--provenance=*|--sbom=*) shift ;;
      --push)       GRC_PUSH=1; shift ;;
      # —— 带值、但 kaniko 无对应物的参数：连值一起吃掉，只记一笔 ——
      --iidfile)    GRC_IID=$2; shift 2 ;;
      --metadata-file) GRC_META=$2; shift 2 ;;
      --attest|--provenance|--sbom) shift 2 ;;
      --label)      GRC_LABELS="$GRC_LABELS $2"; shift 2 ;;
      --cache-from|--cache-to) handle_cache_arg "$2"; shift 2 ;;
      --secret|--ssh|--output|-o|--builder|--network|--shm-size)
                    GRC_IGNORED="$GRC_IGNORED $1"; shift 2 ;;
      # —— 布尔、无对应物的参数 ——
      --no-cache)   GRC_NOCACHE=1; shift ;;
      --progress|--pull|--quiet|-q|--load|--compress)
                    GRC_IGNORED="$GRC_IGNORED $1"; shift ;;
      --)           shift; [ $# -gt 0 ] && GRC_CTX=$1 && shift ;;
      -*)           GRC_IGNORED="$GRC_IGNORED $1"; shift ;;
      *)            GRC_CTX=$1; shift ;;
    esac
  done
  [ -n "$GRC_CTX" ] || GRC_CTX=.
  # 构建请求是交给**另一个容器**执行的，相对路径在那里没有意义 → 一律转绝对路径
  GRC_CTX=$(cd "$GRC_CTX" && pwd)
  [ -n "$GRC_FILE" ] || GRC_FILE="$GRC_CTX/Dockerfile"
  [ -z "$GRC_IGNORED" ] || log "忽略 kaniko 无对应物的参数：$GRC_IGNORED"
  case "$GRC_PLATFORM" in
    *,*) die "多平台构建（--platform=$GRC_PLATFORM）本平台做不到：kaniko 一次只能构建一个平台（多平台需要 binfmt/QEMU 模拟，本集群安全基线不允许特权）" ;;
  esac
  printf '%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n' "$GRC_CTX" "$GRC_FILE" "$GRC_ARGS" "$GRC_TARGET" "$GRC_PLATFORM" "$GRC_TAGS" "$GRC_LABELS" "$GRC_NOCACHE" "$GRC_CACHE_REPO" "$GRC_CACHE_UNSUPPORTED" > "$SPEC"
}

# 把一次构建（GRC_* + GRC_DEST）交给 kaniko 容器，并把它的输出流回来
submit() {
  [ -d "$SHARED" ] || die "这个 job 里没有 kaniko 容器（$SHARED 不存在）：请让平台把该 job 识别为构建任务"
  dir="$SHARED/req/$$-$(date +%s)"
  meta=""; iid=""
  for a in ${GRC_META:+"$GRC_META"}; do meta=$a; done
  [ -z "${GRC_META:-}" ] || meta=$GRC_META
  [ -z "${GRC_IID:-}" ] || iid=$GRC_IID
  mkdir -p "$dir"
  : > "$dir/args"
  printf '%s\n' "--context=dir://$GRC_CTX" "--dockerfile=$GRC_FILE" >> "$dir/args"
  for a in $GRC_ARGS; do printf '%s\n' "--build-arg" "$a" >> "$dir/args"; done
  # OCI labels（metadata-action 会传一串）：kaniko 支持 --label，原样转发
  for l in $GRC_LABELS; do printf '%s\n' "--label" "$l" >> "$dir/args"; done
  [ -z "$GRC_TARGET" ]   || printf '%s\n' "--target" "$GRC_TARGET" >> "$dir/args"
  [ -z "$GRC_PLATFORM" ] || printf '%s\n' "--custom-platform" "$GRC_PLATFORM" >> "$dir/args"
  if [ -n "${GRC_DEST:-}" ]; then
    # 引用里带 http:// 就是"这台 registry 是 80、走明文" —— 剥掉协议的同时必须把它告诉 kaniko。
    # 为什么不靠 kaniko 自己判：go-containerregistry 只在私有网段/localhost/loopback/.local 才降级
    # 成 http，其余一律 https；所以 http:// 的公共域名（内网常见的 gitea.corp.local 之类）不显式
    # 指定就会用 https 去连。https:// 与不写协议同为默认（443），只需剥掉协议。
    insecure=""
    for d in $GRC_DEST; do
      case "$d" in
        http://*) h=${d#*://}; insecure="$insecure ${h%%/*}" ;;
      esac
      printf '%s\n' "--destination=$(strip_scheme "$d")" >> "$dir/args"
    done
    seen=""
    for h in $insecure; do
      case " $seen " in *" $h "*) continue ;; esac
      seen="$seen $h"
      printf '%s\n' "--insecure-registry=$h" >> "$dir/args"
    done
    # 缓存只由 workflow 显式开启：cache-from/cache-to 里的 type=registry,ref=... 映射到
    # --cache-repo（kaniko 只有一个缓存仓库，读写共用）。什么都没写 → 不缓存。
    if [ "$GRC_NOCACHE" = 0 ] && [ "$GRC_CACHE_UNSUPPORTED" = 0 ] && [ -n "$GRC_CACHE_REPO" ]; then
      printf '%s\n' "--cache=true" "--cache-repo=$GRC_CACHE_REPO" >> "$dir/args"
    fi
  else
    printf '%s\n' "--no-push" >> "$dir/args"
  fi
  # text：kaniko 默认 --log-format=color（ForceColors，不看 TTY），日志行以 ANSI
  # 颜色码开头，filter_log 的 ^INFO[ 匹配不到 → 会把内部流水原样漏出去。
  # info：构建步骤和 RUN 输出都在这一级（RUN 输出直接走 stdout，没有前缀）；
  # 内部流水由 filter_log 过滤，只留 >>> 指令头、RUN 输出、[cache] 和告警/错误。
  printf '%s\n' "--log-format=text" "--verbosity=info" >> "$dir/args"
  printf '%s\n' "--digest-file" "$dir/digest" >> "$dir/args"
  : > "$dir/env"
  for pair in "HTTP_PROXY:${HTTP_PROXY:-}" "http_proxy:${http_proxy:-}" "HTTPS_PROXY:${HTTPS_PROXY:-}" "https_proxy:${https_proxy:-}"; do
    k=${pair%%:*}; v=${pair#*:}
    [ -n "$v" ] && printf '%s=%s\n' "$k" "$v" >> "$dir/env"
  done
  merged_no=$(printf '%s' "${NO_PROXY:-${no_proxy:-}}")
  if [ -n "${GRC_PLATFORM_NO_PROXY:-}" ]; then
    merged_no=$(printf '%s' "${merged_no:+$merged_no,}${GRC_PLATFORM_NO_PROXY}")
  fi
  [ -n "$merged_no" ] && printf 'NO_PROXY=%s\nno_proxy=%s\n' "$merged_no" "$merged_no" >> "$dir/env"
  # 代理分两路：kaniko 自己那份走 env；RUN 步骤那份必须走 --build-arg（kaniko 不把自身环境带进 RUN）
  for name in HTTP_PROXY http_proxy HTTPS_PROXY https_proxy; do
    eval "v=\${$name:-}"
    [ -n "$v" ] && printf '%s\n' "--build-arg=$name=$v" >> "$dir/args"
  done
  [ -n "$merged_no" ] && printf '%s\n' "--build-arg=NO_PROXY=$merged_no" "--build-arg=no_proxy=$merged_no" >> "$dir/args"
  : > "$dir/go"                       # 准备就绪，kaniko 容器可以开工
  log "构建交给同 Pod 的 kaniko 容器"

  # 边等边把它的输出过滤后流到 job 日志（只追加，不重打；只输出完整行，
  # 末尾没有换行的那一行在 done 之后补一次）
  seen=0
  stream_log() {
    [ -f "$dir/log" ] || return 0
    total=$(wc -l < "$dir/log" 2>/dev/null || echo 0)
    [ "$total" -gt "$seen" ] || return 0
    tail -n +$((seen + 1)) "$dir/log" | head -n $((total - seen)) | filter_log
    seen=$total
  }
  while [ ! -f "$dir/done" ]; do
    stream_log
    sleep 1
  done
  stream_log
  if [ -f "$dir/log" ] && [ "$(tail -c 1 "$dir/log" | od -An -tx1 | tr -d ' \n')" != "0a" ]; then
    tail -n 1 "$dir/log" | filter_log
  fi
  rc=$(cat "$dir/rc" 2>/dev/null || echo 1)
  # 官方 docker actions 靠 --metadata-file / --iidfile 拿 digest：取 kaniko 的 --digest-file
  # 构建失败时 kaniko 不会写 digest 文件 —— 先判存在，避免 dash 在重定向阶段报
  # “cannot open …/digest” 把真正的构建错误淹掉
  digest=""
  [ -f "$dir/digest" ] && digest=$(tr -d '\n' < "$dir/digest")
  tags=$(printf '%s' "${GRC_DEST:-${GRC_TAGS:-}}" | tr ' ' ',' | sed 's/^,*//; s/,*$//')
  if [ -n "$digest" ]; then
    [ -z "$iid" ] || printf '%s' "$digest" > "$iid"
    [ -z "$meta" ] || printf '{"containerimage.digest":"%s","containerimage.config.digest":"%s","image.name":"%s"}' \
      "$digest" "$digest" "$tags" > "$meta"
  fi
  rm -rf "$dir"
  exit "$rc"
}

cmd=${1:-}; [ $# -gt 0 ] && shift || true

case "$cmd" in
  login)
    host=""; user=""; pass=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -u|--username)    user=$2; shift 2 ;;
        -p|--password)    pass=$2; shift 2 ;;
        --password-stdin) pass=$(cat); shift ;;
        -*)               shift ;;
        *)                [ -n "$host" ] || host=$1; shift ;;
      esac
    done
    [ -n "$host" ] || die "login 需要 registry 地址：docker login <host> -u <user> -p <pass>"
    # config.json 的键要与 kaniko 查找时用的主机名一致，带协议会匹配不上凭据
    host=$(strip_scheme "$host")
    mkdir -p "$(dirname "$CREDS")"
    auth=$(printf '%s:%s' "$user" "$pass" | base64 | tr -d '\n')
    printf '{"auths":{"%s":{"auth":"%s"}}}' "$host" "$auth" > "$CREDS"
    ;;

  build|buildx)
    if [ "$cmd" = buildx ]; then
      sub=${1:-}; [ $# -gt 0 ] && shift || true
      case "$sub" in
        # 裸 `docker buildx`（无子命令）：build-push-action 的 isAvailable 就是跑这个并看退出码
        "")      printf 'github.com/docker/buildx v0.17.1 (kaniko shim)\n\nUsage: docker buildx [OPTIONS] COMMAND\n' ; exit 0 ;;
        build)  : ;;
        create|use|ls|rm|stop|prune)
                 log "buildx $sub：本平台没有 buildx builder（构建由 kaniko 完成）"; exit 0 ;;
        version) echo "github.com/docker/buildx v0.17.1 (kaniko shim)"; exit 0 ;;
        inspect)
                 # setup-buildx-action 按行解析这张表（键: 值），并读每个 node 的 endpoint。
                 name=default; while [ $# -gt 0 ]; do case "$1" in --builder) shift; name=$1; shift ;; *) shift ;; esac; done
                 printf 'Name:          %s\nDriver:        docker-container\n\nNodes:\nName:          %s0\nEndpoint:      unix:///var/run/docker.sock\nStatus:        running\nBuildkit:      v0.17.1\nPlatforms:     linux/amd64\n' "$name" "$name"
                 exit 0 ;;
        *)       die "不支持的 buildx 子命令：$sub" ;;
      esac
    fi
    parse_build "$@"
    if [ "$GRC_PUSH" = 1 ] && [ -n "$GRC_TAGS" ]; then
      GRC_DEST="$GRC_TAGS" submit       # 多个 tag 一次构建、推多份
    fi
    GRC_DEST="" submit
    ;;

  push)
    ref=${1:-}; [ -n "$ref" ] || die "push 需要镜像名：docker push <ref>"
    [ -f "$SPEC" ] || die "没有可推送的镜像：本平台没有本地镜像库，push 只能跟在同一个 job 里的 build 之后"
    { read -r GRC_CTX <&3; read -r GRC_FILE <&3; read -r GRC_ARGS <&3; read -r GRC_TARGET <&3; read -r GRC_PLATFORM <&3; read -r GRC_TAGS <&3; read -r GRC_LABELS <&3; read -r GRC_NOCACHE <&3; read -r GRC_CACHE_REPO <&3; read -r GRC_CACHE_UNSUPPORTED <&3; } 3< "$SPEC" || true
    # 如果 ref 是上次 build 的某个 tag，就把那批 tag 一起推掉（kaniko 一次可推多个 destination）
    case " ${GRC_TAGS:-} " in
      *" $ref "*) GRC_DEST="$GRC_TAGS" ;;
      *)          GRC_DEST="$GRC_TAGS $ref" ;;
    esac
    log "push $ref：本平台没有本地镜像库，按上次 build 的参数重新构建并推送"
    submit
    ;;

  tag)   log "tag 已忽略（构建时直接打目标 tag）" ;;
  logout) log "logout 已忽略（本平台凭据只写在共享目录里的 kaniko config，job 结束随 Pod 一起消失）" ;;
  version|--version)
    case " $* " in
      *--format*) echo "27.0.0" ;;                    # action 里常见：--format '{{.Client.Version}}'
      *) echo "Client: Docker Engine - Community (gitea-runner-controller kaniko shim)\nServer: kaniko (no docker daemon)" ;;
    esac ;;
  info)
    case " $* " in
      *--format*) echo "27.0.0" ;;
      *) echo "Server: kaniko (no docker daemon in this platform; builds run in the pod's kaniko container)" ;;
    esac ;;

  # 官方 setup-qemu-action 会 pull binfmt 镜像并 run 它注册模拟器。本平台不做多平台模拟
  # （需要 binfmt/qemu → 需要特权，baseline 不许），所以这两步明确跳过并告警，不当成失败。
  pull)  log "跳过 docker pull $*：本平台没有 docker daemon，镜像拉取由 kaniko 构建时完成" ;;

  # 只读探测类命令：action 常用来取镜像 id / 列表 / 上下文。本平台没有本地镜像库，
  # 给一个形状正确的空/占位结果，让它们不至于因此中断（它们本来也只是探测）。
  images|image)
    sub=${1:-}; [ $# -gt 0 ] && shift || true
    case "$sub" in
      ""|ls|list) echo '[]' ;;          # `docker images` / `docker image ls`
      inspect)
        ref=${1:-unknown}
        printf '[{"Id":"sha256:%064d","RepoTags":["%s"],"Architecture":"amd64","Os":"linux","Config":{}}]' 0 "$ref" ;;
      *) die "不支持的 docker image 子命令：$sub" ;;
    esac ;;
  inspect)
    ref=${1:-unknown}
    printf '[{"Id":"sha256:%064d","RepoTags":["%s"],"Architecture":"amd64","Os":"linux","Config":{}}]' 0 "$ref" ;;
  context)
    sub=${1:-}; [ $# -gt 0 ] && shift || true
    case "$sub" in
      show) echo default ;;
      inspect)
        case " $* " in
          *--format*) echo default ;;      # 常见：--format '{{.Name}}'
          *) printf '[{"Name":"default","Metadata":{},"Endpoints":{"docker":{"Host":"unix:///var/run/docker.sock","SkipTLSVerify":false}}}]' ;;
        esac ;;
      ls)   printf '{"currentContext":"default","contexts":[{"Name":"default","Current":true}]}' ;;
      *)    echo default ;;
    esac ;;
  run)
    case " $* " in
      *binfmt*|*qemu*|*tonistiigi/binfmt*)
        # setup-qemu-action 会把这条命令的**输出当 JSON 解析**（取已注册的模拟器列表），
        # 所以这里不能只打日志：给一份形状正确的结果；同时明确告警说模拟器并未真正注册
        # （多平台构建在本集群结构上做不到：binfmt/QEMU 需要特权，安全基线不允许）。
        log "跳过 docker run $*：本平台不做多平台模拟构建（binfmt/QEMU 需要特权，安全基线不允许）"
        printf '{"supported":["arm","arm64","amd64"],"emulators":{"arm":{"path":"/usr/bin/qemu-arm"},"arm64":{"path":"/usr/bin/qemu-aarch64"}}}' ;;
      *)
        die "本平台没有 docker daemon，不支持 docker run（只有 binfmt/QEMU 这类注册步骤会被跳过）" ;;
    esac ;;
  "")    printf 'Usage: docker [OPTIONS] COMMAND\n\n(docker shim for this platform: builds run in the pod'"'"'s kaniko container)\nAvailable: login, build, buildx build, push, tag, image inspect, images, context, version, info\n' ;;
  *)     die "本平台没有 docker daemon，不支持 docker $cmd（可用：login / build / buildx build / push / tag）" ;;
esac
