# 部署指南

## 一、先判断你的 VPS 属于哪类

```bash
ip -6 route
```

| 输出特征 | 类型 | 需要插件 | 说明 |
|---|---|---|---|
| `2400:xxxx::/48 dev eth0 proto kernel` | A | **是** | 整段 IPv6 直接挂在网卡上，上游逐地址做 NDP 解析 |
| `2400:xxxx::/64 via fe80::1 dev eth0` | B | 否 | 整段路由到本机 MAC，容器自带可达性 |
| 无全局 IPv6 | C | 否 | 本项目的纯 IPv6 流程不适用（面板监控仍可用） |

一行判断：

```bash
ip -6 route | grep -q "dev eth0 proto kernel" && echo "A 类：需要插件" || echo "非 A 类"
```

---

## 二、安装

```bash
apt-get update
apt-get install -y lxc e2fsprogs curl tar iproute2 util-linux

curl -fsSL -o /usr/local/bin/webmgr \
  https://github.com/<you>/webmgr/releases/latest/download/webmgr-linux-amd64
chmod +x /usr/local/bin/webmgr
```

## 三、初始化

### B 类（不需要插件）

```bash
webmgr setup --systemd --token "$(openssl rand -hex 16)"
```

### A 类（需要插件）

```bash
# 1) 装插件
mkdir -p /usr/local/lib/webmgr/plugins/ndp-proxy
cp plugins/ndp-proxy/* /usr/local/lib/webmgr/plugins/ndp-proxy/
chmod +x /usr/local/lib/webmgr/plugins/ndp-proxy/webmgr-plugin.sh
/usr/local/lib/webmgr/plugins/ndp-proxy/webmgr-plugin.sh setup

# 2) 初始化面板并指向插件
webmgr setup --systemd \
  --plugin /usr/local/lib/webmgr/plugins/ndp-proxy/webmgr-plugin.sh \
  --token "$(openssl rand -hex 16)"
```

初始化会做：检查依赖 → 建网桥 → 开 IPv6 转发 → 写 `/etc/webmgr.conf` → 下载 Alpine 模板 → （可选）装 systemd 服务。

## 四、访问

`webmgr setup` 结束会打印地址；带 token 的形式：

```
http://<服务器IP>:8099/?t=<token>
```

首次访问后 cookie 记住 30 天，之后可省略 token。

---

## 五、B 类 VPS 的容器网络（两种做法）

### 做法 1：桥接（推荐）

把物理网卡与容器 veth 放进同一网桥，容器直接拿公网 IPv6：

```bash
ip link add br0 type bridge
ip link set br0 address "$(cat /sys/class/net/eth0/address)"
ip link set eth0 master br0
ip addr add <你的IPv4>/24 dev br0
ip addr add <你的IPv6>/64 dev br0
ip route add default via <IPv4网关> dev br0
ip -6 route add default via <IPv6网关> dev br0
```

然后配置：`BRIDGE=br0`、`CONTAINER_GATEWAY=<上游IPv6网关>`。

> ⚠️ 改网卡会短时断网，务必准备回滚手段（例如提前挂一个 5 分钟后执行的恢复脚本）。

### 做法 2：路由 + proxy_ndp（不动 eth0）

```bash
sysctl -w net.ipv6.conf.all.forwarding=1
sysctl -w net.ipv6.conf.all.proxy_ndp=1
ip -6 route add <容器地址>/128 dev lxcbr6
ip -6 neigh add proxy <容器地址> dev eth0
```

B 类场景下内核 proxy_ndp 通常即可工作，`NETWORK_PLUGIN` 留空。

---

## 六、日常运维

```bash
webmgr boot              # 手动触发容器自启（开机由 webmgr-boot.service 调用）
systemctl status webmgr
journalctl -u webmgr -f
systemctl restart webmgr # 改完配置

# 插件
/usr/local/lib/webmgr/plugins/ndp-proxy/webmgr-plugin.sh status
curl -s "http://127.0.0.1:8099/api/plugin?t=<token>"
```

### 容器镜像与磁盘

每个容器是一个独立的 ext4 镜像（默认 500 MB，稀疏）：

```bash
ls -lh /var/lib/lxc/<容器名>/rootfs.raw   # 名义大小
du -h  /var/lib/lxc/<容器名>/rootfs.raw   # 实际占用
```

---

## 七、故障排查

| 现象 | 排查 |
|---|---|
| 页面 401 | 配置了 TOKEN，访问需带 `?t=<token>` |
| 容器创建失败：无法挂载 rootfs | 检查 `TEMPLATE` 是否存在、`curl` 能否访问 dl-cdn.alpinelinux.org |
| 容器有 IPv6 但无法出网 | A 类：插件是否在跑（`webmgr-plugin.sh status`）；B 类：路由/桥接 |
| 外部无法访问容器 | 同上，多为回程路由问题 |
| 容器启动报 ABORTING | 检查镜像是否挂载、`/sbin/init` 是否存在；建议用 `loop:` 后端 |
| 删除容器后列表仍有残留 | 老版本缓存竞态，`systemctl restart webmgr` |
| 磁盘扩容失败 | 需先停止容器；确认装了 `e2fsprogs` |
| `lxc-autostart -L` 为空但配置是 1 | LXC 兼容问题；用 `webmgr boot` 替代 |
| 容器重启后没起来 | 检查 `webmgr-boot.service` 是否 enabled；`lxc.start.auto` 是否为 1 |

### 核心诊断命令

```bash
# A 类：看上游是否在问、本机是否在答
tcpdump -ni eth0 -vv 'icmp6[icmp6type]==135 or icmp6[icmp6type]==136'
journalctl -u ndp-responder -f

# 容器网络
lxc-info -n <名字> -iH
ip -6 route | grep <容器地址>
```

---

## 八、安全

1. **设置 TOKEN**，或只绑 `127.0.0.1` 用 SSH 隧道
2. 面板能力等同部分服务器控制权，不要裸奔公网
3. HTTPS 建议在前面挂 Caddy / Nginx 反代
4. systemd 单元**不要**加 `ProtectSystem=strict`、`NoNewPrivileges=true`、过小 `MemoryMax`
   —— LXC 需要挂载 cgroup 与 rootfs，加了会失败
