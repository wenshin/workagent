#!/bin/sh
# 执行体入口：先封私网出站，再把进程交给 sandbox-agent。
#
# 为什么不能只靠 NetworkPolicy：OrbStack / k3d 默认没有 CNI，
# NetworkPolicy 会被 API Server 接受、但不执行——本地「看起来配了，实际没封」，
# 实测沙箱能直接访问同集群的其它 Pod。容器里有 NET_ADMIN，
# 这一层不依赖 CNI，本地与生产都成立。
#
# 只动 OUTPUT：入站（kubelet 探针、控制面调 8323）不受影响。
set -u

# 规则在 netns 里，Pod 重启容器时 netns 保留，直接 -A 会逐次累积。
# 用 -C 先查再加，保证多次启动后规则集不变。
add_rule() {
  iptables -w -C OUTPUT "$@" 2>/dev/null || iptables -w -A OUTPUT "$@" 2>/dev/null
}

block_private() {
  # DNS 必须先放行：kube-dns 的 ClusterIP 落在 10/8 里，
  # 不放行的表现是「域名全部解析失败，但 curl 打 IP 通」。
  add_rule -p udp --dport 53 -j ACCEPT || return 1
  add_rule -p tcp --dport 53 -j ACCEPT || return 1

  for cidr in \
    10.0.0.0/8 \
    172.16.0.0/12 \
    192.168.0.0/16 \
    169.254.0.0/16 \
    100.64.0.0/10 \
    198.18.0.0/15 \
    240.0.0.0/4; do
    # 只拒「主动发起」的 TCP（带 SYN 且不带 ACK）：--
    # 回包（SYN-ACK）没有纯 SYN 标志，不会被命中。这一条是不封到自己的关键——
    # 按目标网段整段 REJECT 会把 kubelet 探针的 SYN-ACK 一起打掉，
    # 表现是 Pod 一直 Running 但探针超时、被反复重启。
    add_rule -p tcp -d "$cidr" --syn -j REJECT || return 1
    # UDP 没有 SYN，只能按目标段拒；53 已在前面放行。
    add_rule -p udp -d "$cidr" -j REJECT || return 1
  done
  return 0
}

if block_private; then
  echo "wa: 已在容器内封禁私网出站（主动 TCP + UDP，DNS 除外）" >&2
else
  echo "wa: 容器内 iptables 不可用（缺少 NET_ADMIN 或内核不支持），私网封禁只剩 NetworkPolicy" >&2
fi

exec "$@"
