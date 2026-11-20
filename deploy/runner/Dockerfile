# 执行容器镜像：只装「跑命令」需要的东西，不装任何平台代码。
#
# 关键点（踩过的坑，别删）：
#  1. 不能用 distroless：bwrap 的只读基线需要 /bin/sh、/etc/passwd、/etc/ssl、/lib64，
#     缺一项就是「命令一律 ENOENT」这种和权限完全无关的报错。
#  2. 必须带 bubblewrap：容器内做空间级隔离。
#  3. /dev/shm 默认 64Mi 会让 Node/JVM 类工具炸掉，编排层要给 memory emptyDir。
#  4. 语言运行时全部落在 /usr/local 下：bwrap 只挂 /usr，装到 /opt 在沙箱里看不见。
FROM debian:bookworm-slim

# 国内构建走镜像；海外构建改 --build-arg DEBIAN_MIRROR=deb.debian.org
ARG DEBIAN_MIRROR=mirrors.aliyun.com
# 只写进 /etc/wa/env.json 用于排障对账，不影响行为
ARG IMAGE_TAG=""

# 运行时版本：默认装「当前最新的一套」。老版本不预装——模型需要时自己装到空间目录
# （python 用 uv，node/deno 用预编译包解压），见 install-runtimes.sh。
ARG PYTHON_VERSION=3.15
ARG NODE_VERSION=26.13.1
ARG DENO_VERSION=2.9.8
ARG UV_VERSION=0.13.9
ARG FNM_VERSION=1.39.0
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
RUN PYTHON_VERSION="${PYTHON_VERSION}" \
    NODE_VERSION="${NODE_VERSION}" \
    DENO_VERSION="${DENO_VERSION}" \
    UV_VERSION="${UV_VERSION}" \
    FNM_VERSION="${FNM_VERSION}" \
    NPM_MIRROR="${NPM_MIRROR}" \
    PYPI_INDEX="${PYPI_INDEX}" \
    sh /usr/local/lib/wa/install-runtimes.sh

# 自述文件在镜像里生成：模型据此决定能写什么命令，手写必然漂移。
# 列进去的工具就是「模型敢用」的工具，加包时同步改 gen-env-facts.py 的 TOOLS。
RUN IMAGE_TAG="${IMAGE_TAG}" python3 /usr/local/lib/wa/gen-env-facts.py /etc/wa/env.json \
    && cat /etc/wa/env.json

# PATH 必须显式写：buildEnv 只从父进程继承 PATH，而 /usr/local/bin 排在前面
# 才能让 /usr/local/bin/python3（3.15）盖过 /usr/bin/python3（系统 3.11）。
#
# 源地址写成镜像 ENV，是为了让「装老版本」的二阶操作也走国内镜像：
# 这些键同时在 sandbox.baseEnvKeys 里，否则会被 buildEnv 那颗白名单挡在沙箱外。
ENV PATH=/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin \
    NPM_CONFIG_REGISTRY=${NPM_REGISTRY} \
    PIP_INDEX_URL=${PYPI_INDEX} \
    UV_DEFAULT_INDEX=${PYPI_INDEX} \
    FNM_NODE_DIST_MIRROR=${NPM_MIRROR}/node \
    SANDBOX_REQUIRE=true \
    SANDBOX_AGENT_ADDR=:8323 \
    SANDBOX_AGENT_ROOT=/var/lib/workagent

EXPOSE 8323
# 入口先封私网出站（不依赖 CNI），再 exec sandbox-agent；
# 没有 NET_ADMIN 时不阻断启动，只降级为「只有 NetworkPolicy」。
ENTRYPOINT ["/usr/local/bin/wa-entrypoint", "/usr/local/bin/sandbox-agent"]
