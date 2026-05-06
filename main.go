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
	CONTROL_PORT = "18579"
	APP_MUTEX    = "Global\\MihomoLauncher_Unique_Mutex"
	API_URL      = "http://127.0.0.1:9090"
	CONFIG_FILE  = "mihomo-launcher.ini"
	REG_RUN      = `Software\Microsoft\Windows\CurrentVersion\Run`
	REG_PROXY    = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	APP_NAME     = "MihomoLauncher"

	StateStop = 0; StateError = 1; StateTun = 2; StateProxy = 3; StateDefault = 4
)

var (
	isReallyExiting bool
	isHandover      bool // 用于标记是否正在交接，防止交接时触发内核清理
	hJob            windows.Handle
	hMutex          windows.Handle
	httpClient      = &http.Client{Timeout: 1 * time.Second}
	exePath, _      = os.Executable()
	baseDir         = filepath.Dir(exePath)
	configData      = make(map[string]string)
	configMu        sync.RWMutex
	lastState       = -1
)

// --- 1. 核心合并：聪明的夺权逻辑 ---

func smartTakeover() {
	// 先尝试唤醒或礼貌交接
	client := http.Client{Timeout: 300 * time.Millisecond}
	
	// 尝试唤醒（针对隐藏图标的情况）
	_, err := client.Get("http://127.0.0.1:" + CONTROL_PORT + "/wakeup")
	if err == nil {
		// 唤醒成功，说明旧进程活得很好，新进程直接消失
		os.Exit(0)
	}

	// 如果唤醒失败，尝试让旧进程温和退位（释放句柄）
	_, err = client.Get("http://127.0.0.1:" + CONTROL_PORT + "/kill_old")
	if err == nil {
		// 给旧进程一点释放资源的时间
		time.Sleep(500 * time.Millisecond)
	}

	// --- 最后的武力保障（最新版的优点） ---
	// 如果上面都失败了，说明旧进程可能卡死了，执行物理清场
	currentPid := os.Getpid()
	exeName := filepath.Base(exePath)
	snapshot, _ := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if snapshot != 0 {
		var proc windows.ProcessEntry32
		proc.Size = uint32(unsafe.Sizeof(proc))
		for windows.Process32Next(snapshot, &proc) == nil {
			name := windows.UTF16ToString(proc.ExeFile[:])
			if strings.EqualFold(name, exeName) && int(proc.ProcessID) != currentPid {
				p, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, proc.ProcessID)
				if err == nil {
					windows.TerminateProcess(p, 0)
					windows.CloseHandle(p)
				}
			}
		}
		windows.CloseHandle(snapshot)
	}

	// 确保端口释放并由我监听
	for i := 0; i < 20; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:"+CONTROL_PORT)
		if err == nil {
			go func() {
				mux := http.NewServeMux()
				// 唤醒接口：弹出面板
				mux.HandleFunc("/wakeup", func(w http.ResponseWriter, r *http.Request) {
					openWebPanel()
				})
				// 交接接口：释放句柄后退出
				mux.HandleFunc("/kill_old", func(w http.ResponseWriter, r *http.Request) {
					isHandover = true 
					if hJob != 0 {
						windows.CloseHandle(hJob)
						hJob = 0
					}
					os.Exit(0)
				})
				_ = http.Serve(l, mux)
			}()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// --- 2. 内核守护：不激进的监控 ---

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting || isHandover { return }

		_, err := httpClient.Get(API_URL)
		if err != nil {
			// 只有当进程真的不存在时才启动，避免最新版中“偶尔重启”的问题
			if !isProcessRunning("mihomo.exe") {
				cmd := exec.Command(target, "-d", baseDir)
				cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
				if err := cmd.Start(); err == nil && hJob != 0 {
					hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
					_ = windows.AssignProcessToJobObject(hJob, hp)
					windows.CloseHandle(hp)
					
					// 启动后给内核一点时间初始化，不要立即进行下一次检测
					time.Sleep(2 * time.Second)
					go syncConfigToKernel() 
				}
				updateIconByState(StateStop)
				lastState = StateStop
			} else {
				// 进程在跑但 API 不通，说明可能是初始化中或配置错误
				if lastState != StateError {
					updateIconByState(StateError)
					lastState = StateError
				}
			}
		} else {
			curr := checkSystemState()
			if curr != lastState {
				updateIconByState(curr)
				lastState = curr
			}
		}
		time.Sleep(2 * time.Second)
	}
}

// --- 基础工具函数（保留原始逻辑） ---

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
	// 注意：此处不再重复 Get API，由 monitor 统一处理
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

func syncConfigToKernel() {
	for i := 0; i < 10; i++ {
		_, err := httpClient.Get(API_URL)
		if err == nil {
			configMu.RLock()
			if configData["tun"] == "true" { setTunMode(true) }
			if m := configData["mode"]; m != "" { setMihomoMode(m) }
			if configData["system_proxy"] == "true" { setSystemProxy(true) }
			configMu.RUnlock()
			return
		}
		time.Sleep(time.Second)
	}
}

// --- 界面逻辑 ---

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
			enable := !mTun.Checked(); setTunMode(enable)
			if enable { mTun.Check() } else { mTun.Uncheck() }
		case <-mSystemProxy.ClickedCh:
			enable := !mSystemProxy.Checked(); setSystemProxy(enable)
			if enable { mSystemProxy.Check() } else { mSystemProxy.Uncheck() }
		case <-mAutoRun.ClickedCh:
			toggleAutoRun()
			if isAutoRunEnabled() { mAutoRun.Check() } else { mAutoRun.Uncheck() }
		case <-mDir.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		case <-mRestart.ClickedCh: restartKernelForce()
		case <-mHide.ClickedCh: systray.Quit()
		case <-mExit.ClickedCh:
			isReallyExiting = true
			systray.Quit()
		}
	}
}

func onExit() {
	if isReallyExiting {
		restartKernelForce()
		if hJob != 0 { windows.CloseHandle(hJob) }
		os.Exit(0)
	}
	// 如果只是隐藏图标，则主线程在这里死循环，保持后端运行
	for { time.Sleep(time.Hour) }
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

func setSystemProxy(enable bool) {
	key, _ := registry.OpenKey(registry.CURRENT_USER, REG_PROXY, registry.SET_VALUE)
	defer key.Close()
	val := uint32(0); if enable { val = 1 }
	key.SetDWordValue("ProxyEnable", val)
	saveIniConfig("system_proxy", fmt.Sprint(enable))
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

func restartKernelForce() {
	cmd := exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe")
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	_ = cmd.Run()
}

// --- 配置读写（采用最新版的健壮性） ---

func loadIniConfigAll() {
	b, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	configMu.Lock()
	defer configMu.Unlock()
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") { continue }
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
		if k != "" { buf.WriteString(fmt.Sprintf("%s=%s\n", k, v)) }
	}
	configMu.Unlock()
	_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), buf.Bytes(), 0644)
}

// --- 入口 ---

func main() {
	if !isAdmin() {
		runAsAdmin()
		os.Exit(0)
	}

	// 互斥检测与接力/唤醒逻辑
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	h, _ := windows.CreateMutex(nil, false, mName)
	if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		if h != 0 { windows.CloseHandle(h) }
		// 这里不再直接启动新进程，而是交给 smartTakeover 处理唤醒
	}
	hMutex = h

	// 执行合并后的智能接管
	smartTakeover()

	os.Chdir(baseDir)
	initJobObject()
	go monitorKernelDaemon()

	systray.Run(onReady, onExit)
}
