# 执行容器镜像：只装「跑命令」需要的东西，不装任何平台代码。
#
# 关键点（踩过的坑，别删）：
#  1. 不能用 distroless：bwrap 的只读基线需要 /bin/sh、/etc/passwd、/etc/ssl、/lib64，
#     缺一项就是「命令一律 ENOENT」这种和权限完全无关的报错。
#  2. 必须带 bubblewrap：容器内做空间级隔离。
#  3. /dev/shm 默认 64Mi 会让 Node/JVM 类工具炸掉，编排层要给 memory emptyDir。
#  4. 语言运行时统一由 mise 管理（见 install-runtimes.sh）：mise 本体在 /usr/local/bin，
#     它装的 node / python 落 MISE_DATA_DIR（指向租户缓存），Pod 重建后还在。
FROM debian:bookworm-slim

# 国内构建走镜像；海外构建改 --build-arg DEBIAN_MIRROR=deb.debian.org
ARG DEBIAN_MIRROR=mirrors.aliyun.com
# 只写进 /etc/wa/env.json 用于排障对账，不影响行为
ARG IMAGE_TAG=""
ARG DENO_VERSION=2.9.8
# 空表示装 mise 最新版；需要可复现就钉成具体版本号（不带 v 前缀）
ARG MISE_VERSION=""
# 国内走 npmmirror 的 binaries；海外构建改 https://nodejs.org/dist
ARG NPM_MIRROR=https://cdn.npmmirror.com/binaries
ARG PYPI_INDEX=https://mirrors.aliyun.com/pypi/simple
# npm 包仓库（与上面的 binaries 不是一回事）；海外构建改 https://registry.npmjs.org
ARG NPM_REGISTRY=https://registry.npmmirror.com

RUN set -eux; \
    for f in /etc/apt/sources.list /etc/apt/sources.list.d/debian.sources; do \
      [ -f "$f" ] && sed -i "s|deb.debian.org|${DEBIAN_MIRROR}|g" "$f" || true; \
    done; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        bubblewrap \
        ca-certificates \
        curl \
        iptables \
        tzdata \
        procps \
        coreutils \
        bash \
        git \
        jq \
        python3 \
        python3-pip \
        python3-dev \
        python3-venv \
        tar \
        xz-utils \
        unzip \
        gcc \
        make \
        pkg-config; \
    rm -rf /var/lib/apt/lists/*

COPY bin/sandbox-agent /usr/local/bin/sandbox-agent
COPY bin/sandbox-probe /usr/local/bin/sandbox-probe
COPY install-runtimes.sh /usr/local/lib/wa/install-runtimes.sh
COPY gen-env-facts.py /usr/local/lib/wa/gen-env-facts.py
COPY entrypoint.sh /usr/local/bin/wa-entrypoint
RUN chmod +x /usr/local/bin/wa-entrypoint

# 语言运行时单独一层：改版本号只重建这一层，不用重跑 apt。
RUN DENO_VERSION="${DENO_VERSION}" \
    MISE_VERSION="${MISE_VERSION}" \
    NPM_MIRROR="${NPM_MIRROR}" \
    sh /usr/local/lib/wa/install-runtimes.sh

# 自述文件在镜像里生成：模型据此决定能写什么命令，手写必然漂移。
# 列进去的工具就是「模型敢用」的工具，加包时同步改 gen-env-facts.py 的 TOOLS。
RUN IMAGE_TAG="${IMAGE_TAG}" python3 /usr/local/lib/wa/gen-env-facts.py /etc/wa/env.json \
    && cat /etc/wa/env.json

# PATH 必须显式写：buildEnv 只从父进程继承 PATH，/usr/local/bin 排在前面才能让
# sandbox-agent / mise / deno 被找到。mise 装的运行时不在 PATH 上，走 shims
# （由 InstallEnv 拼进去），这样镜像里的 PATH 与「装了哪个版本」解耦。
#
# 源地址写成镜像 ENV，是为了让「装老版本」的二阶操作也走国内镜像：
# 这些键同时在 sandbox.baseEnvKeys 里，否则会被 buildEnv 那颗白名单挡在沙箱外。
#
# MISE_DATA_DIR / MISE_CONFIG_DIR 指到租户缓存：容器 HOME 是每次执行的临时目录，
# 放默认位置等于每次重装；指到缓存后 `mise use -g` 的版本与全局配置都持久。
ENV PATH=/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin \
    NPM_CONFIG_REGISTRY=${NPM_REGISTRY} \
    PIP_INDEX_URL=${PYPI_INDEX} \
    MISE_DATA_DIR=/cache/mise \
    MISE_CACHE_DIR=/cache/mise/cache \
    MISE_CONFIG_DIR=/cache/mise/config \
    MISE_NODE_MIRROR=${NPM_MIRROR}/node \
    MISE_PYTHON_COMPILE=false \
    MISE_YES=1 \
    MISE_TRUSTED_CONFIG_PATHS=/ \
    SANDBOX_AGENT_ADDR=:8323 \
    SANDBOX_AGENT_ROOT=/var/lib/workagent

EXPOSE 8323
# 入口先封私网出站（不依赖 CNI），再 exec sandbox-agent；
# 没有 NET_ADMIN 时不阻断启动，只降级为「只有 NetworkPolicy」。
ENTRYPOINT ["/usr/local/bin/wa-entrypoint", "/usr/local/bin/sandbox-agent"]
