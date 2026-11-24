// 网页初始化向导：不传任何参数启动，打开页面自动检测环境并引导配置
//
//	GET  /api/setup/status   环境检测结果
//	POST /api/setup/apply    执行初始化
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DepItem 单条环境检查
type DepItem struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail"`
	FixHint string `json:"fix_hint,omitempty"`
}

// SetupStatus 向导首屏所需状态
type SetupStatus struct {
	Initialized      bool      `json:"initialized"`
	IsRoot           bool      `json:"is_root"`
	OS               string    `json:"os"`
	Hostname         string    `json:"hostname"`
	ConfPath         string    `json:"conf_path"`
	Listen           string    `json:"listen"`
	Title            string    `json:"title"`
	TokenSet         bool      `json:"token_set"`
	Bridge           string    `json:"bridge"`
	Gateway          string    `json:"gateway"`
	Prefix           string    `json:"prefix"`
	PrefixDetected   bool      `json:"prefix_detected"`
	Deps             []DepItem `json:"deps"`
	AllDepsOK        bool      `json:"all_deps_ok"`
	BridgeExists     bool      `json:"bridge_exists"`
	ForwardingOn     bool      `json:"forwarding_on"`
	TemplateExists   bool      `json:"template_exists"`
	PluginConfigured bool      `json:"plugin_configured"`
	PluginPath       string    `json:"plugin_path"`
	Candidates       []string  `json:"plugin_candidates"`
	SuggestToken     string    `json:"suggest_token"`
	SuggestPlugin    bool      `json:"suggest_plugin"` // 依据路由判断是否建议启用插件
	Warnings         []string  `json:"warnings"`
}

func which(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

func parseKeyVals(s string) string {
	// 从 /etc/os-release 取 PRETTY_NAME
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
		}
	}
	return "Linux"
}

// scanPluginCandidates 扫描插件目录
func scanPluginCandidates() []string {
	out := []string{}
	base := "/usr/local/lib/webmgr/plugins"
	entries, err := os.ReadDir(base)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(base, e.Name(), "webmgr-plugin.sh")
		if st, err := os.Stat(p); err == nil && st.Mode()&0111 != 0 {
			out = append(out, p)
		}
	}
	return out
}

// needPlugin 判断当前 IPv6 是否属于「逐地址 NDP 解析」型
func needPlugin() bool {
	out := runShort(2*time.Second, "ip", "-6", "route")
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "dev ") && strings.Contains(line, "proto kernel") &&
			!strings.Contains(line, "fe80") {
			return true
		}
	}
	return false
}

func genToken() string {
	// 16 字节随机 → 32 位十六进制，不引入外部依赖
	b := make([]byte, 16)
	f, err := os.Open("/dev/urandom")
	if err == nil {
		_, _ = io.ReadFull(f, b)
		f.Close()
	} else {
		for i := range b {
			b[i] = byte(time.Now().UnixNano() >> (i % 8 * 8))
		}
	}
	return fmt.Sprintf("%x", b)
}

func buildSetupStatus() SetupStatus {
	st := SetupStatus{
		ConfPath:   *confPath,
		Listen:     cfg.Listen,
		Title:      cfg.Title,
		Bridge:     cfg.Bridge,
		Gateway:    cfg.ContainerGW,
		Prefix:     cfg.Prefix,
		IsRoot:     os.Geteuid() == 0,
		TokenSet:   strings.TrimSpace(cfg.Token) != "",
		AllDepsOK:  true,
		Candidates: []string{},
	}
	st.Hostname, _ = os.Hostname()
	st.OS = parseKeyVals(readFile("/etc/os-release"))

	// 是否已初始化：配置文件存在且有 TOKEN 或 插件配置
	if _, err := os.Stat(*confPath); err == nil {
		st.Initialized = true
	}

	// 依赖
	type dep struct{ name, bin, hint string }
	for _, d := range []dep{
		{"lxc-create", "lxc-create", "apt-get install -y lxc"},
		{"mkfs.ext4", "mkfs.ext4", "apt-get install -y e2fsprogs"},
		{"tar", "tar", "apt-get install -y tar"},
		{"curl", "curl", "apt-get install -y curl"},
		{"findmnt", "findmnt", "apt-get install -y util-linux"},
		{"ip", "ip", "apt-get install -y iproute2"},
	} {
		ok := which(d.bin)
		item := DepItem{Name: d.name, OK: ok, FixHint: ""}
		if ok {
			item.Detail = "已安装"
		} else {
			item.Detail = "缺失"
			item.FixHint = d.hint
			st.AllDepsOK = false
		}
		st.Deps = append(st.Deps, item)
	}
	// root
	item := DepItem{Name: "root 权限", OK: st.IsRoot, Detail: "是"}
	if !st.IsRoot {
		item.Detail = "否（初始化需要 root）"
		item.FixHint = "用 root 用户或 sudo 运行"
		st.AllDepsOK = false
	}
	st.Deps = append(st.Deps, item)

	// 前缀
	detected := prefixOf()
	if strings.TrimSpace(st.Prefix) == "" {
		st.Prefix = detected
	}
	st.PrefixDetected = strings.TrimSpace(detected) != ""

	// 网桥与转发
	st.BridgeExists = isLinkExists(cfg.Bridge)
	st.ForwardingOn = readFile(*procRoot+"/sys/net/ipv6/conf/all/forwarding") == "" && false
	if v := strings.TrimSpace(readFile(*procRoot + "/sys/net/ipv6/conf/all/forwarding")); v == "1" {
		st.ForwardingOn = true
	}

	// 模板
	if stt, err := os.Stat(cfg.Template); err == nil && stt.Size() > 1_000_000 {
		st.TemplateExists = true
	}

	// 插件
	st.PluginPath = cfg.NetworkPlugin
	st.PluginConfigured = strings.TrimSpace(cfg.NetworkPlugin) != ""
	st.Candidates = scanPluginCandidates()
	st.SuggestPlugin = needPlugin() && len(st.Candidates) > 0
	st.SuggestToken = genToken()

	// 提示
	if !st.TokenSet {
		st.Warnings = append(st.Warnings, "未设置访问令牌：任何能访问本页面的人都能管理容器")
	}
	if st.SuggestPlugin && !st.PluginConfigured {
		st.Warnings = append(st.Warnings,
			"检测到 IPv6 为「逐地址 NDP 解析」模式，建议启用网络插件，否则容器可能无法与外界通信")
	}
	if !st.PrefixDetected {
		st.Warnings = append(st.Warnings, "未检测到全局 IPv6 地址，容器将无法分配公网 IPv6")
	}
	return st
}

func isLinkExists(name string) bool {
	if name == "" {
		return false
	}
	_, err := os.Stat("/sys/class/net/" + name)
	return err == nil
}

func handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(buildSetupStatus())
}

// setupApplyReq 初始化请求
type setupApplyReq struct {
	Title     string `json:"title"`
	Listen    string `json:"listen"`
	Token     string `json:"token"`
	AutoToken bool   `json:"auto_token"`
	Prefix    string `json:"prefix"`
	Bridge    string `json:"bridge"`
	Gateway   string `json:"gateway"`
	Plugin    string `json:"plugin"`
	MemMB     int    `json:"mem_mb"`
	DiskMB    int    `json:"disk_mb"`
}

func handleSetupApply(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	steps := []string{}
	resp := func(ok bool, msg string, extra map[string]any) {
		m := map[string]any{"ok": ok, "steps": steps, "message": msg}
		if !ok {
			m["error"] = msg
		}
		for k, v := range extra {
			m[k] = v
		}
		_ = json.NewEncoder(w).Encode(m)
	}

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		resp(false, "method not allowed", nil)
		return
	}
	if os.Geteuid() != 0 {
		w.WriteHeader(http.StatusForbidden)
		resp(false, "初始化需要 root 权限启动 webmgr", nil)
		return
	}

	var req setupApplyReq
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 8192))
	if err := json.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		resp(false, "请求格式错误", nil)
		return
	}

	// ---- 校验 ----
	if strings.TrimSpace(req.Listen) != "" && !strings.Contains(req.Listen, ":") {
		w.WriteHeader(http.StatusBadRequest)
		resp(false, "监听地址格式应为 IP:端口，例如 0.0.0.0:8099", nil)
		return
	}
	if req.MemMB == 0 {
		req.MemMB = 192
	}
	if req.DiskMB == 0 {
		req.DiskMB = 500
	}
	if req.MemMB < 32 || req.MemMB > 1024 {
		w.WriteHeader(http.StatusBadRequest)
		resp(false, "默认内存需在 32-1024 MB", nil)
		return
	}
	if req.DiskMB < 128 || req.DiskMB > 8192 {
		w.WriteHeader(http.StatusBadRequest)
		resp(false, "默认磁盘需在 128-8192 MB", nil)
		return
	}
	// 令牌：留空则自动生成
	tokenVal := strings.TrimSpace(req.Token)
	generated := false
	if tokenVal == "" || req.AutoToken {
		tokenVal = genToken()
		generated = true
	}
	if len(tokenVal) < 8 {
		w.WriteHeader(http.StatusBadRequest)
		resp(false, "访问令牌至少 8 位", nil)
		return
	}
	if strings.TrimSpace(req.Prefix) != "" {
		if _, err := normalizeAddr(strings.TrimSpace(req.Prefix) + "1"); err != nil {
			// 前缀形式可能有误，给出提示但仍允许
			steps = append(steps, "警告：IPv6 前缀格式可能不正确")
		}
	}
	// 插件校验
	if p := strings.TrimSpace(req.Plugin); p != "" {
		st, err := os.Stat(p)
		if err != nil || st.Mode()&0111 == 0 {
			w.WriteHeader(http.StatusBadRequest)
			resp(false, "插件不可执行: "+p, nil)
			return
		}
	}

	// ---- 执行 ----
	if strings.TrimSpace(req.Listen) != "" {
		cfg.Listen = strings.TrimSpace(req.Listen)
	}
	if strings.TrimSpace(req.Title) != "" {
		cfg.Title = strings.TrimSpace(req.Title)
	}
	if strings.TrimSpace(req.Bridge) != "" {
		cfg.Bridge = strings.TrimSpace(req.Bridge)
	}
	if strings.TrimSpace(req.Gateway) != "" {
		cfg.ContainerGW = strings.TrimSpace(req.Gateway)
	}
	if strings.TrimSpace(req.Prefix) != "" {
		p := strings.TrimSpace(req.Prefix)
		if !strings.HasSuffix(p, "::") && !strings.Contains(p, "::") {
			p += "::"
		}
		cfg.Prefix = p
	}
	if strings.TrimSpace(req.Plugin) != "" {
		cfg.NetworkPlugin = strings.TrimSpace(req.Plugin)
		cfg.PluginEnable = true
	}
	cfg.Token = tokenVal
	cfg.DefaultMemMB = req.MemMB
	cfg.DefaultDiskMB = req.DiskMB
	if strings.TrimSpace(cfg.Prefix) == "" {
		cfg.Prefix = prefixOf()
	}

	// 1) 网桥
	steps = append(steps, "准备网桥 "+cfg.Bridge)
	if !isLinkExists(cfg.Bridge) {
		if out, err := execQuiet("ip", "link", "add", cfg.Bridge, "type", "bridge"); err != nil {
			resp(false, "创建网桥失败: "+out, nil)
			return
		}
		steps = append(steps, "网桥已创建")
	}
	execQuiet("ip", "link", "set", cfg.Bridge, "up")
	execQuiet("ip", "-6", "addr", "add", cfg.ContainerGW+"/64", "dev", cfg.Bridge, "scope", "link")

	// 2) 转发
	steps = append(steps, "开启 IPv6 转发")
	execQuiet("sysctl", "-qw", "net.ipv6.conf.all.forwarding=1")
	execQuiet("sysctl", "-qw", "net.ipv6.conf.all.proxy_ndp=1")
	_ = os.MkdirAll("/etc/sysctl.d", 0755)
	_ = os.WriteFile("/etc/sysctl.d/99-webmgr-ipv6.conf",
		[]byte("net.ipv6.conf.all.forwarding=1\nnet.ipv6.conf.all.proxy_ndp=1\n"), 0644)
	execQuiet("ip6tables", "-C", "FORWARD", "-i", cfg.Bridge, "-j", "ACCEPT")
	execQuiet("ip6tables", "-A", "FORWARD", "-i", cfg.Bridge, "-j", "ACCEPT")
	execQuiet("ip6tables", "-C", "FORWARD", "-o", cfg.Bridge, "-j", "ACCEPT")
	execQuiet("ip6tables", "-A", "FORWARD", "-o", cfg.Bridge, "-j", "ACCEPT")

	// 3) 写配置
	steps = append(steps, "写入 "+*confPath)
	if err := writeConf(); err != nil {
		resp(false, "写配置失败: "+err.Error(), nil)
		return
	}

	// 4) 模板
	steps = append(steps, "准备 Alpine 模板")
	if st, err := os.Stat(cfg.Template); err != nil || st.Size() < 1_000_000 {
		if err := ensureTemplateFile(); err != nil {
			steps = append(steps, "模板下载失败（首次新建容器时会重试）")
		} else {
			steps = append(steps, "模板已下载")
		}
	} else {
		steps = append(steps, "模板已存在")
	}

	// 5) 插件 setup（如果是 ndp-proxy，跑一次它的 setup）
	if cfg.NetworkPlugin != "" {
		if strings.Contains(cfg.NetworkPlugin, "ndp-proxy") {
			steps = append(steps, "初始化网络插件")
			if out := runShort(60*time.Second, cfg.NetworkPlugin, "setup"); out != "" {
				steps = append(steps, out)
			}
		}
	}

	// 6) 令牌写 cookie，避免重新登录
	http.SetCookie(w, &http.Cookie{Name: "wt", Value: cfg.Token, Path: "/", HttpOnly: true, MaxAge: 86400 * 30})

	resp(true, "初始化完成", map[string]any{
		"token":     cfg.Token,
		"generated": generated,
		"listen":    cfg.Listen,
		"prefix":    cfg.Prefix,
		"bridge":    cfg.Bridge,
		"gateway":   cfg.ContainerGW,
		"plugin":    cfg.NetworkPlugin,
	})
}

// setupGuard 未初始化时允许访问设置接口（但仍需要能连上页面）
func setupAllowed(r *http.Request) bool {
	if authOK(r) {
		return true
	}
	// 未初始化时放行（否则用户无法完成初始化）
	return !cfgInitialized()
}

func cfgInitialized() bool {
	if _, err := os.Stat(*confPath); err != nil {
		return false
	}
	return strings.TrimSpace(cfg.Token) != "" || strings.TrimSpace(cfg.NetworkPlugin) != ""
}

var _ = flag.Parse
