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
	APP_MUTEX  = "Global\\MihomoUltimateManager_V15"
	REG_RUN    = `Software\Microsoft\Windows\CurrentVersion\Run`
)

var (
	isReallyExiting bool // 只有点击“彻底退出”才会设为真
	hJob            windows.Handle
	httpClient      = &http.Client{Timeout: 2 * time.Second}
	exePath, _      = os.Executable()
	baseDir         = filepath.Dir(exePath)
)

// --- 进程树绑定：确保主程序崩溃时内核也退出，但正常隐藏时不影响 ---
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

// --- 权限与调用 ---
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

// 修复后的命令执行：强制指定子目录并使用绝对路径
func runCmdSilent(fullPath string) {
	dir := filepath.Dir(fullPath)
	cmd := exec.Command("cmd.exe", "/C", filepath.Base(fullPath))
	cmd.Dir = dir // 关键：切换到脚本所在目录
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
		if isReallyExiting { return }
		_, err := httpClient.Get(API_URL)
		if err != nil {
			// 内核不在运行，启动它
			runCmdSilent(target)
		}
		time.Sleep(5 * time.Second)
	}
}

func onReady() {
	systray.SetIcon(getIcon("default.ico"))

	mWeb := systray.AddMenuItem("控制面板", "")
	mDir := systray.AddMenuItem("打开程序目录", "")
	systray.AddSeparator()

	mProxy := systray.AddMenuItemCheckbox("系统代理", "", false)
	mTun := systray.AddMenuItemCheckbox("TUN 模式", "", false)
	systray.AddSeparator()

	mSvcRoot := systray.AddMenuItem("服务管理", "")
	mSvcBat := mSvcRoot.AddSubMenuItem("管理服务 (BAT)", "")
	mSvcInst := mSvcRoot.AddSubMenuItem("安装服务", "")
	mSvcUninst := mSvcRoot.AddSubMenuItem("卸载服务", "")
	
	mAuto := systray.AddMenuItemCheckbox("开机自动启动", "", false)
	mHide := systray.AddMenuItem("隐藏托盘图标", "点击后图标消失，进程常驻后台")
	systray.AddSeparator()

	mRestart := systray.AddMenuItem("重启内核", "")
	mExit := systray.AddMenuItem("彻底退出", "")

	// 自动同步状态
	go func() {
		for {
			if isReallyExiting { return }
			installed := isServiceInstalled()
			if installed {
				mSvcInst.Disable()
				mSvcUninst.Enable()
			} else {
				mSvcInst.Enable()
				mSvcUninst.Disable()
			}
			syncUI(mProxy, mTun, mAuto)
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
			// 仅退出托盘 UI 循环，不杀死进程
			systray.Quit()
		case <-mRestart.ClickedCh:
			// 杀死内核进程，monitorKernel 会自动拉起
			exec.Command("taskkill", "/F", "/IM", "mihomo.exe", "/T").Run()
		case <-mExit.ClickedCh:
			isReallyExiting = true
			systray.Quit()
		}
	}
}

func syncUI(mP, mT, mA *systray.MenuItem) {
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

	k2, err := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.QUERY_VALUE)
	if err == nil {
		_, _, e := k2.GetStringValue("MihomoLauncher")
		if e == nil { mA.Check() } else { mA.Uncheck() }
		k2.Close()
	}

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
	// 关键逻辑：如果标志位为假，说明是“隐藏”操作，不执行 os.Exit(0)
	if isReallyExiting {
		if hJob != 0 { windows.CloseHandle(hJob) }
		os.Exit(0)
	}
}
