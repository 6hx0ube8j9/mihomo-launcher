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
	WAKEUP_PORT = "18579"
	APP_MUTEX   = "Global\\MihomoLauncher_Unique_Mutex"
	API_URL     = "http://127.0.0.1:9090"
	CONFIG_FILE = "config.ini"
	REG_RUN     = `Software\Microsoft\Windows\CurrentVersion\Run`
	REG_PROXY   = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	APP_NAME    = "MihomoLauncher"

	StateStop = 0; StateError = 1; StateTun = 2; StateProxy = 3; StateDefault = 4
)

var (
	isReallyExiting bool
	hJob            windows.Handle
	httpClient      = &http.Client{Timeout: 1 * time.Second}
	exePath, _      = os.Executable()
	baseDir         = filepath.Dir(exePath)
	configData      = make(map[string]string)
	configMu        sync.RWMutex
	lastState       = -1
)

// --- 基础工具函数 (保持不动) ---

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

func checkSystemState() int {
	_, err := httpClient.Get(API_URL)
	if err != nil {
		if !isProcessRunning("mihomo.exe") { return StateStop }
		return StateError
	}
	if isInterfaceExisted("Mihomo") { return StateTun }
	if isProxyEnabledInRegistry() { return StateProxy }
	return StateDefault
}

func isInterfaceExisted(name string) bool {
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if strings.Contains(strings.ToLower(i.Name), strings.ToLower(name)) { return true }
	}
	return false
}

func isProxyEnabledInRegistry() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, REG_PROXY, registry.QUERY_VALUE)
	if err != nil { return false }
	defer key.Close()
	val, _, err := key.GetIntegerValue("ProxyEnable")
	return err == nil && val == 1
}

func setSystemProxy(enable bool) {
	key, _ := registry.OpenKey(registry.CURRENT_USER, REG_PROXY, registry.SET_VALUE)
	defer key.Close()
	val := uint32(0); if enable { val = 1 }
	key.SetDWordValue("ProxyEnable", val)
	saveIniConfig("system_proxy", fmt.Sprint(enable))
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

// --- 核心守护逻辑 (已优化) ---

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		// 即使托盘退出了，只要 isReallyExiting 是 false，这个循环就一直跑
		if isReallyExiting { return }

		_, err := httpClient.Get(API_URL)
		if err == nil {
			// 接管模式：静默并同步 UI 状态
			curr := checkSystemState()
			if curr != lastState {
				updateIconByState(curr)
				lastState = curr
			}
		} else {
			if isProcessRunning("mihomo.exe") {
				// 异常静默：进程在但 API 不通，不操作
				if lastState != StateError {
					updateIconByState(StateError)
					lastState = StateError
				}
			} else {
				// 守护拉起：环境空白，主动补位
				cmd := exec.Command(target, "-d", baseDir)
				cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
				if err := cmd.Start(); err == nil && hJob != 0 {
					hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
					_ = windows.AssignProcessToJobObject(hJob, hp)
					windows.CloseHandle(hp)
				}
				lastState = StateStop
				updateIconByState(StateStop)
			}
		}
		time.Sleep(2 * time.Second)
	}
}

// --- 界面逻辑 (支持假退出) ---

func onReady() {
	loadIniConfigAll()
	systray.SetTooltip(APP_NAME)
	updateIconByState(StateDefault)

	mWeb := systray.AddMenuItem("进入控制面板", "")
	systray.AddSeparator()

	mode := getIniConfig("mode")
	mModeR := systray.AddMenuItemCheckbox("规则模式", "", mode == "rule" || mode == "")
	mModeG := systray.AddMenuItemCheckbox("全局模式", "", mode == "global")
	mModeD := systray.AddMenuItemCheckbox("直连模式", "", mode == "direct")
	systray.AddSeparator()

	mTun := systray.AddMenuItemCheckbox("TUN 模式", "", getIniConfig("tun") == "true")
	mSystemProxy := systray.AddMenuItemCheckbox("系统代理", "", getIniConfig("system_proxy") == "true" || isProxyEnabledInRegistry())
	systray.AddSeparator()

	mAutoRun := systray.AddMenuItemCheckbox("开机自启", "", isAutoRunEnabled())
	mDir := systray.AddMenuItem("浏览本地文件", "")
	mRestart := systray.AddMenuItem("重启内核", "")
	mHide := systray.AddMenuItem("隐藏托盘图标", "")
	mExit := systray.AddMenuItem("退出程序", "")

	go syncConfigToKernel()

	for {
		select {
		case <-mWeb.ClickedCh: openWebPanel()
		case <-mModeR.ClickedCh: setMihomoMode("rule"); mModeR.Check(); mModeG.Uncheck(); mModeD.Uncheck()
		case <-mModeG.ClickedCh: setMihomoMode("global"); mModeR.Uncheck(); mModeG.Check(); mModeD.Uncheck()
		case <-mModeD.ClickedCh: setMihomoMode("direct"); mModeR.Uncheck(); mModeG.Uncheck(); mModeD.Check()
		case <-mTun.ClickedCh:
			enable := !mTun.Checked()
			setTunMode(enable)
			if enable { mTun.Check() } else { mTun.Uncheck() }
		case <-mSystemProxy.ClickedCh:
			enable := !mSystemProxy.Checked()
			setSystemProxy(enable)
			if enable { mSystemProxy.Check() } else { mSystemProxy.Uncheck() }
		case <-mAutoRun.ClickedCh:
			toggleAutoRun()
			if isAutoRunEnabled() { mAutoRun.Check() } else { mAutoRun.Uncheck() }
		case <-mDir.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		case <-mRestart.ClickedCh: restartKernel()
		case <-mHide.ClickedCh: 
			// 假退出：只退出托盘循环，不设置 isReallyExiting
			systray.Quit()
		case <-mExit.ClickedCh:
			isReallyExiting = true
			systray.Quit()
		}
	}
}

func onExit() {
	if isReallyExiting {
		if hJob != 0 { windows.CloseHandle(hJob) }
		os.Exit(0)
	}
	// 隐藏模式下，此函数执行完后，控制权回到 main 的 for 循环
}

// --- 其余功能函数 (保持不动) ---

func syncConfigToKernel() {
	for i := 0; i < 15; i++ {
		if _, err := httpClient.Get(API_URL); err == nil {
			if getIniConfig("tun") == "true" { setTunMode(true) }
			if m := getIniConfig("mode"); m != "" { setMihomoMode(m) }
			if getIniConfig("system_proxy") == "true" { setSystemProxy(true) }
			return
		}
		time.Sleep(time.Second)
	}
}

func updateIconByState(state int) {
	if state < 0 { return }
	files := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
	b, err := iconFs.ReadFile("icons/" + files[state])
	if err != nil { b, _ = iconFs.ReadFile("icons/default.ico") }
	systray.SetIcon(b)
}

func openWebPanel() {
	windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
}

func setMihomoMode(mode string) {
	saveIniConfig("mode", mode)
	json := fmt.Sprintf(`{"mode": "%s"}`, mode)
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(json)))
	if resp, err := httpClient.Do(req); err == nil { resp.Body.Close() }
}

func setTunMode(enable bool) {
	saveIniConfig("tun", fmt.Sprint(enable))
	json := fmt.Sprintf(`{"tun": {"enable": %v}}`, enable)
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(json)))
	if resp, err := httpClient.Do(req); err == nil { resp.Body.Close() }
}

func toggleAutoRun() {
	key, _ := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.SET_VALUE|registry.QUERY_VALUE)
	defer key.Close()
	if isAutoRunEnabled() { key.DeleteValue(APP_NAME) } else { key.SetStringValue(APP_NAME, exePath) }
}

func isAutoRunEnabled() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.QUERY_VALUE)
	if err != nil { return false }; defer key.Close()
	_, _, err = key.GetStringValue(APP_NAME)
	return err == nil
}

func restartKernel() {
	cmd := exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe")
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	_ = cmd.Run()
}

func loadIniConfigAll() {
	b, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	for _, line := range strings.Split(string(b), "\n") {
		if parts := strings.SplitN(strings.TrimSpace(line), "=", 2); len(parts) == 2 {
			configMu.Lock()
			configData[parts[0]] = parts[1]
			configMu.Unlock()
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
		if k != "" { buf.WriteString(fmt.Sprintf("%s=%s\n", k, v)) }
	}
	configMu.Unlock()
	_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), buf.Bytes(), 0644)
}

// --- 程序入口 (真假退出核心控制) ---

func main() {
	if !isAdmin() { runAsAdmin(); os.Exit(0) }
	
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	hMutex, _ := windows.CreateMutex(nil, false, mName)
	if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		if hMutex != 0 { windows.CloseHandle(hMutex) }
		// 唤醒逻辑：重复开启时，打开 Web 界面并静默退出
		httpClient.Get("http://127.0.0.1:" + WAKEUP_PORT)
		os.Exit(0)
	}

	os.Chdir(baseDir)
	initJobObject()

	// 启动后台守护进程 (它是常驻的，不随托盘图标消失而消失)
	go monitorKernelDaemon()

	// 唤醒监听服务
	go func() {
		http.ListenAndServe("127.0.0.1:"+WAKEUP_PORT, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			openWebPanel()
		}))
	}()

	// 托盘控制循环
	for {
		systray.Run(onReady, onExit)
		
		// 如果点击的是“退出程序”，isReallyExiting 为真，跳出循环彻底结束进程
		if isReallyExiting {
			break
		}
		
		// 如果点击的是“隐藏图标”，此处会阻塞，直到程序被强杀或外部触发
		// 此时 monitorKernelDaemon 依然在后台 go 协程中运行
		select {
		case <-time.After(time.Hour * 8760): // 潜伏一年
		}
	}
}
