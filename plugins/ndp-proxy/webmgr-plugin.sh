#!/bin/bash
# ==============================================================================
# webmgr 网络插件 · ndp-proxy
#
# 作用：让「逐地址 NDP 解析」型服务商的 IPv6 前缀下的容器地址变得可达。
#   - 在宿主上运行 NDP 应答器，替容器地址回答上游的邻居请求
#   - 注册/注销 /128 路由
#
# 插件契约（webmgr 会这样调用本文件）：
#   webmgr-plugin.sh register   <ipv6>
#   webmgr-plugin.sh unregister <ipv6>
#   webmgr-plugin.sh status
#   webmgr-plugin.sh setup
#
# webmgr 通过环境变量传递上下文：
#   WM_NAME / WM_IP / WM_BRIDGE / WM_PREFIX / WM_ACTION
# ==============================================================================
set -u

DIR="$(cd "$(dirname "$0")" && pwd)"
CONF="${NDP_CONF:-/etc/ndp-responder.conf}"
BRIDGE="${WM_BRIDGE:-lxcbr6}"
RESPONDER=/usr/local/sbin/ndp-responder.py
SERVICE=ndp-responder

c_ok(){ printf '[plugin:ndp-proxy] %s\n' "$*"; }
c_err(){ printf '[plugin:ndp-proxy] 错误: %s\n' "$*" >&2; }

reload_service(){
  systemctl reload "$SERVICE" 2>/dev/null \
    || systemctl restart "$SERVICE" 2>/dev/null \
    || true
}

cmd_register(){
  local ip="${1:?register 需要 IPv6 地址}"
  # 1) 指向容器的 /128 路由
  ip -6 route replace "$ip/128" dev "$BRIDGE" 2>/dev/null \
    || { c_err "添加路由失败: $ip dev $BRIDGE"; return 1; }
  # 2) 加入 NDP 应答清单
  touch "$CONF"
  if ! grep -qxF "$ip" "$CONF"; then
    echo "$ip" >> "$CONF"
    reload_service
  fi
  c_ok "已注册 $ip"
  return 0
}

cmd_unregister(){
  local ip="${1:?unregister 需要 IPv6 地址}"
  ip -6 route del "$ip/128" 2>/dev/null || true
  if [ -f "$CONF" ] && grep -qxF "$ip" "$CONF"; then
    awk -v ip="$ip" '$0 != ip' "$CONF" > "$CONF.tmp" 2>/dev/null && cat "$CONF.tmp" > "$CONF"
    rm -f "$CONF.tmp"
    reload_service
  fi
  c_ok "已注销 $ip"
  return 0
}

cmd_status(){
  local active n
  active=$(systemctl is-active "$SERVICE" 2>/dev/null || echo unknown)
  n=$(grep -vc '^\s*#' "$CONF" 2>/dev/null || echo 0)
  if [ "$active" = "active" ]; then
    printf 'NDP 代理运行中 · 已注册 %s 个容器地址 · 网桥 %s\n' "$n" "$BRIDGE"
    return 0
  fi
  printf 'NDP 代理未运行（%s）· 清单 %s 个地址\n' "$active" "$n"
  return 1
}

cmd_setup(){
  [ "$(id -u)" -eq 0 ] || { c_err "setup 需要 root"; return 1; }

  # 依赖
  command -v python3 >/dev/null || DEBIAN_FRONTEND=noninteractive apt-get install -y -qq python3 >/dev/null

  # 应答器
  if [ -f "$DIR/ndp_responder.py" ]; then
    install -m 0755 "$DIR/ndp_responder.py" "$RESPONDER"
  elif [ ! -x "$RESPONDER" ]; then
    c_err "缺少 ndp_responder.py，无法安装"
    return 1
  fi

  # 网桥
  ip link show "$BRIDGE" >/dev/null 2>&1 || ip link add "$BRIDGE" type bridge
  ip link set "$BRIDGE" up

  # 转发 + 路由通告
  sysctl -qw net.ipv6.conf.all.forwarding=1
  sysctl -qw net.ipv6.conf.all.proxy_ndp=1
  cat > /etc/sysctl.d/99-webmgr-ndp.conf <<EOF
net.ipv6.conf.all.forwarding=1
net.ipv6.conf.all.proxy_ndp=1
EOF
  ip6tables -C FORWARD -i "$BRIDGE" -j ACCEPT 2>/dev/null || ip6tables -A FORWARD -i "$BRIDGE" -j ACCEPT
  ip6tables -C FORWARD -o "$BRIDGE" -j ACCEPT 2>/dev/null || ip6tables -A FORWARD -o "$BRIDGE" -j ACCEPT

  # 应答清单
  touch "$CONF"

  # systemd
  cat > "/etc/systemd/system/$SERVICE.service" <<EOF
[Unit]
Description=NDP responder for LXC containers (webmgr plugin ndp-proxy)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Environment=NDP_IFACE=${NDP_IFACE:-eth0}
Environment=NDP_CONF=$CONF
ExecStart=/usr/bin/python3 $RESPONDER
ExecReload=/bin/kill -HUP \$MAINPID
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable --now "$SERVICE" >/dev/null 2>&1
  sleep 1
  local st; st=$(systemctl is-active "$SERVICE" 2>/dev/null)
  [ "$st" = "active" ] && { c_ok "安装完成，$SERVICE 运行中"; return 0; }
  c_err "服务未能启动：$st"
  return 1
}

ACTION="${1:-status}"
shift 2>/dev/null || true
case "$ACTION" in
  register)   cmd_register "${1:-}";;
  unregister) cmd_unregister "${1:-}";;
  status)     cmd_status;;
  setup)      cmd_setup;;
  *) c_err "未知动作: $ACTION（可用: register|unregister|status|setup）"; exit 2;;
esac
