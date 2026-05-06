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
    // 创建作业对象
    h, _ := windows.CreateJobObject(nil, nil)
    if h != 0 {
        var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
        // 关键标志：JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
        // 含义：当指向该作业的所有句柄关闭时，自动终止关联的所有进程
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
        // 用户主动点击“退出程序”
        // 1. 杀掉内核
        restartKernel() 
        // 2. 释放 Job 句柄
        if hJob != 0 {
            windows.CloseHandle(hJob)
            hJob = 0
        }
        // 3. 彻底退出
        os.Exit(0)
    }
    
    // --- 假退出逻辑 ---
    // systray.Run 返回到这里，图标已消失。
    // 我们不释放 hJob，Launcher 进程进入静默阻塞状态。
    // 此时，如果 Launcher 被任务管理器杀掉，由于 hJob 没被手动释放且进程消失，
    // 内核 mihomo.exe 会被 Windows 强制杀掉。
    for {
        // 如果此循环被打破，或者进程被外界终止，内核必死
        time.Sleep(time.Hour)
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

    // --- 1. 互斥检测与接力请求 ---
    mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
    hMutex, _ := windows.CreateMutex(nil, false, mName)
    if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
        if hMutex != 0 { windows.CloseHandle(hMutex) }
        
        // 发现旧进程，发送接力信号
        // 使用短超时防止新进程卡死
        client := http.Client{Timeout: 500 * time.Millisecond}
        client.Get("http://127.0.0.1:" + WAKEUP_PORT + "/kill_old")
        
        // 给旧进程释放资源的时间
        time.Sleep(600 * time.Millisecond)
        
        // 启动新实例并退出当前中转进程
        cmd := exec.Command(exePath)
        cmd.Start()
        os.Exit(0)
    }

    os.Chdir(baseDir)
    initJobObject()

    // --- 2. 核心守护协程 ---
    go monitorKernelDaemon()

    // --- 3. 接力监听服务 ---
    go func() {
        mux := http.NewServeMux()
        // 接力接口：只有通过这个接口退出，内核才能活
        mux.HandleFunc("/kill_old", func(w http.ResponseWriter, r *http.Request) {
            if hJob != 0 {
                // 【核心动作】手动关闭 Job 句柄
                // 这会导致句柄计数减一，但因为新进程即将接管，
                // 我们通过“温和”的方式释放，避免触发强制清理
                windows.CloseHandle(hJob)
                hJob = 0 
            }
            os.Exit(0) 
        })
        
        // 普通唤醒：比如假退出后双击图标（如果接力失败或仅需弹出 UI）
        mux.HandleFunc("/wakeup", func(w http.ResponseWriter, r *http.Request) {
            openWebPanel()
        })
        
        server := &http.Server{Addr: "127.0.0.1:" + WAKEUP_PORT, Handler: mux}
        server.ListenAndServe()
    }()

    systray.Run(onReady, onExit)
}
