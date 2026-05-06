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
	APP_NAME    = "MihomoLauncher"

	StateStop = 0; StateError = 1; StateTun = 2; StateProxy = 3; StateDefault = 4
)

var (
	isReallyExiting bool
	isHidden        bool
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
			uintptr(h),
			9,
			uintptr(unsafe.Pointer(&info)),
			uintptr(uint32(unsafe.Sizeof(info))),
		)
		hJob = h
	}
}

// --- 状态检测 ---

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
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
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

// --- UI 逻辑 ---

func onReady() {
	loadIniConfigAll()
	// 每次启动默认显示图标，不再从 INI 读取隐藏状态
	updateIconByState(StateDefault)

	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	mDir := systray.AddMenuItem("打开程序目录", "")
	systray.AddSeparator()

	mMode := systray.AddMenuItem("代理模式切换", "")
	mModeR := mMode.AddSubMenuItemCheckbox("规则模式 (Rule)", "", false)
	mModeG := mMode.AddSubMenuItemCheckbox("全局模式 (Global)", "", false)
	mModeD := mMode.AddSubMenuItemCheckbox("直连模式 (Direct)", "", false)

	mTun := systray.AddMenuItemCheckbox("TUN 模式开关", "", false)
	mAutoRun := systray.AddMenuItemCheckbox("随系统启动", "", isAutoRunEnabled())
	
	systray.AddSeparator()
	mRestart := systray.AddMenuItem("重启内核进程", "")
	mHide := systray.AddMenuItem("隐藏托盘图标", "仅退出托盘，不退内核")
	mExit := systray.AddMenuItem("彻底退出程序", "")

	go monitorKernelDaemon()

	// 唤醒逻辑：双击 exe 时重新触发 main -> wakeup，此处重载托盘
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/wakeup", func(w http.ResponseWriter, r *http.Request) {
			// 如果已经调用过 systray.Quit()，再次通过 exe 启动会执行新的进程 main 逻辑
			fmt.Fprint(w, "ok")
		})
		http.ListenAndServe("127.0.0.1:"+WAKEUP_PORT, mux)
	}()

	for {
		select {
		case <-mWeb.ClickedCh:
			openWebPanel()
		case <-mDir.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		case <-mModeR.ClickedCh:
			setMihomoMode("rule"); mModeR.Check(); mModeG.Uncheck(); mModeD.Uncheck()
		case <-mModeG.ClickedCh:
			setMihomoMode("global"); mModeR.Uncheck(); mModeG.Check(); mModeD.Uncheck()
		case <-mModeD.ClickedCh:
			setMihomoMode("direct"); mModeR.Uncheck(); mModeG.Uncheck(); mModeD.Check()
		case <-mTun.ClickedCh:
			if mTun.Checked() { setTunMode(false); mTun.Uncheck() } else { setTunMode(true); mTun.Check() }
		case <-mAutoRun.ClickedCh:
			toggleAutoRun()
			if isAutoRunEnabled() { mAutoRun.Check() } else { mAutoRun.Uncheck() }
		case <-mRestart.ClickedCh:
			restartKernel()
		case <-mHide.ClickedCh:
			// 修复：仅退出托盘，不设置退出标记，不销毁内核
			systray.Quit()
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

		if !isHidden && curr != lastState {
			updateIconByState(curr)
			lastState = curr
		}
		time.Sleep(2 * time.Second)
	}
}

func updateIconByState(state int) {
	files := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
	b, err := iconFs.ReadFile("icons/" + files[state])
	if err != nil { b, _ = iconFs.ReadFile("icons/default.ico") }
	systray.SetIcon(b)
}

func openWebPanel() {
	windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
}

func setMihomoMode(mode string) {
	json := fmt.Sprintf(`{"mode": "%s"}`, mode)
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(json)))
	if resp, err := httpClient.Do(req); err == nil { resp.Body.Close() }
}

func setTunMode(enable bool) {
	state := "false"; if enable { state = "true" }
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
	// 彻底退出时隐藏黑框
	cmd := exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe")
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	_ = cmd.Run()
}

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

func saveIniConfig(key, val string) {
	configMu.Lock()
	configData[key] = val
	var buf bytes.Buffer
	for k, v := range configData { buf.WriteString(fmt.Sprintf("%s=%s\n", k, v)) }
	configMu.Unlock()
	_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), buf.Bytes(), 0644)
}

func onExit() {
	if !isReallyExiting { return }
	if hJob != 0 { windows.CloseHandle(hJob) }
	// 修复：彻底退出程序时，隐藏 taskkill 的黑框
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
		// 唤醒已有实例并退出（如果已有实例已隐藏托盘，此处仅作探测，新实例由于无法获取 Mutex 会自动退出）
		httpClient.Get("http://127.0.0.1:" + WAKEUP_PORT + "/wakeup")
		os.Exit(0)
	}

	os.Chdir(baseDir)
	initJobObject()
	systray.Run(onReady, onExit)
}
