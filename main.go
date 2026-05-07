package main

import (
	"bytes"
	"embed"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	APP_MUTEX   = "Global\\MihomoLauncher_Unique_Mutex"
	API_URL     = "http://127.0.0.1:9090"
	CONFIG_FILE = "mihomo-launcher.ini"
	REG_RUN     = `Software\Microsoft\Windows\CurrentVersion\Run`
	REG_PROXY   = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	APP_NAME    = "MihomoLauncher"

	StateStop = 0; StateError = 1; StateTun = 2; StateProxy = 3; StateDefault = 4
)

var (
	isReallyExiting bool
	hJob            windows.Handle
	hMutex          windows.Handle
	httpClient      = &http.Client{Timeout: 1 * time.Second}
	exePath, _      = os.Executable()
	baseDir         = filepath.Dir(exePath)
	configData      = make(map[string]string)
	configMu        sync.RWMutex
	lastState       = -1
)

// --- 基础工具函数 ---

func isAdmin() bool {
	var token windows.Token
	err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token)
	if err != nil { return false }
	defer token.Close()
	return token.IsElevated()
}

func runAsAdmin() {
	verb, _ := syscall.UTF16PtrFromString("runas")
	exe, _ := syscall.UTF16PtrFromString(exePath)
	cwd, _ := syscall.UTF16PtrFromString(baseDir)
	windows.ShellExecute(0, verb, exe, nil, cwd, windows.SW_HIDE)
}

func initJobObject() {
	h, _ := windows.CreateJobObject(nil, nil)
	if h != 0 {
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		windows.NewLazySystemDLL("kernel32.dll").NewProc("SetInformationJobObject").Call(
			uintptr(h), 9, uintptr(unsafe.Pointer(&info)), uintptr(uint32(unsafe.Sizeof(info))),
		)
		hJob = h
	}
}

// --- 配置管理 (来自第二份的强健解析) ---

func loadIniConfigAll() {
	b, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	configMu.Lock()
	defer configMu.Unlock()
	configData = make(map[string]string)
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") { continue }
		if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
			configData[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
}

func getIniConfig(key string) string {
	configMu.RLock()
	defer configMu.RUnlock()
	return configData[key]
}

func saveIniConfig(key, val string) {
	configMu.Lock()
	configData[key] = val
	var buf bytes.Buffer
	for k, v := range configData {
		if k != "" { buf.WriteString(fmt.Sprintf("%s = %s\n", k, v)) }
	}
	configMu.Unlock()
	_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), buf.Bytes(), 0644)
}

// --- 核心逻辑：自动同步 (来自第一份的优点) ---

func syncConfigToKernel() {
	// 给内核 10 秒启动宽限期，每秒尝试一次同步
	for i := 0; i < 10; i++ {
		_, err := httpClient.Get(API_URL)
		if err == nil {
			configMu.RLock()
			tun := configData["tun_enabled"] == "true"
			mode := configData["mode"]
			proxy := configData["system_proxy_enabled"] == "true"
			configMu.RUnlock()

			if tun { setTunMode(true) }
			if mode != "" { setMihomoMode(mode) }
			if proxy { setProxyRegistry(true) }
			return
		}
		time.Sleep(time.Second)
	}
}

// --- 守护进程逻辑 ---

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting { return }
		
		if !isProcessRunning("mihomo.exe") {
			// 内核未运行，尝试拉起
			cmd := exec.Command(target, "-d", baseDir)
			cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
			if err := cmd.Start(); err == nil && hJob != 0 {
				hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
				_ = windows.AssignProcessToJobObject(hJob, hp)
				windows.CloseHandle(hp)
				// 拉起后，立即触发异步配置同步
				go syncConfigToKernel()
			}
			lastState = StateStop
			updateIconByState(StateStop)
		} else {
			// 内核运行中，更新状态图标
			curr := checkSystemState()
			if curr != lastState {
				updateIconByState(curr)
				lastState = curr
			}
		}
		time.Sleep(2 * time.Second)
	}
}

func checkSystemState() int {
	_, err := httpClient.Get(API_URL)
	if err != nil { return StateError }
	
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if strings.Contains(strings.ToLower(i.Name), "mihomo") { return StateTun }
	}
	
	key, _ := registry.OpenKey(registry.CURRENT_USER, REG_PROXY, registry.QUERY_VALUE)
	val, _, _ := key.GetIntegerValue("ProxyEnable")
	key.Close()
	if val == 1 { return StateProxy }
	
	return StateDefault
}

func isProcessRunning(name string) bool {
	snapshot, _ := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if snapshot == 0 { return false }
	defer windows.CloseHandle(snapshot)
	var proc windows.ProcessEntry32
	proc.Size = uint32(unsafe.Sizeof(proc))
	for windows.Process32Next(snapshot, &proc) == nil {
		if strings.EqualFold(windows.UTF16ToString(proc.ExeFile[:]), name) { return true }
	}
	return false
}

// --- UI 菜单逻辑 (精简版) ---

func onReady() {
	loadIniConfigAll()
	updateIconByState(StateDefault)

	mWeb := systray.AddMenuItem("进入控制面板", "")
	systray.AddSeparator()

	curMode := getIniConfig("mode")
	mModeR := systray.AddMenuItemCheckbox("规则模式", "", curMode == "rule" || curMode == "")
	mModeG := systray.AddMenuItemCheckbox("全局模式", "", curMode == "global")
	mModeD := systray.AddMenuItemCheckbox("直连模式", "", curMode == "direct")
	systray.AddSeparator()

	mTun := systray.AddMenuItemCheckbox("TUN 模式", "", getIniConfig("tun_enabled") == "true")
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", getIniConfig("system_proxy_enabled") == "true")
	systray.AddSeparator()

	mAuto := systray.AddMenuItemCheckbox("开机自启", "", getIniConfig("auto_start") == "true")
	mDir := systray.AddMenuItem("浏览本地文件", "")
	mRestart := systray.AddMenuItem("重启内核", "")
	mExit := systray.AddMenuItem("退出程序", "")

	for {
		select {
		case <-mWeb.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
		case <-mModeR.ClickedCh: setMihomoMode("rule"); mModeR.Check(); mModeG.Uncheck(); mModeD.Uncheck()
		case <-mModeG.ClickedCh: setMihomoMode("global"); mModeR.Uncheck(); mModeG.Check(); mModeD.Uncheck()
		case <-mModeD.ClickedCh: setMihomoMode("direct"); mModeR.Uncheck(); mModeG.Uncheck(); mModeD.Check()
		case <-mTun.ClickedCh:
			next := !mTun.Checked(); setTunMode(next)
			if next { mTun.Check() } else { mTun.Uncheck() }
		case <-mProxy.ClickedCh:
			next := !mProxy.Checked(); setProxyRegistry(next)
			if next { mProxy.Check() } else { mProxy.Uncheck() }
		case <-mAuto.ClickedCh:
			next := !mAuto.Checked(); toggleAutoStart(next)
			if next { mAuto.Check() } else { mAuto.Uncheck() }
		case <-mDir.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		case <-mRestart.ClickedCh:
			exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
		case <-mExit.ClickedCh:
			isReallyExiting = true
			systray.Quit()
		}
	}
}

func onExit() {
	if isReallyExiting {
		exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
		if hJob != 0 { windows.CloseHandle(hJob) }
		if hMutex != 0 { windows.CloseHandle(hMutex) }
		os.Exit(0)
	}
}

// --- 系统操作 ---

func setMihomoMode(mode string) {
	saveIniConfig("mode", mode)
	json := fmt.Sprintf(`{"mode": "%s"}`, mode)
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(json)))
	if resp, err := httpClient.Do(req); err == nil { resp.Body.Close() }
}

func setTunMode(enable bool) {
	saveIniConfig("tun_enabled", fmt.Sprint(enable))
	json := fmt.Sprintf(`{"tun": {"enable": %v}}`, enable)
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(json)))
	if resp, err := httpClient.Do(req); err == nil { resp.Body.Close() }
}

func setProxyRegistry(enable bool) {
	saveIniConfig("system_proxy_enabled", fmt.Sprint(enable))
	key, _ := registry.OpenKey(registry.CURRENT_USER, REG_PROXY, registry.SET_VALUE)
	val := uint32(0); if enable { val = 1 }
	key.SetDWordValue("ProxyEnable", val)
	key.Close()
}

func toggleAutoStart(enable bool) {
	saveIniConfig("auto_start", fmt.Sprint(enable))
	key, _ := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.SET_VALUE)
	if enable { key.SetStringValue(APP_NAME, exePath) } else { key.DeleteValue(APP_NAME) }
	key.Close()
}

func updateIconByState(state int) {
	files := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
	if state < 0 || state >= len(files) { return }
	b, err := iconFs.ReadFile("icons/" + files[state])
	if err == nil { systray.SetIcon(b) }
}

// --- 程序入口 ---

func main() {
	// 1. 提权
	if !isAdmin() { runAsAdmin(); os.Exit(0) }

	// 2. 极速单实例判定 (静默退出)
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	h, _ := windows.CreateMutex(nil, false, mName)
	if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		if h != 0 { windows.CloseHandle(h) }
		os.Exit(0) 
	}
	hMutex = h

	// 3. 基础资源初始化
	os.Chdir(baseDir)
	initJobObject()

	// 4. 启动后台守护协程
	go monitorKernelDaemon()

	// 5. 运行托盘 UI
	systray.Run(onReady, onExit)
}
