// 容器配置查看与调整：内存上限、磁盘上限（仅增大）、最大进程数、开机自启
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type ctrConfig struct {
	Name       string   `json:"name"`
	State      string   `json:"state"`
	MemMB      int      `json:"mem_mb"`
	DiskMB     int      `json:"disk_mb"`
	DiskUsedMB float64  `json:"disk_used_mb"`
	PidsMax    int      `json:"pids_max"`
	Autostart  bool     `json:"autostart"`
	IPv6       []string `json:"ipv6"`
	SwapsOff   bool     `json:"swap_off"`
	ConfigPath string   `json:"config_path"`
}

var (
	reMemMax = regexp.MustCompile(`(?m)^\s*lxc\.cgroup2\.memory\.max\s*=\s*(\d+)\s*$`)
	rePids   = regexp.MustCompile(`(?m)^\s*lxc\.cgroup2\.pids\.max\s*=\s*(\d+)\s*$`)
	reAuto   = regexp.MustCompile(`(?m)^\s*lxc\.start\.auto\s*=\s*(\d+)\s*$`)
)

func readCtrConfig(name string) ctrConfig {
	cfgPath := filepath.Join(*lxcDir, name, "config")
	txt := readFile(cfgPath)
	raw := cfgPath
	c := ctrConfig{Name: name, ConfigPath: raw, PidsMax: 512}

	if m := reMemMax.FindStringSubmatch(txt); len(m) == 2 {
		if n, err := strconv.ParseUint(m[1], 10, 64); err == nil {
			c.MemMB = int(n / 1024 / 1024)
		}
	}
	if m := rePids.FindStringSubmatch(txt); len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil {
			c.PidsMax = n
		}
	}
	if m := reAuto.FindStringSubmatch(txt); len(m) == 2 {
		c.Autostart = m[1] == "1"
	}
	c.SwapsOff = strings.Contains(txt, "lxc.cgroup2.memory.swap.max = 0")
	c.IPv6 = configIPv6(name)
	c.State = "STOPPED"
	if st := runShort(3*time.Second, "lxc-info", "-n", name, "-sH"); st != "" {
		c.State = st
	}

	img := filepath.Join(*lxcDir, name, "rootfs.raw")
	if st, err := os.Stat(img); err == nil {
		c.DiskMB = int(math.Round(float64(st.Size()) / 1024 / 1024))
		if used := imageUsedBytes(img); used > 0 {
			c.DiskUsedMB = math.Round(float64(used)/1024/1024*10) / 10
		}
	}
	return c
}

// imageUsedBytes 返回镜像文件实际分配的字节数（稀疏文件按块计）
func imageUsedBytes(path string) uint64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint64(sys.Blocks) * 512
	}
	return 0
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	// 先读一次 body（POST 时），后面多次解析都基于这份缓冲，避免二次解码 EOF
	var bodyBytes []byte
	if r.Method == http.MethodPost {
		bodyBytes, _ = io.ReadAll(http.MaxBytesReader(w, r.Body, 16384))
	}

	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" && len(bodyBytes) > 0 {
		var probe struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(bodyBytes, &probe)
		name = probe.Name
	}
	valid := false
	for _, n := range listContainerNames() {
		if n == name {
			valid = true
			break
		}
	}
	if !valid {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"ok":false,"error":"容器不存在"}`))
		return
	}

	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "config": readCtrConfig(name)})
		return
	}
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

	// 用指针区分「未提交」与「提交了零值」，避免部分更新误改其他字段
	var req struct {
		Name      string `json:"name"`
		MemMB     *int   `json:"mem_mb"`
		DiskMB    *int   `json:"disk_mb"`
		PidsMax   *int   `json:"pids_max"`
		Autostart *bool  `json:"autostart"`
	}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error":"bad json"}`))
		return
	}
	req.Name = name

	cur := readCtrConfig(name)
	changes := []string{}
	fail := func(code int, msg string) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg, "changes": changes})
	}

	// 未提交的字段保持原值
	memMB := cur.MemMB
	if req.MemMB != nil {
		memMB = *req.MemMB
	}
	pidsMax := cur.PidsMax
	if req.PidsMax != nil {
		pidsMax = *req.PidsMax
	}
	diskMB := cur.DiskMB
	if req.DiskMB != nil {
		diskMB = *req.DiskMB
	}
	autoOn := cur.Autostart
	if req.Autostart != nil {
		autoOn = *req.Autostart
	}

	// ---- 校验 ----
	if memMB < 32 || memMB > 1024 {
		fail(400, "内存需在 32-1024 MB 之间")
		return
	}
	if pidsMax < 64 || pidsMax > 8192 {
		fail(400, "最大进程数需在 64-8192 之间")
		return
	}
	if diskMB < cur.DiskMB {
		fail(400, "磁盘只支持扩容，不支持缩小（当前 "+strconv.Itoa(cur.DiskMB)+" MB）")
		return
	}
	if diskMB > 8192 {
		fail(400, "磁盘上限不能超过 8192 MB")
		return
	}
	// 内存不能超过宿主可用
	avail := collectMem()
	maxAllowed := int(float64(avail.Total) / 1024 / 1024 * 0.85)
	if memMB > maxAllowed {
		fail(400, fmt.Sprintf("内存上限不能超过宿主总内存的 85%%（上限 %d MB）", maxAllowed))
		return
	}

	cfgPath := filepath.Join(*lxcDir, name, "config")
	txt := readFile(cfgPath)
	if txt == "" {
		fail(500, "读取配置失败")
		return
	}

	// ---- 写配置 ----
	memBytes := strconv.Itoa(memMB * 1024 * 1024)
	if reMemMax.MatchString(txt) {
		txt = reMemMax.ReplaceAllString(txt, "lxc.cgroup2.memory.max = "+memBytes)
	} else {
		txt = strings.TrimRight(txt, "\n") + "\nlxc.cgroup2.memory.max = " + memBytes + "\n"
	}
	pidsStr := strconv.Itoa(pidsMax)
	if rePids.MatchString(txt) {
		txt = rePids.ReplaceAllString(txt, "lxc.cgroup2.pids.max = "+pidsStr)
	} else {
		txt = strings.TrimRight(txt, "\n") + "\nlxc.cgroup2.pids.max = " + pidsStr + "\n"
	}
	autoVal := "0"
	if autoOn {
		autoVal = "1"
	}
	if reAuto.MatchString(txt) {
		txt = reAuto.ReplaceAllString(txt, "lxc.start.auto = "+autoVal)
	} else {
		txt = strings.TrimRight(txt, "\n") + "\nlxc.start.auto = " + autoVal + "\n"
	}
	if !cGValidate(txt) {
		fail(500, "生成的配置不合法，已中止（未修改任何内容）")
		return
	}
	backup := cfgPath + ".bak." + time.Now().Format("20060102150405")
	_ = os.WriteFile(backup, []byte(readFile(cfgPath)), 0644)
	if err := os.WriteFile(cfgPath, []byte(txt), 0644); err != nil {
		fail(500, "写配置失败: "+err.Error())
		return
	}
	if memMB != cur.MemMB {
		changes = append(changes, fmt.Sprintf("内存 %d MB → %d MB", cur.MemMB, memMB))
	}
	if pidsMax != cur.PidsMax {
		changes = append(changes, fmt.Sprintf("最大进程数 %d → %d", cur.PidsMax, pidsMax))
	}
	if autoOn != cur.Autostart {
		if autoOn {
			changes = append(changes, "开机自启：关闭 → 开启")
		} else {
			changes = append(changes, "开机自启：开启 → 关闭")
		}
	}

	// ---- 热生效（运行中容器直接写 cgroup）----
	liveApplied := []string{}
	if cur.State == "RUNNING" {
		if cg := cgroupDir(name); cg != "" {
			if memMB != cur.MemMB {
				if out := runShort(5*time.Second, "sh", "-c", "echo "+memBytes+" > "+cg+"/memory.max"); out == "" {
					liveApplied = append(liveApplied, "内存上限已热生效")
				}
			}
			if pidsMax != cur.PidsMax {
				if out := runShort(5*time.Second, "sh", "-c", "echo "+pidsStr+" > "+cg+"/pids.max"); out == "" {
					liveApplied = append(liveApplied, "进程数上限已热生效")
				}
			}
		}
	}

	// ---- 磁盘扩容 ----
	// 注意：镜像可能已是目标大小但内部 ext4 尚未扩容（例如上次只做了 truncate），
	// 因此只要提交了磁盘参数且不缩小，就在容器停止时尝试一次「对齐」。
	if req.DiskMB != nil && diskMB >= cur.DiskMB {
		if cur.State == "RUNNING" {
			if diskMB > cur.DiskMB {
				fail(409, "磁盘扩容需要先停止容器（当前运行中）。请先停止再试。")
				return
			}
		} else {
			before := cur.DiskMB
			if msg, err := growDisk(name, diskMB); err != nil {
				fail(500, "磁盘扩容失败: "+msg)
				return
			}
			if diskMB > before {
				changes = append(changes, fmt.Sprintf("磁盘 %d MB → %d MB（已扩容并调整文件系统）", before, diskMB))
			} else {
				changes = append(changes, "磁盘文件系统已对齐镜像大小")
			}
		}
	}

	// 清缓存（延迟二次清理，避免后台刷新把旧数据写回）
	invalidateCache()

	newCfg := readCtrConfig(name)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "changes": changes, "live": liveApplied, "config": newCfg,
		"message": strings.Join(changes, "；"),
	})
}

// 简单校验：确保关键指令存在且格式正确
func cGValidate(txt string) bool {
	must := []string{
		"lxc.rootfs.path = ",
		"lxc.net.0.ipv6.address = ",
	}
	for _, k := range must {
		if !strings.Contains(txt, k) {
			return false
		}
	}
	if !reMemMax.MatchString(txt) || !reAuto.MatchString(txt) {
		return false
	}
	return true
}

// growDisk 扩容镜像并在线调整 ext4（需容器已停止）
func growDisk(name string, newMB int) (string, error) {
	raw := filepath.Join(*lxcDir, name, "rootfs.raw")
	st, err := os.Stat(raw)
	if err != nil {
		return "镜像不存在", err
	}
	curMB := int(math.Round(float64(st.Size()) / 1024 / 1024))
	// 镜像可能已经是目标大小（上次只做了 truncate），此时仍需 mount+resize2fs 对齐文件系统，
	// 所以这里不做「相等就返回」，只跳过 truncate 步骤。
	if newMB > curMB {
		if total, free, err := statfs("/var/lib/lxc"); err == nil {
			_ = total
			need := uint64(newMB-curMB) * 1024 * 1024
			if free < need+64*1024*1024 {
				return fmt.Sprintf("宿主磁盘余量不足（需要 %.0f MB，可用 %.0f MB）",
					float64(need)/1048576, float64(free)/1048576), fmt.Errorf("no space")
			}
		}
		if out := runShort(20*time.Second, "truncate", "-s", fmt.Sprintf("%dM", newMB), raw); out != "" {
			if _, err := os.Stat(raw); err != nil {
				return "truncate 失败: " + out, fmt.Errorf("truncate")
			}
		}
	}
	mnt := filepath.Join(*lxcDir, name, "rootfs")
	_ = os.MkdirAll(mnt, 0755)
	// 若已被挂载先卸载（LXC 停止后通常已卸载）
	if isMountpoint(mnt) {
		runShort(10*time.Second, "umount", mnt)
	}
	if out := runShort(20*time.Second, "mount", "-o", "loop", raw, mnt); out != "" && !isMountpoint(mnt) {
		return "挂载失败: " + out, fmt.Errorf("mount")
	}
	// resize2fs 需要块设备路径（挂载点是目录，直接传会报 "Is a directory"）
	dev := strings.TrimSpace(strings.Split(runShort(5*time.Second, "findmnt", "-n", "-o", "SOURCE", mnt), "\n")[0])
	if dev == "" || !strings.HasPrefix(dev, "/dev/") {
		dev = strings.TrimSpace(strings.Split(runShort(5*time.Second, "losetup", "-j", raw), "\n")[0])
		if i := strings.Index(dev, ":"); i > 0 {
			dev = strings.TrimSpace(dev[:i])
		}
	}
	if dev == "" || !strings.HasPrefix(dev, "/dev/") {
		runShort(15*time.Second, "umount", mnt)
		return "无法确定镜像对应的块设备", fmt.Errorf("no device")
	}
	out := runShort(90*time.Second, "resize2fs", dev)
	runShort(15*time.Second, "umount", mnt)
	if out != "" && !strings.Contains(out, "Nothing to do") && !strings.Contains(out, "is now") &&
		!strings.Contains(out, "The filesystem is already") {
		return out, fmt.Errorf("resize2fs")
	}
	return out, nil
}
