#!/bin/sh
# 在镜像构建期装「当前最新的一套」语言运行时。
#
# 为什么不装 pyenv / nvm / mise：
#   它们是「按需下载并编译另一个版本」的工具，而这个容器默认断网、HOME 是每次
#   执行的临时目录、/usr 只读。装进去的结果是模型看得见、一次也用不成，然后
#   反复换写法重试。
#
# 这里换一个思路：最新版本装死在镜像里（构建期一次，运行期零网络），老版本由
# 模型自己装到**空间目录**（可写、持久）。python 走 uv（单二进制，能把解释器
# 装到任意可写目录），node/deno 走预编译包解压。
#
# 所有产物都落在 /usr/local 下：bwrap 的只读基线挂了 /usr，/opt 不在其中，
# 装到 /opt 的东西在沙箱里根本看不见。
set -eu

PY_VERSION="${PYTHON_VERSION:?}"
NODE_VERSION="${NODE_VERSION:?}"
DENO_VERSION="${DENO_VERSION:?}"
UV_VERSION="${UV_VERSION:?}"
FNM_VERSION="${FNM_VERSION:?}"

# 国内构建走 npmmirror 的 binaries；海外构建改 --build-arg NPM_MIRROR=https://nodejs.org/dist
NPM_MIRROR="${NPM_MIRROR:-https://cdn.npmmirror.com/binaries}"
PYPI_INDEX="${PYPI_INDEX:-https://mirrors.aliyun.com/pypi/simple}"

arch="$(dpkg --print-architecture)"
case "$arch" in
  amd64) node_arch=x64;   deno_arch=x86_64  ;;
  arm64) node_arch=arm64; deno_arch=aarch64 ;;
  *) echo "不支持的架构: $arch" >&2; exit 1 ;;
esac

echo "==> uv ${UV_VERSION}"
# 用系统 python 3.11 的 pip 装：PEP 668 会拦，构建期就是要改系统环境，显式放行。
pip3 install --break-system-packages --no-cache-dir -q -i "$PYPI_INDEX" "uv==${UV_VERSION}"

echo "==> python ${PY_VERSION}"
# uv 装的是 python-build-standalone（免编译，含 pip/venv）。
# 装到 /usr/local/opt/python 而不是默认的 ~/.local：镜像里没有「用户目录」。
UV_PYTHON_INSTALL_DIR=/usr/local/opt/python uv python install "$PY_VERSION"

py_dir="$(ls -d /usr/local/opt/python/cpython-"${PY_VERSION}".* 2>/dev/null | head -n 1)"
if [ -z "$py_dir" ]; then
  echo "找不到 uv 安装的 python ${PY_VERSION}" >&2
  ls -l /usr/local/opt/python >&2 || true
  exit 1
fi
# /usr/local/bin 在 PATH 里排在 /usr/bin 前面，所以 python3 默认是新版本；
# 系统自带的 3.11 仍在 /usr/bin/python3，apt 自己的脚本用的是它。
ln -sf "$py_dir/bin/python3" /usr/local/bin/python3
ln -sf "$py_dir/bin/python3" /usr/local/bin/python
ln -sf "$py_dir/bin/pip3" /usr/local/bin/pip3
ln -sf "$py_dir/bin/pip3" /usr/local/bin/pip

echo "==> node ${NODE_VERSION}"
curl -fsSL -o /tmp/node.tar.xz \
  "${NPM_MIRROR}/node/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${node_arch}.tar.xz"
mkdir -p /usr/local/opt/node
tar -xJf /tmp/node.tar.xz -C /usr/local/opt/node
mv "/usr/local/opt/node/node-v${NODE_VERSION}-linux-${node_arch}" "/usr/local/opt/node/${NODE_VERSION}"
for b in node npm npx; do
  ln -sf "/usr/local/opt/node/${NODE_VERSION}/bin/$b" "/usr/local/bin/$b"
done
# corepack 从 Node 25 起被移除，软链过去会得到一个「command -v 找得到、一执行就
# ENOENT」的悬空链接——比没有更糟。要 pnpm 就直接装 pnpm。
npm install -g --prefix /usr/local --silent pnpm
rm -f /tmp/node.tar.xz

echo "==> deno ${DENO_VERSION}"
curl -fsSL -o /tmp/deno.zip \
  "${NPM_MIRROR}/deno/v${DENO_VERSION}/deno-${deno_arch}-unknown-linux-gnu.zip"
unzip -qo /tmp/deno.zip -d /usr/local/bin
chmod +x /usr/local/bin/deno
rm -f /tmp/deno.zip

echo "==> fnm ${FNM_VERSION}"
# node 的版本切换用 fnm 而不是 nvm：nvm 是 shell 函数，靠 source 改当前 shell 的
# PATH，而 shell_exec 每次都是独立的非交互 sh -c，函数和 PATH 都不继承——
# 结果是「nvm install 20 能跑通，下一条命令里却没有 node」。
# fnm 是静态二进制，走 `fnm exec --using=20 node`，不依赖 shell 集成。
case "$arch" in
  amd64) fnm_asset=fnm-linux ;;
  arm64) fnm_asset=fnm-arm64 ;;
esac
curl -fsSL -o /tmp/fnm.zip \
  "https://github.com/Schniz/fnm/releases/download/v${FNM_VERSION}/${fnm_asset}.zip"
unzip -qo /tmp/fnm.zip -d /usr/local/bin
chmod +x /usr/local/bin/fnm
rm -f /tmp/fnm.zip

echo "==> 校验（构建期就要跑一遍，别等运行期第一条命令才发现）"
python3 --version
pip3 --version
node --version
npm --version
pnpm --version
deno --version | head -n 1
uv --version
fnm --version
