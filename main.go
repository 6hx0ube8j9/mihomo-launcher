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
	APP_MUTEX  = "Global\\MihomoUltimateManager_V16"
	REG_RUN    = `Software\Microsoft\Windows\CurrentVersion\Run`
)

var (
	isReallyExiting bool 
	hJob            windows.Handle
	httpClient      = &http.Client{Timeout: 2 * time.Second}
	exePath, _      = os.Executable()
	baseDir         = filepath.Dir(exePath)
)

// --- 修复点 1: 强制进程绑定 (主程序死，内核死) ---
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

// --- 修复点 2: 解决 BAT 无反应，使用 ShellExecute 保证脚本执行 ---
func runBat(path string) {
	verbPtr, _ := syscall.UTF16PtrFromString("runas") // 以管理员运行脚本
	pathPtr, _ := syscall.UTF16PtrFromString(path)
	dirPtr, _ := syscall.UTF16PtrFromString(filepath.Dir(path))
	windows.ShellExecute(0, verbPtr, pathPtr, nil, dirPtr, windows.SW_SHOWNORMAL)
}

// 启动内核专用（保持静默）
func runKernelSilent(path string) {
	// 检查是否已经有 mihomo.exe 在跑，防止无限开启
	check := exec.Command("tasklist")
	out, _ := check.Output()
	if strings.Contains(string(out), "mihomo.exe") {
		return 
	}

	cmd := exec.Command(path, "-d", baseDir)
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
		// 通过 API 检测内核是否真的活着
		_, err := httpClient.Get(API_URL)
		if err != nil {
			runKernelSilent(target)
		}
		time.Sleep(10 * time.Second) // 增加间隔，防止瞬时拉起多个
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
	mHide := systray.AddMenuItem("隐藏托盘图标", "")
	systray.AddSeparator()

	mRestart := systray.AddMenuItem("重启内核", "")
	mExit := systray.AddMenuItem("彻底退出", "")

	go func() {
		for {
			if isReallyExiting { return }
			// 动态置灰逻辑
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
			// 使用 ShellExecute 运行 BAT
			runBat(filepath.Join(baseDir, "mihomo-service", "mihomo-service.bat"))
		case <-mSvcInst.ClickedCh:
			runBat(filepath.Join(baseDir, "mihomo-service", "install.bat"))
		case <-mSvcUninst.ClickedCh:
			runBat(filepath.Join(baseDir, "mihomo-service", "uninstall.bat"))
		case <-mAuto.ClickedCh:
			toggleAutoStart(!mAuto.Checked())
		case <-mHide.ClickedCh:
			// 退出 UI 线程，进入纯后台模式
			systray.Quit()
		case <-mRestart.ClickedCh:
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
	// 修复点 3: 只有明确彻底退出时才结束进程
	if isReallyExiting {
		if hJob != 0 { windows.CloseHandle(hJob) }
		os.Exit(0)
	}
}
