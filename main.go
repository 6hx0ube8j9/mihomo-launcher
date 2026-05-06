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
	isInitializing  = true // 启动初始化标记，用于防抖
	hJob            windows.Handle
	httpClient      = &http.Client{Timeout: 1 * time.Second}
	exePath, _      = os.Executable()
	baseDir         = filepath.Dir(exePath)
	configData      = make(map[string]string)
	configMu        sync.RWMutex
	lastState       = -1
)

// --- 核心状态检测 ---
func checkSystemState() int {
	// 1. API 通信检查
	_, err := httpClient.Get(API_URL)
	if err != nil {
		if !isProcessRunning("mihomo.exe") {
			return StateStop // 红色：内核进程没跑
		}
		return StateStop // 进程在但 API 不通，也视为异常
	}

	configMu.RLock()
	expectTun := configData["tun"] == "true"
	expectProxy := configData["system_proxy"] == "true"
	configMu.RUnlock()

	hasInterface := isInterfaceExisted("Mihomo")
	proxyEnabled := isProxyEnabledInRegistry()

	// --- 优先级判定逻辑 ---

	// 1. 只要检测到网卡，说明 TUN 成功 -> 绿色
	if hasInterface {
		return StateTun
	}

	// 2. 如果配置要求开 TUN，但没网卡
	if expectTun {
		// 如果还在启动初始化那几秒，先不报黄，显示默认灰
		if isInitializing {
			return StateDefault
		}
		return StateError // 初始化结束后还没网卡 -> 黄色
	}

	// 3. 没开 TUN，但检测到系统代理 -> 蓝色
	if proxyEnabled || expectProxy {
		return StateProxy
	}

	// 4. 默认状态 -> 灰色
	return StateDefault
}

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

// 强行恢复记忆：把 .ini 状态同步给内核 API
func syncConfigToKernel() {
	for i := 0; i < 20; i++ {
		_, err := httpClient.Get(API_URL)
		if err == nil {
			configMu.RLock()
			// 强行注入 TUN 状态
			if configData["tun"] == "true" {
				setTunMode(true)
			} else {
				setTunMode(false)
			}
			// 强行注入模式
			if m := configData["mode"]; m != "" {
				setMihomoMode(m)
			}
			// 强行同步系统代理
			if configData["system_proxy"] == "true" {
				setSystemProxy(true)
			}
			configMu.RUnlock()
			return
		}
		time.Sleep(800 * time.Millisecond)
	}
}

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting { return }
		curr := checkSystemState()

		// 初始化完成后，才允许守护进程根据检测结果修改图标
		if !isInitializing {
			if curr != lastState {
				updateIconByState(curr)
				lastState = curr
			}
		}

		if curr == StateStop {
			cmd := exec.Command(target, "-d", baseDir)
			cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
			if err := cmd.Start(); err == nil && hJob != 0 {
				hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
				_ = windows.AssignProcessToJobObject(hJob, hp)
				windows.CloseHandle(hp)
				// 内核重启后，再次同步配置
				go syncConfigToKernel()
			}
		}
		time.Sleep(2 * time.Second)
	}
}

// --- UI 逻辑 ---

func onReady() {
	loadIniConfigAll()
	systray.SetTooltip(APP_NAME)
	
	// 1. 初始化图标为默认灰色
	updateIconByState(StateDefault)
	lastState = StateDefault

	// 2. 加载菜单项
	mWeb := systray.AddMenuItem("进入控制面板", "")
	systray.AddSeparator()

	mode := getIniConfig("mode")
	mModeR := systray.AddMenuItemCheckbox("规则模式", "", mode == "rule" || mode == "")
	mModeG := systray.AddMenuItemCheckbox("全局模式", "", mode == "global")
	mModeD := systray.AddMenuItemCheckbox("直连模式", "", mode == "direct")
	systray.AddSeparator()

	mTun := systray.AddMenuItemCheckbox("TUN 模式", "", getIniConfig("tun") == "true")
	mSystemProxy := systray.AddMenuItemCheckbox("系统代理", "", getIniConfig("system_proxy") == "true")
	systray.AddSeparator()

	mAutoRun := systray.AddMenuItemCheckbox("开机自启", "", isAutoRunEnabled())
	mDir := systray.AddMenuItem("浏览本地文件", "")
	mRestart := systray.AddMenuItem("重启内核", "")
	mHide := systray.AddMenuItem("隐藏托盘图标", "")
	mExit := systray.AddMenuItem("退出程序", "")

	// 3. 异步执行“硬恢复”并防抖刷新图标
	go func() {
		isInitializing = true
		syncConfigToKernel()
		
		// 等待内核处理指令及系统创建网卡（2秒缓冲）
		time.Sleep(2 * time.Second)
		
		isInitializing = false
		// 恢复完后，进行第一次真实的图标同步
		finalState := checkSystemState()
		updateIconByState(finalState)
		lastState = finalState
	}()

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
		case <-mHide.ClickedCh: systray.Quit()
		case <-mExit.ClickedCh:
			isReallyExiting = true
			systray.Quit()
		}
	}
}

func onExit() {
	if isReallyExiting {
		restartKernel() 
		if hJob != 0 { windows.CloseHandle(hJob) }
		os.Exit(0)
	}
}

func updateIconByState(state int) {
	if state < 0 { return }
	files := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
	b, err := iconFs.ReadFile("icons/" + files[state])
	if err != nil { 
		// 兜底逻辑
		b, _ = iconFs.ReadFile("icons/default.ico") 
	}
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
	lines := strings.Split(string(b), "\n")
	configMu.Lock()
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" { continue }
		if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
			configData[parts[0]] = parts[1]
		}
	}
	configMu.Unlock()
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

func main() {
	if !isAdmin() { runAsAdmin(); os.Exit(0) }

	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	hMutex, _ := windows.CreateMutex(nil, false, mName)
	if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		if hMutex != 0 { windows.CloseHandle(hMutex) }
		client := http.Client{Timeout: 500 * time.Millisecond}
		_, err := client.Get("http://127.0.0.1:" + WAKEUP_PORT + "/kill_old")
		if err == nil { time.Sleep(800 * time.Millisecond) }
		cmd := exec.Command(exePath)
		cmd.Start()
		os.Exit(0)
	}

	os.Chdir(baseDir)
	initJobObject()
	
	// 启动核心守护进程
	go monitorKernelDaemon()

	// 监听端口用于唤醒或关闭旧进程
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/kill_old", func(w http.ResponseWriter, r *http.Request) {
			if hJob != 0 {
				windows.CloseHandle(hJob)
				hJob = 0 
			}
			os.Exit(0) 
		})
		mux.HandleFunc("/wakeup", func(w http.ResponseWriter, r *http.Request) {
			openWebPanel()
		})
		server := &http.Server{Addr: "127.0.0.1:" + WAKEUP_PORT, Handler: mux}
		server.ListenAndServe()
	}()

	systray.Run(onReady, onExit)
}
