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
	
	// 1. 优先匹配从 YAML 嗅探到的 device 名称
	target := strings.ToLower(getIniConfig("tun_device_name"))
	if target != "" && strings.Contains(name, target) {
		return true
	}

	// 2. 保底匹配：涵盖常见内核默认名
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

// --- 配置管理核心 (INI 保底 + YAML 矫正) ---

func ensureDefaultConfig() {
	configMu.Lock()
	// 注意：此处不使用 defer Unlock，因为我们要手动控制解锁时机以调用 saveIniConfig

	// 1. 【读取阶段】从乱序或含有杂质的文件中提取合法配置
	b, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	lines := strings.Split(string(b), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// 忽略空行和注释
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 只抓取符合 key = value 格式的内容，其余文字（乱码）在此步被过滤掉
		if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			configData[key] = val
		}
	}

	// 2. 【保底阶段】如果某些关键字段被用户删了，或者值为空，赋予默认值
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

	// 3. 【解锁并刷新】
	configMu.Unlock() 

	// 强制调用保存逻辑：
	// 此时 saveIniConfig 会按照你定义的 priorityKeys 顺序，
	// 把内存里的干净数据写回文件。这步操作会完成：
	// A. 物理抹除所有乱码文档
	// B. 强制按顺序排列字段
	saveIniConfig("", "") 
}

func sniffAndSolidifyConfig() {
	configPath := filepath.Join(baseDir, "config.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return
	}

	lines := strings.Split(string(data), "\n")
	inTunSection := false // 状态机：标记是否在 tun 配置块内

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// 识别进入 tun 块
		if strings.HasPrefix(trimmed, "tun:") {
			inTunSection = true
			continue
		}
		// 如果遇到非缩进的顶层 key，说明退出了 tun 块
		if inTunSection && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inTunSection = false
		}

		// 在 tun 块内寻找 device
		if inTunSection && strings.Contains(trimmed, "device:") {
			parts := strings.SplitN(trimmed, ":", 2)
			if len(parts) == 2 {
				devName := strings.Trim(parts[1], " \"'")
				if devName != "" {
					saveIniConfig("tun_device_name", devName)
				}
			}
		}

		// 原有的 API 和端口嗅探逻辑
		if strings.HasPrefix(trimmed, "external-controller:") {
			addr := strings.Trim(strings.TrimPrefix(trimmed, "external-controller:"), " \"'")
			if strings.HasPrefix(addr, ":") { addr = "127.0.0.1" + addr }
			if addr != "" { saveIniConfig("external-controller", "http://"+addr) }
		}
		if strings.HasPrefix(trimmed, "secret:") {
			saveIniConfig("secret", strings.Trim(strings.TrimPrefix(trimmed, "secret:"), " \"'"))
		}
		if strings.HasPrefix(trimmed, "mixed-port:") || (strings.HasPrefix(trimmed, "port:") && getIniConfig("proxy_address") == "127.0.0.1:7890") {
			port := strings.Trim(strings.SplitN(trimmed, ":", 2)[1], " \"'")
			if port != "" { saveIniConfig("proxy_address", "127.0.0.1:"+port) }
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
	// 1. 更新内存数据
	if key != "" {
		configData[key] = val
	}

	// 2. 这里的顺序决定了文件里每一行的位置（置顶排序）
	priorityKeys := []string{
		"mode",
		"tun_enabled",
		"system_proxy_enabled",
		"startup_enabled",
		"proxy_address",
		"tun_device_name",
		"external-controller",
		"secret",
	}

	var buf bytes.Buffer
	// 3. 严格按照名单生成内容，名单外的垃圾内容会被直接物理抹除
	for _, k := range priorityKeys {
		if v, ok := configData[k]; ok {
			buf.WriteString(fmt.Sprintf("%s = %s\n", k, v))
		}
	}
	content := buf.Bytes()
	configMu.Unlock()

	// 4. 覆盖写入，让文件瞬间变整洁
	configPath := filepath.Join(baseDir, CONFIG_FILE)
	_ = os.WriteFile(configPath, content, 0644)
}

// 统一请求包装，处理 Secret
func doAPIRequest(method, path string, payload interface{}) (*http.Response, error) {
	url := getIniConfig("external-controller") + path
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

// --- 核心逻辑 ---

func syncConfigToKernel() {
	tun := getIniConfig("tun_enabled") == "true"
	mode := getIniConfig("mode")
	proxy := getIniConfig("system_proxy_enabled") == "true"

	payload := map[string]interface{}{
		"mode": mode,
		"tun":  map[string]bool{"enable": tun},
	}
	resp, err := doAPIRequest("PATCH", "/configs", payload)
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
				_ = cmd.Wait()
			}
		}
		time.Sleep(2 * time.Second)
	}
}

func monitorIconState() {
	for {
		if isReallyExiting {
			return
		}
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
	// 1. 尝试请求 API，如果连不上，说明内核没启动或崩溃了
	resp, err := doAPIRequest("GET", "", nil)
	if err != nil {
		tunErrorCounter = 0
		return StateStop
	}
	defer resp.Body.Close()

	// 如果 API 响应了，重置初始化状态
	if isSystemInitializing {
		isSystemInitializing = false
	}

	// 核心配置同步：确保内核运行后的配置与 Launcher 的 INI 一致
	onceSync.Do(func() {
		go syncConfigToKernel()
	})

	// 2. 获取用户期望的状态
	wantTun := getIniConfig("tun_enabled") == "true"
	wantProxy := getIniConfig("system_proxy_enabled") == "true"

	// 3. 如果开启了 TUN 模式，进行网卡实测
	if wantTun {
		hasTunInterface := false
		ifaces, err := net.Interfaces()
		if err == nil {
			for _, i := range ifaces {
				// 调用你刚添加的动态匹配函数
				if isTunInterfaceMatch(i.Name) {
					hasTunInterface = true
					break
				}
			}
		}

		if hasTunInterface {
			tunErrorCounter = 0
			return StateTun
		} else {
			// 容错处理：网卡可能还没来得及创建，连续 5 次检测不到才报 Error
			tunErrorCounter++
			if tunErrorCounter > 5 {
				return StateError
			}
			return StateTun // 暂时维持 Tun 状态，等待重试
		}
	}

	// 4. 如果没开 TUN 但开了系统代理
	if wantProxy {
		return StateProxy
	}

	// 5. 默认运行状态（普通模式）
	return StateDefault
}



func watchTunState() {
	var (
		// 动态加载 iphlpapi.dll，这是监听 Windows 网络层变更最优雅的方式
		modiphlpapi          = syscall.NewLazyDLL("iphlpapi.dll")
		procNotifyAddrChange = modiphlpapi.NewProc("NotifyAddrChange")
		handle               syscall.Handle
		overlapped           syscall.Overlapped
	)

	for {
		// 1. 阻塞等待：只有当系统网卡状态发生变化（如拨号、网卡启用/禁用）时才会被唤醒
		// 相比于 time.Sleep 轮询，这种方式几乎不占 CPU
		procNotifyAddrChange.Call(uintptr(unsafe.Pointer(&handle)), uintptr(unsafe.Pointer(&overlapped)))
		
		// 2. 缓冲等待：网卡从“出现”到“分配 IP 完毕”需要一点时间
		time.Sleep(500 * time.Millisecond)

		// 3. 检测是否存在匹配的 TUN 网卡
		hasTun := false
		ifaces, err := net.Interfaces()
		if err == nil {
			for _, i := range ifaces {
				// 关键点：调用你代码中通过 YAML 动态嗅探名字的匹配函数
				if isTunInterfaceMatch(i.Name) {
					hasTun = true
					break
				}
			}
		}

		// 4. 状态同步逻辑
		// 仅在非初始化阶段处理（防止 setTunMode 切换时产生竞态冲突）
		if mTun != nil && !isSystemInitializing {
			
			// A. 更新 UI 勾选状态
			if hasTun {
				mTun.Check()
			} else {
				mTun.Uncheck()
			}

			// B. 同步持久化配置
			// 获取当前 ini 记录的值，只有当网卡实体状态与配置不符时才写入磁盘
			currentConfig := getIniConfig("tun_enabled") == "true"
			if hasTun != currentConfig {
				saveIniConfig("tun_enabled", fmt.Sprint(hasTun))
			}
		}

		// 5. 兜底延迟，防止极端情况下（如 API 异常）产生高频死循环
		time.Sleep(200 * time.Millisecond)
	}
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
    // 1. 立即初始化保底配置
    ensureDefaultConfig()
    // 2. 尝试从 yaml 矫正
    sniffAndSolidifyConfig()

    setProxyRegistry(getIniConfig("system_proxy_enabled") == "true")
    updateIconByState(StateStop)

    // --- 第一组：Web 入口 (纯文本，无装饰) ---
    mWeb := systray.AddMenuItem("进入 Web 面板", "") 
    // 这道分隔线会将 Web 面板与下方的核心功能完全隔开
    systray.AddSeparator()

    // --- 第二组：功能开关 ---
    mProxy := systray.AddMenuItemCheckbox("系统代理", "", getIniConfig("system_proxy_enabled") == "true")
    mTun = systray.AddMenuItemCheckbox("TUN 模式", "", getIniConfig("tun_enabled") == "true")
    systray.AddSeparator()

    // --- 第三组：运行模式 ---
    curMode := getIniConfig("mode")
    modeMenus := make(map[string]*systray.MenuItem)
    modeMenus["rule"] = systray.AddMenuItemCheckbox("规则模式", "", curMode == "rule")
    modeMenus["global"] = systray.AddMenuItemCheckbox("全局模式", "", curMode == "global")
    modeMenus["direct"] = systray.AddMenuItemCheckbox("直连模式", "", curMode == "direct")
    systray.AddSeparator()

    // --- 第四组：系统项 ---
	isAuto := checkAutoStartStatus()
    mAuto := systray.AddMenuItemCheckbox("开机自启动", "", isAuto)
	if fmt.Sprint(isAuto) != getIniConfig("startup_enabled") {
	    saveIniConfig("startup_enabled", fmt.Sprint(isAuto))
	}	
    mDir := systray.AddMenuItem("打开程序目录", "")
    mRestart := systray.AddMenuItem("重启内核", "")
    systray.AddSeparator()
    mExit := systray.AddMenuItem("关闭程序", "")

	for {
		select {
		case <-mWeb.ClickedCh:
			// 自动提取配置并拼接全自动登录 URL
			apiAddr := getIniConfig("external-controller")
			secret := getIniConfig("secret")
			host, port := "127.0.0.1", "9090"
			
			cleanAddr := strings.TrimPrefix(strings.TrimPrefix(apiAddr, "http://"), "https://")
			if parts := strings.Split(cleanAddr, ":"); len(parts) == 2 {
				host, port = parts[0], parts[1]
			}
			
			// 最终合成：地址 + 免密参数 + 直接跳转到代理页面
			finalURL := fmt.Sprintf("%s/ui/?hostname=%s&port=%s&secret=%s#/proxies",
				apiAddr, host, port, secret)

			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(finalURL), nil, nil, windows.SW_SHOWNORMAL)

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
			setTunMode(next)
			if next { mTun.Check() } else { mTun.Uncheck() }
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
			go func() {
				// 强制结束内核进程以实现彻底重启
				exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
				onceSync = sync.Once{}
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

// toggleAutoStart 实现开机自启动逻辑：使用 Windows 计划任务取代注册表
func toggleAutoStart(enable bool) {
    const taskName = "MihomoLauncherTask"
    
    // 无论开启还是关闭，先尝试清理注册表旧项，保持配置一致
    if key, err := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.SET_VALUE); err == nil {
        _ = key.DeleteValue(APP_NAME)
        key.Close()
    }
    saveIniConfig("startup_enabled", fmt.Sprint(enable))

    if enable {
        // 第一步：创建基础任务（利用 schtasks 处理权限最简单）
        createCmd := exec.Command("schtasks", "/Create",
            "/TN", taskName,
            "/TR", "\""+exePath+"\"",
            "/SC", "ONLOGON",
            "/RL", "HIGHEST",
            "/F")
        createCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
        if err := createCmd.Run(); err != nil {
            return 
        }

        // 第二步：使用你刚刚测试成功的 PowerShell 逻辑进行深度加固
        // 增加 -ExecutionTimeLimit ([TimeSpan]::Zero) 彻底解决 72 小时停止问题
        psScript := fmt.Sprintf(
            `$s = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero); Set-ScheduledTask -TaskName '%s' -Settings $s`, 
            taskName,
        )
        
        modifyCmd := exec.Command("powershell", "-Command", psScript)
        modifyCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
        _ = modifyCmd.Run()
        
    } else {
        // 删除任务
        deleteCmd := exec.Command("schtasks", "/Delete", "/TN", taskName, "/F")
        deleteCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
        _ = deleteCmd.Run()
    }
}

// checkAutoStartStatus 实时检测系统计划任务列表中是否存在该自启项
func checkAutoStartStatus() bool {
    const taskName = "MihomoLauncherTask"
    
    // 通过 /Query 命令查询指定名称的任务
    cmd := exec.Command("schtasks", "/Query", "/TN", taskName)
    cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
    
    // 如果任务存在，schtasks 返回码为 0，err 则为 nil
    err := cmd.Run()
    return err == nil
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

func main() {
    // 1. 单实例互斥锁检查
    mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
    h, err := windows.CreateMutex(nil, false, mName)
    if err != nil {
        return
    }
    event, _ := windows.WaitForSingleObject(h, 0)
    if event == uint32(windows.WAIT_TIMEOUT) || event == uint32(windows.WAIT_FAILED) {
        if h != 0 {
            windows.CloseHandle(h)
        }
        return
    }
    hMutex = h

    // 2. 管理员权限检查与提权
    if !isAdmin() {
        // 提权前必须释放当前进程的互斥锁，否则新启动的管理员进程会因为检测到锁而直接退出
        if hMutex != 0 {
            windows.CloseHandle(hMutex)
            hMutex = 0
        }
        runAsAdmin()
        os.Exit(0)
    }

    // 3. 环境初始化
    os.Chdir(baseDir)
    initJobObject()

    // 4. 启动后台监控协程 (必须在 systray.Run 之前)
    go monitorKernelDaemon() // 守护 mihomo.exe
    go monitorIconState()   // 刷新托盘图标
    go watchTunState()      // 监听网络状态变更

    // 5. 启动托盘运行循环
    // onReady 内部会执行 ensureDefaultConfig 和 sniffAndSolidifyConfig
    systray.Run(onReady, onExit)
}
