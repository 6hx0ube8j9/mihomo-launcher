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
	APP_MUTEX   = "Global\\MihomoLauncher_Unique_Mutex"
	CONFIG_FILE = "mihomo-launcher.ini"
	REG_RUN     = `Software\Microsoft\Windows\CurrentVersion\Run`
	REG_PROXY   = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	APP_NAME    = "MihomoLauncher"

	StateStop    = 0
	StateError   = 1
	StateTun     = 2
	StateProxy   = 3
	StateDefault = 4
)

var (
	isReallyExiting      bool
	hJob                 windows.Handle
	hMutex               windows.Handle
	httpClient           = &http.Client{Timeout: 1 * time.Second}
	exePath, _           = os.Executable()
	baseDir              = filepath.Dir(exePath)
	configData           = make(map[string]string)
	configMu             sync.RWMutex
	lastState            = -1
	tunErrorCounter      = 0
	onceSync             sync.Once
	mTun                 *systray.MenuItem
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

func isTunInterfaceMatch(ifaceName string) bool {
	name := strings.ToLower(ifaceName)
	target := strings.ToLower(getIniConfig("tun_device_name"))
	if target != "" && strings.Contains(name, target) {
		return true
	}
	keywords := []string{"mihomo", "meta", "clash", "sing-box", "wintun"}
	for _, kw := range keywords {
		if strings.Contains(name, kw) {
			return true
		}
	}
	return false
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

// --- 配置管理核心 ---

func ensureDefaultConfig() {
	configMu.Lock()
	b, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	lines := strings.Split(string(b), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
			configData[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	defaults := [][]string{
		{"mode", "rule"},
		{"tun_enabled", "false"},
		{"system_proxy_enabled", "false"},
		{"startup_enabled", "false"},
		{"proxy_address", "127.0.0.1:7890"},
		{"tun_device_name", "Mihomo"},
		{"external-controller", "http://127.0.0.1:9090"},
		{"secret", ""},
	}
	for _, pair := range defaults {
		if val, exists := configData[pair[0]]; !exists || val == "" {
			configData[pair[0]] = pair[1]
		}
	}
	configMu.Unlock()
	saveIniConfig("", "")
}

func sniffAndSolidifyConfig() {
	configPath := filepath.Join(baseDir, "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	inTunSection := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "tun:") {
			inTunSection = true
			continue
		}
		if inTunSection && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inTunSection = false
		}
		if inTunSection && strings.Contains(trimmed, "device:") {
			parts := strings.SplitN(trimmed, ":", 2)
			if len(parts) == 2 {
				devName := strings.Trim(parts[1], " \"'")
				if devName != "" {
					saveIniConfig("tun_device_name", devName)
				}
			}
		}
		if strings.HasPrefix(trimmed, "external-controller:") {
			addr := strings.Trim(strings.TrimPrefix(trimmed, "external-controller:"), " \"'")
			if strings.HasPrefix(addr, ":") {
				addr = "127.0.0.1" + addr
			}
			if addr != "" {
				saveIniConfig("external-controller", "http://"+addr)
			}
		}
		if strings.HasPrefix(trimmed, "secret:") {
			saveIniConfig("secret", strings.Trim(strings.TrimPrefix(trimmed, "secret:"), " \"'"))
		}
		if strings.HasPrefix(trimmed, "mixed-port:") || (strings.HasPrefix(trimmed, "port:") && getIniConfig("proxy_address") == "127.0.0.1:7890") {
			port := strings.Trim(strings.SplitN(trimmed, ":", 2)[1], " \"'")
			if port != "" {
				saveIniConfig("proxy_address", "127.0.0.1:"+port)
			}
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
	if key != "" {
		configData[key] = val
	}
	priorityKeys := []string{"mode", "tun_enabled", "system_proxy_enabled", "startup_enabled", "proxy_address", "tun_device_name", "external-controller", "secret"}
	var buf bytes.Buffer
	for _, k := range priorityKeys {
		if v, ok := configData[k]; ok {
			buf.WriteString(fmt.Sprintf("%s = %s\n", k, v))
		}
	}
	configMu.Unlock()
	_ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), buf.Bytes(), 0644)
}

func doAPIRequest(method, path string, payload interface{}) (*http.Response, error) {
    base := strings.TrimSuffix(getIniConfig("external-controller"), "/")
	url := base + "/" + strings.TrimPrefix(path, "/")
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
	}
	req, err := http.NewRequest(method, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if secret := getIniConfig("secret"); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	return httpClient.Do(req)
}

// --- 系统操作 ---

func setMihomoMode(mode string) {
	saveIniConfig("mode", mode)
	payload := map[string]string{"mode": mode}
	_, _ = doAPIRequest("PATCH", "/configs", payload)
}

func setTunMode(enable bool) {
	isSystemInitializing = true
	saveIniConfig("tun_enabled", fmt.Sprint(enable))
	payload := map[string]interface{}{"tun": map[string]bool{"enable": enable}}
	_, _ = doAPIRequest("PATCH", "/configs", payload)
	go func() {
		time.Sleep(3 * time.Second)
		isSystemInitializing = false
	}()
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
		_ = key.SetStringValue("ProxyServer", getIniConfig("proxy_address"))
	} else {
		_ = key.SetDWordValue("ProxyEnable", 0)
	}
}

func reloadConfigFile() {
    isSystemInitializing = true

    configPath, _ := filepath.Abs(filepath.Join(baseDir, "config.yaml"))
    
    // 关键点：在 Payload 中同时带上当前的 mode 和 path
    // 这样内核会知道：在不改变当前模式的情况下，重新读取这个路径的文件
    payload := map[string]interface{}{
        "path": configPath,
        "mode": getIniConfig("mode"), 
    }

    // 使用 PUT 方法，这是 Mihomo 触发完整 Realloc 逻辑的标准
    // 参数 force=false 保证了如果配置没变，就不重启连接
    resp, err := doAPIRequest("PUT", "/configs?force=false", payload)
    
    if err != nil {
        fmt.Printf("重载失败: %v\n", err)
        isSystemInitializing = false
        return
    }
    defer resp.Body.Close()

    if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
        fmt.Println("内核已接收重载指令")
        // 缩短锁定时间，给内核 500ms 足够解析文本了
        go func() {
            time.Sleep(500 * time.Millisecond)
            isSystemInitializing = false
        }()
    } else {
        isSystemInitializing = false
    }
}
func toggleAutoStart(enable bool) {
	const taskName = "MihomoLauncherTask"
	if key, err := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.SET_VALUE); err == nil {
		_ = key.DeleteValue(APP_NAME)
		key.Close()
	}
	saveIniConfig("startup_enabled", fmt.Sprint(enable))

	if enable {
		createCmd := exec.Command("schtasks", "/Create", "/TN", taskName, "/TR", "\""+exePath+"\"", "/SC", "ONLOGON", "/RL", "HIGHEST", "/F")
		createCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		if err := createCmd.Run(); err != nil {
			return
		}
		psScript := fmt.Sprintf(`$s = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero); Set-ScheduledTask -TaskName '%s' -Settings $s`, taskName)
		modifyCmd := exec.Command("powershell", "-Command", psScript)
		modifyCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		_ = modifyCmd.Run()
	} else {
		deleteCmd := exec.Command("schtasks", "/Delete", "/TN", taskName, "/F")
		deleteCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		_ = deleteCmd.Run()
	}
}

func checkAutoStartStatus() bool {
	const taskName = "MihomoLauncherTask"
	cmd := exec.Command("schtasks", "/Query", "/TN", taskName)
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.Run() == nil
}

// --- 监控逻辑 ---

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	for {
		if isReallyExiting { return }
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
				_ = cmd.Wait()
			}
		}
		time.Sleep(2 * time.Second)
	}
}

func monitorIconState() {
	for {
		if isReallyExiting { return }
		var curr int
		if !isProcessRunning("mihomo.exe") {
			curr = StateStop
		} else {
			curr = checkSystemState()
		}
		if curr != lastState {
			updateIconByState(curr)
			lastState = curr
		}
		time.Sleep(1 * time.Second)
	}
}

func checkSystemState() int {
    // 1. 尝试探测内核 API 活性
    resp, err := doAPIRequest("GET", "", nil)
    if err != nil {
        // 如果 API 不通，说明内核可能正在启动、崩溃或被防火墙拦截
        // 此时不应清空 tunErrorCounter，而是让它保持现状，直到确定内核已完全关闭
        return StateStop
    }
    defer resp.Body.Close()

    // --- 关键点：移除此处的 onceSync.Do ---
    // 不要在每秒执行一次的监控函数里触发同步逻辑，这会导致逻辑死锁
    // 同步逻辑应由 main 或 onReady 里的独立协程完成

    // 2. 检查 TUN 网卡状态
    // 我们优先信任用户在 INI 里的选择，如果用户开启了 TUN，我们去系统里找网卡
    if getIniConfig("tun_enabled") == "true" {
        hasTun := false
        ifaces, _ := net.Interfaces()
        for _, i := range ifaces {
            if isTunInterfaceMatch(i.Name) {
                hasTun = true
                break
            }
        }

        if hasTun {
            tunErrorCounter = 0 // 找到网卡，计数器归零
            return StateTun
        } else {
            // 如果内核 API 通了，但找不到网卡，可能是内核正在创建网卡或创建失败
            // 给予 8 秒左右的宽限期（配合 monitorIconState 的 1s 间隔）
            tunErrorCounter++
            if tunErrorCounter > 8 {
                return StateError // 超过 8 秒还没网卡，显示感叹号图标
            }
            // 宽限期内暂时显示停止状态，避免图标闪烁
            return StateStop
        }
    }

    // 3. 检查系统代理状态
    // 如果没有开启 TUN，检查 INI 是否开启了系统代理
    if getIniConfig("system_proxy_enabled") == "true" {
        return StateProxy
    }

    // 4. 默认状态（内核运行中，但未开启 TUN 或系统代理）
    return StateDefault
}

func watchTunState() {
	modiphlpapi := syscall.NewLazyDLL("iphlpapi.dll")
	procNotifyAddrChange := modiphlpapi.NewProc("NotifyAddrChange")
	var handle syscall.Handle
	var overlapped syscall.Overlapped

	for {
		// 阻塞等待 Windows 网络事件
		procNotifyAddrChange.Call(uintptr(unsafe.Pointer(&handle)), uintptr(unsafe.Pointer(&overlapped)))
		time.Sleep(800 * time.Millisecond) // 稍微等待网络协议栈稳定

		// 核心保护：如果系统正在同步或刚启动，跳过此次监听
		if isSystemInitializing {
			continue
		}

		// 核心保护：只有内核 API 正常响应时，才认为当前网卡状态是“受控”的
		resp, err := doAPIRequest("GET", "", nil)
		if err != nil {
			// API 不通，说明内核可能崩溃了，此时不要去反写 INI
			continue
		}
		resp.Body.Close()

		// 检查 TUN 网卡是否存在
		hasTun := false
		ifaces, _ := net.Interfaces()
		for _, i := range ifaces {
			if isTunInterfaceMatch(i.Name) {
				hasTun = true
				break
			}
		}

		// 同步逻辑：如果网卡实际状态与 INI 配置不一致，说明用户可能通过外部（如 Web UI）改了状态
		if mTun != nil {
			currentIniConfig := getIniConfig("tun_enabled") == "true"
			if hasTun != currentIniConfig {
				fmt.Printf("[Monitor] 检测到外部状态变更: TUN -> %v. 同步至 INI。\n", hasTun)
				
				// 同步菜单 UI
				if hasTun { mTun.Check() } else { mTun.Uncheck() }
				
				// 同步到 INI 文件
				saveIniConfig("tun_enabled", fmt.Sprint(hasTun))
			}
		}
	}
}

func isProcessRunning(name string) bool {
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil { return false }
	defer windows.CloseHandle(h)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	currPid := uint32(os.Getpid())
	if err := windows.Process32First(h, &pe); err != nil { return false }
	for {
		pname := windows.UTF16ToString(pe.ExeFile[:])
		if strings.EqualFold(pname, name) && pe.ProcessID != currPid { return true }
		pe.Size = uint32(unsafe.Sizeof(pe))
		if err := windows.Process32Next(h, &pe); err != nil { break }
	}
	return false
}

func updateIconByState(state int) {
	files := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
	if state < 0 || state >= len(files) { return }
	b, err := iconFs.ReadFile("icons/" + files[state])
	if err == nil { systray.SetIcon(b) }
}

var syncMu sync.Mutex
func syncConfigToKernel() {
	syncMu.Lock()
	defer syncMu.Unlock()

	// 开启初始化锁，防止 watchTunState 在此期间反向干扰
	isSystemInitializing = true
	fmt.Println("[Sync] 正在将本地 INI 配置同步至内核...")

	// 1. 读取本地 INI 作为“最终真理”
	tun := getIniConfig("tun_enabled") == "true"
	mode := getIniConfig("mode")

	// 2. 构造负载
	payload := map[string]interface{}{
		"mode": mode,
		"tun":  map[string]bool{"enable": tun},
	}

	// 3. 执行 Patch 请求 (带重试逻辑)
	var err error
	var resp *http.Response
	for i := 0; i < 3; i++ {
		resp, err = doAPIRequest("PATCH", "/configs", payload)
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	if err != nil {
		fmt.Printf("[Sync] 同步配置到内核失败: %v\n", err)
		isSystemInitializing = false // 失败则立即解锁
		return
	}

	// 4. 异步处理稳定期与锁释放
	go func() {
		// 给内核留出创建网卡或切换模式的时间
		if tun {
			time.Sleep(2 * time.Second)
		} else {
			time.Sleep(500 * time.Millisecond)
		}
		
		isSystemInitializing = false
		lastState = -1 // 强制图标监控下次运行阶段立即刷新图标
		fmt.Println("[Sync] 内核同步完成，保护锁已释放")
	}()

	// 5. 立即同步菜单 UI 的勾选状态，不需要等轮询
	if mTun != nil {
		if tun { mTun.Check() } else { mTun.Uncheck() }
	}
}

func onReady() {
	ensureDefaultConfig()
	sniffAndSolidifyConfig()

	// 根据配置初始化代理注册表
	setProxyRegistry(getIniConfig("system_proxy_enabled") == "true")
	updateIconByState(StateStop)

	// 独立协程：监听内核就绪并触发初次同步
	go func() {
		fmt.Println("[Init] 正在等待内核 API 响应...")
		success := false
		for i := 0; i < 20; i++ {
			resp, err := doAPIRequest("GET", "", nil)
			if err == nil {
				resp.Body.Close()
				success = true
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		if success {
			fmt.Println("[Init] 内核 API 已就绪，触发自动激活同步")
			syncConfigToKernel()
		} else {
			fmt.Println("[Init] 内核启动超时，释放初始化锁")
			isSystemInitializing = false
		}
	}()

	// --- 菜单项创建逻辑 ---
	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	systray.AddSeparator()

	mProxy := systray.AddMenuItemCheckbox("系统代理", "", getIniConfig("system_proxy_enabled") == "true")
	mTun = systray.AddMenuItemCheckbox("TUN 模式", "", getIniConfig("tun_enabled") == "true")
	systray.AddSeparator()

	curMode := getIniConfig("mode")
	modeMenus := make(map[string]*systray.MenuItem)
	modeMenus["rule"] = systray.AddMenuItemCheckbox("规则模式", "", curMode == "rule")
	modeMenus["global"] = systray.AddMenuItemCheckbox("全局模式", "", curMode == "global")
	modeMenus["direct"] = systray.AddMenuItemCheckbox("直连模式", "", curMode == "direct")
	systray.AddSeparator()

	isAuto := checkAutoStartStatus()
	mAuto := systray.AddMenuItemCheckbox("开机自启动", "", isAuto)
	mReload := systray.AddMenuItem("重载配置文件", "手动通知内核读取 config.yaml")
	mDir := systray.AddMenuItem("打开程序目录", "")
	mRestart := systray.AddMenuItem("重启内核", "")
	systray.AddSeparator()
	mExit := systray.AddMenuItem("关闭程序", "")

	// 事件循环逻辑
	for {
		select {
		case <-mWeb.ClickedCh:
			apiAddr := getIniConfig("external-controller")
			secret := getIniConfig("secret")
			host, port := "127.0.0.1", "9090"
			cleanAddr := strings.TrimPrefix(strings.TrimPrefix(apiAddr, "http://"), "https://")
			if parts := strings.Split(cleanAddr, ":"); len(parts) == 2 {
				host, port = parts[0], parts[1]
			}
			finalURL := fmt.Sprintf("%s/ui/?hostname=%s&port=%s&secret=%s#/proxies", apiAddr, host, port, secret)
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(finalURL), nil, nil, windows.SW_SHOWNORMAL)
		case <-mReload.ClickedCh:
			sniffAndSolidifyConfig()
			reloadConfigFile()
		case <-modeMenus["rule"].ClickedCh:
			setMihomoMode("rule")
			modeMenus["rule"].Check(); modeMenus["global"].Uncheck(); modeMenus["direct"].Uncheck()
		case <-modeMenus["global"].ClickedCh:
			setMihomoMode("global")
			modeMenus["rule"].Uncheck(); modeMenus["global"].Check(); modeMenus["direct"].Uncheck()
		case <-modeMenus["direct"].ClickedCh:
			setMihomoMode("direct")
			modeMenus["rule"].Uncheck(); modeMenus["global"].Uncheck(); modeMenus["direct"].Check()
		case <-mTun.ClickedCh:
			next := !mTun.Checked()
			saveIniConfig("tun_enabled", fmt.Sprint(next))
			go syncConfigToKernel()
		case <-mProxy.ClickedCh:
			next := !mProxy.Checked()
			saveIniConfig("system_proxy_enabled", fmt.Sprint(next))
			setProxyRegistry(next)
			if next { mProxy.Check() } else { mProxy.Uncheck() }
		case <-mAuto.ClickedCh:
			next := !mAuto.Checked()
			toggleAutoStart(next)
			if next { mAuto.Check() } else { mAuto.Uncheck() }
		case <-mDir.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		case <-mRestart.ClickedCh:
			isSystemInitializing = true
			onceSync = sync.Once{}
			go func() {
				exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
			}()
		case <-mExit.ClickedCh:
			isReallyExiting = true
			systray.Quit()
			return
		}
	}
}

func onExit() {
	if isReallyExiting {
		setProxyRegistry(false)
		time.Sleep(50 * time.Millisecond)
		if hJob != 0 { windows.CloseHandle(hJob) }
		if hMutex != 0 { windows.CloseHandle(hMutex) }
	}
}

func main() {
	// 1. 【手术第一步】优先处理权限切换
	// 如果不是管理员，直接提权并退出，不触碰 Mutex
	if !isAdmin() {
		runAsAdmin()
		os.Exit(0)
	}

	// 2. 【手术第二步】确认是管理员权限后，再进行单实例检测
	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	h, err := windows.CreateMutex(nil, false, mName)
	if err != nil {
		return
	}
	
	// 尝试获取所有权
	event, _ := windows.WaitForSingleObject(h, 0)
	if event == uint32(windows.WAIT_TIMEOUT) || event == uint32(windows.WAIT_FAILED) {
		if h != 0 {
			windows.CloseHandle(h)
		}
		return
	}
	hMutex = h

	// 3. 后续初始化逻辑
	os.Chdir(baseDir)
	initJobObject()

	go monitorKernelDaemon()
	go monitorIconState()
	go watchTunState()

	systray.Run(onReady, onExit)
}
