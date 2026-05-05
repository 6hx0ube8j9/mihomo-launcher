package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/getlantern/systray"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed icons/*.ico
var iconFs embed.FS

const (
	API_URL    = "http://127.0.0.1:9090"
	PROXY_ADDR = "127.0.0.1:7890"
	LOCK_FILE  = "tun_on.lock"
	APP_MUTEX  = "Global\\MihomoFullManager_Final"
	REG_RUN    = `Software\Microsoft\Windows\CurrentVersion\Run`
)

var (
	isExiting  bool
	httpClient = &http.Client{Timeout: 2 * time.Second}
	exePath, _ = os.Executable()
	baseDir    = filepath.Dir(exePath)
)

// --- 权限与进程控制 ---

func isAdmin() bool {
	var token windows.Token
	err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token)
	if err != nil { return false }
	defer token.Close()
	return token.IsElevated()
}

func runAsAdmin() {
	verb, _ := syscall.UTF16PtrFromString("runas")
	exePtr, _ := syscall.UTF16PtrFromString(exePath)
	cwdPtr, _ := syscall.UTF16PtrFromString(baseDir)
	// 使用 SW_HIDE 确保提权后的瞬间不会闪现黑框
	windows.ShellExecute(0, verb, exePtr, nil, cwdPtr, windows.SW_HIDE)
}

func runCmdSilent(path string, args ...string) {
	cmd := exec.Command(path, args...)
	cmd.Dir = baseDir
	// CREATE_NO_WINDOW: 彻底隐藏所有命令行窗口
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
	_ = cmd.Start()
}

// --- 程序入口 ---

func main() {
	// 1. 检查权限：确保安装服务和修改系统设置有效
	if !isAdmin() {
		runAsAdmin()
		os.Exit(0)
	}

	// 2. 单实例锁：防止托盘图标重复
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	h, err := windows.CreateMutex(nil, false, mName)
	if err != nil || windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		if h != 0 { windows.CloseHandle(h) }
		os.Exit(0)
	}

	// 3. 设置运行环境
	os.Chdir(baseDir)

	// 4. 启动内核守护协程
	go monitorKernel()

	// 5. 启动托盘界面
	systray.Run(onReady, onExit)
}

func monitorKernel() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isExiting { return }
		// 检查内核存活状态
		_, err := httpClient.Get(API_URL)
		if err != nil {
			// 内核未响应，尝试清理并重启
			runCmdSilent("taskkill", "/F", "/IM", "mihomo.exe", "/T")
			time.Sleep(500 * time.Millisecond)
			runCmdSilent(target, "-d", baseDir)
		}
		time.Sleep(5 * time.Second)
	}
}

// --- 托盘 UI 逻辑 ---

func onReady() {
	systray.SetIcon(getIcon("tray_default.ico"))

	// 基础管理
	mWeb := systray.AddMenuItem("控制面板", "打开 Web UI")
	mDir := systray.AddMenuItem("打开程序目录", "打开资源管理器")
	systray.AddSeparator()

	// 功能切换
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", false)
	mTun := systray.AddMenuItemCheckbox("TUN 模式", "", false)
	systray.AddSeparator()

	// 服务与启动管理
	mService := systray.AddMenuItem("服务管理", "")
	mInst := mService.AddSubMenuItem("安装服务", "执行 install.bat")
	mUninst := mService.AddSubMenuItem("卸载服务", "执行 uninstall.bat")
	
	mAuto := systray.AddMenuItemCheckbox("开机自动启动", "", isAutoStart())
	systray.AddSeparator()

	mRestart := systray.AddMenuItem("重启内核", "重启 mihomo.exe")
	mExit := systray.AddMenuItem("彻底退出", "退出程序并关闭内核")

	// 状态同步协程
	go func() {
		for {
			if isExiting { return }
			syncStatus(mProxy, mTun)
			time.Sleep(2 * time.Second)
		}
	}()

	// 事件循环
	for {
		select {
		case <-mWeb.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
		case <-mDir.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		case <-mProxy.ClickedCh:
			toggleProxy(!mProxy.Checked())
		case <-mTun.ClickedCh:
			enable := !mTun.Checked()
			if enable {
				os.Create(filepath.Join(baseDir, LOCK_FILE))
			} else {
				os.Remove(filepath.Join(baseDir, LOCK_FILE))
			}
			setCfg(fmt.Sprintf(`{"tun":{"enable":%v}}`, enable))
		case <-mInst.ClickedCh:
			runCmdSilent(filepath.Join(baseDir, "mihomo-service", "install.bat"))
		case <-mUninst.ClickedCh:
			runCmdSilent(filepath.Join(baseDir, "mihomo-service", "uninstall.bat"))
		case <-mAuto.ClickedCh:
			toggleAutoStart(!mAuto.Checked(), mAuto)
		case <-mRestart.ClickedCh:
			runCmdSilent("taskkill", "/F", "/IM", "mihomo.exe", "/T")
		case <-mExit.ClickedCh:
			systray.Quit()
		}
	}
}

// --- 功能实现细节 ---

func syncStatus(mP, mT *systray.MenuItem) {
	// 1. 同步系统代理状态
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err == nil {
		v, _, _ := k.GetIntegerValue("ProxyEnable")
		if v == 1 { mP.Check() } else { mP.Uncheck() }
		k.Close()
	}

	// 2. 同步内核 TUN 状态
	resp, err := httpClient.Get(API_URL + "/configs")
	if err == nil {
		var d struct { Tun struct { Enable bool } `json:"tun"` }
		json.NewDecoder(resp.Body).Decode(&d)
		resp.Body.Close()
		if d.Tun.Enable {
			mT.Check()
			systray.SetIcon(getIcon("tray_tun.ico"))
		} else {
			mT.Uncheck()
			systray.SetIcon(getIcon("tray_default.ico"))
		}
	}
}

func isAutoStart() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.QUERY_VALUE)
	if err != nil { return false }
	defer k.Close()
	_, _, err = k.GetStringValue("MihomoLauncher")
	return err == nil
}

func toggleAutoStart(enable bool, item *systray.MenuItem) {
	k, _ := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.SET_VALUE)
	defer k.Close()
	if enable {
		k.SetStringValue("MihomoLauncher", exePath)
		item.Check()
	} else {
		k.DeleteValue("MihomoLauncher")
		item.Uncheck()
	}
}

func toggleProxy(enable bool) {
	k, _, _ := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.ALL_ACCESS)
	if enable {
		k.SetDWordValue("ProxyEnable", 1)
		k.SetStringValue("ProxyServer", PROXY_ADDR)
	} else {
		k.SetDWordValue("ProxyEnable", 0)
	}
	k.Close()
	// 刷新系统代理设置
	windows.NewLazySystemDLL("user32.dll").NewProc("UpdatePerUserSystemParameters").Call(0, 0, 0, 0)
}

func setCfg(jsonBody string) {
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(jsonBody)))
	if r, err := httpClient.Do(req); err == nil { r.Body.Close() }
}

func getIcon(name string) []byte {
	data, _ := iconFs.ReadFile("icons/" + name)
	return data
}

func onExit() {
	isExiting = true
	os.Exit(0)
}
