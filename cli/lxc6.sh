#!/bin/bash
# ==============================================================================
# lxc6 —— 纯 IPv6 LXC 容器管理 CLI（单文件，无依赖，不需要 web 面板）
#
#   lxc6 list                    列出容器（状态/地址/内存/磁盘/自启）
#   lxc6 new <名字> [选项]        新建容器
#   lxc6 del <名字>               删除容器
#   lxc6 start|stop|restart <名字>
#   lxc6 set <名字> [选项]        调整配置
#   lxc6 info <名字>              查看详情
#   lxc6 addr                     查看 IPv6 地址分配
#   lxc6 status                   宿主与容器总览
#   lxc6 setup                    首次初始化（网桥 / 转发 / 可选 NDP）
#   lxc6 web [二进制路径]         安装并启动 web 面板
#
# new / set 选项：
#   --ip <后缀|完整地址>   --mem <MB>   --disk <MB>   --pids <N>
#   --start                创建后立即启动
#   --auto on|off          开机自启（new 里写 --auto 表示开启）
#   --yes                  删除时跳过交互确认
#
# 配置文件：/etc/lxc6.conf（可用 LXC6_CONF 环境变量指定）
# ==============================================================================
set -u

CONF="${LXC6_CONF:-/etc/lxc6.conf}"

# ---------- 默认配置 ----------
LXCDIR=/var/lib/lxc
BRIDGE=lxcbr6
GATEWAY=fe80::1
PREFIX=
TEMPLATE=/var/lib/lxc/.alpine-rootfs.tar.gz
NETWORK_PLUGIN=
PLUGIN_ENABLE=1
NDP_CONF=/etc/ndp-responder.conf
DEFAULT_MEM=192
DEFAULT_DISK=500
IFACE=eth0

[ -f "$CONF" ] && . "$CONF"

# ---------- 输出 ----------
if [ -t 1 ]; then
  C_R=$'\033[31m'; C_G=$'\033[32m'; C_Y=$'\033[33m'; C_C=$'\033[36m'
  C_B=$'\033[1m';  C_D=$'\033[2m'; C_N=$'\033[0m'
else
  C_R=; C_G=; C_Y=; C_C=; C_B=; C_D=; C_N=
fi
info(){ printf '%s==>%s %s\n' "$C_C" "$C_N" "$*"; }
ok(){   printf '%s[✓]%s %s\n' "$C_G" "$C_N" "$*"; }
warn(){ printf '%s[!]%s %s\n' "$C_Y" "$C_N" "$*"; }
die(){  printf '%s[x]%s %s\n' "$C_R" "$C_N" "$*" >&2; exit 1; }

need_root(){ [ "$(id -u)" -eq 0 ] || die "需要 root 权限"; }

# ---------- 工具 ----------
detect_prefix(){
  [ -n "$PREFIX" ] && return
  local ip6
  ip6=$(ip -6 -o addr show scope global 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -1)
  if [ -n "${ip6:-}" ]; then
    PREFIX="$(echo "$ip6" | awk -F: '{print $1":"$2":"$3"::"}')"
  else
    PREFIX=""
  fi
}

full_addr(){ # $1 = 后缀或完整地址
  local a="$1"
  case "$a" in
    *:*) case "$a" in ::*) echo "${PREFIX}${a#::}";; *) echo "$a";; esac ;;
    *)   echo "${PREFIX}${a}" ;;
  esac
}

ct_names(){ lxc-ls -1 2>/dev/null | sort; }

ct_exists(){
  local n="$1"
  [ -n "$n" ] && [ -d "$LXCDIR/$n" ] && return 0
  return 1
}

ct_state(){ lxc-info -n "$1" -sH 2>/dev/null || echo "UNKNOWN"; }

cfg_get(){ # 从容器配置读数值
  local name="$1" key="$2"
  awk -v k="$key" '$0 ~ "^[ \t]*"k"[ \t]*=" {gsub(/^[^=]*=[ \t]*/,""); print; exit}' \
    "$LXCDIR/$name/config" 2>/dev/null
}

cfg_set(){ # 替换或追加配置项： cfg_set name key value
  local name="$1" key="$2" val="$3" f
  f="$LXCDIR/$name/config"
  local tmp; tmp=$(mktemp)
  if grep -qE "^[[:space:]]*${key//./\\.}[[:space:]]*=" "$f" 2>/dev/null; then
    awk -v k="$key" -v v="$val" '
      $0 ~ "^[ \t]*"k"[ \t]*=" { print k" = "v; next } { print }
    ' "$f" > "$tmp"
  else
    cat "$f" > "$tmp"; printf '%s = %s\n' "$key" "$val" >> "$tmp"
  fi
  cat "$tmp" > "$f"; rm -f "$tmp"
}

ct_ipv6(){ lxc-info -n "$1" -iH 2>/dev/null | grep -v fe80 | head -1; }

cfg_ipv6(){ # 从配置读容器地址
  awk -F'= *' '/lxc\.net\.[0-9]+\.ipv6\.address/ {print $2}' "$LXCDIR/$1/config" 2>/dev/null \
    | sed 's#/[0-9]*$##' | head -1
}

mem_limit_mb(){
  local b; b=$(cfg_get "$1" "lxc.cgroup2.memory.max")
  [ -n "${b:-}" ] && [ "$b" != "max" ] && echo $((b/1024/1024)) || echo "-"
}

cgroup_dir(){
  local n="$1" p
  for p in "/sys/fs/cgroup/lxc.payload.$n" "/sys/fs/cgroup/lxc/$n"; do
    [ -d "$p" ] && { echo "$p"; return; }
  done
  echo ""
}

human_mb(){ awk -v b="${1:-0}" 'BEGIN{printf "%.1f", b/1048576}'; }

img_used_mb(){
  local raw="$LXCDIR/$1/rootfs.raw" blocks bsize
  [ -f "$raw" ] || { echo "0"; return; }
  blocks=$(stat -c %b "$raw" 2>/dev/null || echo 0)
  bsize=$(stat -c %B "$raw" 2>/dev/null || echo 512)
  awk -v b="$blocks" -v s="$bsize" 'BEGIN{printf "%.1f", b*s/1048576}'
}

img_total_mb(){
  local raw="$LXCDIR/$1/rootfs.raw"
  [ -f "$raw" ] || { echo "-"; return; }
  awk -v b="$(stat -c %s "$raw")" 'BEGIN{printf "%.0f", b/1048576}'
}

ensure_template(){
  [ -f "$TEMPLATE" ] && [ "$(stat -c %s "$TEMPLATE" 2>/dev/null || echo 0)" -gt 1000000 ] && return 0
  info "下载 Alpine 模板（首次约 4MB）"
  mkdir -p "$(dirname "$TEMPLATE")"
  local base="https://dl-cdn.alpinelinux.org/alpine/latest-stable/releases/x86_64"
  local f
  f=$(curl -fsSL "$base/" 2>/dev/null | grep -oE 'alpine-minirootfs-[0-9]+\.[0-9]+\.[0-9]+-x86_64\.tar\.gz' | sort -u | tail -1)
  [ -z "${f:-}" ] && { warn "无法获取模板列表，请手动放到 $TEMPLATE"; return 1; }
  curl -fsSL -o "$TEMPLATE" "$base/$f" || { warn "模板下载失败"; return 1; }
  ok "模板就绪: $f"
}

# 网络插件：与 webmgr 面板共用同一套契约
plugin_ok(){
  [ "${PLUGIN_ENABLE:-1}" = "1" ] || return 1
  [ -n "${NETWORK_PLUGIN:-}" ] && [ -x "$NETWORK_PLUGIN" ]
}

call_plugin(){ # call_plugin <action> <ip> [name]
  local action="$1" ip="${2:-}" name="${3:-}"
  WM_ACTION="$action" WM_IP="$ip" WM_NAME="$name" \
  WM_BRIDGE="$BRIDGE" WM_PREFIX="${PREFIX:-}" \
    "$NETWORK_PLUGIN" "$action" ${ip:+"$ip"} 2>&1
}

register_ip(){ # 注册地址（优先交给插件，无插件时只加路由）
  local ip="$1" name="${2:-}"
  if plugin_ok; then
    call_plugin register "$ip" "$name" >/dev/null 2>&1 || {
      warn "网络插件注册失败: $ip（仅添加本地路由）"
      ip -6 route replace "$ip/128" dev "$BRIDGE" 2>/dev/null
    }
  else
    ip -6 route replace "$ip/128" dev "$BRIDGE" 2>/dev/null
  fi
}

unregister_ip(){
  local ip="$1" name="${2:-}"
  if plugin_ok; then
    call_plugin unregister "$ip" "$name" >/dev/null 2>&1 || true
  fi
  ip -6 route del "$ip/128" 2>/dev/null || true
}

next_free_suffix(){
  local used
  used=$(
    {
      [ -f "$NDP_CONF" ] && grep -v '^\s*#' "$NDP_CONF" 2>/dev/null
      for n in $(ct_names); do cfg_ipv6 "$n"; done
      ip -6 -o addr show scope global 2>/dev/null | awk '{print $4}' | cut -d/ -f1
    } | sed -n 's/.*:\([0-9a-fA-F]\{1,4\}\)$/\1/p' | while read -r h; do printf '%d\n' "0x$h"; done | sort -n | uniq
  )
  local n=16
  while echo "$used" | grep -qx "$n"; do n=$((n+1)); done
  printf '%x\n' "$n"
}

# ---------- 子命令 ----------
cmd_list(){
  detect_prefix
  local names; names=$(ct_names)
  if [ -z "$names" ]; then
    warn "没有容器。用 'lxc6 new <名字>' 创建一个。"
    return 0
  fi
  printf '%s\n' "${C_B}$(printf '%-12s %-10s %-5s %-26s %-13s %-17s %s' "名称" "状态" "自启" "IPv6" "内存(MB)" "磁盘(MB)" "端口")${C_N}"
  printf '%s\n' "${C_D}$(printf '%.0s─' {1..100})${C_N}"
  local n st auto ip6 mem dused dtotal ports cg memcur
  for n in $names; do
    st=$(ct_state "$n")
    auto="关"
    [ "$(cfg_get "$n" "lxc.start.auto")" = "1" ] && auto="开"
    ip6=$(ct_ipv6 "$n"); [ -z "$ip6" ] && ip6=$(cfg_ipv6 "$n")
    mem=$(mem_limit_mb "$n")
    dused=$(img_used_mb "$n"); dtotal=$(img_total_mb "$n")
    ports=""
    if [ "$st" = "RUNNING" ]; then
      ports=$(lxc-attach -n "$n" -- netstat -tlnu 2>/dev/null \
              | awk '$4 ~ /:[0-9]+$/ {split($4,a,":"); print a[length(a)]}' \
              | sort -un | head -4 | tr '\n' ',' | sed 's/,$//')
      cg=$(cgroup_dir "$n")
      if [ -n "$cg" ] && [ -r "$cg/memory.current" ]; then
        memcur=$(human_mb "$(cat "$cg/memory.current")")
        mem="${memcur}/${mem}"
      fi
    fi
    local color="$C_N"
    [ "$st" = "RUNNING" ] || color="$C_D"
    printf "${color}%-12s %-10s %-5s %-26s %-13s %-17s %s%s\n" \
      "$n" "$st" "$auto" "${ip6:--}" "${mem}" "${dused}/${dtotal}" "${ports:--}" "$C_N"
  done
  echo
  printf '%s共 %d 个容器 · 前缀 %s · 网桥 %s%s\n' "$C_D" "$(echo "$names" | wc -l)" "${PREFIX:-未探测}" "$BRIDGE" "$C_N"
}

cmd_addr(){
  detect_prefix
  info "IPv6 地址分配（前缀 ${PREFIX:-未探测}）"
  printf '%s%-26s %-12s %-12s %s%s\n' "$C_B" "地址" "状态" "容器" "说明" "$C_N"
  printf '%s%s%s\n' "$C_D" "$(printf '%.0s─' {1..80})" "$C_N"
  local regs="" ip holder
  regs=$([ -f "$NDP_CONF" ] && grep -v '^\s*#' "$NDP_CONF" 2>/dev/null)
  for ip in $(echo "$regs" | sed '/^$/d'); do
    holder=""
    for n in $(ct_names); do
      [ "$(cfg_ipv6 "$n")" = "$ip" ] && holder="$n"
    done
    if [ -n "$holder" ]; then
      printf '%-26s %s%-10s%s %-12s %s\n' "$ip" "$C_G" "在用" "$C_N" "$holder" ""
    else
      printf '%-26s %s%-10s%s %-12s %s\n' "$ip" "$C_Y" "已注册待用" "$C_N" "-" "无容器占用"
    fi
  done
  for n in $(ct_names); do
    ip=$(cfg_ipv6 "$n")
    [ -z "$ip" ] && continue
    if ! echo "$regs" | grep -qxF "$ip"; then
      printf '%-26s %s%-10s%s %-12s %s\n' "$ip" "$C_R" "未注册" "$C_N" "$n" "入站可能不通"
    fi
  done
  echo
  printf '下一个可用后缀: %s%s%s\n' "$C_C" "$(next_free_suffix)" "$C_N"
}

cmd_info(){
  local n="${1:-}"; [ -n "$n" ] || die "用法: lxc6 info <名字>"
  ct_exists "$n" || die "容器不存在: $n"
  local cg; cg=$(cgroup_dir "$n")
  echo
  printf '%s容器 %s%s\n' "$C_B" "$n" "$C_N"
  printf '  状态      %s\n' "$(ct_state "$n")"
  printf '  自启      %s\n' "$([ "$(cfg_get "$n" lxc.start.auto)" = 1 ] && echo 已开启 || echo 未开启)"
  printf '  IPv6      %s\n' "$(ct_ipv6 "$n" 2>/dev/null || echo -)"
  printf '  配置      %s\n' "$LXCDIR/$n/config"
  printf '  镜像      %s  (%s / %s MB)\n' "$LXCDIR/$n/rootfs.raw" "$(img_used_mb "$n")" "$(img_total_mb "$n")"
  printf '  内存上限  %s MB\n' "$(mem_limit_mb "$n")"
  printf '  进程上限  %s\n' "$(cfg_get "$n" lxc.cgroup2.pids.max)"
  if [ "$(ct_state "$n")" = "RUNNING" ] && [ -n "$cg" ]; then
    printf '  内存实际  %s MB\n' "$(human_mb "$(cat "$cg/memory.current" 2>/dev/null || echo 0)")"
    printf '  构成      anon %s MB / cache %s MB / kernel %s MB\n' \
      "$(awk '/^anon /{printf "%.1f", $2/1048576}' "$cg/memory.stat" 2>/dev/null)" \
      "$(awk '/^file /{printf "%.1f", $2/1048576}' "$cg/memory.stat" 2>/dev/null)" \
      "$(awk '/^kernel /{printf "%.1f", $2/1048576}' "$cg/memory.stat" 2>/dev/null)"
    printf '  端口      %s\n' "$(lxc-attach -n "$n" -- netstat -tlnu 2>/dev/null | awk '$4 ~ /:[0-9]+$/ {split($4,a,":"); print a[length(a)]}' | sort -un | tr '\n' ' ')"
  fi
  echo
}

cmd_new(){
  need_root; detect_prefix
  local name="" iparg="" mem="$DEFAULT_MEM" disk="$DEFAULT_DISK" do_start=0 auto=0 pids=512
  while [ $# -gt 0 ]; do
    case "$1" in
      --ip)    iparg="$2"; shift 2;;
      --mem)   mem="$2"; shift 2;;
      --disk)  disk="$2"; shift 2;;
      --pids)  pids="$2"; shift 2;;
      --start) do_start=1; shift;;
      --auto)  auto=1; shift;;
      *) if [ -z "$name" ]; then name="$1"; shift; else die "未知参数: $1"; fi;;
    esac
  done
  [ -n "$name" ] || die "用法: lxc6 new <名字> [--ip 后缀] [--mem MB] [--disk MB] [--start] [--auto]"
  echo "$name" | grep -qE '^[a-z][a-z0-9-]{1,30}$' || die "名称需小写字母开头，2-31 位，可含数字与连字符"
  ct_exists "$name" && die "容器已存在: $name"
  [ -n "${PREFIX:-}" ] || die "未探测到 IPv6 前缀，请先在 $CONF 里设置 PREFIX="

  iparg="${iparg:-$(next_free_suffix)}"
  local addr; addr=$(full_addr "$iparg")
  # 地址占用检查
  local dup
  dup=$({ [ -f "$NDP_CONF" ] && grep -v '^\s*#' "$NDP_CONF"; for n in $(ct_names); do cfg_ipv6 "$n"; done; } | grep -xF "$addr" || true)
  [ -n "$dup" ] && die "地址已被占用: $addr"

  info "创建容器 $name  (地址 $addr, 内存 ${mem}MB, 磁盘 ${disk}MB)"
  ensure_template || die "模板不可用"

  local dir rootfs raw
  dir="$LXCDIR/$name"; rootfs="$dir/rootfs"; raw="$dir/rootfs.raw"
  mkdir -p "$rootfs"

  info "1/6 建立磁盘镜像"
  truncate -s "${disk}M" "$raw" || die "创建镜像失败"
  mkfs.ext4 -q -F "$raw" || die "格式化失败"

  info "2/6 挂载并写入系统"
  mount -o loop "$raw" "$rootfs" || die "挂载失败"
  tar -xzf "$TEMPLATE" -C "$rootfs" || { umount "$rootfs"; die "解压模板失败"; }
  [ -f "$rootfs/sbin/init" ] || { umount "$rootfs"; die "模板缺少 /sbin/init"; }

  info "3/6 基础配置"
  echo "$name" > "$rootfs/etc/hostname"
  printf '127.0.0.1 localhost\n::1 localhost\n%s %s\n' "$addr" "$name" > "$rootfs/etc/hosts"
  printf 'auto lo\niface lo inet loopback\n\nauto eth0\niface eth0 inet manual\n' > "$rootfs/etc/network/interfaces"
  printf 'nameserver 2606:4700:4700::1111\nnameserver 2001:4860:4860::8888\n' > "$rootfs/etc/resolv.conf"
  umount "$rootfs" || warn "卸载失败，容器可能无法启动"

  info "4/6 生成 LXC 配置"
  cat > "$dir/config" <<EOF
lxc.include = /usr/share/lxc/config/common.conf
lxc.arch = amd64
lxc.uts.name = $name
lxc.rootfs.path = loop:$raw
lxc.mount.auto = proc:mixed sys:ro cgroup:mixed
lxc.apparmor.profile = unconfined
lxc.net.0.type = veth
lxc.net.0.link = $BRIDGE
lxc.net.0.flags = up
lxc.net.0.ipv6.address = $addr/128
lxc.net.0.ipv6.gateway = $GATEWAY
lxc.cgroup2.memory.max = $((mem*1024*1024))
lxc.cgroup2.memory.swap.max = 0
lxc.cgroup2.pids.max = $pids
lxc.start.auto = $auto
EOF

  info "5/6 注册地址与路由"
  register_ip "$addr" "$name"

  if [ "$do_start" = "1" ]; then
    info "6/6 启动容器"
    lxc-start -n "$name" -d || warn "启动失败，可稍后手动 lxc6 start $name"
    sleep 2
  fi

  echo
  ok "容器 $name 创建完成"
  echo "    地址: $addr"
  echo "    配置: $dir/config"
  [ "$do_start" = "1" ] && [ "$(ct_state "$name")" = "RUNNING" ] && ok "已启动"
  return 0
}

cmd_start(){
  local n="${1:-}"; [ -n "$n" ] || die "用法: lxc6 start <名字>"
  ct_exists "$n" || die "容器不存在: $n"
  lxc-start -n "$n" -d 2>&1 | grep -v '^$' || true
  sleep 2
  local ip; ip=$(cfg_ipv6 "$n"); [ -n "$ip" ] && register_ip "$ip" "$n"
  [ "$(ct_state "$n")" = "RUNNING" ] && ok "$n 已启动" || die "$n 启动失败"
}

cmd_stop(){
  local n="${1:-}"; [ -n "$n" ] || die "用法: lxc6 stop <名字>"
  ct_exists "$n" || die "容器不存在: $n"
  lxc-stop -n "$n" --timeout 5 2>/dev/null || true
  [ "$(ct_state "$n")" = "RUNNING" ] && lxc-stop -n "$n" -k 2>/dev/null
  sleep 1
  [ "$(ct_state "$n")" = "RUNNING" ] && die "停止失败" || ok "$n 已停止"
}

cmd_restart(){
  cmd_stop "$1"; sleep 1; cmd_start "$1"
}

cmd_del(){
  need_root
  local n="${1:-}" yes=0
  shift 2>/dev/null || true
  [ "${1:-}" = "--yes" ] && yes=1
  [ -n "$n" ] || die "用法: lxc6 del <名字> [--yes]"
  ct_exists "$n" || die "容器不存在: $n"
  if [ "$yes" -ne 1 ]; then
    warn "即将删除容器 $n（含磁盘镜像、地址注册与路由），不可恢复"
    printf '请输入容器名确认: '
    read -r ans
    [ "$ans" = "$n" ] || die "确认失败"
  fi
  local ip; ip=$(cfg_ipv6 "$n")
  [ "$(ct_state "$n")" = "RUNNING" ] && { info "停止容器"; cmd_stop "$n" >/dev/null; }
  [ -n "$ip" ] && unregister_ip "$ip" "$n"
  info "销毁容器"
  lxc-destroy -n "$n" -f >/dev/null 2>&1 || true
  if [ -d "$LXCDIR/$n" ]; then
    umount "$LXCDIR/$n/rootfs" 2>/dev/null || umount -l "$LXCDIR/$n/rootfs" 2>/dev/null || true
    rm -rf "$LXCDIR/$n"
  fi
  ok "容器 $n 已删除"
}

cmd_set(){
  need_root
  local n="${1:-}"; shift 2>/dev/null || true
  [ -n "$n" ] || die "用法: lxc6 set <名字> [--mem MB] [--disk MB] [--pids N] [--auto on|off]"
  ct_exists "$n" || die "容器不存在: $n"
  local mem="" disk="" pids="" auto="" changed=0
  while [ $# -gt 0 ]; do
    case "$1" in
      --mem)  mem="$2"; shift 2;;
      --disk) disk="$2"; shift 2;;
      --pids) pids="$2"; shift 2;;
      --auto) auto="$2"; shift 2;;
      *) die "未知参数: $1";;
    esac
  done
  local st; st=$(ct_state "$n")

  if [ -n "$mem" ]; then
    [ "$mem" -ge 32 ] && [ "$mem" -le 1024 ] || die "内存需在 32-1024 MB"
    cfg_set "$n" "lxc.cgroup2.memory.max" "$((mem*1024*1024))"
    if [ "$st" = "RUNNING" ]; then
      local cg; cg=$(cgroup_dir "$n")
      [ -n "$cg" ] && echo "$((mem*1024*1024))" > "$cg/memory.max" 2>/dev/null && ok "内存上限 ${mem}MB 已热生效"
    else
      ok "内存上限 ${mem}MB（下次启动生效）"
    fi
    changed=1
  fi

  if [ -n "$pids" ]; then
    cfg_set "$n" "lxc.cgroup2.pids.max" "$pids"
    if [ "$st" = "RUNNING" ]; then
      local cg; cg=$(cgroup_dir "$n")
      [ -n "$cg" ] && echo "$pids" > "$cg/pids.max" 2>/dev/null && ok "进程上限 $pids 已热生效"
    fi
    changed=1
  fi

  if [ -n "$auto" ]; then
    case "$auto" in
      on|1|true)  cfg_set "$n" "lxc.start.auto" "1"; ok "开机自启已开启";;
      off|0|false) cfg_set "$n" "lxc.start.auto" "0"; ok "开机自启已关闭";;
      *) die "--auto 只能是 on 或 off";;
    esac
    changed=1
  fi

  if [ -n "$disk" ]; then
    local cur; cur=$(img_total_mb "$n")
    [ "$disk" -ge "$cur" ] || die "磁盘只支持扩容（当前 ${cur}MB）"
    [ "$st" = "RUNNING" ] && die "磁盘扩容需要先停止容器：lxc6 stop $n"
    local raw="$LXCDIR/$n/rootfs.raw" rootfs="$LXCDIR/$n/rootfs"
    [ "$disk" -gt "$cur" ] && { truncate -s "${disk}M" "$raw" || die "扩容失败"; }
    mount -o loop "$raw" "$rootfs" 2>/dev/null || die "挂载失败"
    local dev; dev=$(findmnt -n -o SOURCE "$rootfs" 2>/dev/null)
    [ -z "${dev:-}" ] && dev=$(losetup -j "$raw" | head -1 | cut -d: -f1)
    [ -n "${dev:-}" ] && resize2fs "$dev" >/dev/null 2>&1
    umount "$rootfs"
    ok "磁盘已扩容至 ${disk}MB（文件系统已调整）"
    changed=1
  fi

  [ "$changed" -eq 0 ] && warn "没有指定要修改的项"
  return 0
}

cmd_status(){
  detect_prefix
  echo
  printf '%s宿主%s\n' "$C_B" "$C_N"
  printf '  主机    %s\n' "$(hostname)"
  printf '  内核    %s\n' "$(uname -r)"
  printf '  在线    %s\n' "$(uptime -p 2>/dev/null | sed 's/^up //')"
  printf '  负载    %s\n' "$(awk '{print $1", "$2", "$3}' /proc/loadavg)"
  printf '  内存    %s\n' "$(free -m | awk '/^Mem:/{printf "%d/%d MB (%.0f%%)", $3,$2,$3*100/$2}')"
  printf '  Swap    %s\n' "$(free -m | awk '/^Swap:/{printf "%d/%d MB", $3,$2}')"
  printf '  磁盘    %s\n' "$(df -h / | awk 'NR==2{printf "%s/%s (%s)", $3,$2,$5}')"
  printf '  前缀    %s\n' "${PREFIX:-未探测}"
  printf '  网桥    %s (%s)\n' "$BRIDGE" "$([ -d /sys/class/net/$BRIDGE ] && echo 存在 || echo 缺失)"
  if plugin_ok; then
    printf '  插件    %s\n' "$(call_plugin status "" 2>/dev/null | head -1)"
  else
    printf '  插件    %s\n' "未启用"
  fi
  echo
  cmd_list
}

cmd_setup(){
  need_root; detect_prefix
  info "初始化运行环境"

  info "1/5 检查依赖"
  local miss=0
  for c in lxc-create lxc-info lxc-destroy mkfs.ext4 tar; do
    command -v "$c" >/dev/null || { miss=1; warn "缺少 $c"; }
  done
  if [ "$miss" = "1" ]; then
    info "安装依赖（apt）"
    DEBIAN_FRONTEND=noninteractive apt-get update -qq && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq lxc e2fsprogs curl tar iproute2 util-linux >/dev/null
  fi

  info "2/5 网桥 $BRIDGE"
  ip link show "$BRIDGE" >/dev/null 2>&1 || { ip link add "$BRIDGE" type bridge; ok "已创建"; }
  ip link set "$BRIDGE" up
  ip -6 addr add "$GATEWAY/64" dev "$BRIDGE" scope link 2>/dev/null || true

  info "3/5 IPv6 转发"
  sysctl -qw net.ipv6.conf.all.forwarding=1
  sysctl -qw net.ipv6.conf.all.proxy_ndp=1
  cat > /etc/sysctl.d/99-lxc6.conf <<EOF
net.ipv6.conf.all.forwarding=1
net.ipv6.conf.all.proxy_ndp=1
EOF
  ip6tables -C FORWARD -i "$BRIDGE" -j ACCEPT 2>/dev/null || ip6tables -A FORWARD -i "$BRIDGE" -j ACCEPT
  ip6tables -C FORWARD -o "$BRIDGE" -j ACCEPT 2>/dev/null || ip6tables -A FORWARD -o "$BRIDGE" -j ACCEPT

  info "4/5 配置文件"
  if [ ! -f "$CONF" ]; then
    cat > "$CONF" <<EOF
# lxc6 配置（$(date '+%F %T') 生成）
PREFIX=$PREFIX
BRIDGE=$BRIDGE
GATEWAY=$GATEWAY
LXCDIR=$LXCDIR
TEMPLATE=$TEMPLATE
NETWORK_PLUGIN=$NETWORK_PLUGIN
PLUGIN_ENABLE=$PLUGIN_ENABLE
NDP_CONF=$NDP_CONF
DEFAULT_MEM=$DEFAULT_MEM
DEFAULT_DISK=$DEFAULT_DISK
EOF
    ok "已写入 $CONF"
  else
    warn "$CONF 已存在，未覆盖"
  fi

  info "5/5 模板"
  ensure_template || warn "模板未就绪（新建容器时会重试）"

  echo
  ok "初始化完成"
  [ -z "${NETWORK_PLUGIN:-}" ] && cat <<EOF
  ${C_Y}提示${C_N}：若容器起得来但外网访问不到，说明你的服务商需要逐地址 NDP 应答。
        安装 webmgr-plugin-ndp-proxy 插件后，在 $CONF 里加一行：
        NETWORK_PLUGIN=/usr/local/lib/webmgr/plugins/ndp-proxy/webmgr-plugin.sh
EOF
}

cmd_web(){
  need_root
  local bin="${1:-}"
  if [ -z "$bin" ]; then
    [ -x /usr/local/bin/webmgr ] || die "用法: lxc6 web /path/to/webmgr-linux-amd64"
    systemctl enable --now webmgr >/dev/null 2>&1
    ok "webmgr 已启动：$(systemctl is-active webmgr)"
    grep -E '^(LISTEN|TOKEN)=' /etc/webmgr.conf 2>/dev/null | sed 's/^/    /'
    return 0
  fi
  [ -f "$bin" ] || die "找不到文件: $bin"
  install -m 0755 "$bin" /usr/local/bin/webmgr
  [ -f /etc/webmgr.conf ] || cat > /etc/webmgr.conf <<EOF
LISTEN=0.0.0.0:8099
TOKEN=
BRIDGE=$BRIDGE
CONTAINER_GATEWAY=$GATEWAY
IPV6_PREFIX=$PREFIX
NETWORK_PLUGIN=$NETWORK_PLUGIN
EOF
  cat > /etc/systemd/system/webmgr.service <<'EOF'
[Unit]
Description=webmgr - lightweight LXC management panel
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/webmgr -conf /etc/webmgr.conf
Restart=always
RestartSec=3
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable --now webmgr >/dev/null 2>&1
  sleep 2
  ok "webmgr 已安装并启动：$(systemctl is-active webmgr)"
  local p; p=$(grep '^LISTEN=' /etc/webmgr.conf | cut -d: -f2)
  echo "    访问: http://$(hostname -I | awk '{print $1}'):${p:-8099}/"
}

cmd_help(){ sed -n '3,26p' "$0" | sed 's/^# \{0,1\}//'; }

# ---------- 入口 ----------
[ $# -eq 0 ] && { cmd_help; exit 0; }
CMD="$1"; shift
case "$CMD" in
  list|ls)            cmd_list ;;
  addr|addresses)     cmd_addr ;;
  info|show)          cmd_info "$@" ;;
  new|create)         cmd_new "$@" ;;
  start)              cmd_start "$@" ;;
  stop)               cmd_stop "$@" ;;
  restart)            cmd_restart "$@" ;;
  del|remove|rm)      cmd_del "$@" ;;
  set|edit)           cmd_set "$@" ;;
  status|st)          cmd_status ;;
  setup|init)         cmd_setup ;;
  web|panel)          cmd_web "$@" ;;
  -h|--help|help)     cmd_help ;;
  *) die "未知命令: $CMD
运行 'lxc6 help' 查看用法" ;;
esac
