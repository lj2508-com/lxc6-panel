// 内建的初始化与服务安装：不需要外部 install.sh
//
//	webmgr setup   [--systemd] [--plugin PATH] [--token S] [--listen A] [--prefix P]
//	webmgr service install|remove
//	webmgr version
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const unitPath = "/etc/systemd/system/webmgr.service"

func isRoot() bool { return os.Geteuid() == 0 }

func execQuiet(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func runSetup(args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	withSystemd := fs.Bool("systemd", false, "同时安装并启动 systemd 服务")
	plugin := fs.String("plugin", "", "网络插件路径（写入配置）")
	token := fs.String("token", "", "访问令牌")
	listen := fs.String("listen", "", "监听地址，默认 0.0.0.0:8099")
	prefix := fs.String("prefix", "", "IPv6 前缀，留空自动探测")
	bridge := fs.String("bridge", "", "容器网桥，默认 lxcbr6")
	gw := fs.String("gw", "", "容器网关，默认 fe80::1")
	quiet := fs.Bool("quiet", false, "精简输出")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	say := func(f string, a ...any) {
		if !*quiet {
			fmt.Printf(f+"\n", a...)
		}
	}

	if !isRoot() {
		fmt.Println("错误：setup 需要 root 权限")
		return 1
	}

	// 读取已有配置（幂等）
	_ = loadSettings(*confPath)
	bindGlobals()

	if *listen != "" {
		cfg.Listen = *listen
	}
	if *token != "" {
		cfg.Token = *token
	}
	if *bridge != "" {
		cfg.Bridge = *bridge
	}
	if *gw != "" {
		cfg.ContainerGW = *gw
	}
	if *prefix != "" {
		cfg.Prefix = *prefix
	}
	if *plugin != "" {
		cfg.NetworkPlugin = *plugin
	}
	if strings.TrimSpace(cfg.Prefix) == "" {
		cfg.Prefix = prefixOf()
	}

	// 1) 依赖检查
	say("==> 1/6 检查依赖")
	missing := []string{}
	for _, c := range []string{"lxc-create", "lxc-info", "lxc-destroy", "mkfs.ext4", "tar", "curl", "findmnt"} {
		if _, err := exec.LookPath(c); err != nil {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		fmt.Printf("缺少命令: %s\n", strings.Join(missing, ", "))
		fmt.Println("请先安装：apt-get install -y lxc e2fsprogs curl tar iproute2 util-linux")
		return 1
	}
	say("    依赖齐全")

	// 2) 网桥
	say("==> 2/6 准备网桥 %s", cfg.Bridge)
	if _, err := execQuiet("ip", "link", "show", cfg.Bridge); err != nil {
		if out, err := execQuiet("ip", "link", "add", cfg.Bridge, "type", "bridge"); err != nil {
			fmt.Printf("创建网桥失败: %s\n", out)
			return 1
		}
		say("    已创建")
	} else {
		say("    已存在")
	}
	execQuiet("ip", "link", "set", cfg.Bridge, "up")
	execQuiet("ip", "-6", "addr", "add", cfg.ContainerGW+"/64", "dev", cfg.Bridge, "scope", "link")

	// 3) 转发
	say("==> 3/6 开启 IPv6 转发")
	execQuiet("sysctl", "-qw", "net.ipv6.conf.all.forwarding=1")
	execQuiet("sysctl", "-qw", "net.ipv6.conf.all.proxy_ndp=1")
	_ = os.MkdirAll("/etc/sysctl.d", 0755)
	_ = os.WriteFile("/etc/sysctl.d/99-webmgr-ipv6.conf",
		[]byte("net.ipv6.conf.all.forwarding=1\nnet.ipv6.conf.all.proxy_ndp=1\n"), 0644)
	execQuiet("ip6tables", "-C", "FORWARD", "-i", cfg.Bridge, "-j", "ACCEPT")
	execQuiet("ip6tables", "-A", "FORWARD", "-i", cfg.Bridge, "-j", "ACCEPT")
	execQuiet("ip6tables", "-C", "FORWARD", "-o", cfg.Bridge, "-j", "ACCEPT")
	execQuiet("ip6tables", "-A", "FORWARD", "-o", cfg.Bridge, "-j", "ACCEPT")

	// 4) 写配置
	say("==> 4/6 写入 %s", *confPath)
	if err := writeConf(); err != nil {
		fmt.Printf("写配置失败: %v\n", err)
		return 1
	}

	// 5) Alpine 模板
	say("==> 5/6 准备 Alpine 模板")
	if _, err := os.Stat(cfg.Template); err == nil {
		say("    已存在")
	} else if err := ensureTemplateFile(); err != nil {
		fmt.Printf("    模板下载失败（首次建容器时会重试）: %v\n", err)
	} else {
		say("    已下载")
	}

	// 6) systemd
	say("==> 6/6 服务")
	if *withSystemd {
		if err := installService(); err != nil {
			fmt.Printf("    安装 systemd 服务失败: %v\n", err)
			return 1
		}
		say("    已安装并启动 webmgr.service")
	} else {
		say("    跳过（需要时执行：%s service install）", exeName())
	}

	// 汇总
	fmt.Println()
	fmt.Println("初始化完成")
	fmt.Printf("  配置文件  %s\n", *confPath)
	fmt.Printf("  监听      %s\n", cfg.Listen)
	fmt.Printf("  IPv6 前缀 %s\n", orDash(cfg.Prefix))
	fmt.Printf("  网桥      %s (网关 %s)\n", cfg.Bridge, cfg.ContainerGW)
	if cfg.NetworkPlugin != "" {
		fmt.Printf("  网络插件  %s\n", cfg.NetworkPlugin)
	} else {
		fmt.Println("  网络插件  未启用")
	}
	fmt.Printf("  访问令牌  %s\n", orDash(cfg.Token))
	fmt.Println()
	if *withSystemd {
		fmt.Println("已作为服务运行，访问：")
	} else {
		fmt.Println("直接运行：", exeName())
	}
	fmt.Printf("  http://<服务器IP>:%s/%s\n", portOf(cfg.Listen), tokenQuery(cfg.Token))
	return 0
}

func writeConf() error {
	var b strings.Builder
	b.WriteString("# webmgr 配置（由 webmgr setup 生成）\n")
	fmt.Fprintf(&b, "LISTEN=%s\n", cfg.Listen)
	fmt.Fprintf(&b, "TOKEN=%s\n", cfg.Token)
	fmt.Fprintf(&b, "LXCDIR=%s\n", cfg.LXCDir)
	fmt.Fprintf(&b, "BRIDGE=%s\n", cfg.Bridge)
	fmt.Fprintf(&b, "CONTAINER_GATEWAY=%s\n", cfg.ContainerGW)
	fmt.Fprintf(&b, "IPV6_PREFIX=%s\n", cfg.Prefix)
	fmt.Fprintf(&b, "TEMPLATE=%s\n", cfg.Template)
	fmt.Fprintf(&b, "DEFAULT_MEM_MB=%d\n", cfg.DefaultMemMB)
	fmt.Fprintf(&b, "DEFAULT_DISK_MB=%d\n", cfg.DefaultDiskMB)
	fmt.Fprintf(&b, "SERVICES=%s\n", strings.Join(cfg.Services, ","))
	fmt.Fprintf(&b, "DOCKER_CONTAINERS=%s\n", strings.Join(cfg.DockerContainers, ","))
	if cfg.NetworkPlugin != "" {
		fmt.Fprintf(&b, "NETWORK_PLUGIN=%s\n", cfg.NetworkPlugin)
		fmt.Fprintf(&b, "PLUGIN_ENABLE=1\n")
	}
	fmt.Fprintf(&b, "PROC=%s\n", cfg.ProcRoot)
	if err := os.MkdirAll(filepath.Dir(*confPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(*confPath, []byte(b.String()), 0600)
}

// ensureTemplateFile 用 curl 拉取 Alpine minirootfs
func ensureTemplateFile() error {
	base := "https://dl-cdn.alpinelinux.org/alpine/latest-stable/releases/x86_64"
	out, err := execQuiet("sh", "-c",
		`curl -fsSL "`+base+`/" | grep -oE 'alpine-minirootfs-[0-9]+\.[0-9]+\.[0-9]+-x86_64\.tar\.gz' | sort -u | tail -1`)
	if err != nil || out == "" {
		return fmt.Errorf("无法获取版本列表")
	}
	_ = os.MkdirAll(filepath.Dir(cfg.Template), 0755)
	if o, err := execQuiet("curl", "-fsSL", "-o", cfg.Template, base+"/"+out); err != nil {
		return fmt.Errorf("%s", o)
	}
	st, err := os.Stat(cfg.Template)
	if err != nil || st.Size() < 1_000_000 {
		return fmt.Errorf("下载文件异常")
	}
	return nil
}

func installService() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	unit := fmt.Sprintf(`[Unit]
Description=webmgr - lightweight LXC management panel
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s -conf %s
Restart=always
RestartSec=3
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
`, exe, *confPath)
	if err := os.WriteFile(unitPath, []byte(unit), 0644); err != nil {
		return err
	}
	execQuiet("systemctl", "daemon-reload")
	execQuiet("systemctl", "enable", "--now", "webmgr.service")
	execQuiet("systemctl", "restart", "webmgr.service")
	return nil
}

func removeService() error {
	execQuiet("systemctl", "disable", "--now", "webmgr.service")
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	execQuiet("systemctl", "daemon-reload")
	return nil
}

func runServiceCmd(args []string) int {
	if len(args) == 0 {
		fmt.Println("用法: webmgr service install|remove")
		return 2
	}
	if !isRoot() {
		fmt.Println("错误：需要 root 权限")
		return 1
	}
	_ = loadSettings(*confPath)
	bindGlobals()
	switch args[0] {
	case "install":
		if _, err := os.Stat(*confPath); err != nil {
			fmt.Println("未找到配置文件，请先执行: webmgr setup")
			return 1
		}
		if err := installService(); err != nil {
			fmt.Printf("安装失败: %v\n", err)
			return 1
		}
		fmt.Println("已安装并启动 webmgr.service")
		fmt.Println("查看状态: systemctl status webmgr")
		return 0
	case "remove":
		if err := removeService(); err != nil {
			fmt.Printf("移除失败: %v\n", err)
			return 1
		}
		fmt.Println("已移除 webmgr.service")
		return 0
	default:
		fmt.Println("用法: webmgr service install|remove")
		return 2
	}
}

func exeName() string {
	if e, err := os.Executable(); err == nil {
		return e
	}
	return "webmgr"
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return "8099"
}

func tokenQuery(t string) string {
	if t == "" {
		return ""
	}
	return "?t=" + t
}

func printHelp() {
	fmt.Print(`webmgr - 轻量 LXC 管理面板

用法:
  webmgr                     启动面板（读 /etc/webmgr.conf，若无则用默认值+自动探测）
  webmgr setup [选项]        初始化：写配置、建网桥、开转发、下模板
  webmgr service install     安装为 systemd 服务并启动
  webmgr service remove      移除服务
  webmgr version             显示版本
  webmgr help                显示本帮助

setup 选项:
  --systemd            初始化后顺便装成服务
  --plugin <路径>      启用网络插件（写进配置）
  --token <字符串>     设置访问令牌
  --listen <地址>      监听地址，默认 0.0.0.0:8099
  --prefix <前缀>      IPv6 前缀，留空自动探测
  --bridge <名字>      容器网桥，默认 lxcbr6
  --gw <地址>          容器网关，默认 fe80::1
  --quiet              精简输出

运行选项（也可写在配置文件里）:
  -conf <路径>         配置文件，默认 /etc/webmgr.conf
  -listen <地址>       覆盖监听地址
  -token <字符串>      覆盖访问令牌
  -proc <路径>         覆盖 /proc
`)
}
