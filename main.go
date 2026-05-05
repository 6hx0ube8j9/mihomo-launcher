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
	"strings"
	"syscall"
	"time"
	"unsafe"

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
	APP_MUTEX  = "Global\\MihomoFullManagerMutex"
	REG_RUN    = `Software\Microsoft\Windows\CurrentVersion\Run`
)

var (
	isExiting  bool
	httpClient = &http.Client{Timeout: 2 * time.Second}
	exePath, _ = os.Executable()
	baseDir    = filepath.Dir(exePath)
)

// --- 核心：管理员权限与互斥锁 ---
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
	windows.ShellExecute(0, verb, exePtr, nil, cwdPtr, windows.SW_HIDE)
}

// --- 核心：执行外部脚本/程序（彻底告别黑窗） ---
func runCmdSilent(path string, args ...string) {
	cmd := exec.Command(path, args...)
	cmd.Dir = baseDir
	// 关键：CREATE_NO_WINDOW 阻止黑窗，DETACHED_PROCESS 允许主程序退出后子进程继续跑
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW | 0x00000008, 
	}
	_ = cmd.Start()
}

func main() {
	if !isAdmin() {
		runAsAdmin()
		os.Exit(0)
	}

	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	h, err := windows.CreateMutex(nil, false, mName)
	if err != nil || windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		if h != 0 { windows.CloseHandle(h) }
		os.Exit(0)
	}

	os.Chdir(baseDir)
	go monitorKernel()
	systray.Run(onReady, onExit)
}

func monitorKernel() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isExiting { return }
		_, err := httpClient.Get(API_URL)
		if err != nil {
			runCmdSilent("taskkill", "/F", "/IM", "mihomo.exe", "/T")
			time.Sleep(500 * time.Millisecond)
			runCmdSilent(target, "-d", baseDir)
		}
		time.Sleep(5 * time.Second)
	}
}

// --- 托盘逻辑 ---
func onReady() {
	systray.SetIcon(getIcon("tray_default.ico"))

	// 菜单定义
	mWeb := systray.AddMenuItem("控制面板", "")
	mDir := systray.AddMenuItem("打开程序目录", "")
	systray.AddSeparator()
	
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", false)
	mTun := systray.AddMenuItemCheckbox("TUN 模式", "", false)
	systray.AddSeparator()

	mService := systray.AddMenuItem("服务管理", "")
	mInst := mService.AddSubMenuItem("安装服务", "")
	mUninst := mService.AddSubMenuItem("卸载服务", "")
	
	mAuto := systray.AddMenuItemCheckbox("开机自动启动", "", isAutoStart())
	systray.AddSeparator()
	
	mRestart := systray.AddMenuItem("重启内核", "")
	mExit := systray.AddMenuItem("彻底退出", "")

	go func() {
		for {
			if isExiting { return }
			syncStatus(mProxy, mTun)
			time.Sleep(2 * time.Second)
		}
	}()

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
			if enable { os.Create(LOCK_FILE) } else { os.Remove(LOCK_FILE) }
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

// --- 逻辑补充 ---

func syncStatus(mP, mT *systray.MenuItem) {
	// 代理状态
	k, _ := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if k != 0 {
		v, _, _ := k.GetIntegerValue("ProxyEnable")
		if v == 1 { mP.Check() } else { mP.Uncheck() }
		k.Close()
	}
	// 内核状态
	resp, err := httpClient.Get(API_URL + "/configs")
	if err == nil {
		var d struct{ Tun struct{ Enable bool } `json:"tun"` }
		json.NewDecoder(resp.Body).Decode(&d)
		resp.Body.Close()
		if d.Tun.Enable { mT.Check(); systray.SetIcon(getIcon("tray_tun.ico")) } else { mT.Uncheck(); systray.SetIcon(getIcon("tray_default.ico")) }
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

func toggleProxy(e bool) {
	k, _, _ := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.ALL_ACCESS)
	if e {
		k.SetDWordValue("ProxyEnable", 1)
		k.SetStringValue("ProxyServer", PROXY_ADDR)
	} else {
		k.SetDWordValue("ProxyEnable", 0)
	}
	k.Close()
	windows.NewLazySystemDLL("user32.dll").NewProc("UpdatePerUserSystemParameters").Call(0, 0, 0, 0)
}

func setCfg(j string) {
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(j)))
	if r, err := httpClient.Do(req); err == nil { r.Body.Close() }
}

func getIcon(n string) []byte { b, _ := iconFs.ReadFile("icons/" + n); return b }

func onExit() {
	isExiting = true
	os.Exit(0)
}
