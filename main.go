package main

import (
	"bytes"
	"embed"
	"encoding/json"
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
	APP_MUTEX          = "Global\\MihomoLauncher_Unique_Mutex"
	API_URL            = "http://127.0.0.1:9090"
	CONFIG_FILE        = "mihomo-launcher.ini"
	REG_RUN            = `Software\Microsoft\Windows\CurrentVersion\Run`
	REG_PROXY          = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	APP_NAME           = "MihomoLauncher"
	DEFAULT_PROXY_ADDR = "127.0.0.1:7890"

	// 状态定义
	StateStop    = 0
	StateError   = 1
	StateTun     = 2
	StateProxy   = 3
	StateDefault = 4
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
	mTun            *systray.MenuItem
	onceSync        sync.Once
	isSystemInitializing = true
)

// --- 基础工具函数 ---

func isAdmin() bool {
	var token windows.Token
	err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token)
	if err != nil {
		return false
	}
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

// --- 配置管理 ---

func loadIniConfigAll() {
	b, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	configMu.Lock()
	defer configMu.Unlock()

	configData = make(map[string]string)
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
			configData[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}

	defaults := map[string]string{
		"tun_enabled":          "false",
		"system_proxy_enabled": "false",
		"mode":                 "rule",
		"auto_start":           "false",
	}

	needsSave := false
	for k, v := range defaults {
		if _, exists := configData[k]; !exists {
			configData[k] = v
			needsSave = true
		}
	}

	if needsSave {
		var buf bytes.Buffer
		for k, v := range configData {
			buf.WriteString(fmt.Sprintf("%s = %s\n", k, v))
		}
		_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), buf.Bytes(), 0644)
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
		if k = strings.TrimSpace(k); k != "" {
			buf.WriteString(fmt.Sprintf("%s = %s\n", k, v))
		}
	}
	content := buf.Bytes()
	configMu.Unlock()
	_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), content, 0644)
}

// --- 核心逻辑 ---

func syncConfigToKernel() {
	configMu.RLock()
	tun := configData["tun_enabled"] == "true"
	mode := configData["mode"]
	if mode == "" {
		mode = "rule"
	}
	proxy := configData["system_proxy_enabled"] == "true"
	configMu.RUnlock()

	payload := map[string]interface{}{
		"mode": mode,
		"tun":  map[string]bool{"enable": tun},
	}
	jsonPayload, _ := json.Marshal(payload)

	req, err := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if (resp.StatusCode == 204 || resp.StatusCode == 200) && proxy {
			setProxyRegistry(true)
		}
	}
}

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting {
			return
		}

		if !isProcessRunning("mihomo.exe") {
			onceSync = sync.Once{}
			killCmd := exec.Command("taskkill", "/F", "/IM", "mihomo.exe", "/T")
			killCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
			_ = killCmd.Run()

			time.Sleep(300 * time.Millisecond)

			cmd := exec.Command(target, "-d", baseDir)
			cmd.SysProcAttr = &windows.SysProcAttr{
				CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_BREAKAWAY_FROM_JOB,
			}

			if err := cmd.Start(); err == nil {
				if hJob != 0 {
					hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
					_ = windows.AssignProcessToJobObject(hJob, hp)
					windows.CloseHandle(hp)
				}
				// 启动后立即触发一次图标检查
				time.Sleep(1 * time.Second)
				syncInitialState()
				_ = cmd.Wait()
			}
		}
		time.Sleep(2 * time.Second)
	}
}

// syncInitialState 用于启动时或内核重启后同步一次图标
// 暴力同步函数：死磕内核 API 直到成功或超时
func syncInitialState() {
	// 1. 启动即进入保护期，禁止 watchTunState 写入配置文件
	isSystemInitializing = true
	fmt.Println("[Init] 进入启动保护期，锁定配置文件写入权限")

	// 2. 循环探测内核 API (最多尝试 20 次)
	apiReady := false
	for i := 0; i < 20; i++ {
		// 尝试请求内核基础接口
		_, err := httpClient.Get(API_URL)
		if err == nil {
			apiReady = true
			fmt.Printf("[Init] 第 %d 秒：内核 API 已就绪\n", i+1)
			break
		}
		fmt.Printf("[Init] 第 %d 秒：等待内核 API 启动...\n", i+1)
		time.Sleep(1 * time.Second)
	}

	if apiReady {
		// 3. 从配置文件读取用户想要的最终状态
		configMu.RLock()
		tunWanted := configData["tun_enabled"] == "true"
		modeWanted := configData["mode"]
		if modeWanted == "" { modeWanted = "rule" }
		configMu.RUnlock()

		// 4. 暴力下发指令（这里直接构造请求，不触发会写配置文件的逻辑）
		payload := map[string]interface{}{
			"mode": modeWanted,
			"tun":  map[string]bool{"enable": tunWanted},
		}
		jsonBytes, _ := json.Marshal(payload)
		
		req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer(jsonBytes))
		req.Header.Set("Content-Type", "application/json")
		
		resp, err := httpClient.Do(req)
		if err == nil {
			resp.Body.Close()
			fmt.Printf("[Init] 指令送达成功: TUN=%v, Mode=%s\n", tunWanted, modeWanted)
		}
	} else {
		fmt.Println("[Init] 警告：内核 API 启动超时，放弃初始同步")
	}

	// 5. 关键：继续锁定一段时间，给虚拟网卡创建留出充足的“静默时间”
	// 这样 watchTunState 即使被唤醒，也只能改 UI 图标，不能改配置文件
	time.Sleep(12 * time.Second)
	
	isSystemInitializing = false
	fmt.Println("[Init] 启动保护期结束，配置权限已放开")
}

func isProcessRunning(name string) bool {
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	currPid := uint32(os.Getpid())

	if err := windows.Process32First(h, &pe); err != nil {
		return false
	}
	for {
		pname := windows.UTF16ToString(pe.ExeFile[:])
		if strings.EqualFold(pname, name) && pe.ProcessID != currPid {
			return true
		}
		pe.Size = uint32(unsafe.Sizeof(pe))
		if err := windows.Process32Next(h, &pe); err != nil {
			break
		}
	}
	return false
}

func onReady() {
	// 1. 初始化基础 UI 加载
	loadIniConfigAll()
	systray.SetTooltip("Mihomo Launcher")

	// 2. 预设菜单状态（完全信任 INI 配置文件）
	configMu.RLock()
	initialTun := configData["tun_enabled"] == "true"
	initialMode := configData["mode"]
	if initialMode == "" {
		initialMode = "rule"
	}
	configMu.RUnlock()

	mWeb := systray.AddMenuItem("进入控制面板", "")
	systray.AddSeparator()

	// 模式选择菜单
	mModeR := systray.AddMenuItemCheckbox("规则模式", "", initialMode == "rule")
	mModeG := systray.AddMenuItemCheckbox("全局模式", "", initialMode == "global")
	mModeD := systray.AddMenuItemCheckbox("直连模式", "", initialMode == "direct")
	systray.AddSeparator()

	// 状态切换菜单
	mTun = systray.AddMenuItemCheckbox("TUN 模式", "", initialTun)
	// 系统代理实时读注册表，确保 UI 准确
	mSystemProxy := systray.AddMenuItemCheckbox("系统代理", "", isProxyEnabledInRegistry())
	systray.AddSeparator()

	mAutoRun := systray.AddMenuItemCheckbox("开机自启", "", isAutoRunEnabled())
	mDir := systray.AddMenuItem("浏览本地文件", "")
	mRestart := systray.AddMenuItem("重启内核", "")
	mExit := systray.AddMenuItem("退出程序", "")

	// --- 核心逻辑启动区 ---

	// 3. 执行“暴力同步”协程：死磕内核直到配置下发成功
	go func() {
		isSystemInitializing = true // 激活全局锁，禁止 watchTunState 修改 INI
		fmt.Println("[Init] 启动保护激活，锁定配置文件...")

		apiReady := false
		for i := 0; i < 20; i++ {
			_, err := httpClient.Get(API_URL + "/version") // 探测 API 是否存活
			if err == nil {
				apiReady = true
				fmt.Printf("[Init] 第 %d 秒：内核 API 响应成功\n", i+1)
				break
			}
			fmt.Printf("[Init] 第 %d 秒：内核 API 尚未就绪...\n", i+1)
			time.Sleep(1 * time.Second)
		}

		if apiReady {
			fmt.Printf("[Init] 正在同步预设状态: TUN=%v, Mode=%s\n", initialTun, initialMode)
			
			payload := map[string]interface{}{
				"mode": initialMode,
				"tun":  map[string]bool{"enable": initialTun},
			}
			jsonBytes, _ := json.Marshal(payload)
			req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer(jsonBytes))
			req.Header.Set("Content-Type", "application/json")
			if resp, err := httpClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}

		// 关键：静默期，给 Windows 虚拟网卡创建留出时间
		time.Sleep(12 * time.Second)
		isSystemInitializing = false
		fmt.Println("[Init] 启动保护结束，移交配置写入权")
	}()

	// 4. 开启系统地址变动监听
	go watchTunState()

	// --- 事件循环 ---
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
			// 用户手动操作，不受 isSystemInitializing 限制
			now := !mTun.Checked()
			setTunMode(now)
			if now { mTun.Check() } else { mTun.Uncheck() }

		case <-mSystemProxy.ClickedCh:
			enable := !mSystemProxy.Checked()
			setProxyRegistry(enable) // 修正函数名引用
			if enable { mSystemProxy.Check() } else { mSystemProxy.Uncheck() }

		case <-mAutoRun.ClickedCh:
			enable := !mAutoRun.Checked()
			toggleAutoStart(enable) // 修正函数名引用
			if enable { mAutoRun.Check() } else { mAutoRun.Uncheck() }

		case <-mDir.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)

		case <-mRestart.ClickedCh:
			restartKernel()

		case <-mExit.ClickedCh:
			isReallyExiting = true
			systray.Quit()
		}
	}
}
func onExit() {
	if isReallyExiting {
		setProxyRegistry(false)
		time.Sleep(50 * time.Millisecond)
		if hJob != 0 {
			windows.CloseHandle(hJob)
		}
		if hMutex != 0 {
			windows.CloseHandle(hMutex)
		}
	}
}

// --- 系统操作 ---

func setMihomoMode(mode string) {
	saveIniConfig("mode", mode)
	payload := map[string]string{"mode": mode}
	jsonBody, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer(jsonBody))
	if resp, err := httpClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

func setTunMode(enable bool) {
	saveIniConfig("tun_enabled", fmt.Sprint(enable))
	payload := map[string]interface{}{"tun": map[string]bool{"enable": enable}}
	jsonBody, _ := json.Marshal(payload)
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer(jsonBody))
	if resp, err := httpClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

func setProxyRegistry(enable bool) {
	if !isReallyExiting {
		saveIniConfig("system_proxy_enabled", fmt.Sprint(enable))
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, REG_PROXY, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer key.Close()

	if enable {
		_ = key.SetDWordValue("ProxyEnable", 1)
		_ = key.SetStringValue("ProxyServer", DEFAULT_PROXY_ADDR)
	} else {
		_ = key.SetDWordValue("ProxyEnable", 0)
	}
}

func toggleAutoStart(enable bool) {
	saveIniConfig("auto_start", fmt.Sprint(enable))
	key, _ := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.SET_VALUE)
	defer key.Close()
	if enable {
		_ = key.SetStringValue(APP_NAME, exePath)
	} else {
		_ = key.DeleteValue(APP_NAME)
	}
}

func updateIconByState(state int) {
	files := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
	if state < 0 || state >= len(files) {
		return
	}
	b, err := iconFs.ReadFile("icons/" + files[state])
	if err == nil {
		systray.SetIcon(b)
	}
}

func reloadConfigFile() {
	configPath := filepath.Join(baseDir, "config.yaml")
	body := map[string]interface{}{"path": configPath}
	jsonPayload, _ := json.Marshal(body)
	req, _ := http.NewRequest("PUT", API_URL+"/configs?force=true", bytes.NewBuffer(jsonPayload))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := httpClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

// watchTunState 核心监听：网卡变动立刻同步 UI
func watchTunState() {
    var (
        modiphlpapi          = syscall.NewLazyDLL("iphlpapi.dll")
        procNotifyAddrChange = modiphlpapi.NewProc("NotifyAddrChange")
        handle               syscall.Handle
        overlapped           syscall.Overlapped
    )

    for {
        // 等待系统信号
        procNotifyAddrChange.Call(uintptr(unsafe.Pointer(&handle)), uintptr(unsafe.Pointer(&overlapped)))
        
        // 收到信号后先等 500ms 让网卡状态稳定
        time.Sleep(500 * time.Millisecond)

        // 检查网卡是否存在
        hasTun := false
        ifaces, _ := net.Interfaces()
        for _, i := range ifaces {
            name := strings.ToLower(i.Name)
            if strings.Contains(name, "mihomo") || strings.Contains(name, "meta") {
                hasTun = true
                break
            }
        }

        // 只有当“系统实际状态”与“菜单勾选状态”不一致时，才需要处理
        if mTun != nil && hasTun != mTun.Checked() {
            // A. 同步 UI：网卡在就打钩，不在就取消
            if hasTun {
                mTun.Check()
                updateIconByState(StateTun)
            } else {
                mTun.Uncheck()
                if getIniConfig("system_proxy_enabled") == "true" {
                    updateIconByState(StateProxy)
                } else {
                    updateIconByState(StateDefault)
                }
            }

            // B. 暴力权限检查：保护期内严禁反向修改配置文件
            if !isSystemInitializing {
                saveIniConfig("tun_enabled", fmt.Sprint(hasTun))
            }
            // 如果在 15s 内发生变化，上面这段代码会直接跳过写入，
            // 从而保护了启动时 ini 里的原始 true 不被覆盖。
        }
    }
}

func main() {
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	h, err := windows.CreateMutex(nil, false, mName)
	if err != nil { return }
	event, _ := windows.WaitForSingleObject(h, 0)
	if event == uint32(windows.WAIT_TIMEOUT) || event == uint32(windows.WAIT_FAILED) {
		if h != 0 { windows.CloseHandle(h) }
		return
	}
	hMutex = h

	if !isAdmin() {
		if hMutex != 0 { windows.CloseHandle(hMutex) }
		runAsAdmin()
		os.Exit(0)
	}

	os.Chdir(baseDir)
	initJobObject()

	go monitorKernelDaemon()
	systray.Run(onReady, onExit)
}
