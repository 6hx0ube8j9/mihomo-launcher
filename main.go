package main

import (
	"embed"
	"encoding/json"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/getlantern/systray"
)

// --- 资源嵌入 (仅嵌入图标，不嵌入后端) ---
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
	// 核心后端路径：指向根目录下的 mihomo.exe
	coreExe    = filepath.Join(baseDir, "mihomo.exe")
)

func main() {
	// 1. 单实例检测 (TCP 握手实现双击唤醒信号)
	ln, err := net.Listen("tcp", "127.0.0.1:54321")
	if err != nil {
		conn, _ := net.Dial("tcp", "127.0.0.1:54321")
		if conn != nil {
			conn.Write([]byte("WAKEUP"))
			conn.Close()
		}
		return
	}
	defer ln.Close()

	loadConfig()
	
	// 启动内核管理协程 (如果不是服务模式，则由 Launcher 负责拉起)
	if !conf.ServiceMode {
		go manageCoreLifecycle()
	}

	systray.Run(onReady, onExit)
}

func onReady() {
	refreshIcon("default.ico")
	systray.SetTitle("Mihomo Launcher")

	// --- 菜单项构建 ---
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
	mRunBat := mSettings.AddSubMenuItem("管理服务 (BAT)", "")

	systray.AddSeparator()
	mHide := systray.AddMenuItem("隐藏托盘图标", "")
	mRestart := systray.AddMenuItem("重启内核", "")
	mExit := systray.AddMenuItem("退出程序", "")

	updateMenuState(mInstallSvc, mUninstallSvc)

	// --- 异步事件处理 ---
	go func() {
		for {
			select {
			case <-mProxy.ClickedCh:
				conf.SystemProxy = !mProxy.Checked()
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
			case <-mRestart.ClickedCh:
				terminateCore() // 先杀掉
				if !conf.ServiceMode { go manageCoreLifecycle() } // 便携模式重启
			case <-mHide.ClickedCh:
				conf.TrayHidden = true
				saveConfig()
				systray.Quit()
			case <-mExit.ClickedCh:
				refreshIcon("stop.ico")
				terminateAll() // 暴力清理所有残留
				systray.Quit()
			}
		}
	}()
}

// 核心后端生命周期管理 (针对便携模式)
func manageCoreLifecycle() {
	if _, err := os.Stat(coreExe); os.IsNotExist(err) {
		refreshIcon("error.ico")
		return
	}
	// 启动外部 mihomo.exe
	cmd := exec.Command(coreExe)
	// 隐藏后端黑窗口
	cmd.SysProcAttr = &os.ProcAttr{Files: []*os.File{nil, nil, nil}} 
	cmd.Run()
}

func terminateCore() {
	exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
}

func terminateAll() {
	// 1. 关闭注册表系统代理
	exec.Command("reg", "add", `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`, "/v", "ProxyEnable", "/t", "REG_DWORD", "/d", "0", "/f").Run()
	// 2. 暴力杀掉所有相关进程
	terminateCore()
	// 3. 如果是服务模式，停止服务
	if conf.ServiceMode {
		exec.Command("sc", "stop", "mihomo").Run()
	}
}

func refreshIcon(name string) {
	data, err := iconFs.ReadFile("icons/" + name)
	if err == nil {
		systray.SetIcon(data)
	}
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
	data, err := ioutil.ReadFile(filepath.Join(baseDir, "config.json"))
	if err == nil {
		json.Unmarshal(data, &conf)
	} else {
		conf = Config{ProxyMode: "rule", ServiceMode: false}
	}
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
