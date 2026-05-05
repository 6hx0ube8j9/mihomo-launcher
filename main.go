package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/getlantern/systray"
	"golang.org/x/sys/windows/registry"
)

//go:embed icons/*.ico
var iconFs embed.FS

type Config struct {
	SystemProxy bool   `json:"system_proxy"`
	TunEnabled  bool   `json:"tun_enabled"`
	ProxyMode   string `json:"proxy_mode"` // rule, global, direct
	AutoStart   bool   `json:"auto_start"`
	ServiceMode bool   `json:"service_mode"`
}

var (
	conf       Config
	exePath, _ = os.Executable()
	baseDir    = filepath.Dir(exePath)
	coreExe    = filepath.Join(baseDir, "mihomo.exe")
	lockPort   = "127.0.0.1:54321"
)

func main() {
	ln, err := net.Listen("tcp", lockPort)
	if err != nil {
		conn, _ := net.Dial("tcp", lockPort)
		if conn != nil {
			conn.Write([]byte("WAKEUP"))
			conn.Close()
		}
		return
	}
	defer ln.Close()

	minimized := flag.Bool("minimized", false, "start minimized")
	flag.Parse()

	loadConfig()

	go func() {
		for {
			conn, _ := ln.Accept()
			if conn != nil { conn.Close() }
		}
	}()

	// 核心拉起逻辑：显式指定工作目录
	if !conf.ServiceMode && !*minimized {
		go runMihomoCore()
	}

	systray.Run(onReady, onExit)
}

func onReady() {
	refreshIcon("default.ico")
	systray.SetTitle("Mihomo Launcher")

	// --- 1. Web 面板 ---
	mWeb := systray.AddMenuItem("进入 Web 面板", "")

	systray.AddSeparator()

	// --- 2. 模式开关 (一级) ---
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", conf.SystemProxy)
	mTun := systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", conf.TunEnabled)

	systray.AddSeparator()

	// --- 3. 路由模式 (一级) ---
	mRule := systray.AddMenuItemCheckbox("规则模式", "", conf.ProxyMode == "rule")
	mGlobal := systray.AddMenuItemCheckbox("全局模式", "", conf.ProxyMode == "global")
	mDirect := systray.AddMenuItemCheckbox("直连模式", "", conf.ProxyMode == "direct")

	systray.AddSeparator()

	// --- 4. 自启动设置 (一级菜单) ---
	mSettings := systray.AddMenuItem("自启动设置", "")
	mAutoStart := mSettings.AddSubMenuItemCheckbox("开机自动启动", "", conf.AutoStart)
	mInstallSvc := mSettings.AddSubMenuItem("安装后台服务", "")
	mUninstallSvc := mSettings.AddSubMenuItem("卸载后台服务", "")
	mRunBat := mSettings.AddSubMenuItem("管理服务 (BAT)", "")
	mExitPro := mSettings.AddSubMenuItem("彻底退出程序", "")

	systray.AddSeparator()

	// --- 5. 辅助功能 (一级) ---
	mOpenDir := systray.AddMenuItem("打开程序目录", "")
	mHide := systray.AddMenuItem("隐藏托盘图标", "")
	mRestart := systray.AddMenuItem("重启内核", "")

	updateSvcMenu(mInstallSvc, mUninstallSvc)

	// --- 事件循环 ---
	go func() {
		for {
			select {
			case <-mWeb.ClickedCh:
				exec.Command("cmd", "/c", "start", "http://127.0.0.1:9090/ui").Run()

			case <-mProxy.ClickedCh:
				conf.SystemProxy = !mProxy.Checked()
				toggleSystemProxy(conf.SystemProxy)
				if conf.SystemProxy { mProxy.Check() } else { mProxy.Uncheck() }
				saveConfig()

			case <-mTun.ClickedCh:
				conf.TunEnabled = !mTun.Checked()
				if conf.TunEnabled { mTun.Check(); refreshIcon("tun.ico") } else { mTun.Uncheck(); refreshIcon("proxy.ico") }
				saveConfig()
				restartCore()

			case <-mRule.ClickedCh:
				updateMode("rule", mRule, mGlobal, mDirect)
			case <-mGlobal.ClickedCh:
				updateMode("global", mRule, mGlobal, mDirect)
			case <-mDirect.ClickedCh:
				updateMode("direct", mRule, mGlobal, mDirect)

			case <-mAutoStart.ClickedCh:
				conf.AutoStart = !mAutoStart.Checked()
				if conf.AutoStart { mAutoStart.Check() } else { mAutoStart.Uncheck() }
				setRegistryAutoStart(conf.AutoStart)
				saveConfig()

			case <-mInstallSvc.ClickedCh:
				runSvcAction("install")
				conf.ServiceMode = true
				updateSvcMenu(mInstallSvc, mUninstallSvc)
				saveConfig()

			case <-mUninstallSvc.ClickedCh:
				runSvcAction("uninstall")
				conf.ServiceMode = false
				updateSvcMenu(mInstallSvc, mUninstallSvc)
				saveConfig()

			case <-mRunBat.ClickedCh:
				batPath := filepath.Join(baseDir, "mihomo-service", "mihomo-service.bat")
				exec.Command("cmd", "/c", "start", "", batPath).Run()

			case <-mExitPro.ClickedCh:
				fullCleanup()
				systray.Quit()

			case <-mOpenDir.ClickedCh:
				exec.Command("explorer", baseDir).Run()

			case <-mHide.ClickedCh:
				systray.Quit() // 仅退出托盘，进程驻留

			case <-mRestart.ClickedCh:
				restartCore()
			}
		}
	}()
}

// 核心：解决拉不起来后端的问题
func runMihomoCore() {
	if _, err := os.Stat(coreExe); os.IsNotExist(err) {
		return
	}
	cmd := exec.Command(coreExe, "-d", baseDir)
	cmd.Dir = baseDir // 关键：指定工作目录，否则内核找不到 config.yaml
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
	err := cmd.Start()
	if err != nil {
		fmt.Printf("Startup error: %v\n", err)
	}
}

func restartCore() {
	exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
	time.Sleep(500 * time.Millisecond)
	if !conf.ServiceMode {
		go runMihomoCore()
	} else {
		exec.Command("sc", "start", "mihomo").Run()
	}
}

func updateMode(mode string, r, g, d *systray.MenuItem) {
	conf.ProxyMode = mode
	r.Uncheck(); g.Uncheck(); d.Uncheck()
	switch mode {
	case "rule": r.Check()
	case "global": g.Check()
	case "direct": d.Check()
	}
	saveConfig()
	// 此处可添加调用内核 API 切换模式的代码
}

func fullCleanup() {
	toggleSystemProxy(false)
	exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
	if conf.ServiceMode {
		exec.Command("sc", "stop", "mihomo").Run()
	}
}

func toggleSystemProxy(enable bool) {
	val := uint32(0)
	if enable { val = 1 }
	k, _ := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.ALL_ACCESS)
	defer k.Close()
	k.SetDWordValue("ProxyEnable", val)
}

func setRegistryAutoStart(enable bool) {
	k, _ := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.ALL_ACCESS)
	defer k.Close()
	if enable {
		k.SetStringValue("MihomoLauncher", "\""+exePath+"\" -minimized")
	} else {
		k.DeleteValue("MihomoLauncher")
	}
}

func refreshIcon(name string) {
	data, _ := iconFs.ReadFile("icons/" + name)
	systray.SetIcon(data)
}

func updateSvcMenu(ins, unins *systray.MenuItem) {
	if conf.ServiceMode {
		ins.Disable(); unins.Enable()
	} else {
		ins.Enable(); unins.Disable()
	}
}

func runSvcAction(action string) {
	svcExe := filepath.Join(baseDir, "mihomo-service", "mihomo-service.exe")
	exec.Command(svcExe, action).Run()
}

func loadConfig() {
	data, err := ioutil.ReadFile(filepath.Join(baseDir, "config.json"))
	if err == nil {
		json.Unmarshal(data, &conf)
	} else {
		conf = Config{ProxyMode: "rule", AutoStart: false, ServiceMode: false}
	}
}

func saveConfig() {
	data, _ := json.MarshalIndent(conf, "", "  ")
	_ = ioutil.WriteFile(filepath.Join(baseDir, "config.json"), data, 0644)
}

func onExit() {
	os.Exit(0)
}
