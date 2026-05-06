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

// --- 核心逻辑：杀进程、抢端口、再现身 ---

func killAndTakeover() {
	currentPid := os.Getpid()
	exeName := filepath.Base(exePath)

	// 1. 遍历并强杀所有同名进程（排除自己，防止自杀）
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

	// 2. 暴力清理可能占用端口的僵尸连接（针对 TIME_WAIT 状态）
	exec.Command("cmd", "/c", fmt.Sprintf("for /f \"tokens=5\" %%a in ('netstat -aon ^| findstr :%s') do taskkill /F /PID %%a", CONTROL_PORT)).Run()
	
	// 3. 阻塞式监听：只有抢到端口，才说明夺权成功，允许现身
	for {
		l, err := net.Listen("tcp", "127.0.0.1:"+CONTROL_PORT)
		if err == nil {
			// 成功占领阵地，启动后台指令监听服务
			go func() {
				mux := http.NewServeMux()
				mux.HandleFunc("/wakeup", func(w http.ResponseWriter, r *http.Request) {
					// 此处可以预留远程唤醒逻辑
				})
				http.Serve(l, mux)
			}()
			return // 夺权完成，退出阻塞
		}
		// 抢夺失败，说明旧实例还没释放资源，稍后再试
		time.Sleep(150 * time.Millisecond)
	}
}

// --- 配置管理 ---

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

// --- 守护进程逻辑 ---

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting || isHandover { return }
		_, err := httpClient.Get(API_URL)
		if err != nil {
			if !isProcessRunning("mihomo.exe") {
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
		} else {
			curr := checkSystemState()
			if curr != lastState { updateIconByState(curr); lastState = curr }
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

// --- UI 交互 ---

func onReady() {
	loadIniConfigAll()
	saveIniConfig("run_mode", "normal") // 启动托盘意味着从隐藏恢复
	
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
	mHide := systray.AddMenuItem("隐藏托盘图标", "")
	mExit := systray.AddMenuItem("退出程序", "")

	for {
		select {
		case <-mWeb.ClickedCh: windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
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
		case <-mDir.ClickedCh: windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		case <-mHide.ClickedCh:
			saveIniConfig("run_mode", "hidden")
			systray.Quit()
		case <-mExit.ClickedCh:
			isReallyExiting = true
			saveIniConfig("run_mode", "normal")
			systray.Quit()
		}
	}
}

func onExit() {
	if isReallyExiting {
		exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
		if hJob != 0 { windows.CloseHandle(hJob) }
		os.Exit(0)
	}
	// 如果是隐藏图标，main 会继续运行，此协程阻塞
	select {}
}

// --- 系统功能组件 ---

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
	b, err := iconFs.ReadFile("icons/" + files[state])
	if err == nil { systray.SetIcon(b) }
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

// --- 入口函数 ---

func main() {
	if !isAdmin() { runAsAdmin(); os.Exit(0) }

	// 1. 强力清场并抢夺端口
	killAndTakeover()

	// 2. 拿到端口后，初始化 Mutex 锁和 JobObject
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	h, _ := windows.CreateMutex(nil, false, mName)
	hMutex = h

	os.Chdir(baseDir)
	initJobObject()
	go monitorKernelDaemon()

	// 3. 启动 UI
	systray.Run(onReady, onExit)
}
