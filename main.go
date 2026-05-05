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
	APP_MUTEX  = "Global\\MihomoUltimateManager_V14"
	REG_RUN    = `Software\Microsoft\Windows\CurrentVersion\Run`
)

var (
	isExiting      bool
	hJob           windows.Handle
	httpClient     = &http.Client{Timeout: 2 * time.Second}
	exePath, _     = os.Executable()
	baseDir        = filepath.Dir(exePath)
)

// --- 进程联动核心：Job Object ---
func initJobObject() {
	h, _ := windows.CreateJobObject(nil, nil)
	if h != 0 {
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		windows.NewLazySystemDLL("kernel32.dll").NewProc("SetInformationJobObject").Call(
			uintptr(h), uintptr(windows.JobObjectExtendedLimitInformation),
			uintptr(unsafe.Pointer(&info)), uintptr(uint32(unsafe.Sizeof(info))),
		)
		hJob = h
	}
}

// --- 权限与静默调用 ---
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

func runCmdSilent(path string, args ...string) {
	cmd := exec.Command(path, args...)
	cmd.Dir = baseDir
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	if hJob != 0 {
		_ = cmd.Start()
		hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
		_ = windows.AssignProcessToJobObject(hJob, hp)
		windows.CloseHandle(hp)
	} else {
		_ = cmd.Start()
	}
}

// --- 服务检测逻辑 ---
func isServiceInstalled() bool {
	m, _ := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if m == 0 { return false }
	defer windows.CloseServiceHandle(m)
	s, err := windows.OpenService(m, windows.StringToUTF16Ptr("mihomo"), windows.SERVICE_QUERY_CONFIG)
	if err == nil {
		windows.CloseServiceHandle(s)
		return true
	}
	return false
}

func main() {
	if !isAdmin() { runAsAdmin(); os.Exit(0) }
	
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	h, err := windows.CreateMutex(nil, false, mName)
	if err != nil || windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		if h != 0 { windows.CloseHandle(h) }
		os.Exit(0)
	}

	initJobObject()
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
			runCmdSilent(target, "-d", baseDir)
		}
		time.Sleep(5 * time.Second)
	}
}

func onReady() {
	// 初始化图标
	systray.SetIcon(getIcon("default.ico"))

	// 1. 顶部菜单
	mWeb := systray.AddMenuItem("控制面板", "")
	mDir := systray.AddMenuItem("打开程序目录", "")
	systray.AddSeparator()

	// 2. 模式切换
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", false)
	mTun := systray.AddMenuItemCheckbox("TUN 模式", "", false)
	systray.AddSeparator()

	// 3. 服务管理菜单 (带状态置灰)
	mSvcRoot := systray.AddMenuItem("服务管理", "")
	mSvcBat := mSvcRoot.AddSubMenuItem("管理服务 (BAT)", "")
	mSvcInst := mSvcRoot.AddSubMenuItem("安装服务", "")
	mSvcUninst := mSvcRoot.AddSubMenuItem("卸载服务", "")
	
	// 4. 自启与隐藏
	mAuto := systray.AddMenuItemCheckbox("开机自动启动", "", false)
	mHide := systray.AddMenuItem("隐藏托盘图标", "")
	systray.AddSeparator()

	// 5. 退出
	mRestart := systray.AddMenuItem("重启内核", "")
	mExit := systray.AddMenuItem("彻底退出", "")

	// 循环更新 UI 状态
	go func() {
		for {
			if isExiting { return }
			
			// A. 服务按钮状态同步
			installed := isServiceInstalled()
			if installed {
				mSvcInst.Disable()
				mSvcUninst.Enable()
			} else {
				mSvcInst.Enable()
				mSvcUninst.Disable()
			}

			// B. 代理与 TUN 状态图标同步
			syncUI(mProxy, mTun, mAuto)
			
			time.Sleep(2 * time.Second)
		}
	}()

	// 交互事件处理
	for {
		select {
		case <-mWeb.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
		case <-mDir.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		case <-mProxy.ClickedCh:
			toggleProxy(!mProxy.Checked())
		case <-mTun.ClickedCh:
			setCfg(fmt.Sprintf(`{"tun":{"enable":%v}}`, !mTun.Checked()))
		case <-mSvcBat.ClickedCh:
			runCmdSilent(filepath.Join(baseDir, "mihomo-service", "mihomo-service.bat"))
		case <-mSvcInst.ClickedCh:
			runCmdSilent(filepath.Join(baseDir, "mihomo-service", "install.bat"))
		case <-mSvcUninst.ClickedCh:
			runCmdSilent(filepath.Join(baseDir, "mihomo-service", "uninstall.bat"))
		case <-mAuto.ClickedCh:
			toggleAutoStart(!mAuto.Checked())
		case <-mHide.ClickedCh:
			systray.Quit() // 销毁托盘，但不设置 isExiting，进程保持后台运行
		case <-mRestart.ClickedCh:
			runCmdSilent("taskkill", "/F", "/IM", "mihomo.exe", "/T")
		case <-mExit.ClickedCh:
			isExiting = true
			systray.Quit()
		}
	}
}

func syncUI(mP, mT, mA *systray.MenuItem) {
	// 系统代理
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	proxyOn := false
	if err == nil {
		v, _, _ := k.GetIntegerValue("ProxyEnable")
		if v == 1 { 
			mP.Check()
			proxyOn = true
		} else { mP.Uncheck() }
		k.Close()
	}

	// 自启状态
	k2, err := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.QUERY_VALUE)
	if err == nil {
		_, _, e := k2.GetStringValue("MihomoLauncher")
		if e == nil { mA.Check() } else { mA.Uncheck() }
		k2.Close()
	}

	// 内核状态与图标切换 (default, tun, proxy)
	resp, err := httpClient.Get(API_URL + "/configs")
	if err == nil {
		var d struct { Tun struct { Enable bool } `json:"tun"` }
		json.NewDecoder(resp.Body).Decode(&d)
		resp.Body.Close()

		if d.Tun.Enable {
			mT.Check()
			systray.SetIcon(getIcon("tun.ico"))
		} else if proxyOn {
			systray.SetIcon(getIcon("proxy.ico"))
		} else {
			mT.Uncheck()
			systray.SetIcon(getIcon("default.ico"))
		}
	}
}

func toggleAutoStart(enable bool) {
	k, _ := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.SET_VALUE)
	defer k.Close()
	if enable { k.SetStringValue("MihomoLauncher", exePath) } else { k.DeleteValue("MihomoLauncher") }
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

func getIcon(n string) []byte {
	b, _ := iconFs.ReadFile("icons/" + n)
	return b
}

func onExit() {
	if isExiting {
		if hJob != 0 { windows.CloseHandle(hJob) }
		os.Exit(0)
	}
}
