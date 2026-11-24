# 贡献指南

感谢你愿意改进 lxc6-panel。本文档说明如何报告问题与提交代码。

## 目录

- [报告问题](#报告问题)
- [报告安全问题](#报告安全问题)
- [开发环境](#开发环境)
- [代码约定](#代码约定)
- [提交信息](#提交信息)
- [Pull Request](#pull-request)
- [许可](#许可)
- [行为准则](#行为准则)

## 报告问题

新建 Issue 前请先搜索是否已有相同问题。提交时请包含：

- **环境**：`cat /etc/os-release`、`uname -r`、`lxc-info --version`、`webmgr version`
- **IPv6 类型**：`ip -6 route` 的输出
- **复现步骤**与期望结果
- **相关日志**：`journalctl -u webmgr -n 50`、`journalctl -u ndp-responder -n 50`

> ⚠️ 提交前请把 IPv6 前缀替换成 `2001:db8::/32`（文档专用测试前缀）。
>
> **请勿在公开 Issue 中粘贴**：访问令牌（`TOKEN`）、服务器公网 IP、真实 IPv6 前缀、任何密码。

## 报告安全问题

本面板可以启停与删除容器，等同于部分服务器控制权。**安全漏洞请勿公开提交 Issue**，请通过
GitHub 仓库的 Private vulnerability reporting 私下联系维护者。

## 开发环境

要求：**Go 1.21+**、**LXC 5.0+**、Linux（操作容器需要 root）。

```bash
git clone https://github.com/lj2508-com/lxc6-panel.git
cd lxc6-panel

go vet ./...
go build -trimpath -ldflags "-s -w" -o build/webmgr .

# 用独立配置与端口启动，避免影响已有部署
sudo ./build/webmgr -conf /tmp/test.conf -listen 127.0.0.1:8099
```

建议在**测试 VPS 或虚机**中开发。**不要在承载业务的生产机上测试删除容器**。

## 代码约定

### 1. 核心保持零第三方依赖

`go.mod` 目前没有任何 `require`。请优先使用标准库。

确需引入依赖时，请在 PR 中说明理由与体积影响 —— 目标是保持**单文件约 7 MB 的二进制**。

### 2. 插件契约 v1 不可破坏

面板与网络插件只通过以下 4 个子命令通信：

```
<plugin> register   <ipv6>
<plugin> unregister <ipv6>
<plugin> status
<plugin> setup
```

通过环境变量传入上下文：`WM_ACTION`、`WM_IP`、`WM_NAME`、`WM_BRIDGE`、`WM_PREFIX`。

改动**不得要求插件实现新的必需命令**。可以新增可选能力，但必须保证现有插件（包括第三方自己写的）继续可用。

### 3. 前端保持单文件

`web/index.html` 通过 `go:embed` 内嵌，样式与脚本全部内联。

**不要引入 CDN、外部字体、npm 构建链** —— 面板必须能在完全离线的服务器上正常显示。

### 4. 不要给 systemd 单元添加加固选项

以下选项都会导致 LXC 无法挂载 cgroup / rootfs，容器直接起不来（真实踩坑）：

- `ProtectSystem=strict`
- `ProtectControlGroups=true`
- `NoNewPrivileges=true`
- 过小的 `MemoryMax`（低于约 128 MB）

### 5. 容器 rootfs 使用 `loop:` 后端

`lxc.rootfs.path` 应使用 `loop:/path/to/rootfs.raw`，让 LXC 自己挂载镜像。
使用 `dir:` 后端且宿主未挂载时，会出现 `Failed to exec "/sbin/init"`。

### 6. 格式化与静态检查

```bash
gofmt -w *.go
go vet ./...
```

提交前请确保两者均无输出。

## 提交信息

使用 `type: subject` 形式：

```
feat: 容器创建支持自定义 IPv6 后缀
fix: 删除容器后列表缓存未刷新
docs: 补充 A 类服务商的插件说明
```

常用 type：`feat`、`fix`、`docs`、`refactor`、`perf`、`test`、`chore`、`ci`

## Pull Request

1. **一个 PR 只做一件事**
2. 说明动机与**验证方式**（跑了哪些命令、结果如何）
3. 界面改动请附截图：约 1440×900，**使用假数据**，前缀用 `2001:db8::`
4. 涉及容器生命周期或网络逻辑的改动，请说明在哪种服务商类型（A / B / C，见 README）上验证过
5. 不要提交 `webmgr.conf`、token、服务器 IP、真实 IPv6 前缀

## 许可

本项目以 [MIT](LICENSE) 授权。

**提交 Pull Request 即表示你同意你的贡献以 MIT 协议发布**，并确认你有权提交这些代码。

## 行为准则

保持礼貌、就事论事。不接受人身攻击、歧视性言论或恶意刷屏。
