// 容器创建功能：以 Alpine minirootfs 为模板，创建纯 IPv6 的 LXC 容器
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	nameRe     = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	templateGZ = "/var/lib/lxc/.alpine-rootfs.tar.gz" // 默认值，配置文件可覆盖
)

func tmplPath() string {
	if cfg.Template != "" {
		return cfg.Template
	}
	return templateGZ
}

type createReq struct {
	Name     string `json:"name"`
	Suffix   string `json:"suffix"`    // 例如 "11" 或 "::11"，或完整地址
	MemMB    int    `json:"mem_mb"`    // 内存上限
	DiskMB   int    `json:"disk_mb"`   // 磁盘上限
	Start    bool   `json:"start"`     // 创建后是否启动
	Autostart bool  `json:"autostart"` // 是否开机自启
}

type createResp struct {
	OK      bool     `json:"ok"`
	Error   string   `json:"error,omitempty"`
	Message string   `json:"message,omitempty"`
	Name    string   `json:"name,omitempty"`
	IPv6    string   `json:"ipv6,omitempty"`
	Steps   []string `json:"steps,omitempty"`
}

// 计算下一个可用地址后缀
func nextFreeSuffix(containers []Container) string {
	used := map[int]bool{}
	addUsed := func(ip string) {
		// 形如 2001:db8:abcd::NN
		if i := strings.LastIndex(ip, "::"); i >= 0 {
			tail := ip[i+2:]
			if tail != "" {
				if n, err := strconv.ParseInt(tail, 16, 32); err == nil {
					used[int(n)] = true
				}
			}
		}
	}
	for _, line := range strings.Split(readFile(*ndpConf), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			addUsed(line)
		}
	}
	for _, c := range containers {
		for _, ip := range c.IPv6 {
			addUsed(ip)
		}
	}
	// 宿主自身占用的 ::1..::N 也算
	for _, ip := range collectNet().IPv6 {
		addUsed(ip)
	}
	// 从 ::10 开始找（与已用地址同一数值空间：均为十六进制解析）
	for n := 0x10; n < 0xfffe; n++ {
		if !used[n] {
			return fmt.Sprintf("%x", n)
		}
	}
	return ""
}

func prefixOf() string {
	if strings.TrimSpace(cfg.Prefix) != "" {
		return cfg.Prefix
	}
	for _, ip := range collectNet().IPv6 {
		parts := strings.Split(ip, ":")
		if len(parts) >= 3 {
			return parts[0] + ":" + parts[1] + ":" + parts[2] + "::"
		}
	}
	return ""
}

// 把用户输入的后缀/地址规范化成完整 IPv6
func normalizeAddr(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", fmt.Errorf("地址不能为空")
	}
	var full string
	switch {
	case strings.Contains(input, ":"):
		if strings.HasPrefix(input, "::") {
			full = prefixOf() + strings.TrimPrefix(input, "::")
		} else {
			full = input
		}
	default:
		full = prefixOf() + input
	}
	full = strings.TrimSuffix(full, "/128")
	ip := net.ParseIP(full)
	if ip == nil || ip.To4() != nil {
		return "", fmt.Errorf("地址格式不正确: %s", input)
	}
	if !strings.HasPrefix(full, prefixOf()) {
		return "", fmt.Errorf("地址必须属于本站前缀 %s/48", prefixOf())
	}
	return full, nil
}

func addrInUse(addr string) bool {
	for _, line := range strings.Split(readFile(*ndpConf), "\n") {
		if strings.TrimSpace(line) == addr {
			return true
		}
	}
	for _, n := range listContainerNames() {
		for _, ip := range configIPv6(n) {
			if ip == addr {
				return true
			}
		}
	}
	for _, ip := range collectNet().IPv6 {
		if ip == addr {
			return true
		}
	}
	return false
}

// 确保 Alpine 模板存在
func ensureTemplate() (string, error) {
	if st, err := os.Stat(tmplPath()); err == nil && st.Size() > 1_000_000 {
		return "", nil
	}
	// 下载 minirootfs
	out := runShort(20*time.Second, "sh", "-c",
		`base=https://dl-cdn.alpinelinux.org/alpine/latest-stable/releases/x86_64; `+
			`f=$(curl -fsSL "$base/" | grep -oE 'alpine-minirootfs-[0-9]+\.[0-9]+\.[0-9]+-x86_64\.tar\.gz' | sort -u | tail -1); `+
			`echo "$f"`)
	if out == "" || !strings.HasPrefix(out, "alpine-minirootfs") {
		return "无法获取 Alpine 模板版本列表", fmt.Errorf("template list failed")
	}
	base := "https://dl-cdn.alpinelinux.org/alpine/latest-stable/releases/x86_64"
	if msg := runShort(180*time.Second, "curl", "-fsSL", "-o", tmplPath(), base+"/"+out); msg != "" {
		// curl 失败时通常无输出，检查文件
		_ = msg
	}
	if st, err := os.Stat(tmplPath()); err != nil || st.Size() < 1_000_000 {
		return "下载 Alpine 模板失败", fmt.Errorf("template download failed")
	}
	return "", nil
}

func handleCreate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"ok":false,"error":"method not allowed"}`))
		return
	}
	if !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error":"content-type must be application/json"}`))
		return
	}
	var req createReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error":"bad json"}`))
		return
	}

	steps := []string{}
	fail := func(code int, msg string) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(createResp{OK: false, Error: msg, Steps: steps})
	}

	// 参数校验
	req.Name = strings.TrimSpace(strings.ToLower(req.Name))
	if !nameRe.MatchString(req.Name) {
		fail(400, "名称需为 2-31 位小写字母/数字/连字符，且以字母开头")
		return
	}
	if req.MemMB <= 0 {
		req.MemMB = 128
	}
	if req.MemMB < 32 || req.MemMB > 1024 {
		fail(400, "内存需在 32-1024 MB 之间")
		return
	}
	if req.DiskMB <= 0 {
		req.DiskMB = 500
	}
	if req.DiskMB < 128 || req.DiskMB > 4096 {
		fail(400, "磁盘需在 128-4096 MB 之间")
		return
	}
	// 磁盘余量检查
	if total, free, err := statfs("/var/lib/lxc"); err == nil {
		_ = total
		if free < uint64(req.DiskMB)*1024*1024/4 {
			fail(400, fmt.Sprintf("磁盘余量不足（可用 %.1f GB）", float64(free)/1024/1024/1024))
			return
		}
	}
	if _, err := os.Stat(filepath.Join(*lxcDir, req.Name)); err == nil {
		fail(409, "容器已存在: "+req.Name)
		return
	}
	addr, err := normalizeAddr(req.Suffix)
	if err != nil {
		fail(400, err.Error())
		return
	}
	if addrInUse(addr) {
		fail(409, "地址已被占用: "+addr)
		return
	}

	// 串行创建，避免并发打架
	actionMu.Lock()
	if t, ok := lastAction["__create__"]; ok && time.Since(t) < 5*time.Second {
		actionMu.Unlock()
		fail(429, "创建过于频繁，请稍候")
		return
	}
	lastAction["__create__"] = time.Now()
	actionMu.Unlock()

	dir := filepath.Join(*lxcDir, req.Name)
	rootfs := filepath.Join(dir, "rootfs")
	raw := filepath.Join(dir, "rootfs.raw")

	// 1) 模板
	steps = append(steps, "检查 Alpine 模板")
	if msg, err := ensureTemplate(); err != nil {
		fail(500, msg)
		return
	}

	// 2) 目录与镜像
	steps = append(steps, "创建 500MB 磁盘镜像")
	if err := os.MkdirAll(rootfs, 0755); err != nil {
		fail(500, "创建目录失败: "+err.Error())
		return
	}
	if out := runShort(20*time.Second, "truncate", "-s", fmt.Sprintf("%dM", req.DiskMB), raw); out != "" {
		// truncate 正常无输出
	}
	if _, err := os.Stat(raw); err != nil {
		fail(500, "创建镜像文件失败")
		return
	}
	if out := runShort(30*time.Second, "mkfs.ext4", "-q", "-F", raw); out != "" {
		_ = out
	}

	// 3) 挂载并解压模板
	steps = append(steps, "挂载镜像")
	if out := runShort(20*time.Second, "mount", "-o", "loop", raw, rootfs); out != "" && !isMountpoint(rootfs) {
		fail(500, "挂载失败: "+out)
		return
	}
	steps = append(steps, "解压 Alpine 系统")
	if out := runShort(60*time.Second, "tar", "-xzf", tmplPath(), "-C", rootfs); out != "" {
		// tar 成功无输出
	}
	if _, err := os.Stat(filepath.Join(rootfs, "sbin", "init")); err != nil {
		runShort(10*time.Second, "umount", rootfs)
		fail(500, "模板解压后缺少 /sbin/init，创建中止")
		return
	}

	// 4) 基础配置
	steps = append(steps, "写入基础配置")
	_ = os.WriteFile(filepath.Join(rootfs, "etc/hostname"), []byte(req.Name+"\n"), 0644)
	_ = os.WriteFile(filepath.Join(rootfs, "etc/hosts"),
		[]byte("127.0.0.1 localhost\n::1 localhost\n"+addr+" "+req.Name+"\n"), 0644)
	_ = os.WriteFile(filepath.Join(rootfs, "etc/network/interfaces"),
		[]byte("auto lo\niface lo inet loopback\n\nauto eth0\niface eth0 inet manual\n"), 0644)
	_ = os.WriteFile(filepath.Join(rootfs, "etc/resolv.conf"),
		[]byte("nameserver 2606:4700:4700::1111\nnameserver 2001:4860:4860::8888\n"), 0644)

	// 5) 卸载（随后由 LXC 自行挂载）
	steps = append(steps, "卸载镜像")
	runShort(15*time.Second, "umount", rootfs)

	// 6) 写 LXC 配置
	steps = append(steps, "生成 LXC 配置")
	autostart := 0
	if req.Autostart {
		autostart = 1
	}
	cfgTxt := strings.NewReplacer(
		"BRIDGE_NAME", cfg.Bridge,
		"GW_ADDR", cfg.ContainerGW,
	).Replace(fmt.Sprintf(`lxc.include = /usr/share/lxc/config/common.conf
lxc.arch = amd64
lxc.uts.name = %s
lxc.rootfs.path = loop:%s
lxc.mount.auto = proc:mixed sys:ro cgroup:mixed
lxc.apparmor.profile = unconfined
lxc.net.0.type = veth
lxc.net.0.link = BRIDGE_NAME
lxc.net.0.flags = up
lxc.net.0.ipv6.address = %s/128
lxc.net.0.ipv6.gateway = GW_ADDR
lxc.cgroup2.memory.max = %d
lxc.cgroup2.memory.swap.max = 0
lxc.cgroup2.pids.max = 512
lxc.start.auto = %d
`, req.Name, raw, addr, req.MemMB*1024*1024, autostart))
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(cfgTxt), 0644); err != nil {
		fail(500, "写配置失败: "+err.Error())
		return
	}

	// 7) 交给网络插件注册地址（未配置插件时跳过）
	pluginMsg := ""
	if pluginConfigured() && cfg.PluginEnable {
		steps = append(steps, "网络插件注册地址")
		res := pluginRegister(req.Name, addr)
		if !res.OK {
			pluginMsg = warnIfPluginFailed(res)
		}
	}

	// 8) 可选启动
	if req.Start {
		steps = append(steps, "启动容器")
		out := runShort(30*time.Second, "lxc-start", "-n", req.Name, "-d")
		time.Sleep(2500 * time.Millisecond)
		if st := runShort(3*time.Second, "lxc-info", "-n", req.Name, "-sH"); st != "RUNNING" {
			fail(500, "容器创建成功但启动失败: "+out)
			return
		}
	}

	// 清缓存（延迟二次清理，避免后台刷新把旧数据写回）
	invalidateCache()

	msg := fmt.Sprintf("容器 %s 创建成功，地址 %s，内存 %d MB，磁盘 %d MB", req.Name, addr, req.MemMB, req.DiskMB)
	if req.Start {
		msg += "，已启动"
	}
	if pluginMsg != "" {
		msg += "；" + pluginMsg
	}
	_ = json.NewEncoder(w).Encode(createResp{
		OK: true, Message: msg, Name: req.Name, IPv6: addr, Steps: steps,
	})
}

func openAppend(path string) *os.File {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil
	}
	return f
}

// 供 /api/addresses 使用：给出下一个可用后缀
func nextFreeSuffixForView() string {
	return nextFreeSuffix(collectContainers())
}

var _ = math.Round
