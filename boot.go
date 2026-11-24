// 开机自启：不依赖 lxc-autostart，直接按容器配置里的 lxc.start.auto 启动
//
//	webmgr boot     启动所有 start.auto=1 且未运行的容器
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

func runBoot(args []string) int {
	fs := flag.NewFlagSet("boot", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "只显示将启动哪些容器")
	quiet := fs.Bool("quiet", false, "静默")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	_ = loadSettings(*confPath)
	bindGlobals()

	names := listContainerNames()
	started, skipped, failed := 0, 0, 0
	for _, name := range names {
		cfgTxt := readFile(*lxcDir + "/" + name + "/config")
		if !strings.Contains(cfgTxt, "lxc.start.auto = 1") {
			continue
		}
		state := runShort(3*time.Second, "lxc-info", "-n", name, "-sH")
		if state == "RUNNING" {
			skipped++
			if !*quiet {
				fmt.Printf("  跳过 %-12s 已在运行\n", name)
			}
			continue
		}
		if *dryRun {
			fmt.Printf("  将启动 %-12s\n", name)
			started++
			continue
		}
		out := runShort(30*time.Second, "lxc-start", "-n", name, "-d")
		time.Sleep(1500 * time.Millisecond)
		st := runShort(3*time.Second, "lxc-info", "-n", name, "-sH")
		if st == "RUNNING" {
			started++
			if !*quiet {
				fmt.Printf("  已启动 %-12s\n", name)
			}
			ensureNetRegistered(name)
		} else {
			failed++
			fmt.Fprintf(os.Stderr, "  启动失败 %-12s %s\n", name, strings.TrimSpace(out))
		}
	}
	if !*quiet {
		fmt.Printf("自启完成：启动 %d，跳过 %d，失败 %d\n", started, skipped, failed)
	}
	if failed > 0 {
		return 1
	}
	return 0
}
