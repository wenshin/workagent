#!/usr/bin/env python3
"""生成执行镜像的自述文件 /etc/wa/env.json。

为什么在镜像构建时生成、而不是在人写一份 JSON：
  环境说明一旦手写，就必然和真实镜像漂移；漂移的表现很隐蔽——模型反复
  执行一个镜像里根本没有的命令，或者反过来不敢用一个明明装了的工具。
  这里直接跑 `--version`，说到什么就是什么。

工具清单只列「模型需要判断可用性」的，不列 coreutils：`ls` 不需要提示词
告诉它存在，而 `python3` / `node` / `jq` 决定了它能写什么命令。
"""
import json
import os
import platform
import shutil
import subprocess
import sys

# 只列会影响「能写什么命令」的工具。加新工具时同步加到这里。
# 不列 bwrap：模型不会直接调用它，写进提示词只是白占 token。
# mise 必须列：node / 其他 python 版本全靠它装，模型不知道就会去解压 tarball。
# node 不在列表里：镜像不预装，由 mise 按需装到租户缓存。
TOOLS = [
    "python3", "pip3", "mise", "deno",
    "git", "jq", "curl", "bash", "tar", "unzip", "gcc", "make", "pkg-config",
]

# 少数工具的版本开关不是 --version：硬套会抓到警告行，
# 表现成「unzip 的版本是 caution: both -n and -o ...」。
VERSION_FLAGS = {"unzip": ["-v"]}


def first_line(name: str, path: str) -> str:
    flags = VERSION_FLAGS.get(name, ["--version"])
    try:
        r = subprocess.run([path] + flags, capture_output=True, text=True, timeout=5)
    except Exception:
        return ""
    text = (r.stdout or r.stderr).strip()
    if not text:
        return ""
    lines = [" ".join(l.split()) for l in text.splitlines() if l.strip()]
    # unzip -v 首行是版本、后面是文件清单；--version 类工具首行就是版本
    if name == "unzip":
        for line in lines:
            if line.lower().startswith("unzip"):
                return line
        return ""
    return lines[0] if lines else ""


def clean(name: str, line: str) -> str:
    """去掉版本号前面那串啰嗦的品牌前缀，只留能对账的部分。"""
    if not line:
        return ""
    rules = {
        "git": lambda s: s.replace("git version ", ""),
        "python3": lambda s: s.replace("Python ", ""),
        "jq": lambda s: s.replace("jq-", ""),
        "node": lambda s: s.lstrip("v"),
        "bash": lambda s: s.split("version ")[-1].split(" ")[0],
        "bwrap": lambda s: s.split()[-1],
        "curl": lambda s: s.split()[1] if len(s.split()) > 1 else s,
        "tar": lambda s: s.split()[-1],
        "gcc": lambda s: s.split()[-1],
        "make": lambda s: s.split()[-1],
        "unzip": lambda s: s.split()[1] if len(s.split()) > 1 else s,
        "deno": lambda s: s.split()[1] if len(s.split()) > 1 else s,
        "mise": lambda s: s.split()[0],
        "pip3": lambda s: s.split()[1] if len(s.split()) > 1 else s,
    }
    f = rules.get(name)
    return (f(line) if f else line)[:40]


def main() -> int:
    tools = []
    for name in TOOLS:
        path = shutil.which(name)
        if not path:
            continue
        tools.append({"name": name, "version": clean(name, first_line(name, path))})

    facts = {
        "image": os.environ.get("IMAGE_TAG", ""),
        "os": "debian 12 (%s)" % platform.machine().replace("aarch64", "arm64").replace("x86_64", "amd64"),
        "tools": tools,
        # 没有 writable 字段：deps 目录还没落地，写进去就是不存在的事实。
        # 等 deps 真的可写了，由同一份脚本从环境变量读出来填。
        #
        # network 描述的是 shell_exec 的联网策略：它跟镜像无关，但随平台代码一起
        # 发版，所以先写在这里保持同一批次；等「沙箱行为约定不在这里」落地时，
        # 应挪到部署配置。文案里不能出现「默认断网」——那会让模型以为脚本一跑就没网。
        "network": "调用时先用 need_network 声明这条命令要不要外网：声明联网时，"
                   "curl/wget 等网络工具与脚本先请用户确认；只装依赖始终直接有网；"
                   "未声明则无网执行，脚本断网失败后可申请一次联网重跑。"
                   "私网与云元数据地址始终不可达",
        "persistent": False,
    }

    out = sys.argv[1] if len(sys.argv) > 1 else "/etc/wa/env.json"
    os.makedirs(os.path.dirname(out), exist_ok=True)
    with open(out, "w", encoding="utf-8") as f:
        json.dump(facts, f, ensure_ascii=False, indent=2)
        f.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
