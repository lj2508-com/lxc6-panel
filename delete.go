// 删除容器：停止 → 清理 NDP/路由/systemd → 销毁镜像与配置
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func handleDelete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

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
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 8192))
	var req struct {
		Name    string `json:"name"`
		Confirm string `json:"confirm"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"error":"bad json"}`))
		return
	}
	name := strings.TrimSpace(req.Name)
	steps := []string{}
	fail := func(code int, msg string) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg, "steps": steps})
	}

	// 存在性
	exists := false
	for _, n := range listContainerNames() {
		if n == name {
			exists = true
			break
		}
	}
	if !exists {
		fail(404, "容器不存在: "+name)
		return
	}
	// 二次确认：必须完整输入容器名
	if strings.TrimSpace(req.Confirm) != name {
		fail(400, "确认失败：请完整输入容器名 "+name)
		return
	}
	// 频率限制
	actionMu.Lock()
	if t, ok := lastAction["__delete__"]; ok && time.Since(t) < 5*time.Second {
		actionMu.Unlock()
		fail(429, "操作过于频繁，请稍候")
		return
	}
	lastAction["__delete__"] = time.Now()
	actionMu.Unlock()

	ips := configIPv6(name)

	// 1) 停止
	steps = append(steps, "停止容器")
	if st := runShort(3*time.Second, "lxc-info", "-n", name, "-sH"); st == "RUNNING" {
		runShort(25*time.Second, "lxc-stop", "-n", name, "--timeout", "5")
		if runShort(3*time.Second, "lxc-info", "-n", name, "-sH") == "RUNNING" {
			runShort(15*time.Second, "lxc-stop", "-n", name, "-k")
		}
	}

	// 2) 清理 systemd 单元（若为该容器单独建过）
	steps = append(steps, "清理 systemd 单元")
	unit := "/etc/systemd/system/lxc-" + name + ".service"
	if _, err := os.Stat(unit); err == nil {
		runShort(15*time.Second, "systemctl", "disable", "--now", "lxc-"+name+".service")
		_ = os.Remove(unit)
		runShort(10*time.Second, "systemctl", "daemon-reload")
	}

	// 3) 网络插件注销地址
	if pluginConfigured() && cfg.PluginEnable {
		steps = append(steps, "网络插件注销地址")
		for _, ip := range ips {
			if ip != "" {
				_ = pluginUnregister(name, ip)
			}
		}
	}

	// 5) 清理 fstab 残留（老式 dir 后端容器）
	if fstab := readFile("/etc/fstab"); strings.Contains(fstab, "lxc/"+name+"/rootfs.raw") {
		lines := strings.Split(fstab, "\n")
		out := []string{}
		for _, l := range lines {
			if strings.Contains(l, "lxc/"+name+"/rootfs.raw") {
				continue
			}
			out = append(out, l)
		}
		_ = os.WriteFile("/etc/fstab", []byte(strings.Join(out, "\n")), 0644)
	}

	// 6) 销毁
	steps = append(steps, "销毁容器与磁盘镜像")
	out := runShort(90*time.Second, "lxc-destroy", "-n", name, "-f")
	dir := filepath.Join(*lxcDir, name)
	if _, err := os.Stat(dir); err == nil {
		// 兜底：手动卸载并删除
		rootfs := filepath.Join(dir, "rootfs")
		if isMountpoint(rootfs) {
			runShort(10*time.Second, "umount", rootfs)
			if isMountpoint(rootfs) {
				runShort(10*time.Second, "umount", "-l", rootfs)
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			fail(500, fmt.Sprintf("销毁失败（目录仍存在）: %v；lxc-destroy 输出: %s", err, out))
			return
		}
	}

	// 清缓存（延迟二次清理，避免后台刷新把旧数据写回）
	invalidateCache()

	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "name": name, "steps": steps,
		"message": fmt.Sprintf("容器 %s 已删除（含磁盘镜像、NDP 注册与路由）", name),
	})
}
