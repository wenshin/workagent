#!/bin/sh
# 构建期只装两样东西：mise（运行时管理器）与 deno（平台钉死版本）。
#
# 为什么是 mise：
#   `mise use -g node@22` 装进 MISE_DATA_DIR/installs/<tool>/<version>，按 tool@version
#   分目录，天然多版本共存。数据目录指到租户缓存后，Pod 重建、换空间都不用重装；
#   同一租户多个空间装同一版本也只存一份。
#
#   曾经担心「容器 HOME 是每次执行的临时目录、/usr 只读」，于是改成把最新版装死在
#   /usr/local、老版本让模型自己解压。那个担心的前提是 mise 的数据目录跟着 HOME 走；
#   MISE_DATA_DIR / MISE_CONFIG_DIR 一旦指到 /cache，两个问题都不存在了——这也是
#   docs/K8s容器沙箱方案.md「语言运行时」一节的方案。
#
# deno 不走 mise：它的版本必须由平台钉死（插件 ABI 跟它绑定），不能由 agent 换。
set -eu

DENO_VERSION="${DENO_VERSION:?}"
MISE_VERSION="${MISE_VERSION:-}"

# 国内构建走 npmmirror 的 binaries；海外构建改 --build-arg NPM_MIRROR=https://nodejs.org/dist
NPM_MIRROR="${NPM_MIRROR:-https://cdn.npmmirror.com/binaries}"

arch="$(dpkg --print-architecture)"
case "$arch" in
  amd64) deno_arch=x86_64  ;;
  arm64) deno_arch=aarch64 ;;
  *) echo "不支持的架构: $arch" >&2; exit 1 ;;
esac

echo "==> mise ${MISE_VERSION:-latest}"
# 官方安装脚本把 MISE_INSTALL_PATH 当成**完整文件路径**（不是目录），
# 传目录会让最后的 mv 失败。装到 /usr/local/bin 是为了落在 bwrap 的 /usr 基线里。
# 不钉版本时取 latest；要可复现就传 --build-arg MISE_VERSION=<x.y.z>（不带 v）。
if [ -n "$MISE_VERSION" ]; then
  curl -fsSL https://mise.run | MISE_VERSION="$MISE_VERSION" MISE_INSTALL_PATH=/usr/local/bin/mise sh
else
  curl -fsSL https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh
fi

echo "==> deno ${DENO_VERSION}"
curl -fsSL -o /tmp/deno.zip \
  "${NPM_MIRROR}/deno/v${DENO_VERSION}/deno-${deno_arch}-unknown-linux-gnu.zip"
unzip -qo /tmp/deno.zip -d /usr/local/bin
chmod +x /usr/local/bin/deno
rm -f /tmp/deno.zip

echo "==> 校验（构建期就要跑一遍，别等运行期第一条命令才发现）"
mise --version
deno --version | head -n 1
python3 --version
pip3 --version
