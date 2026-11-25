#!/usr/bin/env bash
# 执行面（容器化沙箱）本地流程：构建镜像 → 部署执行体 → 跑部署后验。
#
# 用法:
#   scripts/sandbox-exec.sh build   只跨编译 + 构建镜像（生产推 registry 用）
#   scripts/sandbox-exec.sh up      跨编译 + 构建镜像 + 部署
#   scripts/sandbox-exec.sh check   跑 sandbox-probe（会重建 Job，用 IMAGE 指定镜像）
#   scripts/sandbox-exec.sh logs    跟踪执行体日志
#   scripts/sandbox-exec.sh down    删除本地执行面
#
# 前置：kubectl 指向本地集群（OrbStack / kind / k3d 均可），docker 可用。
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

IMAGE="${IMAGE:-workagent/sandbox-runner:dev}"
NAMESPACE="${NAMESPACE:-workagent}"
GOARCH="${GOARCH:-$(go env GOARCH)}"
BIN_DIR="deploy/runner/bin"

build() {
  echo "==> 跨编译 linux/${GOARCH}"
  mkdir -p "$BIN_DIR"
  CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -trimpath -ldflags "-s -w" \
    -o "$BIN_DIR/sandbox-agent" ./cmd/sandbox-agent
  CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -trimpath -ldflags "-s -w" \
    -o "$BIN_DIR/sandbox-probe" ./cmd/sandbox-probe

  echo "==> 构建镜像 $IMAGE"
  # IMAGE_TAG 只写进 /etc/wa/env.json：提示词里报的是哪个镜像，
  # 排障时不用再去猜「这个容器到底是哪次构建的」。
  docker build -f deploy/runner/Dockerfile \
    --build-arg IMAGE_TAG="$IMAGE" \
    -t "$IMAGE" deploy/runner
}

# 控制面的存储根（STORAGE_ROOT），执行体必须按相同的绝对路径挂载进来。
# 路径是跨进程契约：控制面把 /…/data/storage/spaces/<id>/files 原样发给执行体，
# 执行体只接受自己根目录（SANDBOX_AGENT_ROOT）之下的路径；两边路径不一致时
# 第一次 shell_exec 就会报「路径 … 不在执行体允许的根目录 … 之下」。
storage_dir() {
  local p="${WA_STORAGE:-}"
  if [[ -z "$p" && -f .env ]]; then
    p="$(sed -n 's/^STORAGE_ROOT=//p' .env | head -1)"
  fi
  p="${p:-./data/storage}"
  case "$p" in
    /*) ;;
    *) p="$ROOT_DIR/${p#./}" ;;
  esac
  mkdir -p "$p"
  ( cd "$p" && pwd -P )
}

up() {
  build
  local storage
  storage="$(storage_dir)"
  echo "==> 部署执行面（存储根 ${storage}）"
  kubectl apply -f deploy/local/00-namespace.yaml
  # 控制面与执行体共享的存储卷：控制面的 STORAGE_ROOT 必须指向它，
  # 否则文件工具在容器里看不到控制面写的文件。
  sed "s|__WA_STORAGE__|${storage}|g" deploy/local/12-storage.yaml | kubectl apply -f -
  # 控制面建 Pod 的权限：只有 pods / pvc 的 get/list/create/delete。
  kubectl apply -f deploy/local/05-rbac.yaml >/dev/null
  # 私网封禁：OrbStack 自带 k8s 没有 CNI，这条会被接受但不生效，
  # 验证它要用 kind + Calico。集群禁用 NetworkPolicy 时也不阻断部署。
  kubectl apply -f deploy/local/15-runner-netpol.yaml >/dev/null 2>&1 \
    || echo "==> 跳过 NetworkPolicy（当前集群不支持）"
  echo "==> 执行面已就绪（执行体由控制面按空间 / 租户创建）"
  echo "    部署后验：scripts/sandbox-exec.sh check"
}

check() {
  kubectl -n "$NAMESPACE" delete job sandbox-probe --ignore-not-found >/dev/null
  # 探针 Job 里写的是默认镜像与默认命名空间，生产 / 指定时按 IMAGE、NAMESPACE 渲染。
  sed -e "s|workagent/sandbox-runner:dev|${IMAGE}|g" \
    -e "s|namespace: workagent|namespace: ${NAMESPACE}|" \
    deploy/local/20-probe.yaml | kubectl apply -f - >/dev/null
  echo "==> 等待探针完成"
  if ! kubectl -n "$NAMESPACE" wait --for=condition=complete job/sandbox-probe --timeout=180s >/dev/null 2>&1; then
    # 超时时只打日志会是空的（Pod 可能 Pending / 镜像拉不下来），先给出现场。
    echo "==> 探针未在 180s 内完成，当前状态：" >&2
    kubectl -n "$NAMESPACE" get job sandbox-probe -o wide >&2 || true
    kubectl -n "$NAMESPACE" get pod -l job-name=sandbox-probe -o wide >&2 || true
  fi
  kubectl -n "$NAMESPACE" logs job/sandbox-probe || true
  # 探针失败时 kubectl wait 不会返回非零，必须自己看 Job 状态
  local failed
  failed=$(kubectl -n "$NAMESPACE" get job sandbox-probe -o jsonpath='{.status.failed}')
  if [[ -n "${failed}" && "${failed}" != "0" ]]; then
    echo "探针失败" >&2
    exit 1
  fi
}

case "${1:-up}" in
  build) build ;;
  up) up ;;
  check) check ;;
  logs) kubectl -n "$NAMESPACE" logs -f job/sandbox-probe ;;
  down) kubectl delete -f deploy/local/ --ignore-not-found ;;
  *) echo "用法: $0 {build|up|check|logs|down}" >&2; exit 2 ;;
esac
