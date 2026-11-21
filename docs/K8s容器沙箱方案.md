# K8s 容器沙箱方案

## 背景与目标

现有方案是 os 沙箱（bwrap）+ 出网代理。沙箱内 `/usr` 只读、默认无网，agent 无法安装系统依赖，工具类插件能做的事很有限。

本方案要做的是：**把「装环境」这件事从运行期挪到有网、有 root 的阶段**，运行期仍然保持不可信代码的隔离。

目标：

- 沙箱内可以安装依赖（系统包、语言运行时包）
- 环境可持久、可复现、可审计
- 运行期不放开系统目录写权限，也不直接联网

非目标：

- 不支持构建自定义镜像（镜像只作为运行时基座，环境靠租户缓存重装，见「不做什么」）
- 不允许运行期绕过代理直连

## 术语

原先"沙箱"一词在三个层次上混用，这里先定清，后文一律按此使用：

| 词 | 指什么 |
|---|---|
| 集群 | k8s 集群 |
| 执行体 | 一个常驻进程（`sandbox-runner`），所有空间共用 |
| 沙箱 | 执行体内由 bwrap 起的受限进程，一次调用一个 |

"容器"在本方案中特指执行体所在的那个容器。**执行体不是每次调用新建的**——它是一个常驻 Deployment，隔离靠 bwrap 和空间级可写层，不靠 Pod 边界。

要按"每空间一个 Pod"部署，只需要把执行体换成按空间调度的控制器；沙箱这一层的契约（`Spec` / `Runner`）不用变。

## 架构

### 层级

```
租户（企业）
└── 空间（项目 / 会话上下文，一租户多个）
    └── 沙箱（一次调用一个进程，用完即回收）
```

- **租户**：隔离与配额的单位，对应一个企业
- **空间**：工作区、依赖、记忆的归属单位
- **沙箱**：一次调用一个 bwrap 进程，由常驻执行体拉起

租户是**缓存共享的边界**（见「依赖缓存」），也是 PVC 的边界：每租户一个缓存 PVC，每空间一个数据 PVC。

代码里现在只有 `Tenant string`（企业名称，来自 `CorpName`，仅用于提示词），没有租户实体。上多租户前需要补 `tenant_id` 并让空间挂在租户下。

### 两套集群

- 沙箱集群：跑执行体，节点可以被容器逃逸类攻击影响
- 生产集群：跑业务应用，不执行任何用户代码

分开的理由是**爆炸半径**：执行体里有 `SYS_ADMIN`（bwrap 需要）、`NET_ADMIN`（iptables 私网封禁需要）、`seccompProfile: Unconfined`（`unshare(CLONE_NEW*)` 需要）。这三项叠加，节点一旦被逃逸，攻击者拿到的就是节点级权限。生产集群不应该承担这个风险。

开发环境可以共用一套，但要接受同样的风险，并且不接生产数据。

### 三类执行体

| 沙箱 | 服务对象 | rootfs 来源 | 生命周期 |
|---|---|---|---|
| 空间 shell | 一个空间 | 基础镜像，`apt` 装进执行体可写层 | 一次命令一个进程 |
| 插件开发 | 一个草稿目录 | 基础镜像，同空间规则 | 一次命令一个进程 |
| 插件运行 | 一个空间 | 基础镜像 + 按 `deno.lock` 预热 | 常驻池，空闲回收 |

三者**不共用沙箱实例**。空间 shell 里 agent 可以 `apt install` 任何东西，插件运行必须保证不受影响——靠的不是 Pod 边界，是这两条：

- 插件沙箱不挂空间目录，代码只读挂载，数据走 host SDK
- 两者只共享租户缓存（`DENO_DIR` 与包缓存），不共享文件系统

`apt install` 装进的是执行体自己的可写层，所有空间共用这一层——这是接受的设计，持久性靠缓存重装，不靠隔离。风险由 `apt` / `dpkg` 的审批兜住。

## 基础镜像与安装源

1. 平台提供默认基础镜像和配套安装源配置（apt / npm / pip / cargo / go / gem），国内用加速源，国外用官方源。地域**由部署配置指定，不做自动探测**——探测慢且不可靠。
2. 基础镜像只预装必须项：Debian slim、`bubblewrap`、`gcc`/`make`/`pkg-config`、`curl`/`wget`/`git`/`jq`、`python3`（系统自带，供 `node-gyp` 一类构建工具用）、`mise`、钉死版本的 `deno`。**node / 其他 python 版本一律由 `mise` 按需安装**，不预装。
3. 允许在部署时提供自定义基础镜像和安装源。
4. 未提供时使用默认镜像与安装源。
5. 安装源可以是直连的镜像站，也可以是代理服务（Nexus / apt-cacher-ng / Athens / Verdaccio）。代理天然跨节点共享并能审计"谁拉了什么"，代价是多一个要运维的服务。节点少时直连更简单，这条与「依赖缓存」一节的取舍对应。

镜像构建的两条硬约束（改动会直接导致沙箱内命令 ENOENT）：

- **镜像预装**的语言运行时全部装在 `/usr/local` 下。bwrap 的白名单只有 `/usr`，装到 `/opt` 在沙箱里看不见。运行时安装是另一回事，装进 `/usr` 可写层，见「空间 shell 执行体」。
- `PATH` 必须显式写进镜像的 `ENV`。沙箱用 `--clearenv`，只继承显式列出的环境变量。

`deno` 必须钉版本，不用 `latest`。它是插件的执行本体，权限 API 在各版本间有变化，行为依赖具体版本。

### 为什么是 Debian

CentOS 已不在可选项里（7 于 2024-06 EOL，8 于 2021-12 EOL），实际比较的是 `debian:bookworm-slim` vs Rocky / Alma 9。

选 Debian 的理由不是发行版本身更优，而是安装与缓存这条链路只用 `apt` 就够：

| 需要的能力 | deb | rpm 系 |
|---|---|---|
| 只下载不安装 | `apt-get install --download-only`，自动解依赖树 | `dnf download`，要装插件，且不解依赖树 |
| 离线按缓存装 | `apt install` 直接命中 `/var/cache/apt/archives` | `dnf` 的 cache 行为更复杂，要额外配置 |
| 包粒度 | 拆得细，依赖链精确 | 拆得粗，`ffmpeg` 一类要走 EPEL |
| 国内源 | 一行 sed 换掉 `deb.debian.org` | 四套源（baseos / appstream / CRB / EPEL）同步完整度不一 |

体积不是理由（两者都在 70MB 量级），稳定性也不是。真正的分界是 **dpkg 系 vs rpm 系**：缓存与按需重装两条机制都围绕 `apt` 建立，换发行版等于换实现，收益为零。

真要换，`Ubuntu LTS` 是低成本选项——仍是 dpkg，上面几行一行不用改。

### 不自建 registry

环境靠租户缓存重装，不再分发"每空间的镜像"，所以也不自建 registry。

### 从哪来

| 来源 | 说明 |
|---|---|
| 公共源 + 镜像加速 | 阿里云 / 腾讯云 / `mirror.gcr.io`，最简单 |
| 公司已有 Harbor | 已有设施，只管拉不管运维 |
| 自建 | 只在需要完全离线时才考虑 |

**"不自建"和"不使用"是两件事。** 基础镜像总得从某处拉，钉死 digest 就行，不需要为它维护一个带 GC 的服务。

### digest 钉死

```yaml
image: registry.internal/wa/base@sha256:9f2c...
```

不用 `:latest`，不用可覆盖的 tag。理由不变：多节点各自拉取，tag 会导致"同一次发布，节点 A 是新版、节点 B 是旧版"。

基础镜像的升级因此是**显式动作**：改配置里的 digest → 新空间用新的 → 老空间继续用创建时的 digest，不自动跟随。

### 节点预热

一个 DaemonSet 就够：

```yaml
initContainers:
  - name: warm
    image: registry.internal/wa/base@sha256:9f2c...
    command: ["true"]
```

基础镜像 200～400M，每个节点一份。不需要 P2P、不需要层去重、不需要 GC 和 `--delete-untagged` 那些坑。

### 省掉了什么

| 原来要做的 | 为什么现在不用 |
|---|---|
| push 空间镜像 | 不构建 |
| 层去重 | 没有每空间的层 |
| registry GC | 只有基础镜像，人手动推 |
| 镜像鉴权 + `imagePullSecrets` | 基础镜像来自已有源 |
| 节点镜像回收（`imagefs` 阈值） | 每个节点只有一份 |

省掉的是**数据面**（分发用户产生的镜像），留下的是**控制面**（拉一个公共镜像）。这是这次取舍的全部内容。

### 环境自述只描述基础镜像

`/etc/wa/env.json` 在镜像构建时写入，**运行时不再更新**。内容只放基础镜像固有的事实：

```json
{
  "image": "wa-base:2026.11",
  "os": "debian 12",
  "arch": "amd64",
  "tools": [
    {"name": "mise",    "version": "2026.1.0"},
    {"name": "python3", "version": "3.11.9"},
    {"name": "deno",    "version": "2.5.0"}
  ]
}
```

`python3` 是系统自带那份，`deno` 是平台钉死的那份，都是镜像的一部分。`image` 只用于排障时对账"提示词说的是哪个镜像"，不参与任何判断。

工具项**只有这三个**。`git`、`jq`、`gcc` 这类即使确定存在也不列——它们一条 `command -v` 就能探出来，不值得占用每轮请求的上下文。

**不放进来的东西**：空间装的 apt 包、`mise` 按需装的 node、`pip install` 的库、agent 自己升级过的 mise。这些是"当前时刻"的事实，写进去就需要一套"安装后刷新"的 hooks，而且永远有机会漏掉某条安装路径。

### 想知道什么就探一次

agent 判断某个能力在不在时，走 `shell_exec`：

```bash
command -v ffmpeg && ffmpeg -version | head -1
mise ls --installed
```

三个收益：

- **结果永远是真的**，不会因为 env 快照过时而误判
- **提示词里不用维护一份会变长的工具清单**。工具数量随空间增长，全列进上下文就是每轮请求都在为"可能有也可能没有"的东西付 token
- **探测失败本身是有效信息**：`command -v` 返回非 0 就是没有，不需要额外解释

对应到提示词，只写一句"基础镜像是 Debian 12 amd64，预装 `deno` 与系统 `python3`，其他能力用 `command -v` 探测"，而不是列举所有存在的命令。

### 和租户缓存的分工

两个东西解决的是不同问题：

| | `/etc/wa/env.json` | 租户缓存 |
|---|---|---|
| 内容 | 基础镜像有什么 | 这个租户装过哪些包 |
| 变化 | 从不 | 每次安装 |
| 谁写 | 构建镜像时 | apt / pip / npm / mise 自己 |
| 用途 | 描述基线 | 让重装不必联网 |

基线不变，增量也不用记——需要时重装一次就行。这也是"不跟随容器更新"能成立的原因：`env.json` 只承诺基础镜像，运行时装的东西本来就不该由它描述。

### 沙箱行为约定不在这里

`network`（出网形态）、`writable`（哪些目录可写）、`persistent`（容器销毁后系统目录是否保留）描述的是**平台怎么跑容器**，不是镜像里有什么。它们直接从部署配置写进提示词，不经过 `env.json`。

现在的 `gen-env-facts.py` 已经在猜这几个值了：镜像构建脚本并不知道运行时挂哪些卷、按什么策略给网，写出来只能是对默认值的复述。部署一改而镜像没重建，自述就是错的。

判断标准很简单：**换一份部署配置就会变的东西，不属于镜像自述。**

三个字段的去向：

| 字段 | 改由谁表达 | 为什么不能进 `env.json` |
|---|---|---|
| `network` | `shell_exec` 的 description | 同一容器里三档并存（网络工具审批、脚本默认无网、其余有网），不是全局事实 |
| `writable` | `shell_exec` 的 description | 可写范围由 `Mode` 决定（系统安装档才放开 `/usr`），与命令的网络无关 |
| `persistent` | 提示词的「平台」段 | 全局且只由部署决定 |

`network` 必须进工具描述而不是全局提示词，因为它**随命令变**：`ls` 有网、`curl` 要先审批、`python` 默认无网，写成一句全局事实必然对一半错一半。

### 和现有实现的差异

设计变更后，下面三处要跟着改，否则文档和代码会长期不一致：

| 位置 | 现状 | 目标 |
|---|---|---|
| `deploy/runner/gen-env-facts.py` | `TOOLS` 17 项；硬编码 `network` / `persistent`；`os` 里混着架构 | 只留 `mise` / `python3` / `deno`；删 `network` / `persistent`；`arch` 拆成独立字段 |
| `internal/sandbox/envfacts.go` | 有 `Writable` / `Sources` / `Persistent` / `Network` 字段 | 失去来源，可删；`writeExecFacts` 与 `envfacts_test.go` 跟着调 |
| `internal/agent/prompt.go` | `execToolsLimit = 24` | 降到 3，或按"只允许三项"校验 |

前两项如果暂时不动，读出来是空值、渲染时跳过，不会出错。但 `TOOLS` 那 17 项必须收——否则上下文照样被撑满，设计意图落不了地。

## 环境持久化

一个空间的环境 = **基础镜像** + **租户缓存**。不构建镜像，不重放依赖，执行体重建后环境回到基础镜像。

### 三层存储

装在系统目录的东西和装在项目目录的东西，持久性完全不同：

| 层 | 例子 | 执行体重建后 | 怎么保住 |
|---|---|---|---|
| 镜像层 | 基础镜像预装的 `deno` / `python3` | 保留 | 构建镜像——不做 |
| 容器可写层 | `apt install` 落到 `/usr` | **丢失** | 重跑安装命令，命中租户缓存 |
| 空间目录 | `.pkg`（venv / npm 全局包）、`node_modules`、项目文件 | 保留 | 本来就持久，不用管 |

规则只有一条：

> **装在系统目录的靠重装，装在空间目录的天然持久。**

```
apt install ffmpeg      → /usr                     → 重建后重装
pip install requests    → <空间>/.pkg/venv         → 不用管
npm i lodash            → ./node_modules           → 不用管
npm i -g typescript     → <空间>/.pkg/npm          → 不用管
```

容器可写层不是缓存，是**临时的当前状态**。它随时会丢，所以要保证"丢了能重新装上"，而不是"丢了能原样恢复"。

### 为什么不做每空间镜像

| | 每空间一个镜像 | 基础镜像 + 重装 |
|---|---|---|
| 冷启动 | 0 秒 | 起沙箱进程，毫秒级 |
| 需要 builder | 是 | 否 |
| 需要自建 registry | 是 | 否 |
| 需要 GC / 预热 / 层去重 | 是 | 否 |
| 环境漂移 | 不可能 | 可能：缓存未命中时装到新版 |
| 基础镜像升级 | 要重算所有 delta | 改 digest，新空间直接用 |

省掉的是整条「构建 → 推送 → 分发 → 回收」链路。代价是环境不保证逐字节一致——但漂移发生在缓存未命中时，会直接报错，不会静默装出别的版本。在"空闲 5 分钟回收"的前提下，这个交换是划算的。

### 空间不做重放

普通 shell 的机制是"没有撤销"：

```
执行体重建 → 可写层消失 → 系统目录回到基础镜像
```

不重放、不记清单、不装 entrypoint 钩子。agent 需要什么就再装一次。

这能成立的前提只有一个：**装的时候命中缓存**。

| 类型 | 冷启动后 | 恢复方式 |
|---|---|---|
| `apt install jq` | 包没了 | 重跑同一条命令，deb 在租户缓存里，1～2 秒 |
| `mise use node@22` | **还在** | `MISE_DATA_DIR` 指向租户缓存，mise 直接识别 |
| `npm i` | **还在** | 装在 `node_modules`，属于工作区 |
| `pip install requests` | **还在** | venv 在 `.pkg/venv`，属于空间目录 |
| `npm i -g typescript` | **还在** | `NPM_CONFIG_PREFIX` 指向 `.pkg/npm` |

`mise`、`npm -g`、`pip` 和项目内依赖都是免费的——它们本来就落在持久位置。真正要重装的只有 `apt install` 那部分，因为它只能装进 `/usr`。

### 缓存不是"省时间"，是"能离线重装"

重装的隐含前提是**断网也能装**。只缓存 deb 不够：

```
/cache/apt/
├── lists/        ← apt-get update 的产物，缺了会报 Unable to locate package
└── archives/     ← deb 本体
```

只挂 `archives` 的典型现象是：包明明在，`apt install` 却报"找不到这个包"——因为 apt 用它自己的索引解析包名，没有索引就不知道 `ffmpeg` 对应哪个 deb。

所以两个目录都要挂，而且都要可写（agent 跑 `apt update` 时写 lists）。

### 提示词要告诉 agent 这件事

环境会被重置，这件事 agent 必须知道，否则它会把"上次装过"当成"现在还有"：

> 容器闲置会被回收，系统目录回到基础镜像。缺什么直接装，包在本地缓存里，不需要联网。

放在「平台」段，和"空闲 5 分钟回收"写在一起。

### 为什么不记清单

记清单是为了重放。不重放，清单就没有消费者，只剩维护成本：

| 清单要解决的问题 | 取消重放后 |
|---|---|
| 装了什么 | agent 重跑命令就知道 |
| 什么版本 | 缓存里的就是上次那份，apt 命中同一个 deb |
| base 变了怎么办 | 不涉及——环境本来就是空的 |

版本漂移的风险还在，但触发条件变窄了：只有**缓存未命中并且有网**时才会装到新版；缓存未命中且断网会直接报错，不会静默换版本。

要彻底杜绝也很简单——安装时带上版本号：

```bash
apt install ffmpeg=7:5.1.6-0+deb12u1
```

缓存里没有这个版本就失败，不会退而求其次。

### 插件也不重放

插件运行体由平台按事件调用，没有"agent 发现缺依赖再装"的机会。但它同样不需要重放——Deno 插件没有系统依赖，依赖在安装期按 `deno.lock` 预热进租户缓存。见「插件运行执行体」。

代价是插件冷启动仍有进程启动开销（Deno 自身 50～100ms），常驻池已经把这个成本摊掉了。

### 语言运行时

`mise` 装的东西落在**租户缓存**，空间之间共享：

```
MISE_DATA_DIR=/cache/mise
```

版本目录天然持久，容器重建后 `mise` 直接识别，不用重装也不用打包。

为什么不放空间目录：同一个租户的 20 个空间装同一个 `node@22`，放空间目录就是 20 份、20 次下载；放租户缓存是 1 份。展开见「依赖缓存」。

`deno` 用基础镜像预装的那份，不走 `mise`——版本必须由平台钉死。

## 依赖缓存

依赖缓存的角色不只是省带宽——它是「环境持久化」里重装的唯一来源。重装能不能离线、能不能在两三秒内完成，全看这一层有没有命中。

### 为什么是租户级，不是全局

安装包天然可共享（文件名带版本，内容不可变），但**共享范围要划在租户边界上**，不能是全局：

| 风险 | 全局共享 | 租户内共享 |
|---|---|---|
| 投毒 | 租户 A 写，租户 B 用，`dpkg -i` 不校验 | 不存在，租户内互信 |
| 信息泄露 | 包列表能推断业务（语音 SDK、财务库） | 不可见 |
| 清理 | 分不清谁在用，不敢删 | 整个租户一起清 |
| 配额 | 摩擦 | 按租户算 |

租户内共享是免费的——同租户的空间本来就在同一个信任域内。

### 布局

```
/tenants/<tenant>/
├── cache/                        租户内所有空间共享
│   ├── apt/
│   │   ├── lists/                apt-get update 的产物，离线装包必需
│   │   └── archives/             .deb
│   ├── mise/
│   │   ├── downloads/            原始 tarball
│   │   └── installs/             解压后的运行时（各版本共存）
│   ├── tarballs/                 node / deno / go / python 官方包
│   ├── go/
│   │   ├── mod/                  GOMODCACHE
│   │   └── build/                GOCACHE
│   ├── npm/
│   ├── pip/
│   ├── uv/
│   ├── deno/<slug>@<version>/    DENO_DIR，按插件版本分，跨空间共享
│   └── xdg/                      XDG_CACHE_HOME
└── spaces/<space>/               空间目录
    ├── .env/                     控制面元数据
    ├── .pkg/
    │   ├── npm/                  全局 node 包（NPM_CONFIG_PREFIX）
    │   └── venv/                 python 环境（python3 -m venv）
    └── （工作区）
```

**每租户一个 PVC**，不用"一个大 PVC 分 subPath"。理由是配额、快照、删除都以 PVC 为单位，混在一起做隔离很别扭。

### 各层怎么接入

| 层 | 缓存内容 | 接入方式 | 需挂载 |
|---|---|---|---|
| apt | `.deb` + 索引 | 两个目录都挂载 | **是** |
| mise | tarball + 解压产物 | `MISE_DATA_DIR` | 否 |
| node / deno / go | 官方 tarball | 安装脚本先查缓存 | 否 |
| Go | 模块 + 编译产物 | `GOMODCACHE` / `GOCACHE` | 否 |
| npm | 包 tarball | `NPM_CONFIG_CACHE` | 否 |
| pip | wheel | `PIP_CACHE_DIR` | 否 |
| uv | wheel | `UV_CACHE_DIR` | 否 |

**只有 apt 不认环境变量**，两种接法：

```yaml
# 接法一：挂载（推荐，不可绕过）
volumeMounts:
  - name: tenant-cache
    mountPath: /var/cache/apt/archives
    subPath: apt/archives
```

```conf
# 接法二：配置（/etc/apt/apt.conf.d/99wa-cache）
Dir::Cache::archives "/cache/apt/archives";
```

其余全部用环境变量，写在 entrypoint 里：

```bash
export MISE_DATA_DIR=/cache/mise
export GOMODCACHE=/cache/go/mod
export GOCACHE=/cache/go/build
export NPM_CONFIG_CACHE=/cache/npm
export PIP_CACHE_DIR=/cache/pip
export UV_CACHE_DIR=/cache/uv
```

### 语言包的全局安装

不改 prefix 的话，`npm i -g` 和 `pip install` 装进系统目录，Pod 重建同样会丢，只能靠缓存兜底。缓存能不能复用取决于开关：

| 命令 | 缓存里有什么 | 断网行为 |
|---|---|---|
| `npm i -g foo` | tarball + packument | 默认先联网校验，断网会回退但慢 |
| `npm i -g --prefer-offline foo` | 同上 | **优先用缓存，缺了才联网** |
| `npm i -g --offline foo` | 同上 | 缺任何一样直接失败 |
| `pip install foo` | wheel | 缺包直接失败，没有回退 |
| `pip install --no-index --find-links=/cache/pip/wheels foo` | 本地 wheel 目录 | 完全离线 |

`npm` 有一个容易误判的地方：它的缓存里除 tarball 还有 packument（包元数据），默认模式会对 packument 做新鲜度检查，所以"这个包下载过"不等于"断网能装"。`--offline` 又太硬——缺一条 transitive dep 就整体失败。`--prefer-offline` 是合适的档位。

版本问题同样存在：`npm i -g typescript` 解析的是 `latest`，重装时只要联网就装到新版。要固定就写 `typescript@5.6.0`，和 apt 带版本号同理。

### Debian 12 的 pip 会拒绝安装

Debian 12 的系统 `python3` 带 `EXTERNALLY-MANAGED` 标记（PEP 668），`pip install requests` 直接报：

```
error: externally-managed-environment
```

这不是配置错误，是发行版有意为之。三条出路：

| 做法 | 命令 | 适用 |
|---|---|---|
| 允许装进系统 | `pip install --break-system-packages requests` | 容器一次性的，可以接受 |
| 装进空间目录 | `python3 -m venv .pkg/venv`，见下节 | 更好，重建后不用重装 |
| 建虚拟环境 | `python3 -m venv .pkg/venv` | 全局环境就该用这个，见下节 |

`--break-system-packages` 名字很吓人，但在"一空间一容器、坏了就重建"的前提下，破坏系统 python 的代价只是重来一次。让它直接可用，比让 agent 反复试 venv 要省事。

提示词里应该直接给出这个写法。否则 agent 会在这个报错上绕很久——它看起来像权限问题，实际是策略问题，`sudo` 和 `chmod` 都解决不了。

### 装进空间目录

`npm` 和 `pip` 的全局安装可以直接落到空间目录，位置本身持久，就不存在"重建后恢复"这件事：

```bash
# 空间容器的 entrypoint
export NPM_CONFIG_PREFIX="$WA_SPACE_DIR/.pkg/npm"
export PATH="$WA_SPACE_DIR/.pkg/venv/bin:$WA_SPACE_DIR/.pkg/npm/bin:$PATH"

# 首次创建（幂等）
python3 -m venv "$WA_SPACE_DIR/.pkg/venv"
```

**pip 用 venv，不用 `PIP_TARGET`：**

| | `PIP_TARGET=.pkg/py` | `venv` |
|---|---|---|
| console script | 进 `target/bin`，容易漏进 `PATH` | 自动进 `venv/bin` |
| 升级 / 卸载 | 只覆盖，旧文件残留 | `pip uninstall` 正常 |
| 多版本叠加 | 多个 `.dist-info` 并存，互相干扰 | venv 内正常解析 |
| PEP 668 | 绕开 | 绕开 |

`--target` 的定位是"往目录里塞包"，不是"做一个可用的 python 环境"。要长期用的环境就该用 venv。

**为什么是空间目录，不是租户缓存：**

`npm i -g` 和 `pip install` 会改 `prefix` 下的 `lib/node_modules` 和 `bin`。多空间共用一个 prefix 时，同名包的版本会互相覆盖——空间 A 装 `typescript@5.6`，空间 B 装 `5.7`，后装的覆盖先装的。

`mise` 能共享是因为它按 `tool@version` 分目录，天然多版本共存；`npm prefix` 和 `venv` 没有这个机制，只能做成空间级。

代价是每个空间一份，`typescript + requests` 这类约 50～100MB，相对 5Gi 配额可以接受。

三条注意：

- **必须用环境变量，不能用 `npm config set`。** 后者写 `~/.npmrc`，而沙箱里 `HOME` 是每次调用新建的临时目录，读不到。
- **`PATH` 要在容器主进程里拼好。** `--clearenv` 之后沙箱只继承 `baseEnvKeys` 列出的变量，在 shell 里 `export` 只影响当前这条命令。
- **基础镜像要预装 `python3-venv`。** Debian 12 slim 默认没有，`python3 -m venv` 会报 `ensurepip is not available`。

最终分类：

| 装到哪 | 例子 | Pod 重建后 |
|---|---|---|
| 空间目录 | `npm -g` → `.pkg/npm`，`pip` → `.pkg/venv`，`node_modules` | 还在，不用管 |
| 租户缓存 | `mise` → `MISE_DATA_DIR`，apt 的 deb | 还在，不用重装 |
| 系统目录 | `apt install` 装进 `/usr` | 没了，重跑命令，命中缓存 |

**能落空间目录的落空间目录，装系统目录的靠缓存重装。** 前者零成本，后者要多一轮。

### 一个必须处理的细节

apt 以 `_apt`（uid 42）身份下载。租户目录的属主通常是 root 或宿主用户，直接写会失败——**而且是静默失败**：apt 会 fallback 到 `/tmp`，包看起来装成功了，但缓存里什么都没有。

```bash
chown -R 42:0 /tenants/<t>/cache/apt/archives
chmod 0775 /tenants/<t>/cache/apt/archives
```

PVC 是持久的，属主只需设一次。但如果挂载后属主被重置，放 entrypoint 里无条件跑一遍最省事。

验证方式：

```bash
apt install -y jq && ls /cache/apt/archives/*.deb
```

没有输出就是权限问题。

### 并发

租户内多个空间同时装同一个包时：

| 缓存 | 并发安全 |
|---|---|
| apt | `partial/` + rename，同包并发会有一方失败重试，可接受 |
| mise | `locks/` 目录 flock |
| Go mod / GOCACHE | 内部加锁 + 内容哈希命名 + 原子 rename |
| npm / pip / uv | 各自有锁 |

apt 是唯一需要留意的。它的锁在 `/var/lib/dpkg/lock`，是**容器内**的，跨容器不共享。所以两个空间同时下同一个 deb 时，后到的那个会写 `partial/` 并 rename 覆盖——结果正确，只是重复下载了一次。

频率不高，不必加分布式锁。

### 容量与清理

单租户估算：

| 层 | 典型占用 |
|---|---|
| apt archives + lists | 630M（约 20 个常用包 + 30M 索引） |
| mise installs | 300M（node + python） |
| tarballs | 100M |
| go mod + build | 1～3G |
| npm / pip / uv | 800M |
| **合计** | **约 3～5G** |

Go 的两块最大，但 Go 自己会在 `GOCACHE` 超过 10G 时自动淘汰最旧的，基本不用管。

清理策略：

| 缓存 | 方式 |
|---|---|
| apt | 按 mtime 删 90 天未访问 |
| mise / tarballs | 保留最近 N 个版本 |
| npm / pip / uv | 各自 `cache clean` |
| GOCACHE | 交给 Go |

**缓存满了不是"不省了"，是故障。** PVC 写满后 `apt install` 直接报 `No space left on device`，而这个错误出现在挂载点上，和"容器磁盘满"无法区分，排查成本很高。

所以三件事都要做：

- 缓存 PVC 与空间数据 PVC 分开，前者满不影响工作区
- 80% 水位告警
- CronJob 每周清理一次，先做成可手动触发

### 预热

新租户建好时缓存是空的，第一个空间装 `jq` 也要走网络。可以让租户创建钩子跑一次：

```bash
# 索引和 deb 要一起下：只下 deb 的话，离线时 apt 解析不出包名
apt-get update \
  -o Dir::State::lists=/cache/apt/lists

apt-get install -y --download-only \
  -o Dir::Cache::archives=/cache/apt/archives \
  jq curl git ffmpeg imagemagick
```

十几个常用包约 200M，把"首次安装"从"下载 + 安装"变成"只解包"。

预热清单要配在部署配置里，不写死——不同租户的常用包差别很大。

### 和按需重装的关系

租户缓存是重装时唯一的包来源：

```
空间冷启动
  1. 挂载 /cache
  2. 系统目录是空的，回到基础镜像
  3. agent 缺什么装什么，命中 /cache
```

`mise` 装的运行时不用管——`MISE_DATA_DIR` 指向 `/cache/mise`，Pod 重建后 mise 直接识别，算"已经装好"。真正需要重跑命令的是 apt 和 pip 这类装进系统目录的东西。

缓存命中时**安装完全不需要网络**，NetworkPolicy 可以在这一段时间里拒掉全部出站。这也是它能替代镜像分发的根本原因：包在租户内，不用跨节点搬。

### 业内的另一种做法：代理缓存

文件共享之外，还有一类做法是把缓存做成 HTTP 服务：

| | 文件共享（本方案） | 代理缓存（Nexus / apt-cacher-ng / Athens / Verdaccio） |
|---|---|---|
| 形态 | RWX PVC | HTTP 服务 |
| 跨节点 | 依赖存储 | 天然 |
| 清理 | 自己写 | 内置 |
| 审计 | 无 | 有（谁拉了什么） |
| 租户隔离 | PVC 边界 | repository 边界 |
| 复杂度 | 低 | 多一个服务要运维 |

代理缓存的真正优势是**跨节点天然共享**和**能看到谁拉了什么**。文件共享在节点数少时更简单，节点多时存储带宽会成为瓶颈。

建议：**现在用文件共享，节点数超过 10 个或需要审计时再加一层代理**。两者可以共存——代理负责回源，文件缓存负责二次命中。

### 可以借鉴的一个机制

BuildKit 的 `RUN --mount=type=cache` 解决的是同一个问题，它的三个设计值得抄：

1. **默认不共享**——每个构建独立，必须显式 `id=` 才能跨构建共享
2. **`sharing=private|shared|locked`**——明确表达并发意图
3. **缓存不进入镜像层**——挂载点里的内容不会被打包

对应到我们：`private` = 每空间独立，`shared` = 租户内共享，`locked` = 共享 + 串行。现在选的是 `shared`。

### 不做的事

| 不做 | 原因 |
|---|---|
| 跨租户共享缓存 | 省下的空间远小于投毒和信息泄露的代价 |
| 缓存命中率监控 | 二期，先保证能用 |
| 自动淘汰 | 满了只是不省，不是故障 |

## 空间 shell 执行体

1. 空间用基础镜像启动，系统目录是干净的。agent 缺什么装什么，安装命中租户缓存。执行体重建后环境回到基础镜像，不重放。
2. agent 在空间容器内可以全局安装任意依赖，**直接装到系统目录**（`apt install` / `pip install` / `npm i -g`），不要求改用 `dpkg -x` 这类手段。装进可写层的东西 Pod 重建就丢，缺的时候再装一次——这是"允许正常安装"的代价，也是它成立的前提。系统工具**只有一个版本**，同一空间内多个插件需要不同版本的 `ffmpeg` 时无法共存，这是「一个空间一个容器」的固有代价，需要用 `mise` 之类做版本隔离。
3. shell 命令仍然走 bwrap，不是直接在容器里跑。

### 网络策略：按命令形态分三档

`shell_exec` 不再「默认断网」，网络由命令形态决定，三档互不兼任：

| 类别 | 判据（每段命令的首个真实命令名） | 网络 | 审批 |
|---|---|---|---|
| 网络工具 | `curl` / `wget` / `nc` / `ncat` / `socat` / `ssh` / `scp` / `sftp` / `rsync` / `telnet` / `ftp` / `/dev/tcp` | 有网 | **必须先审批；拒绝即不执行** |
| 脚本执行 | `sh` / `bash` / `zsh` / `python` / `node` / `deno` / `bun` / `ruby` / `perl` / `php` / `go` | 默认**无网**；调用方声明 `network=true` 后才审批、通过才开网 | 仅在声明联网时 |
| 其他命令 | `ls` / `grep` / `git` / `make` / `npm i` / `pip install` / `apt install` … | 有网 | 否（仍受「需确认命令」名单约束） |

三条理由：

- **网络工具是单一用途的外发通道，它只该在人看过之后执行。** 一条被注入的 `curl` 就能把会话内容拼进 URL 带出去，而命令原文是用户唯一能判断「里面有没有不该发的东西」的依据。所以它不是「批了才有网」，而是拒绝即停止。
- **脚本是「跑一段未知代码」，默认先别给网。** 绝大多数脚本任务是本地处理（统计、改名、跑测试），默认断网就能做完；真要联网时由调用方声明 `network=true`，用户看到命令原文再决定。声明之前脚本访问网络会失败，回执里带一句提示，模型据此决定要不要申请联网——这比「猜这段脚本里有没有 `requests`」可靠得多。
- **其他命令默认有网。** `git clone`、`make`、`npm i`、`apt install`、`pip install` 都不再需要模型猜「这次会不会因为断网失败」。边界从「不给网」移到「网络层封私网 + 命令级审批」，装依赖仍然不弹确认。

给不给网不影响文件可见面：三档都挂整个工作区，`curl -o out.json` 写下的文件就在当前目录，`curl … && python parse.py` 也能连着写。曾经有过一版「网络工具执行时把工作区换成空 tmpfs、只挂 `.deps`」的遮蔽方案，已经删掉——它换来的那点纵深，代价是下载的文件凭空消失、复合命令必须拆开，而真正拦住外发的是审批与 NetworkPolicy。

判据各管一段，互不兼任：

| 判据 | 决定 | 作用点 |
|---|---|---|
| `ShellNetPlan` | 给不给网 + 弹不弹确认 | `Spec.Network` + 审批动作 |
| `shellNeedsSystemWrite` | 放开不放开系统目录 | `Spec.Mode` |

复合命令按「任一段命中就整体升级」处理：`curl ... | sh` 里只要有一段是网络工具，整条命令就审批；反过来 `git clone ... && python build.py` 只有脚本段，按脚本档执行（默认无网，`network=true` 才审批联网）。

安装方把审批档位设成 `off` 时，命令名单与网络准入的确认都不再弹——这是显式例外，等于由安装方自己承担外发风险，卡片上会写明。

私网目标（`10/8`、`172.16/12`、`192.168/16`、`169.254/16`、`100.64/10`、IPv6 私网）与云元数据地址由容器内 iptables 与集群 NetworkPolicy 拒绝，不在应用层判——这一层与上面三档无关，任何一档都给不到私网。

### 运行时安装需要一个新的可写档位

镜像预装和运行时安装是两种动作，约束不同：

| | 镜像预装 | 运行时安装 |
|---|---|---|
| 谁做 | Dockerfile | agent 在 bwrap 内 |
| 目标 | `/usr/local` | `/usr`（可写层） |
| 生效范围 | 所有空间 | 整个执行体 |
| 怎么持久 | 镜像本身 | 执行体重建后重装一次，命中租户缓存 |

原有的三档（`read-only` / `workspace-write` / `full`）覆盖不了它，加了一档 `system-write`：

| 路径 | 权限 |
|---|---|
| `/usr` | 读写，`--bind` 而不是 `--ro-bind` |
| `/var/lib/dpkg` | 读写 |
| `/var/lib/apt/lists` | 读写，指向租户缓存 |
| `/var/cache/apt/archives` | 读写，指向租户缓存 |
| `/etc/alternatives`、`/etc/ld.so.conf.d` | 读写 |
| `/etc/apt` | 只读（`apt` 要读源列表） |
| 其余系统路径 | 只读 |

**共用可写层是刻意的，不是省事。**

这里不做"每个空间一份 `/usr`"。系统安装的持久化靠的是另一条链：

```
执行体重建 → 可写层消失 → 重跑安装命令 → 命中 /cache/apt/archives → 1～2 秒装回来
```

所以缓存里的 deb 才是持久保证，`/usr` 只是临时的当前状态。给每个空间铺一层 overlay 会把磁盘占用放大到「每个空间一份 `apt` 装过的所有包」，而换来的收益只是省掉一次 1～2 秒的重装。

代价要说清：**同一执行体上的空间共享 `/usr`**。一个空间 `apt remove libc6` 会影响其他空间。这个风险由审批兜住（`apt` 命令走人工确认），不由文件系统隔离。

`/etc` 不能整目录放开——`/etc/shadow`、`/etc/sudoers` 必须留在只读侧。

`--remount-ro /` 不用改：已 `--bind` 的目录是独立挂载，父级 remount 管不到它。这是这个档位能在现有实现上成立的技术前提。

`/var/lib/apt/lists` 和 `/var/cache/apt/archives` 在部署里挂到租户缓存，所以重建后的重装都不用联网。

### 安装需要注入哪些环境变量

shell 出网一律 `share-net` 直连，不建代理：`apt` / `pip` / `npm` 只认 `http://host:port`，给它们 unix socket 没有意义。私网边界交给集群 NetworkPolicy 与容器内 iptables，不在应用层判。

所以没有代理变量要补。真正要注入的是**路径类**变量，它们依赖空间 ID，父进程不知道，只能由 `shell_exec` 写进 `spec.Env`：

| 变量 | 值 | 缺了会怎样 |
|---|---|---|
| `NPM_CONFIG_PREFIX` | `<空间>/.pkg/npm` | 全局包装回系统目录，重建后丢 |
| `NPM_CONFIG_CACHE` | `/cache/npm` | 每次重下 |
| `VIRTUAL_ENV` | `<空间>/.pkg/venv` | pip 落到系统 python，撞 PEP 668 |
| `PIP_CACHE_DIR` | `/cache/pip` | 每次重下 |
| `PATH` | `venv/bin` + `npm/bin` + 基线 | 装了也找不到命令 |

`PATH` 必须以基线收尾：`--clearenv` 之后只拼私有目录，连 `/bin/sh` 都会找不到。

进程级只有一个：`WA_CACHE_DIR=/cache`，由部署配置传入，决定租户缓存的根。

一条通用规则：**依赖空间 ID 的变量只能写在 `spec.Env`**。`baseEnvKeys` 的语义是"从父进程继承"，而父进程不知道自己现在服务哪个空间。

### 审批内容要具体

弹窗至少给出：命令原文、将挂载什么、将访问哪个域名。只写一句"请注意数据泄露风险"没有用处——用户没有办法据此判断。

## 插件开发执行体

1. 插件只允许 Deno 脚本（`runtime.entry` 指向 `.ts`）。
2. 草稿目录就是开发执行体的根：`POST /spaces/{id}/plugins/{slug}/dev/exec` 在草稿目录里跑 shell，沙箱规则与空间 shell 同一套，只是把空间目录换成草稿目录——工作区是草稿，依赖进草稿的 `.pkg`。
3. 开发会话不做审批：命令是开发者在界面上敲的，不是模型自动决策。联网范围仍由 NetworkPolicy 兜底。
4. 开发时有网，`deno cache` 能跑通；发布前必须把 `deno.lock` 一并提交——运行期断网，没有 lock 就没法预热。

开发沙箱的能力与插件运行时是两回事：

| | 开发时 | 运行时 |
|---|---|---|
| 能起子进程 | 能 | **不能**（没有 `--allow-run`） |
| 能读写任意路径 | 能（执行体内） | 只能访问挂载进来的路径 |
| 网络 | 总是给网（人为操作） | 只有 socket 代理 |

这个差异必须在开发阶段就暴露，不能等到发布后才发现 `NotCapable`。现在有两道：

1. `dry-run` 接口：用真实权限跑一次，fs / net / 代理都和正式调用一致，只是 `OneShot`。
2. `Publish` 里强制跑 `evals.json` 的确定性用例，失败返回 409。没配用例的插件直接放行——门禁不能依赖外部模型可用。

## 插件运行执行体

1. 使用基础镜像直接起，**不重放任何东西**；依赖在安装期按 `deno.lock` 预热。**与空间 shell 是同一执行体上的不同沙箱实例**。
2. bwrap 无网，唯一出口是**执行体自己在容器内建的** unix socket 代理，按 `plugin.json` 白名单放行域名。控制面只把白名单过线（`Spec.Egress`）：容器之间不共享 `/tmp`，控制面建好的 socket 路径在执行体里根本不存在。
3. 权限由 `denoPermissionFlags` 从同一份 `fs` 白名单推导：`--allow-read` / `--allow-write` / `--allow-net` / `--allow-env`，**不给 `--allow-run` / `--allow-ffi`**，配 `--no-prompt` 让拒绝变成硬错误而不是等待交互。

### 挂载

| 路径 | 权限 | 说明 |
|---|---|---|
| 插件目录 | 只读 | 代码 + `deno.lock` |
| `DENO_DIR` | 可写 | `/cache/deno/<slug>@<version>`，只有 `gen/` 会写 |
| 空间工作区 | 只读 | 插件用 `wa.path()` + `Deno.readTextFile` 直连 |

空间工作区是**只读挂载**而不是不挂：文件改直连之后（`buildSandboxFS` 默认把 `spaceRoot` 放进读表），插件读业务数据不必经 RPC 搬一遍，而写仍然只能来自清单的 `fs.write`——越权写被挂载表与 `--allow-write` 两层同时挡住。之前「数据一律走 host SDK」的描述已经过时。

### 常驻进程

声明 `runtime.resident` 的插件在执行体里常驻，控制面与它之间只有一条双工连接：

| 项 | 实现 |
|---|---|
| 传输 | `POST /v1/procs`：请求体 = 4 字节长度 + JSON `Spec` + 上行 NDJSON，响应体 = 下行 NDJSON |
| 复用键 | `空间\|slug\|目录`，同一个键在执行体内只有一个进程 |
| 终止 | 任一方向关闭连接即杀进程；没有额外的心跳或注册表 |
| 空闲回收 | 控制面 `residentPool`（默认 10 分钟 / 上限 32），执行体不自己回收 |
| 崩溃恢复 | 流断开 → 控制面 `residentProc.done` → 下次调用重起一个 |

为什么不是「每插件一个容器」：常驻的收益是摊掉 Deno 50～180ms 冷启动，每插件一个 Pod 要等调度、挂镜像、建网络，冷启动回到秒级；`DENO_DIR` 也没法按 `slug@version` 跨空间共享。隔离仍然靠 bwrap——同一执行体里每个插件进程的挂载表、网络命名空间各不相同。

一个实现上的坑：`net/http` 在首次 `Flush` 时会尝试把请求体的剩余部分读掉（为了复用连接），而这里的请求体是持续的双向流，双方会一起挂住。客户端必须显式发 `Connection: close`，让服务端跳过这段清理——连接不复用正好符合「一条连接就是一个插件进程」的语义。

### Deno 的缓存在哪

Deno 所有下载物都在一个目录里，由 `DENO_DIR` 指定，默认 `$XDG_CACHE_HOME/deno`（即 `~/.cache/deno`）。Deno 2 的结构：

```
$DENO_DIR/
├── remote/        远程模块（https:// 的源码）
├── npm/           npm 包（npm: 说明符）
├── registries/    npm registry 元数据
└── gen/           编译产物（V8 快照、emit 缓存）
```

对插件运行体来说有两个问题。

**第一，默认位置在临时目录。** 沙箱里 `HOME` 是每次调用新建的临时目录，`DENO_DIR` 跟着落进去，每次调用从零开始。所以必须显式指定：

```bash
DENO_DIR=/cache/deno/<slug>@<version>
```

按版本分目录：插件升级后旧缓存自然失效，多个空间装同一版本则共用一份。

**第二，缓存不等于可离线运行。** 装过不等于断网能跑——Deno 发现缓存命中就直接用，但任何一项缺失时会尝试联网。运行体是 `--unshare-net`，联网请求只会变成超时。

所以运行期还要加 `--cached-only`：缺任何一项就立刻失败，而不是等超时。

### 依赖怎么带进来：只带 lock

发布包只含源码 + `deno.lock`，不含任何依赖产物。

```
插件包/
├── plugin.json
├── index.ts
├── deno.lock          ← 依赖的唯一声明
└── （源码）
```

`deno.lock` 记录每个依赖的版本与完整性 hash，是**可复现的最小信息**。安装时由控制面按它预热：

```bash
DENO_DIR=/cache/deno/<slug>@<version> \
  deno cache --frozen --lock=<plugin>/deno.lock <plugin>/index.ts
```

`deno cache` 的定位就是"只下载不执行"，等价于 `apt-get --download-only`。`--frozen` 保证 lock 与代码不一致时直接失败——作者改了依赖却忘了更新 lock，安装时就该报错，而不是静默装别的版本。

### 为什么不用 vendor

`deno vendor` 也能做到离线，但代价更高：

| | 只带 lock | 带 vendor |
|---|---|---|
| 发布包体积 | 几十 KB | 几 MB 到几十 MB |
| 需要 import map | 否 | 是 |
| 与 `--no-config` 兼容 | 是 | 否 |
| 依赖代码可审计 | 否 | 是 |

最后一行是 vendor 唯一的优势。由于运行体传了 `--no-config`，走 vendor 就必须由平台自己生成 import map，或者放开配置让作者控制——两条都不划算。

Deno 2 起 `deno vendor` 子命令已移除，改由 `deno.json` 的 `"vendor": true` 触发，"与 `--no-config` 冲突"因此更严重。

所以默认走 lock + 预热。只有当插件明确需要人工审计第三方源码时，才在发布阶段额外产出 vendor 快照并存档，运行期用 `--import-map` 显式指定——这是例外通道，不是默认。

### 三条锁一起才成立

| 位置 | 作用 | 缺了会怎样 |
|---|---|---|
| `deno.lock` | 声明版本 + 完整性 hash | 装到什么版本全靠运气 |
| `--frozen` | lock 与代码不一致就失败 | 声明和实际脱节，锁了个寂寞 |
| `--cached-only` | 缓存没命中就失败 | 缺依赖时联网，在 `--unshare-net` 下变成超时 |

三条一起，才有"同一插件在任何节点跑出同样结果"。

### 运行参数

```bash
deno run --quiet --no-prompt --no-config \
  --lock=<plugin>/deno.lock --frozen --cached-only \
  --allow-read=... --allow-write=... --allow-net --allow-env=... \
  bootstrap.ts file://<plugin>/index.ts
```

`DENO_DIR` 走环境变量，不占命令行。原来那条 `--no-lock` 必须去掉——它和 `--lock` 互斥，且在只读目录里"避免写锁"的顾虑已被 `--frozen` 解决（只读不写）。

### 插件没有系统依赖这回事

Deno 装包不做构建：不跑 `postinstall`、不调 `node-gyp`。所以不存在"插件依赖 gcc / make / python"这种情况。

| 包类型 | 表现 |
|---|---|
| 纯 JS/TS | 直接可用，零系统依赖 |
| 带 prebuilt `.node` | 直接加载二进制，不编译 |
| 只有源码、需 node-gyp | **不可用**，装 gcc 也救不回来 |

第三行是关键区别：阻碍它运行的并不是缺少工具，真正的问题是构建脚本永远不会被执行。这是 Deno 的设计选择，补装依赖解决不了。

需要原生能力的正确路径是 **WASM**（`ffmpeg.wasm`、`@db/sqlite`、`@jsquash/*`），`WebAssembly.instantiate` 不需要任何额外权限。

运行体也没有 `--allow-run` 和 `--allow-ffi`：`ffmpeg` 挂进来调不动，`.so` 挂进来 `dlopen` 不了。所以"环境包"对插件没有意义——不是不推荐，是不存在这个机制。

插件引入代码的唯一方式是 `deno.lock` + 安装期预热。运行期 `--cached-only`，缺什么立刻报错，不会偷偷联网。

顺带一句：`plugin.json` 里也确实没有 deps 字段。代码里的 `Deps` 是安全扫描算出的**依赖个数**（`internal/securityscan/report.go`），喂给 OSV 查漏洞用的，不是声明。

## 插件能力注入

### 能力边界必须由一个字段决定

插件的 `host` / `wa` 能力，现在要四层同时满足才可用：

| 层 | 内容 | 由谁提供 |
|---|---|---|
| 文件 | `bootstrap.ts` / `host_sdk.ts` | `installSDK()` 写入 `RuntimeDir` |
| 环境 | `WA_SPACE_DIR` / `WA_EGRESS_PROXY` | `Spec.Env` |
| 信道 | stdin/stdout 的 NDJSON 帧 | 执行体的 RPC 循环 |
| 校验 | method 白名单 | 宿主侧按 `capabilities` 查 |

问题在于：`shell_exec` 拿不到这些，**不是因为它被显式拒绝，而是因为它恰好没设这几个参数**。这种隐式隔离会在某次改动后失效。

改成显式门控：

```go
type Spec struct {
    // ...
    // HostRPC 非 nil 才注入 host/wa 与帧通道。
    // 这是插件能力的唯一入口。
    HostRPC *HostRPC
}

type HostRPC struct {
    RuntimeDir string   // bootstrap.ts / host_sdk.ts 所在目录
    Methods    []string // 允许的 method，宿主侧按此校验
    MaxCalls   int      // 单次调用的 RPC 次数上限
}
```

只有 `plugin/runtime_tool.go` 的 `procSpec` 会设置它。`internal/tool/shell.go` 在旁边写明：

```go
// 不设 HostRPC：shell 是哑执行，只回 stdout/stderr。
// 这不是省略，是设计——设了就等于给 shell 命令一条通往 agent 的 RPC。
```

这样"谁能用 host"变成 `grep HostRPC` 能查到的事实，而不是取决于某处有没有顺手传参。

### 能力清单

```json
{
  "runtime": {
    "host": {
      "log": true,
      "config": true,
      "secret": ["dingtalk"],
      "llm": { "max_calls": 5 },
      "memory": ["project-docs"],
      "ask_user": false
    }
  }
}
```

宿主收到 `host.call` 先查这张表，**没声明的直接拒绝并报错**，不返回空值。这是现有 `secret.get` 的做法，应推广到全部 method。

### 与 agent 交互的四个 API

| method | 语义 | 风险 | 阶段 |
|---|---|---|---|
| `progress` | 单向上报进度，不阻塞 | 无 | 一期 |
| `memory.search` | 检索当前空间记忆 | 越权读取 | 一期，按 `memory` 声明限定库 |
| `llm.complete` | 用 agent 的模型做一次推理 | 花钱、可滥用 | 一期，必须配额 |
| `ask_user` | 暂停 agent 循环等用户回答 | 整条链挂起 | **二期** |

`ask_user` 单独说：插件阻塞等输入，意味着从执行体到控制面到 UI 的整条链路都要支持"挂起再恢复"。现在的 `/v1/run` 是一次性 HTTP，做不到。

`llm.complete` 只返回文本，**不给工具调用能力**。否则插件能驱动 agent 再调别的插件，形成递归。

### SDK 不进镜像

| | 插件运行容器 | shell 容器 |
|---|---|---|
| deno | 有 | 可选 |
| bwrap | 有 | 有 |
| `host_sdk.ts` | **不进** | 不进 |
| 注入时机 | 运行时从 `RuntimeDir` 挂载 | 从不 |

好处：容器镜像泄露也不泄露协议；SDK 升级不用动镜像；shell 容器里根本不存在这个文件，`fs.Read` 配错也读不到。

`RuntimeDir` 建议改成**每次调用建在容器 `emptyDir` 的随机路径**，调用结束即删。稳定路径可猜测，随机路径成本很低。

### 必须修的门控

```go
if spec.Proxy != "" {
    env = append(env, "WA_EGRESS_PROXY="+spec.Proxy)
}
```

这段在通用路径上注入。现在 `shell_exec` 不设 `Proxy`，所以没泄漏。但将来若给 shell 也加代理，`WA_EGRESS_PROXY` 会自动出现在 shell 沙箱里。

而 `wa.fetch` **不依赖帧通道**，只需要这个环境变量加 socket。也就是 shell 里只要有一段 fetch 实现，就能绕过审批直接用白名单代理。

改法：

```go
if spec.Proxy != "" && spec.HostRPC != nil {
    env = append(env, "WA_EGRESS_PROXY="+spec.Proxy)
}
```

或更彻底：由 `procSpec` 通过 `spec.Env` 显式传入，不在通用路径注入。

### 探针要加五项

```
[PASS] 插件沙箱内 host 可用             → host.log 得到回执
[PASS] 插件沙箱内 wa.fetch 走代理        → 白名单外域名被拒
[PASS] shell 沙箱内 host 未定义          → typeof host === "undefined"
[PASS] shell 沙箱内无 WA_EGRESS_PROXY    → env 里没有
[PASS] shell 沙箱内 import host_sdk 失败 → ENOENT
```

后三项是真正的防线：只要它们通过，其他层的配置错误就不会变成能力泄漏。

## 不做什么

| 不做 | 原因 |
|---|---|
| 接受 agent 生成的 Dockerfile | 构建上下文会把空间数据打进镜像层；`RUN` 在构建机上以 root + 有网执行；镜像里的二进制不受 Deno 权限约束 |
| 为每个空间构建镜像（`ADD delta.tar` 或 `commit`） | 换来的只是 3～8 秒冷启动，代价是 builder、registry、GC、预热四条链路要长期运维 |
| 运行期无条件开放 `/usr` 写权限 | 沙箱是执行体内共享的，写 `/usr` 等于给所有沙箱一个跨租户的隐蔽信道，不需要网络就能传数据。现在只在 `apt` / `dpkg` 命令上开，并且走审批 |
| 插件运行期直连网络 | 白名单一旦绕过，`wa.fetch` 的约束全部失效 |

## 失败模式

| 现象 | 原因 | 处理 |
|---|---|---|
| `apt: cannot access /etc/apt` | 沙箱内 `/etc` 是白名单挂载，`/etc/apt` 已经不在了 | 把它加回 `bwrapBaselineRead`（当前已加） |
| `Temporary failure in name resolution`（脚本） | 脚本档默认无网 | 确认确实需要联网后带 `network=true` 重试，用户批准即可联网 |
| `NotCapable` | Deno 权限未开 | 检查 `plugin.json` 的 `fs` 声明，或跑 dry-run 复现 |
| `connection refused` | 代理未建或域名不在白名单 | 检查 `net.hosts` |
| `Read-only file system`（dpkg） | 命令没命中 `apt` / `dpkg` 判据，还在 `workspace-write` 里 | 用 `apt install` 而不是直接调底层工具 |
| `Unable to locate package` | 缓存里只有 deb，没有 `apt/lists` | 把 `/var/lib/apt/lists` 也挂到缓存 |

三类失败要能分开报：缺依赖是 `command not found`，白名单没开是 `NotCapable` / `connection refused`，命令写错是 exit code。混在一起会让排查成本翻倍。

## 资源与配额

| 项 | 值 | 说明 |
|---|---|---|
| 插件常驻回收 | **5 分钟** | 只有常驻 deno 进程有回收；沙箱是单次进程 |
| 单执行体内存 limit | 2Gi | 见 `deploy/local/10-runner.yaml` |
| 并发沙箱上限 | 按节点容量 | 超限排队，不直接失败 |
| 空间磁盘配额 | 每空间 5Gi（`WA_SPACE_QUOTA_MB`） | 含 `.pkg`（全局包 + venv 约 100MB） |
| 缓存保留 | 30 天（`WA_CACHE_TTL_DAYS`） | 只清 `deno/` 与 `apt/archives/*.deb` |
| 租户依赖缓存 | 每租户 20Gi | 80% 告警；写满会导致安装失败 |

**配额是软的，口径要说清**：执行前 `du` 统计一次（30 秒内复用结果），超了拒绝启动新命令。一条命令写超不会被打断，下一条才被拒。硬配额要文件系统（xfs prjquota）或 cgroup v2 配合，当前没做。

**缓存回收按 mtime，不按引用计数**：一个 30 天没调用过的插件，`DENO_DIR` 会被清掉，下次调用如果又断了网就会缺依赖。这比重装一个节点便宜，而且重新安装一次就会预热回来。

节点容量是唯一需要扩容的约束：空间数增长就加执行体副本，不需要改架构。多副本下每台有自己的 `/cache`，插件预热只在某一台生效——所以 `/cache` 要么用共享 PV，要么接受"第一台之后每个副本各预热一次"。

## 已定的参数

| 项 | 决定 |
|---|---|
| 容器密度 | 单执行体多沙箱；扩容加执行体副本 |
| 启动方式 | 沙箱按需起进程（毫秒级）；插件常驻进程空闲 5 分钟回收 |
| 插件运行执行体 | 与空间 shell 同一套 bwrap、独立 Spec；不挂空间目录，改不到别人的环境 |
| 系统安装 | `system-write` 档位直接 bind 执行体可写层；持久性靠 `/cache` 里的 deb 重装，不靠隔离 |
| 运行时管理 | `mise` 统一管理，基础镜像只预装 `deno` 与系统 `python3` |
| 环境持久化 | 基础镜像 + 空间级可写层 + 租户缓存；不重放、不构建镜像 |
| 安装方式 | `apt` 进空间可写层（持久）；`pip` / `npm -g` 进 `.pkg`（持久）；`node_modules` 在工作区（持久） |
| 磁盘配额 | 软配额，执行前 `du` 统计，默认 5Gi（`WA_SPACE_QUOTA_MB`） |
| 缓存回收 | 按 mtime 30 天（`WA_CACHE_TTL_DAYS`），只清 `deno/` 与 `apt/archives` |
| 插件开发执行体 | `POST /spaces/{id}/plugins/{slug}/dev/exec`，草稿目录当工作区，与空间 shell 同规则 |
| 插件依赖 | 只带 `deno.lock`，安装期 `deno cache` 预热到 `/cache/deno/<slug>@<version>`；运行期 `--cached-only`，不 vendor、不装环境包 |
| 语言包全局安装 | `NPM_CONFIG_PREFIX` / `VIRTUAL_ENV` 指向空间目录 `.pkg`，Pod 重建后不必重装；`PATH` 由 `installEnv` 写进 `spec.Env` |
| 租户缓存根 | `WA_CACHE_DIR`，默认 `/cache`，部署挂载；apt 索引与 deb、npm / pip / go 下载、`DENO_DIR` 都在下面 |
| Deno 依赖 | 只带 `deno.lock`；安装期 `deno cache --frozen` 预热到 `/cache/deno/<slug>@<version>`，运行期 `--cached-only` |
| pip 限制 | venv 已预置在 `.pkg/venv` 且在 `PATH` 最前，`pip install` 直接可用，绕开 PEP 668 |
| 基础镜像 | 钉 digest，从公共或已有镜像源拉，不自建 registry |
| 环境自述 | `/etc/wa/env.json` 只描述基础镜像（`os` / `arch` / `mise` / `python3` / `deno`），运行时不更新；其余能力由 agent 用 `shell_exec` 探测 |
| 依赖缓存 | 租户级共享（apt / mise / go / npm / pip / uv），租户间隔离 |
| 缓存回源 | 直连内部源；节点数过 10 或需要审计时加代理服务 |
