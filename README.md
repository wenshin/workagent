# WorkAgent

**企业级 Agent 工作台** —— 说一句话，Agent 就在你的空间里造出工具、技能和界面；权限继承身份，调用全程留痕。

---

## 这是什么

WorkAgent 不是"会聊天的助手"，它把 Agent 放进一个真实的文件空间：人和 Agent 在同一份目录上协作，说一句话就能长出技能、工具、数据连接，甚至一个能嵌进业务系统的界面——不用画流程，不用写代码。

对比通用助手，它解决的是**接得进、管得住**：Agent 以用户身份执行，权限天然继承，越权无从发生；工具按能力清单授权，高危动作二次确认，每次调用留痕可查。

对比低代码 Agent 平台，它**不需要先搭工作流**：流程是从对话和文件里长出来的，改起来也只是改文件。

底层能力可编程：配置、脚本、外部 MCP 三档同属一套插件模型，换实现不换接入方式；执行跑在沙箱里，私有化部署，数据不出内网。

> 让企业像用 Office 一样用 Agent —— 在自己的文件里干活，每一步都可控、可查。

## 定位

| 类型 | 他们解决的 | WorkAgent 解决的 |
|---|---|---|
| 通用助手 | 让 AI 好用 | 让 AI **进得了内网、有权限边界** |
| 低代码 Agent 平台 | 让人把流程搭出来 | 让**对话和文件**长出流程，改文件即改流程 |
| 通用 Agent 产品 | 单点任务自动化 | **企业协作 + 治理**（身份、审批、审计、沙箱） |

## 核心能力

- **空间即工作区**：空间是权限边界，目录是真实文件目录；Agent 与人在同一份文件上协作，不需要额外的项目/团队概念。
- **对话即生产**：在会话里创建技能、工具、数据库、MCP 连接、UI 组件；产出的组件可直接在消息内渲染，也能嵌入业务页面。
- **企业级治理**：Agent 以用户身份执行；四级权限（view / read / write / manage）；高危操作二次确认；审计永久保留。
- **能力可编程**：`skill` / `tool` / `mcp` / `component` 统一为插件，走同一条「开发 → 发布 → 安装 → 运行 → 卸载」链路；风险由能力清单推导，不由作者声明。
- **安全执行**：命令与脚本跑在 `bwrap` 沙箱内，默认无网络；沙箱里只有 `/workspace`、`/pkg`、`/cache` 这类虚拟路径，模型与环境变量看不到宿主目录；密钥以句柄形式下发，插件进程拿不到明文。
- **私有化就绪**：K8s 私有化 + SaaS 多租户同一套代码，可替换模型供应商与国产化依赖。

## 架构速览

```
Web / CLI / Electron
        │  REST + SSE
        ▼
┌──────────────────────────────────────────┐
│ 能力网关：身份 → 权限 → 审批 → 审计 → 配额 │
└──────────────────────────────────────────┘
        │
        ├── Agent Harness（ReAct / Direct）
        ├── 能力层：内置工具 · 插件（Deno）· MCP 客户端 · 技能
        └── 资源层：空间文件 · 数据库 · 凭证
```

控制面与数据面分离；一切皆资源，资源是权限、凭证、审计的统一锚点。

## 快速开始

执行面（沙箱）只有一种形态：**控制面自己建容器**——每空间一个空间容器、每租户一个插件容器。控制面进程不执行任何命令，所以**任何形态都必须先有一个 k8s 集群**，本机调试也不例外。

### macOS + OrbStack（本地开发）

OrbStack 的 k8s 与宿主机共享文件系统，`hostPath` 不需要 `extraMounts`；用 `docker` 构建的镜像对 k8s 直接可见，不需要 `kind load`。

```bash
# 0. 集群（已开启可跳过）：OrbStack 设置里勾选 Kubernetes，或用 CLI
orb start k8s
kubectl config use-context orbstack

# 1. 配置：至少填写 LLM_API_KEY
cp .env.example .env

# 2. 启动依赖服务
make pg-up            # PostgreSQL
make redis-up         # Redis（登录态与会话流缓存）

# 3. 拉齐依赖
make tidy

# 4. 准备执行面：构建 linux 执行体镜像，部署命名空间 / 共享存储 / RBAC / 网络策略
#    执行体不是常驻 Deployment，由控制面按「每空间一个 + 每租户一个」创建 Pod。
#    .env 里的 STORAGE_ROOT（默认 ./data/storage）会被渲染成 hostPath。
make sandbox-up
# 部署后验：能跑、能写自己空间、写不了 /etc、断网、宿主路径不可见、超时生效
make sandbox-check

# 5. 启动 API（127.0.0.1:8321）
#    kubeconfig 自动取 $KUBECONFIG / ~/.kube/config；指向别的集群时设 SANDBOX_KUBECONFIG。
#    令牌已在 .env.example 里预置为本地默认值（与 00-namespace.yaml 的 Secret 一致）。
export SECRET_KEK=C7tPoV6eFsnNrZykBMLlBhjkJcYgWe+w7YSeOHflvkE=   # 仅本地开发；生产用 KMS / ExternalSecret
make run

# 6. 另开一个终端启动前端
cd web && npm install && npm run dev
```

首次启动会自动建表、初始化系统空间与内置技能。启动成功的标志是日志里出现：

```
容器化执行面已就绪 namespace=workagent image=workagent/sandbox-runner:dev
执行面已就绪 role=server transport=k8s
```

`make sandbox-check` 失败说明这套集群不能上线——它查的是「能不能上线」，不是「能不能起来」。

启动失败先看这几条：

| 报错 | 处理 |
|---|---|
| `没有可用的集群凭据` | `kubectl config current-context` 应有值；否则显式设 `SANDBOX_KUBECONFIG=/path/to/kubeconfig` |
| `k8s 执行面缺少令牌` | `SANDBOX_RUNNER_TOKEN` 必须与集群里的 Secret 一致（本地默认 `dev-token-change-me`） |
| `persistentvolumeclaim "workagent-storage" not found` | 没跑 `make sandbox-up`，或其中的存储卷没建起来 |
| `dial tcp 192.168.x.x:8323: connect: no route to host` | 直连 Pod IP 不通，控制面会自动改用「经 API Server 的端口转发」；两条都不通时看 `kubectl -n workagent get pod` 与 `kubectl -n workagent get --raw /readyz` |

### Linux：本地集群（k3d / kind）

与 macOS 的区别只有两点：宿主机目录要显式挂进节点，镜像要显式导入。

```bash
# 0. 建集群；挂载路径必须与 .env 里的 STORAGE_ROOT（默认 ./data/storage）一致
mkdir -p data/storage
k3d cluster create wa --volume "$PWD/data/storage:$PWD/data/storage"
kubectl config use-context k3d-wa
# kind 用 --config 加 extraMounts（样例见 deploy/local/12-storage.yaml 顶部注释）：
# kind create cluster --name wa --config kind.yaml

# 1-3. 同 macOS：cp .env.example .env / make pg-up / make redis-up / make tidy

# 4. 构建 + apply，再把镜像导进集群（不导入会 ImagePullBackOff）
make sandbox-up
k3d image import workagent/sandbox-runner:dev -c wa      # kind: kind load docker-image <镜像> --name wa
make sandbox-check

# 5-6. 同 macOS：make run，前端另开终端 npm run dev
```

### Linux：生产集群

前提：集群允许 `privileged`（执行体要 `SYS_ADMIN` / `NET_ADMIN` / `Unconfined`）；生产集群建议与执行集群分开，理由见「两套集群」。

```bash
REG=registry.internal/wa

# 1. 构建并推执行体镜像（x86 节点用 GOARCH=amd64）
GOARCH=amd64 IMAGE=$REG/sandbox-runner:v1 scripts/sandbox-exec.sh build
docker push $REG/sandbox-runner:v1

# 2. 命名空间（PSA=privileged）与令牌：把开发默认令牌换成随机值
kubectl apply -f deploy/local/00-namespace.yaml
kubectl -n workagent create secret generic sandbox-runner-token \
  --from-literal=token="$(openssl rand -hex 32)" --dry-run=client -o yaml | kubectl apply -f -

# 3. 共享存储：控制面与执行体必须是同一个 RWX 卷
#    已有 NFS / CephFS / EFS：直接建名为 workagent-storage 的 PVC（或改 SANDBOX_STORAGE_PVC）
#    走动态供给：设 SANDBOX_STORAGE_CLASS；但控制面自己仍要挂这个卷（见下）

# 4. 控制面建 Pod 的权限：Role 限定在 workagent 命名空间，碰不到节点
kubectl apply -f deploy/local/05-rbac.yaml
# 执行体出网封禁：需要 CNI 支持 NetworkPolicy，不支持时这条不生效（但不阻断部署）
kubectl apply -f deploy/local/15-runner-netpol.yaml

# 5. 私有仓库：执行体 Pod 用 default ServiceAccount，没有 imagePullSecrets 字段
kubectl -n workagent create secret docker-registry regcred \
  --docker-server=$REG --docker-username=<user> --docker-password=<pass>
kubectl -n workagent patch serviceaccount default \
  -p '{"imagePullSecrets":[{"name":"regcred"}]}'

# 6. 部署控制面（把下面的骨架存成 controlplane.yaml），再跑部署后验
kubectl apply -f controlplane.yaml
IMAGE=$REG/sandbox-runner:v1 scripts/sandbox-exec.sh check
```

控制面镜像自行打包：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/server ./cmd/server`，放进你们的基础镜像即可（静态二进制，`distroless/static` 一类就够；前端按 `make web` 单独构建上传）。

关键环境变量：

| 变量 | 生产取值 |
|---|---|
| `WA_ROLE` / `SANDBOX_BACKEND` | `server` / `k8s` |
| `SANDBOX_NAMESPACE` | `workagent` |
| `SANDBOX_IMAGE` | 推上去的执行体镜像，建议钉 digest |
| `SANDBOX_STORAGE_PVC` | `workagent-storage`（与上面 PVC 同名） |
| `STORAGE_ROOT` | 控制面里挂载该 PVC 的路径，示例用 `/var/lib/workagent` |
| `SANDBOX_CACHE_SHARED` | `true`（集群支持 RWX 时；本地 kind/local-path 保持 `false`） |
| `SANDBOX_STORAGE_CLASS` | RWX 的 StorageClass；为空会落到集群默认类（通常不是 RWX，缓存卷会 Pending） |
| `SANDBOX_RUNNER_TOKEN` | 从 `sandbox-runner-token/token` 注入，与第 2 步一致 |
| `SANDBOX_KUBECONFIG` | 留空：Pod 内走 in-cluster ServiceAccount |

控制面 Deployment 的骨架（数据库、Redis、LLM 等其余 env 按 `.env.example` 补齐）：

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: workagent
  namespace: workagent
spec:
  replicas: 1
  selector:
    matchLabels: {app: workagent}
  template:
    metadata:
      labels: {app: workagent}
    spec:
      serviceAccountName: wa-controlplane      # 05-rbac.yaml 创建
      containers:
        - name: server
          image: registry.internal/wa/workagent:v1
          ports:
            - containerPort: 8321
          env:
            - {name: WA_ROLE, value: server}
            - {name: SANDBOX_BACKEND, value: k8s}
            - {name: SANDBOX_NAMESPACE, value: workagent}
            - {name: SANDBOX_IMAGE, value: "registry.internal/wa/sandbox-runner:v1"}
            - {name: SANDBOX_STORAGE_PVC, value: workagent-storage}
            - {name: SANDBOX_CACHE_SHARED, value: "true"}
            - {name: STORAGE_ROOT, value: /var/lib/workagent}
            - name: SANDBOX_RUNNER_TOKEN
              valueFrom:
                secretKeyRef: {name: sandbox-runner-token, key: token}
          readinessProbe:
            httpGet: {path: /healthz, port: 8321}
          volumeMounts:
            - {name: storage, mountPath: /var/lib/workagent}
      volumes:
        - name: storage
          persistentVolumeClaim: {claimName: workagent-storage}
```

`STORAGE_ROOT` 与执行体的 `/var/lib/workagent` 指向同一个卷即可，绝对路径不需要一致——过线的是相对 key。

**执行面由运行形态决定，开发与生产一视同仁**：`WA_ROLE=server`（默认）只能建容器（`SANDBOX_BACKEND=k8s`）——控制面按空间 / 租户建 Pod，执行体每次握手都校验隔离后端，没有「本机调试就裸跑」这条降级；客户端形态（`WA_ROLE=client`）只接受本进程的 `bwrap`；`WA_ROLE=dev` 已删除，启动即报错。细节见 [K8s 容器沙箱方案](docs/K8s容器沙箱方案.md)「运行形态与执行面」。

```bash
make test     # 单元测试
make build    # 编译到 bin/server
make web      # 构建前端到 web/dist
```

## 目录结构

```
cmd/server        服务入口
internal/
  agent/          Harness 与编排循环
  tool/           工具、能力注册、Invoker
  sandbox/        命令执行沙箱（bwrap / local，路径虚拟化）
  search/         搜索 provider 与脚本搜索源
  mcp/            MCP 客户端与管理器
  policy/         策略引擎与风险模型
  space/          空间、文件、索引、系统空间
  store/          持久层（Postgres）
  platform/       HTTP、SSE、会话
web/              前端（React + Vite + Semi UI）
docs/             技术与产品文档
```

## 文档

| 文档 | 内容 |
|---|---|
| [技术方案](docs/技术方案.md) | 架构分层、领域模型、接口约定、安全与部署 |
| [MVP 版本功能](docs/MVP版本功能.md) | 范围、功能清单、验收与已知缺口 |
| [插件体系设计](docs/插件体系设计.md) | 四类插件的清单、生命周期、权限与运行时 |
| [本地空间与双端架构](docs/本地空间与双端架构.md) | `space.type=local`、本地 runtime、网关路由与数据边界（设计稿） |

## 状态

MVP 开发中。技能、文件、会话、工具调用、MCP、沙箱与脚本搜索源已落地；插件体系已落地——`skill` / `tool` / `mcp` / `component` 共用 `plugin.json`、风险由能力清单推导，走同一条「开发 → 发布 → 安装 → 运行 → 卸载」链路，存量技能启动时一次性迁移到 `files/plugins/<slug>/`。组件以声明式 DSL（`wa-ui/1`）在会话里由宿主原生渲染，没有可执行代码、不用 iframe，数据请求经宿主转成工具调用。已知缺口与补验方式见 [MVP 文档 §2.2.1](docs/MVP版本功能.md)。
