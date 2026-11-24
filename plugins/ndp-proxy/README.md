# ndp-proxy —— webmgr 网络插件

让「逐地址 NDP 解析」型服务商下的容器 IPv6 地址变得可达。

## 何时需要

```bash
ip -6 route | grep -q "dev eth0 proto kernel" && echo "需要" || echo "不需要"
```

`dev eth0 proto kernel` 表示整段 IPv6 直接挂在网卡上，上游路由器会为每个地址单独做 NDP 解析。
Linux 内核的 proxy_ndp 在部分上游设备上不被采纳（只接受「NA 源地址 = 目标地址」的应答），
因此需要本插件在 AF_PACKET 层手工应答。

## 安装

```bash
mkdir -p /usr/local/lib/webmgr/plugins/ndp-proxy
cp webmgr-plugin.sh ndp_responder.py /usr/local/lib/webmgr/plugins/ndp-proxy/
chmod +x /usr/local/lib/webmgr/plugins/ndp-proxy/webmgr-plugin.sh
/usr/local/lib/webmgr/plugins/ndp-proxy/webmgr-plugin.sh setup
```

`setup` 会：装 python3（若无）→ 建网桥 → 开 IPv6 转发 → 装应答器 →
写 systemd 单元 `ndp-responder.service` → 启动。

## 启用（面板 / CLI 二选一或都配）

`/etc/webmgr.conf`：

```ini
NETWORK_PLUGIN=/usr/local/lib/webmgr/plugins/ndp-proxy/webmgr-plugin.sh
PLUGIN_ENABLE=1
```

`/etc/lxc6.conf`：

```ini
NETWORK_PLUGIN=/usr/local/lib/webmgr/plugins/ndp-proxy/webmgr-plugin.sh
PLUGIN_ENABLE=1
```

改完 `systemctl restart webmgr`。

## 契约

```
webmgr-plugin.sh register   <ipv6>     # 加 /128 路由 + 写入应答清单 + reload
webmgr-plugin.sh unregister <ipv6>     # 删路由 + 移出清单 + reload
webmgr-plugin.sh status                # 输出一行状态，健康时退出码 0
webmgr-plugin.sh setup                 # 初始化宿主
```

环境变量：`WM_ACTION` `WM_IP` `WM_NAME` `WM_BRIDGE` `WM_PREFIX`

注册清单文件：`/etc/ndp-responder.conf`（每行一个地址）。

## 手动测试

```bash
P=/usr/local/lib/webmgr/plugins/ndp-proxy/webmgr-plugin.sh
$P status
$P register 2400:xxxx::99
ip -6 route | grep ::99
cat /etc/ndp-responder.conf
$P unregister 2400:xxxx::99
```

## 卸载

```bash
systemctl disable --now ndp-responder
rm -f /etc/systemd/system/ndp-responder.service
rm -rf /usr/local/lib/webmgr/plugins/ndp-proxy
systemctl daemon-reload
```

## 原理

`ndp_responder.py` 用 AF_PACKET 抓取 eth0 上的 Neighbor Solicitation（目标地址在清单内），
然后构造 Neighbor Advertisement 回给请求方：

- **源地址 = 目标地址本身**（关键，部分上游设备强制要求）
- 标志位 `Solicited | Override`
- 附带 target link-layer address 选项（宿主网卡 MAC）
- 并为每个目标地址加入 solicited-node 组播组，确保能收到对应 NS
