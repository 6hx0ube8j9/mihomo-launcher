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
	isHandover      bool
	hJob            windows.Handle
	hMutex          windows.Handle
	httpClient      = &http.Client{Timeout: 1 * time.Second}
	exePath, _      = os.Executable()
	baseDir         = filepath.Dir(exePath)
	configData      = make(map[string]string)
	configMu        sync.RWMutex
	lastState       = -1
)

// --- 1. 核心：智能接管与唤醒逻辑 ---

func smartTakeover() {
	client := http.Client{Timeout: 300 * time.Millisecond}

	// 如果旧进程在后台隐藏，新进程启动时发送 kill_old 让旧进程退出
	// 这样新进程就能顺利接管并显示托盘图标
	_, err := client.Get("http://127.0.0.1:" + CONTROL_PORT + "/kill_old")
	if err == nil {
		time.Sleep(600 * time.Millisecond)
	}

	// 物理清理保底（针对卡死的旧进程）
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

	// 监听控制端口
	for i := 0; i < 20; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:"+CONTROL_PORT)
		if err == nil {
			go func() {
				mux := http.NewServeMux()
				// 唤醒接口（备用）
				mux.HandleFunc("/wakeup", func(w http.ResponseWriter, r *http.Request) {
					openWebPanel()
				})
				// 接力接口：旧进程收到后释放内核句柄并完全消失
				mux.HandleFunc("/kill_old", func(w http.ResponseWriter, r *http.Request) {
					isHandover = true
					if hJob != 0 {
						windows.CloseHandle(hJob)
						hJob = 0
					}
					systray.Quit()
					os.Exit(0)
				})
				_ = http.Serve(l, mux)
			}()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// --- 2. 界面逻辑：增加双击图标开网页 ---

func onReady() {
	// 确保此时配置已加载
	systray.SetTooltip(APP_NAME)
	updateIconByState(StateDefault)

	// 处理左键双击/点击图标：打开控制面板
	go func() {
		for range systray.ClickedCh {
			openWebPanel()
		}
	}()

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

	// 启动后立即与内核同步一次配置
	go syncConfigToKernel()

	for {
		select {
		case <-mWeb.ClickedCh:
			openWebPanel()
		case <-mModeR.ClickedCh:
			setMihomoMode("rule")
			mModeR.Check(); mModeG.Uncheck(); mModeD.Uncheck()
		case <-mModeG.ClickedCh:
			setMihomoMode("global")
			mModeR.Uncheck(); mModeG.Check(); mModeD.Uncheck()
		case <-mModeD.ClickedCh:
			setMihomoMode("direct")
			mModeR.Uncheck(); mModeG.Uncheck(); mModeD.Check()
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
		case <-mRestart.ClickedCh:
			restartKernelForce()
		case <-mHide.ClickedCh:
			systray.Quit() // 仅销毁图标，main 循环会保持后台运行
		case <-mExit.ClickedCh:
			isReallyExiting = true
			systray.Quit()
		}
	}
}

func onExit() {
	if isReallyExiting {
		restartKernelForce()
		if hJob != 0 {
			windows.CloseHandle(hJob)
			hJob = 0
		}
		os.Exit(0)
	}
	// 隐藏模式下，此循环保持后台运行
	for {
		time.Sleep(time.Hour)
	}
}

// --- 3. 内核守护与工具函数 ---

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting || isHandover { return }

		_, err := httpClient.Get(API_URL)
		if err != nil {
			if !isProcessRunning("mihomo.exe") {
				// 进程彻底消失才拉起
				cmd := exec.Command(target, "-d", baseDir)
				cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
				if err := cmd.Start(); err == nil && hJob != 0 {
					hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
					_ = windows.AssignProcessToJobObject(hJob, hp)
					windows.CloseHandle(hp)
					time.Sleep(2 * time.Second)
					go syncConfigToKernel()
				}
				lastState = StateStop
				updateIconByState(StateStop)
			} else if lastState != StateError {
				updateIconByState(StateError)
				lastState = StateError
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

func syncConfigToKernel() {
	// 等待内核 API 就绪后同步 INI 里的状态
	for i := 0; i < 15; i++ {
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

func loadIniConfigAll() {
	b, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	configMu.Lock()
	defer configMu.Unlock()
	configData = make(map[string]string)
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

// --- 4. 程序入口 ---

func main() {
	if !isAdmin() {
		runAsAdmin()
		os.Exit(0)
	}

	// 1. 核心：必须先进入目录加载配置
	os.Chdir(baseDir)
	loadIniConfigAll()

	// 2. 互斥锁检测
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	h, _ := windows.CreateMutex(nil, false, mName)
	if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		if h != 0 { windows.CloseHandle(h) }
	}
	hMutex = h

	// 3. 执行智能夺权（唤醒后台图标）
	smartTakeover()

	// 4. 正式启动
	initJobObject()
	go monitorKernelDaemon()

	systray.Run(onReady, onExit)
}
