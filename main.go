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
	"sync"
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
	ProxyMode   string `json:"proxy_mode"`
	AutoStart   bool   `json:"auto_start"`
	TrayHidden  bool   `json:"tray_hidden"`
	ServiceMode bool   `json:"service_mode"`
}

var (
	conf       Config
	mu         sync.Mutex
	exePath, _ = os.Executable()
	baseDir    = filepath.Dir(exePath)
	coreExe    = filepath.Join(baseDir, "mihomo.exe")
	// 监听端口用于单实例唤醒
	lockPort   = "127.0.0.1:54321"
)

func main() {
	// 1. 单实例检测
	ln, err := net.Listen("tcp", lockPort)
	if err != nil {
		// 已有实例在运行，发送唤醒信号并退出
		conn, _ := net.Dial("tcp", lockPort)
		if conn != nil {
			conn.Write([]byte("WAKEUP"))
			conn.Close()
		}
		return
	}
	defer ln.Close()

	// 解析启动参数（如自启动时可能带参数）
	minimized := flag.Bool("minimized", false, "start minimized to tray")
	flag.Parse()

	loadConfig()

	// 2. 唤醒监听协程
	go func() {
		for {
			conn, _ := ln.Accept()
			if conn != nil {
				// 收到唤醒信号的逻辑可以在这里触发通知
				conn.Close()
			}
		}
	}()

	// 3. 初始内核启动 (非服务模式下)
	if !conf.ServiceMode && !*minimized {
		go runMihomoCore()
	}

	systray.Run(onReady, onExit)
}

func onReady() {
	refreshIcon("default.ico")
	systray.SetTitle("Mihomo Launcher")

	// --- 菜单定义 ---
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", conf.SystemProxy)
	mTun := systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", conf.TunEnabled)
	systray.AddSeparator()

	mModes := systray.AddMenuItem("模式切换", "")
	mRule := mModes.AddSubMenuItemCheckbox("规则模式", "", conf.ProxyMode == "rule")
	mGlobal := mModes.AddSubMenuItemCheckbox("全局模式", "", conf.ProxyMode == "global")
	mDirect := mModes.AddSubMenuItemCheckbox("直连模式", "", conf.ProxyMode == "direct")
	systray.AddSeparator()

	mSettings := systray.AddMenuItem("自启动设置", "")
	mAutoStart := mSettings.AddSubMenuItemCheckbox("开机自动启动 (Run)", "", conf.AutoStart)
	mInstallSvc := mSettings.AddSubMenuItem("安装后台服务", "")
	mUninstallSvc := mSettings.AddSubMenuItem("卸载后台服务", "")
	mRunBat := mSettings.AddSubMenuItem("管理服务 (BAT)", "")

	systray.AddSeparator()
	mRestart := systray.AddMenuItem("重启内核", "")
	mExit := systray.AddMenuItem("退出程序", "")

	// 初始 UI 状态刷新
	updateMenuState(mInstallSvc, mUninstallSvc)

	// --- 事件监听 ---
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
				// 重启内核以应用 TUN 配置
				restartCore()

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

			case <-mRunBat.ClickedCh:
				if !conf.ServiceMode {
					batPath := filepath.Join(baseDir, "mihomo-service", "mihomo-service.bat")
					exec.Command("cmd", "/c", "start", "", batPath).Run()
				}

			case <-mRestart.ClickedCh:
				restartCore()

			case <-mExit.ClickedCh:
				refreshIcon("stop.ico")
				cleanupAndExit()
			}
		}
	}()
}

// --- 核心 Run 逻辑 (mihomo-run) ---
func runMihomoCore() {
	if _, err := os.Stat(coreExe); os.IsNotExist(err) {
		refreshIcon("error.ico")
		return
	}

	// 这里集成你原来的 mihomo-run 参数
	// -d 指定工作目录，确保内核能找到 config.yaml 和 mmdb
	cmd := exec.Command(coreExe, "-d", baseDir)
	
	// 隐藏黑窗口且不继承父进程 Job
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
	// 暴力清场
	exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
}

func cleanupAndExit() {
	toggleSystemProxy(false) // 关代理
	terminateMihomoProcess() // 杀内核
	if conf.ServiceMode {
		exec.Command("sc", "stop", "mihomo").Run()
	}
	systray.Quit()
}

// --- Windows 注册表自启动 ---
func setRegistryAutoStart(enable bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.ALL_ACCESS)
	if err != nil {
		return
	}
	defer k.Close()

	if enable {
		k.SetStringValue("MihomoLauncher", "\""+exePath+"\" -minimized")
	} else {
		k.DeleteValue("MihomoLauncher")
	}
}

// --- 系统代理切换 ---
func toggleSystemProxy(enable bool) {
	val := 0
	if enable { val = 1 }
	regPath := `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	k, _ := registry.OpenKey(registry.CURRENT_USER, regPath, registry.ALL_ACCESS)
	defer k.Close()
	k.SetDWordValue("ProxyEnable", uint32(val))
}

func refreshIcon(name string) {
	data, _ := iconFs.ReadFile("icons/" + name)
	systray.SetIcon(data)
}

func updateMenuState(ins, unins *systray.MenuItem) {
	if conf.ServiceMode {
		ins.Disable()
		unins.Enable()
	} else {
		ins.Enable()
		unins.Disable()
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
	ioutil.WriteFile(filepath.Join(baseDir, "config.json"), data, 0644)
}

func onExit() {
	os.Exit(0)
}
