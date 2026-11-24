// webmgr —— 轻量 LXC 管理面板
// 单二进制、零外部依赖；数据来自 /proc、/sys/fs/cgroup 与 lxc/systemctl 命令。
package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

var (
	confPath   = flag.String("conf", "/etc/webmgr.conf", "配置文件路径")
	listenAddr = flag.String("listen", "", "监听地址（为空则用配置文件值）")
	token      = flag.String("token", "", "访问令牌（为空则不鉴权）")
	procRoot   = flag.String("proc", "", "proc 挂载点")
	version    = "3.0.0"
	startedAt  = time.Now()
	cpuUsage   atomic.Uint64 // math.Float64bits

	// 由配置文件初始化（见 main 中的 setupVars）
	lxcDir  *string
	ndpConf *string

	cacheMu  sync.RWMutex
	cache    *snapshot
	cacheAt  time.Time
	cacheTTL = 4 * time.Second
	busy     atomic.Bool

	actionMu   sync.Mutex
	lastAction = map[string]time.Time{}

	netCacheMu  sync.Mutex
	v6Gateway   string
	gwCacheTime time.Time
)

// ---------------- 数据类型 ----------------

type Stats struct {
	Host       Host        `json:"host"`
	Load       []float64   `json:"load"`
	CPU        CPU         `json:"cpu"`
	Mem        Mem         `json:"mem"`
	Disk       Disk        `json:"disk"`
	Services   []Service   `json:"services"`
	Containers []Container `json:"containers"`
	Net        NetInfo     `json:"net"`
	Counts     Counts      `json:"counts"`
	TS         int64       `json:"ts"`
	CollectMS  int64       `json:"collect_ms"`
}

type Counts struct {
	Containers      int     `json:"containers"`
	ContainersUp    int     `json:"containers_up"`
	MemUsedPct      float64 `json:"mem_used_pct"`
	DiskUsedPct     float64 `json:"disk_used_pct"`
	ServicesUp      int     `json:"services_up"`
	ServicesTotal   int     `json:"services_total"`
}

type Host struct {
	Name      string `json:"name"`
	Kernel    string `json:"kernel"`
	UptimeSec int64  `json:"uptime_sec"`
	CPUs      int    `json:"cpus"`
}

type CPU struct{ UsagePct float64 `json:"usage_pct"` }

type Mem struct {
	Total     uint64 `json:"total"`
	Used      uint64 `json:"used"`
	Available uint64 `json:"available"`
	SwapTotal uint64 `json:"swap_total"`
	SwapUsed  uint64 `json:"swap_used"`
}

type Disk struct {
	Total    uint64  `json:"total"`
	Used     uint64  `json:"used"`
	Free     uint64  `json:"free"`
	UsagePct float64 `json:"usage_pct"`
}

type Service struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type Container struct {
	Name      string   `json:"name"`
	State     string   `json:"state"`
	Autostart bool     `json:"autostart"`
	IPv4      string   `json:"ipv4"`
	IPv6      []string `json:"ipv6"`
	MemCur    uint64   `json:"mem_current"`
	MemMax    uint64   `json:"mem_max"`
	DiskUsed  uint64   `json:"disk_used"`
	DiskTotal uint64   `json:"disk_total"`
	Anon      uint64   `json:"anon"`
	File      uint64   `json:"file"`
	Kernel    uint64   `json:"kernel"`
	Pids      int      `json:"pids"`
	Ports     []string `json:"ports"`
	Config    string   `json:"config"`
	Rootfs    string   `json:"rootfs"`
	Registered bool    `json:"registered"`
}

type NetInfo struct {
	IPv4      string   `json:"ipv4"`
	IPv6      []string `json:"ipv6"`
	GatewayV6 string   `json:"gateway_v6"`
}

type AddrRow struct {
	Address    string `json:"address"`
	Container  string `json:"container"`
	State      string `json:"state"` // used / reserved / free
	Registered bool   `json:"registered"`
	Note       string `json:"note"`
}

type AddrView struct {
	Registered []string  `json:"registered"`
	Rows       []AddrRow `json:"rows"`
	HostAddrs  []string  `json:"host_addrs"`
	Gateway    string    `json:"gateway"`
	Prefix     string    `json:"prefix"`
	NextFree   string    `json:"next_free"`
}

type snapshot struct {
	Services   []Service
	Containers []Container
}

// ---------------- 基础工具 ----------------

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

func readUint(p string) uint64 {
	s := strings.TrimSpace(readFile(p))
	if s == "" || s == "max" {
		return 0
	}
	n, _ := strconv.ParseUint(s, 10, 64)
	return n
}

func statfs(path string) (uint64, uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return st.Blocks * bs, st.Bavail * bs, nil
}

func runShort(timeout time.Duration, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ---------------- 快采集 ----------------

func collectHost() Host {
	h := Host{CPUs: runtime.NumCPU()}
	h.Name, _ = os.Hostname()
	h.Kernel = strings.TrimSpace(readFile(*procRoot + "/sys/kernel/osrelease"))
	if f := strings.Fields(readFile(*procRoot + "/uptime")); len(f) > 0 {
		if v, err := strconv.ParseFloat(f[0], 64); err == nil {
			h.UptimeSec = int64(v)
		}
	}
	return h
}

func collectLoad() []float64 {
	out := make([]float64, 3)
	f := strings.Fields(readFile(*procRoot + "/loadavg"))
	for i := 0; i < 3 && i < len(f); i++ {
		out[i], _ = strconv.ParseFloat(f[i], 64)
	}
	return out
}

type cpuTimes struct{ total, idle uint64 }

func readCPUTimes() cpuTimes {
	var t cpuTimes
	f := strings.Fields(readFile(*procRoot + "/stat"))
	if len(f) < 5 {
		return t
	}
	for i := 1; i <= 8 && i < len(f); i++ {
		n, err := strconv.ParseUint(f[i], 10, 64)
		if err != nil {
			break
		}
		t.total += n
		if i == 4 {
			t.idle = n
		}
	}
	return t
}

func sampleCPU() {
	prev := readCPUTimes()
	for {
		time.Sleep(time.Second)
		cur := readCPUTimes()
		if cur.total > prev.total {
			busy := cur.total - prev.total - (cur.idle - prev.idle)
			cpuUsage.Store(math.Float64bits(float64(busy) / float64(cur.total-prev.total) * 100))
		}
		prev = cur
	}
}

func parseMeminfo(s string) map[string]uint64 {
	out := map[string]uint64{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || !strings.HasSuffix(f[0], ":") {
			continue
		}
		n, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		out[strings.TrimSuffix(f[0], ":")] = n * 1024
	}
	return out
}

func collectMem() Mem {
	var m Mem
	kv := parseMeminfo(readFile(*procRoot + "/meminfo"))
	m.Total = kv["MemTotal"]
	m.Available = kv["MemAvailable"]
	if m.Available == 0 {
		m.Available = kv["MemFree"] + kv["Buffers"] + kv["Cached"]
	}
	if m.Total > m.Available {
		m.Used = m.Total - m.Available
	}
	m.SwapTotal = kv["SwapTotal"]
	if kv["SwapTotal"] > kv["SwapFree"] {
		m.SwapUsed = kv["SwapTotal"] - kv["SwapFree"]
	}
	return m
}

func collectDisk() Disk {
	var d Disk
	total, free, err := statfs("/")
	if err != nil {
		return d
	}
	d.Total, d.Free = total, free
	d.Used = total - free
	if total > 0 {
		d.UsagePct = math.Round(float64(d.Used)/float64(total)*1000) / 10
	}
	return d
}

// ---------------- 慢采集 ----------------

func collectServices() []Service {
	names := cfg.Services
	doc := cfg.DockerContainers
	out := make([]Service, len(names)+len(doc))
	var wg sync.WaitGroup
	for i, n := range names {
		wg.Add(1)
		go func(i int, n string) {
			defer wg.Done()
			out[i] = Service{Name: n, Status: runShort(2*time.Second, "systemctl", "is-active", n)}
		}(i, n)
	}
	for i, c := range doc {
		wg.Add(1)
		go func(i int, c string) {
			defer wg.Done()
			st := runShort(2*time.Second, "docker", "inspect", "-f", "{{.State.Status}}", c)
			if st == "" {
				st = "absent"
			}
			out[len(names)+i] = Service{Name: c, Status: st}
		}(i, c)
	}
	wg.Wait()
	return out
}

func listContainerNames() []string {
	out := []string{}
	for _, n := range strings.Split(runShort(3*time.Second, "lxc-ls", "-1"), "\n") {
		n = strings.TrimSpace(n)
		if n != "" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func cgroupDir(name string) string {
	for _, p := range []string{
		"/sys/fs/cgroup/lxc.payload." + name,
		"/sys/fs/cgroup/lxc/" + name,
		"/sys/fs/cgroup/lxc.payload/" + name,
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func configIPv6(name string) []string {
	out := []string{}
	for _, line := range strings.Split(readFile(filepath.Join(*lxcDir, name, "config")), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "lxc.net") && strings.Contains(line, ".ipv6.address") {
			if i := strings.Index(line, "="); i > 0 {
				v := strings.TrimSpace(line[i+1:])
				v = strings.TrimSuffix(v, "/128")
				v = strings.TrimSuffix(v, "/64")
				if v != "" {
					out = append(out, v)
				}
			}
		}
	}
	return out
}

func collectOne(name string) Container {
	c := Container{
		Name: name, State: "stopped",
		IPv6: []string{}, Ports: []string{},
		Config: filepath.Join(*lxcDir, name, "config"),
		Rootfs: filepath.Join(*lxcDir, name, "rootfs"),
	}
	cfgTxt := readFile(c.Config)
	c.Autostart = strings.Contains(cfgTxt, "lxc.start.auto = 1")

	if st := runShort(3*time.Second, "lxc-info", "-n", name, "-sH"); st != "" {
		c.State = st
	}
	// 停止状态也能从配置里读出地址
	c.IPv6 = configIPv6(name)

	// 磁盘用量
	// loop 后端由 LXC 在容器命名空间内挂载镜像，宿主看不到挂载点，
	// 因此直接用镜像文件本身：名义大小 = 上限，已分配块 = 实际占用。
	rawPath := c.Rootfs + ".raw"
	if st, err := os.Stat(rawPath); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			c.DiskTotal = uint64(st.Size())
			c.DiskUsed = uint64(sys.Blocks) * 512
		}
	} else if total, free, err := statfs(c.Rootfs); err == nil {
		// dir 后端：rootfs 是挂载点，正常 statfs
		c.DiskTotal, c.DiskUsed = total, total-free
	}

	if c.State != "RUNNING" {
		return c
	}

	// 运行中：真实地址优先
	ips := []string{}
	for _, line := range strings.Split(runShort(5*time.Second, "lxc-info", "-n", name, "-iH"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "fe80:") || line == "::1" {
			continue
		}
		ips = append(ips, line)
	}
	if len(ips) > 0 {
		c.IPv6 = ips
	}
	for _, ip := range ips {
		if strings.Count(ip, ".") == 3 { // IPv4
			c.IPv4 = ip
		}
	}

	if cg := cgroupDir(name); cg != "" {
		c.MemCur = readUint(cg + "/memory.current")
		c.MemMax = readUint(cg + "/memory.max")
		c.Pids = int(readUint(cg + "/pids.current"))
		for _, line := range strings.Split(readFile(cg+"/memory.stat"), "\n") {
			f := strings.Fields(line)
			if len(f) != 2 {
				continue
			}
			v, _ := strconv.ParseUint(f[1], 10, 64)
			switch f[0] {
			case "anon":
				c.Anon = v
			case "file":
				c.File = v
			case "kernel":
				c.Kernel = v
			}
		}
	}
	c.Ports = containerPorts(name)
	return c
}

func containerPorts(name string) []string {
	out := []string{}
	seen := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, spec := range [][2]string{{"-uln", "udp"}, {"-tln", "tcp"}} {
		wg.Add(1)
		go func(flagArg, proto string) {
			defer wg.Done()
			res := runShort(2*time.Second, "lxc-attach", "-n", name, "--", "netstat", flagArg)
			for _, line := range strings.Split(res, "\n") {
				f := strings.Fields(line)
				if len(f) < 4 {
					continue
				}
				local := f[3]
				i := strings.LastIndex(local, ":")
				if i < 0 {
					continue
				}
				p, err := strconv.Atoi(strings.TrimSpace(local[i+1:]))
				if err != nil || p <= 0 {
					continue
				}
				key := strconv.Itoa(p) + "/" + proto
				mu.Lock()
				if !seen[key] {
					seen[key] = true
					out = append(out, key)
				}
				mu.Unlock()
			}
		}(spec[0], spec[1])
	}
	wg.Wait()
	sort.Strings(out)
	return out
}

func collectContainers() []Container {
	names := listContainerNames()
	out := make([]Container, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		wg.Add(1)
		go func(i int, n string) {
			defer wg.Done()
			out[i] = collectOne(n)
		}(i, n)
	}
	wg.Wait()

	// 标记 NDP 注册状态
	conf := readFile(*ndpConf)
	for i := range out {
		reg := false
		for _, ip := range out[i].IPv6 {
			if ip != "" && strings.Contains(conf, ip) {
				reg = true
				break
			}
		}
		out[i].Registered = reg
	}
	return out
}

func getSnapshot() snapshot {
	cacheMu.RLock()
	cur := cache
	age := time.Since(cacheAt)
	cacheMu.RUnlock()
	if cur != nil && age < cacheTTL {
		return *cur
	}
	if busy.CompareAndSwap(false, true) {
		go func() {
			defer busy.Store(false)
			var sd snapshot
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); sd.Services = collectServices() }()
			go func() { defer wg.Done(); sd.Containers = collectContainers() }()
			wg.Wait()
			cacheMu.Lock()
			cache = &sd
			cacheAt = time.Now()
			cacheMu.Unlock()
		}()
	}
	if cur != nil {
		return *cur
	}
	var sd snapshot
	sd.Services = collectServices()
	sd.Containers = collectContainers()
	cacheMu.Lock()
	cache = &sd
	cacheAt = time.Now()
	cacheMu.Unlock()
	return sd
}

func collectNet() NetInfo {
	var n NetInfo
	ifaces, err := net.Interfaces()
	if err != nil {
		return n
	}
	for _, ifc := range ifaces {
		if ifc.Name == "lo" {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP
			if ip.To4() != nil {
				if n.IPv4 == "" {
					n.IPv4 = ip.String()
				}
			} else if ip.IsGlobalUnicast() {
				n.IPv6 = append(n.IPv6, ip.String())
			}
		}
	}
	sort.Strings(n.IPv6)
	n.GatewayV6 = gatewayV6()
	return n
}

func gatewayV6() string {
	netCacheMu.Lock()
	defer netCacheMu.Unlock()
	if v6Gateway != "" && time.Since(gwCacheTime) < time.Minute {
		return v6Gateway
	}
	for _, line := range strings.Split(runShort(2*time.Second, "ip", "-6", "route", "show", "default"), "\n") {
		f := strings.Fields(line)
		for i := 0; i < len(f)-1; i++ {
			if f[i] == "via" {
				v6Gateway = f[i+1]
				gwCacheTime = time.Now()
				return v6Gateway
			}
		}
	}
	return v6Gateway
}

// 地址视图
func buildAddrView(cs []Container) AddrView {
	v := AddrView{HostAddrs: []string{}}
	reg := []string{}
	for _, line := range strings.Split(readFile(*ndpConf), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		reg = append(reg, line)
	}
	sort.Strings(reg)
	v.Registered = reg

	// 容器 -> 地址
	owner := map[string]string{}
	for _, c := range cs {
		for _, ip := range c.IPv6 {
			owner[ip] = c.Name
		}
	}
	seen := map[string]bool{}
	for _, ip := range reg {
		seen[ip] = true
		row := AddrRow{Address: ip, Registered: true, State: "reserved"}
		if name, ok := owner[ip]; ok {
			row.Container = name
			row.State = "used"
			row.Note = "容器在用"
		} else {
			row.Note = "已注册 NDP，无容器占用"
		}
		v.Rows = append(v.Rows, row)
	}
	// 容器有地址但未注册
	for _, c := range cs {
		for _, ip := range c.IPv6 {
			if !seen[ip] {
				seen[ip] = true
				v.Rows = append(v.Rows, AddrRow{
					Address: ip, Container: c.Name, State: "unregistered",
					Note: "未在 NDP 清单，可能无法入站",
				})
			}
		}
	}

	// 宿主自身地址
	n := collectNet()
	v.HostAddrs = n.IPv6
	v.Gateway = n.GatewayV6
	if len(n.IPv6) > 0 {
		parts := strings.Split(n.IPv6[0], ":")
		if len(parts) >= 3 {
			v.Prefix = parts[0] + ":" + parts[1] + ":" + parts[2] + "::/48"
		}
	}
	v.NextFree = nextFreeSuffix(cs)
	return v
}

// ---------------- 动作 ----------------

type actionReq struct {
	Name   string `json:"name"`
	Action string `json:"action"`
}

// ensureNetRegistered 容器启动后确保地址已注册（交给网络插件，幂等）
func ensureNetRegistered(name string) {
	if !pluginConfigured() || !cfg.PluginEnable {
		return
	}
	for _, ip := range configIPv6(name) {
		if ip == "" {
			continue
		}
		_ = pluginRegister(name, ip)
	}
}

// ensureRootfsMounted 启动前确保根文件系统可用。
// 两种后端：
//   - loop:/path/rootfs.raw   → LXC 自己挂载，无需干预
//   - dir:/path/rootfs        → 依赖 fstab loop 挂载，未挂载则启动会
//     因 "Failed to exec /sbin/init" 失败，需要先挂载
func ensureRootfsMounted(name string) (mounted bool, msg string) {
	cfg := readFile(filepath.Join(*lxcDir, name, "config"))
	if strings.Contains(cfg, "lxc.rootfs.path = loop:") ||
		strings.Contains(cfg, "lxc.rootfs.path = btrfs:") ||
		strings.Contains(cfg, "lxc.rootfs.path = zfs:") ||
		strings.Contains(cfg, "lxc.rootfs.path = lvm:") {
		return true, "" // 由 LXC 自行挂载
	}
	rootfs := filepath.Join(*lxcDir, name, "rootfs")
	if _, err := os.Stat(rootfs + ".raw"); err != nil {
		return true, "" // 非独立镜像后端，交给 LXC
	}
	if isMountpoint(rootfs) {
		return true, ""
	}
	out := runShort(15*time.Second, "mount", rootfs)
	if isMountpoint(rootfs) {
		return true, ""
	}
	if out == "" {
		out = "挂载后仍不可用"
	}
	return false, "无法挂载 rootfs: " + out
}

func isMountpoint(path string) bool {
	data := readFile("/proc/mounts")
	for _, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == path {
			return true
		}
	}
	return false
}

func handleAction(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"ok":false,"error":"method not allowed"}`))
		return
	}
	// 简单 CSRF 防护：必须是 JSON 请求
	if !strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error":"content-type must be application/json"}`))
		return
	}
	var req actionReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error":"bad json"}`))
		return
	}
	switch req.Action {
	case "start", "stop", "restart":
	default:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error":"unsupported action"}`))
		return
	}
	// 容器名必须真实存在
	valid := false
	for _, n := range listContainerNames() {
		if n == req.Name {
			valid = true
			break
		}
	}
	if !valid {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"ok":false,"error":"container not found"}`))
		return
	}
	// 限流：同一容器 5 秒内只允许一次
	actionMu.Lock()
	if t, ok := lastAction[req.Name]; ok && time.Since(t) < 5*time.Second {
		actionMu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"error":"too frequent, wait 5s"}`))
		return
	}
	lastAction[req.Name] = time.Now()
	actionMu.Unlock()

	var msg string
	failed := false
	switch req.Action {
	case "start", "restart":
		if ok, m := ensureRootfsMounted(req.Name); !ok {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": false, "action": req.Action, "name": req.Name, "error": m,
			})
			return
		}
	}
	switch req.Action {
	case "start":
		msg = runShort(25*time.Second, "lxc-start", "-n", req.Name, "-d")
		time.Sleep(2000 * time.Millisecond)
		ensureNetRegistered(req.Name)
	case "stop":
		// 容器内 busybox init 不响应 SIGPWR，lxc-stop 会一直等到超时；
		// 给 5 秒优雅期，超时后由 LXC 升级为 SIGKILL。
		msg = runShort(20*time.Second, "lxc-stop", "-n", req.Name, "--timeout", "5")
		if runShort(2*time.Second, "lxc-info", "-n", req.Name, "-sH") == "RUNNING" {
			msg = runShort(10*time.Second, "lxc-stop", "-n", req.Name, "-k")
		}
	case "restart":
		runShort(20*time.Second, "lxc-stop", "-n", req.Name, "--timeout", "5")
		if runShort(2*time.Second, "lxc-info", "-n", req.Name, "-sH") == "RUNNING" {
			runShort(10*time.Second, "lxc-stop", "-n", req.Name, "-k")
		}
		time.Sleep(800 * time.Millisecond)
		msg = runShort(25*time.Second, "lxc-start", "-n", req.Name, "-d")
		time.Sleep(2000 * time.Millisecond)
		ensureNetRegistered(req.Name)
	}
	// 结果校验
	state := runShort(3*time.Second, "lxc-info", "-n", req.Name, "-sH")
	switch req.Action {
	case "start", "restart":
		if state != "RUNNING" {
			failed = true
		}
	case "stop":
		if state == "RUNNING" {
			failed = true
		}
	}
	// 清缓存，立即看到新状态
	invalidateCache()

	if failed {
		if msg == "" {
			msg = "操作未生效，当前状态: " + state
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": false, "action": req.Action, "name": req.Name, "state": state,
			"error": "操作失败: " + msg,
		})
		return
	}
	out := map[string]any{"ok": true, "action": req.Action, "name": req.Name, "state": state, "output": msg}
	_ = json.NewEncoder(w).Encode(out)
}

// ---------------- HTTP ----------------

// invalidateCache 清空快照缓存；并做一次延迟清理，
// 防止「清空瞬间正在跑的采集」把过期数据写回后又被读 4 秒。
func invalidateCache() {
	cacheMu.Lock()
	cache = nil
	cacheAt = time.Time{}
	cacheMu.Unlock()
	go func() {
		time.Sleep(700 * time.Millisecond)
		cacheMu.Lock()
		cache = nil
		cacheAt = time.Time{}
		cacheMu.Unlock()
	}()
}

func collectAll() Stats {
	t0 := time.Now()
	st := Stats{TS: time.Now().Unix()}
	st.Host = collectHost()
	st.Load = collectLoad()
	st.CPU.UsagePct = math.Round(math.Float64frombits(cpuUsage.Load())*10) / 10
	st.Mem = collectMem()
	st.Disk = collectDisk()
	sd := getSnapshot()
	st.Services = sd.Services
	st.Containers = sd.Containers
	st.Net = collectNet()

	up := 0
	for _, c := range sd.Containers {
		if c.State == "RUNNING" {
			up++
		}
	}
	svcUp := 0
	for _, s := range sd.Services {
		if s.Status == "active" || s.Status == "running" {
			svcUp++
		}
	}
	st.Counts = Counts{
		Containers: len(sd.Containers), ContainersUp: up,
		ServicesUp: svcUp, ServicesTotal: len(sd.Services),
	}
	if st.Mem.Total > 0 {
		st.Counts.MemUsedPct = math.Round(float64(st.Mem.Used)/float64(st.Mem.Total)*1000) / 10
	}
	st.Counts.DiskUsedPct = st.Disk.UsagePct
	st.CollectMS = time.Since(t0).Milliseconds()
	return st
}

func authOK(r *http.Request) bool {
	if *token == "" {
		return true
	}
	if r.URL.Query().Get("t") == *token {
		return true
	}
	if c, err := r.Cookie("wt"); err == nil && c.Value == *token {
		return true
	}
	return r.Header.Get("Authorization") == "Bearer "+*token
}

// bindGlobals 把配置值绑定到全局指针变量（子命令与主循环共用）
func bindGlobals() {
	lxcDir = &cfg.LXCDir
	ndpConf = &cfg.NDPConf
	if strings.TrimSpace(cfg.ProcRoot) != "" {
		procRoot = &cfg.ProcRoot
	}
	token = &cfg.Token
	listenAddr = &cfg.Listen
}

func main() {
	// 子命令分发（优先级高于 flag）
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "setup":
			os.Exit(runSetup(os.Args[2:]))
		case "service":
			os.Exit(runServiceCmd(os.Args[2:]))
		case "boot":
			os.Exit(runBoot(os.Args[2:]))
		case "version", "-V", "--version":
			fmt.Printf("webmgr %s\n", version)
			return
		case "help", "-h", "--help":
			printHelp()
			return
		}
	}

	flag.Parse()
	// 1) 载入配置文件
	if err := loadSettings(*confPath); err != nil {
		log.Printf("读取配置 %s 失败: %v（使用默认值）", *confPath, err)
	}
	// 2) 命令行显式参数优先
	if *listenAddr != "" {
		cfg.Listen = *listenAddr
	}
	if *token != "" {
		cfg.Token = *token
	}
	if *procRoot != "" {
		cfg.ProcRoot = *procRoot
	}
	// 3) 绑定到全局指针变量（其余代码通过 *lxcDir / *ndpConf / *procRoot 访问）
	lxcDir = &cfg.LXCDir
	ndpConf = &cfg.NDPConf
	procRoot = &cfg.ProcRoot
	token = &cfg.Token
	listenAddr = &cfg.Listen
	// 4) 自动探测 IPv6 前缀（配置为空时）
	if strings.TrimSpace(cfg.Prefix) == "" {
		cfg.Prefix = prefixOf()
	}
	log.Printf("配置: %s prefix=%s", cfg.String(), cfg.Prefix)

	go sampleCPU()
	go getSnapshot()

	mux := http.NewServeMux()

	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "version": version, "title": cfg.Title,
			"uptime_sec": int64(time.Since(startedAt).Seconds()),
		})
	})

	mux.HandleFunc("/api/setup/status", func(w http.ResponseWriter, r *http.Request) {
		if !setupAllowed(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		handleSetupStatus(w, r)
	})

	mux.HandleFunc("/api/setup/apply", func(w http.ResponseWriter, r *http.Request) {
		if !setupAllowed(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
			return
		}
		handleSetupApply(w, r)
	})
	mux.HandleFunc("/api/plugin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if !authOK(r) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(pluginStatus())
	})
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if !authOK(r) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(collectAll())
	})

	mux.HandleFunc("/api/containers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if !authOK(r) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		sd := getSnapshot()
		_ = json.NewEncoder(w).Encode(map[string]any{"containers": sd.Containers})
	})

	mux.HandleFunc("/api/addresses", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if !authOK(r) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		sd := getSnapshot()
		_ = json.NewEncoder(w).Encode(buildAddrView(sd.Containers))
	})

	mux.HandleFunc("/api/action", func(w http.ResponseWriter, r *http.Request) {
		if !authOK(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
			return
		}
		handleAction(w, r)
	})

	mux.HandleFunc("/api/container/create", func(w http.ResponseWriter, r *http.Request) {
		if !authOK(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
			return
		}
		handleCreate(w, r)
	})

	mux.HandleFunc("/api/container/config", func(w http.ResponseWriter, r *http.Request) {
		if !authOK(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
			return
		}
		handleConfig(w, r)
	})

	mux.HandleFunc("/api/container/delete", func(w http.ResponseWriter, r *http.Request) {
		if !authOK(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
			return
		}
		handleDelete(w, r)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if !setupAllowed(r) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("401 unauthorized\n"))
			return
		}
		if *token != "" && r.URL.Query().Get("t") == *token {
			http.SetCookie(w, &http.Cookie{Name: "wt", Value: *token, Path: "/", HttpOnly: true, MaxAge: 86400 * 30})
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})

	srv := &http.Server{Addr: *listenAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("webmgr %s 启动: http://%s (token_enabled=%v)", version, *listenAddr, *token != "")
	if !cfgInitialized() {
		log.Printf("检测到尚未初始化 —— 请用浏览器打开 http://%s/ 完成向导", *listenAddr)
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
