package main

import (
	"embed"
	"encoding/json"
	"flag"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/getlantern/systray"
	"golang.org/x/sys/windows/registry"
)

//go:embed icons/*.ico
var iconFs embed.FS

type Config struct {
	SystemProxy bool   `json:"system_proxy"`
	TunEnabled  bool   `json:"tun_enabled"`
	ProxyMode   string `json:"proxy_mode"`
	AutoStart   bool   `json:"auto_start"`
	TrayHidden  bool   `json:"tray_hidden"`
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
	// 1. 单实例检测与双击唤醒逻辑
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

	// 2. 唤醒信号监听 (空实现，确保端口被占用)
	go func() {
		for {
			conn, _ := ln.Accept()
			if conn != nil { conn.Close() }
		}
	}()

	// 3. 核心启动逻辑 (合并自 mihomo-run)
	if !conf.ServiceMode && !*minimized {
		go runMihomoCore()
	}

	systray.Run(onReady, onExit)
}

func onReady() {
	// 初始状态判断图标
	if conf.TunEnabled {
		refreshIcon("tun.ico")
	} else {
		refreshIcon("default.ico")
	}

	systray.SetTitle("Mihomo Launcher")

	// --- 菜单逻辑 (合并自 mihomo-tray) ---
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", conf.SystemProxy)
	mTun := systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", conf.TunEnabled)
	systray.AddSeparator()

	mModes := systray.AddMenuItem("模式切换", "")
	mRule := mModes.AddSubMenuItemCheckbox("规则模式", "", conf.ProxyMode == "rule")
	mGlobal := mModes.AddSubMenuItemCheckbox("全局模式", "", conf.ProxyMode == "global")
	mDirect := mModes.AddSubMenuItemCheckbox("直连模式", "", conf.ProxyMode == "direct")

	systray.AddSeparator()
	mSettings := systray.AddMenuItem("自启动设置", "")
	mAutoStart := mSettings.AddSubMenuItemCheckbox("开机自动启动", "", conf.AutoStart)
	mInstallSvc := mSettings.AddSubMenuItem("安装后台服务", "")
	mUninstallSvc := mSettings.AddSubMenuItem("卸载后台服务", "")
	
	systray.AddSeparator()
	mRestart := systray.AddMenuItem("重启内核", "")
	mExit := systray.AddMenuItem("完全退出", "")

	// 变量占位规避 Go 编译检查
	_ = mRule; _ = mGlobal; _ = mDirect

	updateMenuState(mInstallSvc, mUninstallSvc)

	// --- 交互事件循环 ---
	go func() {
		for {
			select {
			case <-mProxy.ClickedCh:
				conf.SystemProxy = !mProxy.Checked()
				toggleSystemProxy(conf.SystemProxy)
				if conf.SystemProxy { mProxy.Check() } else { mProxy.Uncheck() }
				saveConfig()

			case <-mTun.ClickedCh:
				conf.TunEnabled = !mTun.Checked()
				if conf.TunEnabled { 
					mTun.Check()
					refreshIcon("tun.ico") 
				} else { 
					mTun.Uncheck()
					refreshIcon("proxy.ico") 
				}
				saveConfig()
				restartCore() // 切换模式需重启内核应用新配置

			case <-mAutoStart.ClickedCh:
				conf.AutoStart = !mAutoStart.Checked()
				if conf.AutoStart { mAutoStart.Check() } else { mAutoStart.Uncheck() }
				setRegistryAutoStart(conf.AutoStart)
				saveConfig()

			case <-mInstallSvc.ClickedCh:
				runSvcAction("install")
				conf.ServiceMode = true
				updateMenuState(mInstallSvc, mUninstallSvc)
				saveConfig()

			case <-mUninstallSvc.ClickedCh:
				runSvcAction("uninstall")
				conf.ServiceMode = false
				updateMenuState(mInstallSvc, mUninstallSvc)
				saveConfig()

			case <-mRestart.ClickedCh:
				restartCore()

			case <-mExit.ClickedCh:
				refreshIcon("stop.ico")
				cleanupAndExit()
			}
		}
	}()
}

// 核心运行逻辑: 合并了原 mihomo-run 的参数传递
func runMihomoCore() {
	if _, err := os.Stat(coreExe); os.IsNotExist(err) {
		refreshIcon("error.ico")
		return
	}
	// 关键：-d 指定内核的工作目录，使其能读取到 config.yaml 和 mmdb
	cmd := exec.Command(coreExe, "-d", baseDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
	cmd.Run()
}

func restartCore() {
	terminateMihomoProcess()
	if !conf.ServiceMode {
		go runMihomoCore()
	} else {
		exec.Command("sc", "start", "mihomo").Run()
	}
}

func terminateMihomoProcess() {
	// 暴力杀掉所有相关内核进程，确保无残留
	exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
}

func cleanupAndExit() {
	toggleSystemProxy(false) // 恢复系统代理状态
	terminateMihomoProcess() 
	if conf.ServiceMode {
		exec.Command("sc", "stop", "mihomo").Run()
	}
	systray.Quit()
}

func setRegistryAutoStart(enable bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.ALL_ACCESS)
	if err != nil { return }
	defer k.Close()
	if enable {
		k.SetStringValue("MihomoLauncher", "\""+exePath+"\" -minimized")
	} else {
		k.DeleteValue("MihomoLauncher")
	}
}

func toggleSystemProxy(enable bool) {
	val := uint32(0)
	if enable { val = 1 }
	k, _ := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.ALL_ACCESS)
	defer k.Close()
	k.SetDWordValue("ProxyEnable", val)
}

func refreshIcon(name string) {
	data, err := iconFs.ReadFile("icons/" + name)
	if err == nil { systray.SetIcon(data) }
}

func updateMenuState(ins, unins *systray.MenuItem) {
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
