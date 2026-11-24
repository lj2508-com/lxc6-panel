# webmgr

> 轻量 LXC 管理面板 —— 单个 Go 二进制 + 可替换的网络插件。
> 系统监控、容器创建/配置/启停/删除、IPv6 地址分配，全部内嵌，服务器零运行时依赖。

面向 **1 核 / 1GB 内存 / 10GB 磁盘** 的小型 VPS：面板常驻约 11 MB 内存，每个 Alpine 容器约 20 MB 磁盘。

---

## 特性

- **单二进制**：6.9 MB，前端资源 `go:embed` 内嵌，服务器无需 Go/Node/Python
- **零安装脚本**：`webmgr setup` 一条命令完成初始化
- **纯 IPv6 容器**：容器只有 IPv6 地址，可指定入口/出口
- **插件化网络**：IPv6 打通方式做成插件，换服务商不用改代码
- **完整生命周期**：创建 → 改配置（内存热生效、磁盘扩容）→ 启停 → 删除
- **CLI 与 Web 双入口**：共用同一插件与配置，状态一致
- **轻量监控**：CPU/内存/磁盘/负载/服务状态/网络，采集耗时 ~0ms（缓存命中）

## 界面

三个标签页：

| 标签 | 内容 |
|---|---|
| 概览 | CPU（含曲线）、内存、磁盘、负载、服务状态、网络地址 |
| 容器 | 容器表格（状态/IPv6/内存/磁盘/端口/自启），点开看详情与操作 |
| 地址分配 | IPv6 池视图（在用 / 已注册待用 / 未注册） |

---

## 快速开始（零参数）

**直接运行，剩下的交给网页向导：**

```bash
apt-get update && apt-get install -y lxc e2fsprogs curl tar iproute2 util-linux

curl -fsSL -o /usr/local/bin/webmgr \
  https://github.com/<you>/webmgr/releases/latest/download/webmgr-linux-amd64
chmod +x /usr/local/bin/webmgr
webmgr                      # 不加任何参数
```

浏览器打开 `http://<服务器IP>:8099/`，页面会自动检测环境并进入**初始化向导**：

| 步骤 | 内容 |
|---|---|
| 1 · 环境检查 | 依赖是否齐全、是否 root、系统信息、IPv6 前缀探测 |
| 2 · 面板设置 | 标题、监听地址、访问令牌（**留空自动生成 32 位**） |
| 3 · 网络 | IPv6 前缀、网桥、网关、**自动检测并建议网络插件**、容器默认规格 |

点「开始初始化」后自动完成：建网桥 → 开 IPv6 转发 → 写配置 → 下载 Alpine 模板 → 调用插件 setup。

完成后浏览器自动获得令牌 cookie，直接进入面板。

> 已初始化过？再次打开页面直接进面板，不会重复引导。

### 1. 安装依赖（Debian / Ubuntu）

```bash
apt-get update
apt-get install -y lxc e2fsprogs curl tar iproute2 util-linux
```

### 2. 下载并初始化

```bash
# 从 Release 下载对应架构
curl -fsSL -o /usr/local/bin/webmgr https://github.com/<you>/webmgr/releases/latest/download/webmgr-linux-amd64
chmod +x /usr/local/bin/webmgr

# 初始化：建网桥、开 IPv6 转发、写配置、下 Alpine 模板
webmgr setup

# 启动（前台）
webmgr

# 或装成系统服务
webmgr setup --systemd
```

- **先看这个** → [docs/architecture.md](docs/architecture.md)（面板与插件的关系）
- **按 VPS 类型选方案** → [docs/deployment.md](docs/deployment.md)（A/B/C 类 IPv6 场景）

---

## 命令

```bash
webmgr                      # 启动面板（读 /etc/webmgr.conf）
webmgr setup [选项]          # 初始化
webmgr service install      # 装为 systemd 服务
webmgr service remove       # 移除服务
webmgr boot                 # 启动所有 start.auto=1 的容器（开机自启用）
webmgr version
webmgr help
```

### setup 选项

| 选项 | 说明 |
|---|---|
| `--systemd` | 初始化后顺便装服务 |
| `--plugin <路径>` | 启用网络插件 |
| `--token <串>` | 设置访问令牌（**强烈建议**） |
| `--listen <地址>` | 默认 `0.0.0.0:8099` |
| `--prefix <前缀>` | IPv6 前缀，留空自动探测 |
| `--bridge <名字>` | 容器网桥，默认 `lxcbr6` |
| `--gw <地址>` | 容器网关，默认 `fe80::1` |

---

## 插件：让容器的 IPv6 真正可达

面板本身**不关心** IPv6 怎么打通。它只调用一个插件：

```
<plugin> register   <ipv6>
<plugin> unregister <ipv6>
<plugin> status
<plugin> setup
```

环境变量传入上下文：`WM_ACTION` `WM_IP` `WM_NAME` `WM_BRIDGE` `WM_PREFIX`

**是否需要插件？**

```bash
ip -6 route
# 2400:xxxx::/48 dev eth0 proto kernel  → 需要（逐地址 NDP 解析型）
# 2400:xxxx::/64 via fe80::1 dev eth0   → 不需要（整段路由到本机）
```

仓库自带 `plugins/ndp-proxy`，用于第一种情况：

```bash
cp -r plugins/ndp-proxy /usr/local/lib/webmgr/plugins/
chmod +x /usr/local/lib/webmgr/plugins/ndp-proxy/webmgr-plugin.sh
/usr/local/lib/webmgr/plugins/ndp-proxy/webmgr-plugin.sh setup
webmgr setup --systemd --plugin /usr/local/lib/webmgr/plugins/ndp-proxy/webmgr-plugin.sh
```

换成自己的实现（桥接、nftables……）只需实现这 4 个命令，配置指过去即可，**Go 代码零改动**。

---

## CLI（可选）

`cli/lxc6.sh` 是纯 Bash 的命令行工具，与面板共用同一个插件：

```bash
install -m 0755 cli/lxc6.sh /usr/local/bin/lxc6

lxc6 setup
lxc6 list
lxc6 new web1 --mem 256 --disk 800 --start --auto
lxc6 set web1 --mem 512          # 内存热生效
lxc6 del web1 --yes
```

---

## 配置

`/etc/webmgr.conf`（`webmgr setup` 自动生成）：

```ini
TITLE=webmgr 控制台
LISTEN=0.0.0.0:8099
TOKEN=                      # 留空则不鉴权，公网部署务必设置
BRIDGE=lxcbr6
CONTAINER_GATEWAY=fe80::1
IPV6_PREFIX=                # 留空自动探测
NETWORK_PLUGIN=             # 留空 = 不用插件
PLUGIN_ENABLE=1
DEFAULT_MEM_MB=192
DEFAULT_DISK_MB=500
SERVICES=docker,fail2ban,ssh
DOCKER_CONTAINERS=
```

完整字段见 [examples/webmgr.conf.example](examples/webmgr.conf.example)。

---

## 自行编译

```bash
# 本机
make build            # 输出 build/webmgr-$(OS)-$(ARCH)

# 交叉编译（多架构）
make release          # 一次产出 linux/darwin/windows × amd64/arm64

# 或直接用 go
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o webmgr .
```

---

## 安全须知

1. **务必设置 `TOKEN`**，否则任何能访问 8099 端口的人都能管理你的容器
2. 更安全：只监听 `127.0.0.1`，通过 SSH 隧道访问
   ```bash
   ssh -L 8099:127.0.0.1:8099 root@your-server
   ```
3. 面板具备启停/删除容器能力，等同于部分服务器控制权
4. systemd 单元**不要**加 `ProtectSystem=strict` / `NoNewPrivileges=true` / 过小的 `MemoryMax`
   —— LXC 需要挂载 cgroup 与 rootfs，否则容器无法启动

---

## 目录结构

```
.
├── main.go            HTTP 服务、采集、路由
├── settings.go        配置解析
├── plugin.go          插件调用层
├── create.go          容器创建
├── config.go          容器配置读写
├── delete.go          容器删除
├── setup.go           setup / service 子命令
├── boot.go            开机自启
├── web/index.html     前端（内嵌）
├── plugins/ndp-proxy  网络插件示例
├── cli/lxc6.sh        Bash CLI
├── examples/          配置示例
└── docs/              架构与部署文档
```

---

## License

[MIT](LICENSE)
