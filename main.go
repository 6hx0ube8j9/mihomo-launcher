package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"io"
	"os/exec"
	"path/filepath"
	"sync/atomic"
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
	// --- 资源句柄与路径 ---
	hJob        windows.Handle // 用于关联内核进程，实现“同生共死”
	hMutex      windows.Handle // 单实例运行互斥锁句柄
	httpClient  = &http.Client{Timeout: 1 * time.Second}
	exePath, _  = os.Executable()
	baseDir     = filepath.Dir(exePath)

	// --- 核心状态保护 ---
	// isSystemInitializing: 逻辑锁。为 true 时，禁止 watchTunState 写入 INI，
	// 防止启动、重启或重载配置时的瞬时状态抖动破坏配置。
	isSystemInitializing = true

	// isSyncing: 并发锁。配合 atomic 包使用，防止多个协程同时发起 API 推送，
	// 避免在快速点击菜单或网络波动时产生竞态冲突。
	isSyncing int32

	// isReallyExiting: 退出锁。防止程序在正常关闭时误触发代理恢复逻辑。
	isReallyExiting bool

	// onceSync: 确保内核启动后，只在第一次连接成功时执行环境同步。
	onceSync sync.Once

	// --- 配置与读写锁 ---
	configMu   sync.RWMutex      // 保护 configData 这个 Map 的线程安全
	configData = make(map[string]string)

	// --- 状态跟踪与 UI ---
	lastState       = -1 // 记录上一次图标状态，避免高频重复刷新任务栏图标
	tunErrorCounter = 0  // TUN 网卡启动失败的容错计数器
	mTun            *systray.MenuItem // 缓存 TUN 菜单项句柄，方便异步更新 Check 状态
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
	apiAddr := getIniConfig("external-controller")
	if apiAddr == "" {
		return nil, fmt.Errorf("API 地址未配置")
	}

	// 1. 规范化 URL 拼接，防止出现 // 或缺少 / 的情况
	apiAddr = strings.TrimSuffix(apiAddr, "/")
	path = "/" + strings.TrimPrefix(path, "/")
	url := apiAddr + path

	// 2. 处理请求体
	var bodyReader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("JSON 序列化失败: %v", err)
		}
		bodyReader = bytes.NewBuffer(b)
	}

	// 3. 创建请求
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %v", err)
	}

	// 4. 注入核心 Header
	req.Header.Set("Content-Type", "application/json")
	if secret := getIniConfig("secret"); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}

	// 5. 执行请求
	resp, err := httpClient.Do(req)
	if err != nil {
		// 如果发生错误，httpClient 会自动关闭连接，无需处理 resp
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
	    io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("API Error: %d", resp.StatusCode)
	}	
	return resp, nil
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
    // 1. 开启保护锁，防止重载期间的逻辑抖动
    isSystemInitializing = true

    configPath := filepath.Join(baseDir, "config.yaml")
    payload := map[string]string{
        "path": configPath,
    }

    // 2. 调用 API 进行热重载
    // 注意：这里我们不需要在 reloadConfigFile 里再次声明 secret，
    // 因为 doAPIRequest 函数内部已经自动处理了从 INI 读取 secret 并加入 Header 的逻辑。
    resp, err := doAPIRequest("PUT", "/configs?force=false", payload)
    
    if err != nil {
        // 请求失败（内核未启动或网络错误），立即解锁，否则 watchTunState 会永久卡死
        isSystemInitializing = false
        return
    }
    defer resp.Body.Close()

    // 3. 只有成功响应时，才交给 syncConfigToKernel 执行后续的“稳定期”同步
    if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
        // syncConfigToKernel 会处理：PATCH 配置 -> 等待 3 秒 -> 解锁 isSystemInitializing
        go syncConfigToKernel()
    } else {
        // 如果内核返回错误（如 400 路径错误），说明没重载成功，直接解锁
        isSystemInitializing = false
    }
}
func toggleAutoStart(enable bool) {
    const taskName = "MihomoLauncherTask"
    // 1. 清理旧的注册表启动项（保持整洁）
    if key, err := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.SET_VALUE); err == nil {
        _ = key.DeleteValue(APP_NAME)
        key.Close()
    }
    saveIniConfig("startup_enabled", fmt.Sprint(enable))

    if enable {
        // 2. 创建任务：新增了 /D 参数定位工作目录
        // 注意：/RL HIGHEST 确保了以管理员权限运行，这对 TUN 模式至关重要
        createCmd := exec.Command("schtasks", "/Create", 
            "/TN", taskName, 
            "/TR", "\""+exePath+"\"", 
            "/D", "\""+baseDir+"\"", 
            "/SC", "ONLOGON", 
            "/RL", "HIGHEST", 
            "/F")
        createCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
        if err := createCmd.Run(); err != nil {
            return
        }
        psScript := fmt.Sprintf(`$s = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero); Set-ScheduledTask -TaskName '%s' -Settings $s`, taskName)
        modifyCmd := exec.Command("powershell", "-Command", psScript)
        modifyCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
        _ = modifyCmd.Run()
    } else {
        // 4. 删除任务
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

func monitorKernelDaemon() {
    target := filepath.Join(baseDir, "mihomo.exe")
    absBaseDir, _ := filepath.Abs(baseDir) // 强制使用绝对路径

    for {
        if isReallyExiting { return }
        
        if !isProcessRunning("mihomo.exe") {
            onceSync = sync.Once{}
            
            // 杀掉可能的残留进程树
            exec.Command("taskkill", "/F", "/IM", "mihomo.exe", "/T").Run()
            time.Sleep(500 * time.Millisecond)

            // 核心修复：-d 指定配置目录，且 cmd.Dir 锁定工作目录
            cmd := exec.Command(target, "-d", ".")
            cmd.Dir = absBaseDir 
            cmd.SysProcAttr = &windows.SysProcAttr{
                CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_BREAKAWAY_FROM_JOB,
            }
            
            if err := cmd.Start(); err == nil {
                if hJob != 0 {
                    hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
                    windows.AssignProcessToJobObject(hJob, hp)
                    windows.CloseHandle(hp)
                }
                cmd.Wait() 
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
	resp, err := doAPIRequest("GET", "", nil)
	if err != nil {
		tunErrorCounter = 0
		return StateStop
	}
	defer resp.Body.Close()

	if isSystemInitializing { isSystemInitializing = false }
	onceSync.Do(func() { go syncConfigToKernel() })

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
			tunErrorCounter = 0
			return StateTun
		} else {
			tunErrorCounter++
			if tunErrorCounter > 8 { return StateError }
			return StateStop
		}
	}
	if getIniConfig("system_proxy_enabled") == "true" { return StateProxy }
	return StateDefault
}

func watchTunState() {
	// 1. 加载网络辅助 DLL
	modiphlpapi := syscall.NewLazyDLL("iphlpapi.dll")
	procNotifyAddrChange := modiphlpapi.NewProc("NotifyAddrChange")

	for {
		// 退出保护：如果程序正在关闭，停止监听
		if isReallyExiting {
			return
		}

		// 2. 阻塞式调用 NotifyAddrChange
		// 这个函数会挂起当前协程，直到 Windows 网络栈（IP 地址、路由、网卡状态）发生任何变化
		// 注意：在同步模式下，handle 和 overlapped 传空即可
		var handle windows.Handle
		var overlapped windows.Overlapped
		procNotifyAddrChange.Call(uintptr(unsafe.Pointer(&handle)), uintptr(unsafe.Pointer(&overlapped)))

		// 3. 【关键】防抖与安定期
		// 网络切换时 Windows 会连续触发多次事件（网卡启动 -> 链路连接 -> 分配IP）
		// 等待 2 秒确保网卡状态已经“坐实”，避免读取到中间瞬时状态导致逻辑误判
		time.Sleep(2 * time.Second)

		// 4. 【过滤锁】检查是否处于“意图变更期”
		// 如果用户刚点完菜单切换模式，或者正在同步配置，我们不应该去反向修改配置
		if isSystemInitializing || atomic.LoadInt32(&isSyncing) == 1 {
			continue
		}

		// 5. 检查内核存活状态
		// 如果 API 都不通，说明内核挂了，此时网卡是否存在都没有意义，跳过
		resp, err := doAPIRequest("GET", "/configs", nil)
		if err != nil {
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		// 6. 【特征匹配】扫描物理网卡，验证我们关心的那个 TUN
		hasTun := false
		ifaces, _ := net.Interfaces()
		for _, i := range ifaces {
			// 使用你之前定义的逻辑，通过名字或特征判断是否为目标网卡
			if isTunInterfaceMatch(i.Name) {
				hasTun = true
				break
			}
		}

		// 7. 【状态对齐】
		// 将“系统真实状态”与“本地持久化配置”进行对比
		configEnabled := getIniConfig("tun_enabled") == "true"

		if hasTun != configEnabled {
			// 如果不一致，说明用户可能在 Web 面板或者通过其他方式手动切换了网卡
			// 我们需要同步这个变化到 UI 菜单和配置文件中
			if mTun != nil {
				if hasTun {
					mTun.Check()
				} else {
					mTun.Uncheck()
				}
			}
			saveIniConfig("tun_enabled", fmt.Sprint(hasTun))
			fmt.Printf("[Monitor] 检测到网络变更: 物理网卡状态(%v) != 配置文件(%v)，已完成同步。\n", hasTun, configEnabled)
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

func syncConfigToKernel() {
    // 1. 【并发锁】使用原子操作防止多个协程同时同步，产生指令交织
    if !atomic.CompareAndSwapInt32(&isSyncing, 0, 1) {
        return 
    }
    defer atomic.StoreInt32(&isSyncing, 0)

    // 2. 【逻辑锁】进入系统初始化状态，此时 watchTunState 监控会静默，不会反向改写 INI
    isSystemInitializing = true
    
    // 自动兜底：无论同步成功失败，10秒后必须解除初始化锁，防止程序逻辑永久死锁
    timer := time.AfterFunc(10*time.Second, func() {
        isSystemInitializing = false
    })
    defer timer.Stop()

    // 准备推送的数据
    tunEnabled := getIniConfig("tun_enabled") == "true"
    payload := map[string]interface{}{
        "mode": getIniConfig("mode"),
        "tun":  map[string]bool{"enable": tunEnabled},
    }

    // 3. 【健壮重试】使用指数退避策略尝试同步，应对内核刚启动 API 还没就绪的情况
    success := false
    for i := 0; i < 3; i++ {
        resp, err := doAPIRequest("PATCH", "/configs", payload)
        if err == nil {
            // ✅ 关键：即使不需要内容，也要读取并关闭，确保连接复用
            io.Copy(io.Discard, resp.Body)
            resp.Body.Close()
            success = true
            break
        }
        
        // 如果失败，等待时间随次数增加 (500ms, 1000ms, 1500ms)
        time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
    }

    // 4. 【状态回填】同步成功后，更新 UI 菜单的勾选状态
    if success && mTun != nil {
        if tunEnabled {
            mTun.Check()
        } else {
            mTun.Uncheck()
        }
    }

    time.Sleep(1 * time.Second)
    isSystemInitializing = false
}

func onReady() {
    ensureDefaultConfig()
    sniffAndSolidifyConfig()

    setProxyRegistry(getIniConfig("system_proxy_enabled") == "true")
    updateIconByState(StateStop)

    // --- 第一部分：基础操作 ---
    mWeb := systray.AddMenuItem("进入 Web 面板", "")
    systray.AddSeparator()

    // --- 第二部分：核心开关 ---
    mProxy := systray.AddMenuItemCheckbox("系统代理", "", getIniConfig("system_proxy_enabled") == "true")
    mTun = systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", getIniConfig("tun_enabled") == "true")
    systray.AddSeparator()

    // --- 第三部分：模式切换 (二级菜单) ---
    mModeRoot := systray.AddMenuItem("模式切换", "")
    curMode := getIniConfig("mode")
    modeMenus := make(map[string]*systray.MenuItem)
    modeMenus["rule"] = mModeRoot.AddSubMenuItemCheckbox("规则模式", "", curMode == "rule")
    modeMenus["global"] = mModeRoot.AddSubMenuItemCheckbox("全局模式", "", curMode == "global")
    modeMenus["direct"] = mModeRoot.AddSubMenuItemCheckbox("直连模式", "", curMode == "direct")
    systray.AddSeparator()

    // --- 第四部分：工具与更多 (二级菜单) ---
    mDir := systray.AddMenuItem("打开目录", "")
    
    mMoreRoot := systray.AddMenuItem("更多", "")
    isAuto := checkAutoStartStatus()
    mAuto := mMoreRoot.AddSubMenuItemCheckbox("开机自动启动", "", isAuto)
    mRestart := mMoreRoot.AddSubMenuItem("重启内核", "")
    mReload := mMoreRoot.AddSubMenuItem("重载配置文件", "手动通知内核读取 config.yaml")
    systray.AddSeparator()

    // --- 第五部分：退出 ---
    mExit := systray.AddMenuItem("关闭程序", "")

    // 事件循环保持不变...
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
			if next { mTun.Check() } else { mTun.Uncheck() }
            go setTunMode(next)
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
            killCmd := exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe")
			killCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
			_ = killCmd.Run()
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
    // 获取真实的执行路径
    exePath, _ = os.Executable()
    baseDir = filepath.Dir(exePath)

    // 强制切换工作目录
    err := os.Chdir(baseDir)
    if err != nil {
        // 如果切换失败，可能导致后续所有相对路径失效
        return 
    }
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
