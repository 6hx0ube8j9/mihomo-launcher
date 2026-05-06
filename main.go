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
	APP_MUTEX   = "Global\\MihomoUltimateManager_V26_Official_Stable"
	API_URL     = "http://127.0.0.1:9090"
	CONFIG_FILE = "mihomo-launcher.ini"
	REG_RUN     = `Software\Microsoft\Windows\CurrentVersion\Run`
	REG_PROXY   = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	APP_NAME    = "MihomoLauncher"

	StateStop = 0; StateError = 1; StateTun = 2; StateProxy = 3; StateDefault = 4
)

var (
	isReallyExiting bool
	isIconHidden    bool
	hJob            windows.Handle
	httpClient      = &http.Client{Timeout: 1 * time.Second}
	exePath, _      = os.Executable()
	baseDir         = filepath.Dir(exePath)
	
	configData      = make(map[string]string)
	configMu        sync.RWMutex
	lastState       = -1
)

// --- 系统底层 ---

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

// --- 状态与代理操作 ---

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
	if enable {
		key.SetDWordValue("ProxyEnable", 1)
		saveIniConfig("system_proxy", "true")
	} else {
		key.SetDWordValue("ProxyEnable", 0)
		saveIniConfig("system_proxy", "false")
	}
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

// --- UI 逻辑 ---

func onReady() {
	loadIniConfigAll()
	updateIconByState(StateDefault)
	systray.SetTooltip("Mihomo Launcher")

	// 1. 顶部：Web面板 (双击默认项)
	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	systray.AddSeparator()

	// 2. 模式切换 (从配置加载状态)
	mode := getIniConfig("mode")
	mModeR := systray.AddMenuItemCheckbox("规则模式 (Rule)", "", mode == "rule" || mode == "")
	mModeG := systray.AddMenuItemCheckbox("全局模式 (Global)", "", mode == "global")
	mModeD := systray.AddMenuItemCheckbox("直连模式 (Direct)", "", mode == "direct")
	systray.AddSeparator()

	// 3. 核心开关 (从配置加载状态)
	tunOn := getIniConfig("tun") == "true"
	mTun := systray.AddMenuItemCheckbox("TUN 模式开关", "", tunOn)
	
	proxyOn := getIniConfig("system_proxy") == "true" || isProxyEnabledInRegistry()
	mSystemProxy := systray.AddMenuItemCheckbox("系统代理开关", "", proxyOn)
	systray.AddSeparator()

	// 4. 程序管理
	mAutoRun := systray.AddMenuItemCheckbox("随系统启动", "", isAutoRunEnabled())
	mDir := systray.AddMenuItem("打开程序目录", "")
	mRestart := systray.AddMenuItem("重启内核进程", "")
	mHide := systray.AddMenuItem("隐藏托盘图标", "隐藏后双击exe唤醒")
	mExit := systray.AddMenuItem("彻底退出程序", "")

	go monitorKernelDaemon()

	// 唤醒服务端
	go func() {
		http.ListenAndServe("127.0.0.1:"+WAKEUP_PORT, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			isIconHidden = false
			updateIconByState(StateDefault)
			fmt.Fprint(w, "ok")
		}))
	}()

	for {
		select {
		case <-mWeb.ClickedCh:
			openWebPanel()
		case <-mModeR.ClickedCh:
			setMihomoMode("rule"); mModeR.Check(); mModeG.Uncheck(); mModeD.Uncheck()
		case <-mModeG.ClickedCh:
			setMihomoMode("global"); mModeR.Uncheck(); mModeG.Check(); mModeD.Uncheck()
		case <-mModeD.ClickedCh:
			setMihomoMode("direct"); mModeR.Uncheck(); mModeG.Uncheck(); mModeD.Check()
		case <-mTun.ClickedCh:
			if mTun.Checked() { 
				setTunMode(false); mTun.Uncheck() 
			} else { 
				setTunMode(true); mTun.Check() 
			}
		case <-mSystemProxy.ClickedCh:
			if mSystemProxy.Checked() { 
				setSystemProxy(false); mSystemProxy.Uncheck() 
			} else { 
				setSystemProxy(true); mSystemProxy.Check() 
			}
		case <-mAutoRun.ClickedCh:
			toggleAutoRun()
			if isAutoRunEnabled() { mAutoRun.Check() } else { mAutoRun.Uncheck() }
		case <-mDir.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		case <-mRestart.ClickedCh:
			restartKernel()
		case <-mHide.ClickedCh:
			isIconHidden = true
			systray.SetIcon([]byte{})
		case <-mExit.ClickedCh:
			isReallyExiting = true
			systray.Quit()
		}
	}
}

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting { return }
		curr := checkSystemState()
		
		if curr == StateStop {
			cmd := exec.Command(target, "-d", baseDir)
			cmd.Dir = baseDir
			cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
			if err := cmd.Start(); err == nil && hJob != 0 {
				hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
				_ = windows.AssignProcessToJobObject(hJob, hp)
				windows.CloseHandle(hp)
			}
		}

		if !isIconHidden && curr != lastState {
			updateIconByState(curr)
			lastState = curr
		}
		time.Sleep(2 * time.Second)
	}
}

func updateIconByState(state int) {
	if isIconHidden { return }
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
	state := "false"; if enable { state = "true" }
	saveIniConfig("tun", state)
	json := fmt.Sprintf(`{"tun": {"enable": %s}}`, state)
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

// --- 配置文件管理 ---

func loadIniConfigAll() {
	b, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	lines := strings.Split(string(b), "\n")
	configMu.Lock()
	defer configMu.Unlock()
	for _, line := range lines {
		parts := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(parts) == 2 { configData[parts[0]] = parts[1] }
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
		if k != "" {
			buf.WriteString(fmt.Sprintf("%s=%s\n", k, v))
		}
	}
	configMu.Unlock()
	_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), buf.Bytes(), 0644)
}

func onExit() {
	if !isReallyExiting { return }
	if hJob != 0 { windows.CloseHandle(hJob) }
	cmd := exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe")
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	_ = cmd.Run()
	os.Exit(0)
}

func main() {
	if !isAdmin() { runAsAdmin(); os.Exit(0) }

	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	hMutex, _ := windows.CreateMutex(nil, false, mName)
	if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		if hMutex != 0 { windows.CloseHandle(hMutex) }
		httpClient.Get("http://127.0.0.1:" + WAKEUP_PORT + "/wakeup")
		os.Exit(0)
	}

	os.Chdir(baseDir)
	initJobObject()
	systray.Run(onReady, onExit)
}
