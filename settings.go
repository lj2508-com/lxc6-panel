// 可移植配置：所有环境相关项从 /etc/webmgr.conf 读取，命令行参数可覆盖
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Settings 汇总所有环境相关配置，默认值适配大多数 Debian/Ubuntu + LXC 环境
type Settings struct {
	Title           string
	Listen          string
	Token           string
	LXCDir          string
	NDPConf         string
	Bridge          string   // 容器网桥名
	ContainerGW     string   // 容器默认网关（网桥上的链路本地地址）
	Prefix          string   // IPv6 前缀，如 2001:db8:abcd::（留空则自动探测）
	Template        string   // Alpine rootfs 模板缓存路径
	Services        []string // 概览页展示的 systemd 服务
	DockerContainers []string // 概览页展示的 docker 容器
	NetworkPlugin   string   // 网络插件路径（register/unregister/status），空=不启用
	PluginEnable    bool     // 插件总开关
	DefaultMemMB    int
	DefaultDiskMB   int
	ProcRoot        string
}

var cfg = Settings{
	Title:           "webmgr 控制台",
	Listen:          "0.0.0.0:8099",
	LXCDir:          "/var/lib/lxc",
	NDPConf:         "/etc/ndp-responder.conf",
	Bridge:          "lxcbr6",
	ContainerGW:     "fe80::1",
	Template:        "/var/lib/lxc/.alpine-rootfs.tar.gz",
	Services:        []string{"v2ray", "docker", "fail2ban", "ssh"},
	DockerContainers: []string{},
	PluginEnable:    true,
	DefaultMemMB:    192,
	DefaultDiskMB:   500,
	ProcRoot:        "/proc",
}

// loadSettings 读取 KEY=VALUE 形式的配置文件；不存在的键保持默认值
func loadSettings(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 没有配置文件时用默认值
		}
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.Index(line, "=")
		if i <= 0 {
			continue
		}
		k := strings.ToUpper(strings.TrimSpace(line[:i]))
		v := strings.TrimSpace(line[i+1:])
		v = strings.Trim(v, `"'`)
		switch k {
		case "TITLE":
			if v != "" {
				cfg.Title = v
			}
		case "LISTEN":
			cfg.Listen = v
		case "TOKEN":
			cfg.Token = v
		case "LXCDIR":
			cfg.LXCDir = v
		case "NDPCONF":
			cfg.NDPConf = v
		case "BRIDGE":
			cfg.Bridge = v
		case "CONTAINER_GATEWAY", "CONTAINER_GW":
			cfg.ContainerGW = v
		case "IPV6_PREFIX", "PREFIX":
			cfg.Prefix = v
		case "TEMPLATE":
			cfg.Template = v
		case "SERVICES":
			cfg.Services = splitList(v)
		case "DOCKER_CONTAINERS":
			cfg.DockerContainers = splitList(v)
		case "NETWORK_PLUGIN":
			cfg.NetworkPlugin = v
		case "PLUGIN_ENABLE":
			cfg.PluginEnable = !(v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "no"))
		case "DEFAULT_MEM_MB":
			if n, e := strconv.Atoi(v); e == nil && n > 0 {
				cfg.DefaultMemMB = n
			}
		case "DEFAULT_DISK_MB":
			if n, e := strconv.Atoi(v); e == nil && n > 0 {
				cfg.DefaultDiskMB = n
			}
		case "PROC":
			cfg.ProcRoot = v
		}
	}
	return nil
}

func splitList(v string) []string {
	out := []string{}
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s Settings) String() string {
	return fmt.Sprintf("listen=%s bridge=%s gw=%s plugin=%q services=%v dockers=%v",
		s.Listen, s.Bridge, s.ContainerGW, s.NetworkPlugin, s.Services, s.DockerContainers)
}
