// 网络插件接口：面板与「IPv6 打通方式」解耦
//
// 契约（插件是任意可执行文件，实现下列子命令）：
//   <plugin> register   <ipv6>   注册容器地址（成功退出码 0）
//   <plugin> unregister <ipv6>   注销容器地址
//   <plugin> status              输出一行或多行状态文本；健康时退出码 0
//   <plugin> setup               初始化宿主环境（可选）
//
// 面板通过环境变量传递上下文：
//   WM_NAME     容器名
//   WM_IP       容器 IPv6
//   WM_BRIDGE   网桥名
//   WM_PREFIX   IPv6 前缀
//   WM_ACTION   register / unregister / status / setup
//
// 未配置插件（NETWORK_PLUGIN 为空）时，所有调用都是 no-op：
//   —— 适用于「IPv6 整段路由到本机」的普通 VPS，容器自带可达性。
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// PluginResult 插件调用结果
type PluginResult struct {
	Output  string `json:"output"`
	OK      bool   `json:"ok"`
	Elapsed int64  `json:"elapsed_ms"`
}

// pluginConfigured 是否配置了网络插件
func pluginConfigured() bool {
	return strings.TrimSpace(cfg.NetworkPlugin) != ""
}

// callPlugin 调用插件子命令
func callPlugin(action, ip string, timeout time.Duration) PluginResult {
	t0 := time.Now()
	path := strings.TrimSpace(cfg.NetworkPlugin)
	if path == "" {
		return PluginResult{OK: true, Output: "", Elapsed: 0}
	}
	if _, err := os.Stat(path); err != nil {
		return PluginResult{OK: false, Output: "插件不存在: " + path, Elapsed: 0}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	args := []string{action}
	if ip != "" {
		args = append(args, ip)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(os.Environ(),
		"WM_ACTION="+action,
		"WM_IP="+ip,
		"WM_BRIDGE="+cfg.Bridge,
		"WM_PREFIX="+cfg.Prefix,
		"WM_NAME="+strings.TrimSpace(os.Getenv("WM_NAME")),
		"LC_ALL=C",
	)
	out, err := cmd.CombinedOutput()
	res := PluginResult{
		Output:  strings.TrimSpace(string(out)),
		OK:      err == nil,
		Elapsed: time.Since(t0).Milliseconds(),
	}
	if err != nil && res.Output == "" {
		res.Output = err.Error()
	}
	return res
}

// pluginRegister 注册地址（容器创建/启动/改址时调用）
func pluginRegister(name, ip string) PluginResult {
	os.Setenv("WM_NAME", name)
	defer os.Unsetenv("WM_NAME")
	return callPlugin("register", ip, 20*time.Second)
}

// pluginUnregister 注销地址（删除容器时调用）
func pluginUnregister(name, ip string) PluginResult {
	os.Setenv("WM_NAME", name)
	defer os.Unsetenv("WM_NAME")
	return callPlugin("unregister", ip, 20*time.Second)
}

// pluginStatus 查询插件状态（面板展示用）
func pluginStatus() map[string]any {
	path := strings.TrimSpace(cfg.NetworkPlugin)
	out := map[string]any{
		"configured": path != "",
		"path":       path,
		"enabled":    cfg.PluginEnable,
	}
	if path == "" {
		out["ok"] = true
		out["output"] = "未配置网络插件（容器自带 IPv6 可达性）"
		return out
	}
	if _, err := os.Stat(path); err != nil {
		out["ok"] = false
		out["output"] = "插件文件不存在"
		return out
	}
	if !cfg.PluginEnable {
		out["ok"] = true
		out["output"] = "插件已配置但被禁用（PLUGIN_ENABLE=0）"
		return out
	}
	res := callPlugin("status", "", 10*time.Second)
	out["ok"] = res.OK
	out["output"] = res.Output
	out["elapsed_ms"] = res.Elapsed
	return out
}

// mustPluginRegister 注册失败时返回可读警告（不中断主流程）
func warnIfPluginFailed(r PluginResult) string {
	if r.OK {
		if r.Output != "" {
			return r.Output
		}
		return ""
	}
	return fmt.Sprintf("网络插件调用失败：%s", r.Output)
}
