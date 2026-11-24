# 架构：面板 + 插件

## 总体结构

```
┌─────────────────────────────────────────┐
│  webmgr（面板 / 主程序）                  │
│  · Go 单二进制，前端内嵌                  │
│  · 系统监控 + 容器生命周期管理             │
│  · 不知道 IPv6 如何打通                   │
└───────────────┬─────────────────────────┘
                │ 调用插件（子命令 + 环境变量）
                ▼
┌─────────────────────────────────────────┐
│  网络插件（可替换）                        │
│  · 让容器地址在上游路由器看来可达           │
│  · 仓库自带 ndp-proxy（逐地址 NDP 场景）   │
└─────────────────────────────────────────┘
```

面板侧调用点在 `plugin.go`，仅三处：

| 时机 | 调用 |
|---|---|
| 容器创建完成 / 启动 | `register <ipv6>` |
| 容器删除 | `unregister <ipv6>` |
| 打开面板 / 看状态 | `status` |

---

## 插件契约 v1

插件是**任意可执行文件**，实现 4 个子命令：

| 子命令 | 参数 | 退出码 |
|---|---|---|
| `register` | `<ipv6>` | 0 = 成功 |
| `unregister` | `<ipv6>` | 0 = 成功（失败不阻塞删除） |
| `status` | 无 | 0 = 健康 |
| `setup` | 无 | 0 = 成功 |

通过环境变量获知上下文：

```
WM_ACTION=register
WM_IP=2a0c:xxxx::11
WM_NAME=web1
WM_BRIDGE=lxcbr6
WM_PREFIX=2a0c:xxxx::
```

### 要求

- **幂等**：同一地址连续 `register` 三次，结果与一次相同
- **不阻塞**：插件失败不能让容器管理失败（面板只记警告）
- **stdout 简短**：`status` 的输出会直接显示在面板上

---

## 三种部署形态

### A. 普通 VPS（IPv6 整段路由到本机）

```bash
webmgr setup --systemd        # 不配 NETWORK_PLUGIN
```

容器靠服务商的路由天然可达，面板跳过注册步骤。

### B. 逐地址 NDP 解析型服务商

```bash
cp -r plugins/ndp-proxy /usr/local/lib/webmgr/plugins/
chmod +x /usr/local/lib/webmgr/plugins/ndp-proxy/webmgr-plugin.sh
/usr/local/lib/webmgr/plugins/ndp-proxy/webmgr-plugin.sh setup

webmgr setup --systemd --plugin /usr/local/lib/webmgr/plugins/ndp-proxy/webmgr-plugin.sh
```

### C. 自定义插件

```bash
#!/bin/bash
case "$1" in
  register)   # 例如把地址加入网桥、写 nftables 规则
              ;;
  unregister) ;;
  status)     echo "custom plugin ok"; exit 0 ;;
  setup)      ;;
esac
```

配置 `NETWORK_PLUGIN=/path/to/script` 即可接入，无需改动 Go 代码。

---

## 双入口共享

CLI `cli/lxc6.sh` 与面板调用**同一个插件**，因此：

```bash
$ lxc6 status
  插件    NDP 代理运行中 · 已注册 2 个容器地址 · 网桥 lxcbr6

$ webmgr        # 面板 /api/plugin 返回同样的字符串
```

---

## 开机自启链路

```
systemd → webmgr-boot.service → `webmgr boot`
                                    ↓
                        读每个容器 config 的 lxc.start.auto
                                    ↓
                          未运行且 auto=1 → lxc-start
                                    ↓
                          调用插件 register（幂等）
```

不依赖 `lxc-autostart`（部分 LXC 版本对 `lxc.start.auto` 的读取存在兼容问题）。

---

## 数据流

```
浏览器 ──GET /api/stats────────────► 快采集（读 /proc，微秒级）
       ──GET /api/containers──────► 慢采集（systemctl/lxc-info，4s 缓存 + 并发）
       ──GET /api/addresses───────► 地址池视图
       ──POST /api/container/create ► 建镜像 → 解压 → 写配置 → 插件注册 → 启动
       ──POST /api/container/config ► 改写 config + 写 cgroup（热生效）
       ──POST /api/container/delete ► 停止 → 插件注销 → lxc-destroy
```
