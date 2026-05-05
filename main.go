package main

import (
	"embed"
	"encoding/json"
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
	conf      Config
	mu        sync.Mutex
	exePath, _ = os.Executable()
	baseDir    = filepath.Dir(exePath)
	pipeName   = `\\.\pipe\mihomo_launcher_wakeup`
)

func main() {
	// 1. 单实例检测 (双击唤醒逻辑)
	l, err := net.Listen("unix", pipeName) // Windows 下 net.Listen 模拟命名管道或使用特定库
	// 简易 Windows 方案：尝试监听一个固定端口或使用命名管道
	ln, err := net.Listen("tcp", "127.0.0.1:54321") 
	if err != nil {
		// 发送唤醒信号给已存在的实例
		conn, _ := net.Dial("tcp", "127.0.0.1:54321")
		if conn != nil {
			conn.Write([]byte("WAKEUP"))
			conn.Close()
		}
		return
	}
	defer ln.Close()

	loadConfig()
	
	// 监听唤醒信号
	go func() {
		for {
			conn, _ := ln.Accept()
			if conn != nil {
				// 收到信号，重置隐藏状态并刷新 UI
				conf.TrayHidden = false
				saveConfig()
				// 注意：systray 不支持动态从退出状态恢复，
				// 这里的唤醒建议配合图标重新加载逻辑
				conn.Close()
			}
		}
	}()

	systray.Run(onReady, onExit)
}

func onReady() {
	refreshIcon("default.ico")
	systray.SetTitle("Mihomo Launcher")

	// 核心开关
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", conf.SystemProxy)
	mTun := systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", conf.TunEnabled)
	systray.AddSeparator()

	// 模式切换
	mModes := systray.AddMenuItem("模式切换", "")
	mRule := mModes.AddSubMenuItemCheckbox("规则模式", "", conf.ProxyMode == "rule")
	mGlobal := mModes.AddSubMenuItemCheckbox("全局模式", "", conf.ProxyMode == "global")
	mDirect := mModes.AddSubMenuItemCheckbox("直连模式", "", conf.ProxyMode == "direct")
	systray.AddSeparator()

	// 自启动设置
	mSettings := systray.AddMenuItem("自启动设置", "")
	mAutoStart := mSettings.AddSubMenuItemCheckbox("开机自动启动", "", conf.AutoStart)
	mInstallSvc := mSettings.AddSubMenuItem("安装后台服务", "")
	mUninstallSvc := mSettings.AddSubMenuItem("卸载后台服务", "")
	mRunBat := mSettings.AddSubMenuItem("管理服务 (BAT)", "")

	systray.AddSeparator()
	mHide := systray.AddMenuItem("隐藏托盘图标", "")
	mRestart := systray.AddMenuItem("重启内核", "")
	mExit := systray.AddMenuItem("退出程序", "")

	// 初始化 UI 状态
	updateMenuState(mInstallSvc, mUninstallSvc)

	// 事件循环
	go func() {
		for {
			select {
			case <-mProxy.ClickedCh:
				conf.SystemProxy = !mProxy.Checked()
				if conf.SystemProxy { mProxy.Check() } else { mProxy.Uncheck() }
				saveConfig()
			case <-mInstallSvc.ClickedCh:
				runSvcCmd("install")
				conf.ServiceMode = true
				saveConfig()
				updateMenuState(mInstallSvc, mUninstallSvc)
			case <-mUninstallSvc.ClickedCh:
				runSvcCmd("uninstall")
				conf.ServiceMode = false
				saveConfig()
				updateMenuState(mInstallSvc, mUninstallSvc)
			case <-mRunBat.ClickedCh:
				batPath := filepath.Join(baseDir, "mihomo-service", "mihomo-service.bat")
				exec.Command("cmd", "/c", "start", "", batPath).Run()
			case <-mHide.ClickedCh:
				conf.TrayHidden = true
				saveConfig()
				systray.Quit()
			case <-mExit.ClickedCh:
				terminateAll()
				systray.Quit()
			}
		}
	}()
}

// 彻底退出逻辑
func terminateAll() {
	// 1. 关闭系统代理
	exec.Command("reg", "add", `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`, "/v", "ProxyEnable", "/t", "REG_DWORD", "/d", "0", "/f").Run()
	// 2. 停止服务 (如果开启)
	if conf.ServiceMode {
		exec.Command("sc", "stop", "mihomo").Run()
	}
	// 3. 暴力清场
	exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
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

func loadConfig() {
	data, _ := ioutil.ReadFile(filepath.Join(baseDir, "config.json"))
	json.Unmarshal(data, &conf)
}

func saveConfig() {
	data, _ := json.MarshalIndent(conf, "", "  ")
	ioutil.WriteFile(filepath.Join(baseDir, "config.json"), data, 0644)
}

func runSvcCmd(arg string) {
	svcExe := filepath.Join(baseDir, "mihomo-service", "mihomo-service.exe")
	exec.Command(svcExe, arg).Run()
}

func onExit() {
	os.Exit(0)
}
