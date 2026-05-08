package main

import (
    "bufio"
	"regexp"
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

	StateStop    = 0
	StateError   = 1
	StateTun     = 2
	StateProxy   = 3
	StateDefault = 4
)

var (
    // 降维后的成品变量（直接给程序执行用，不含逻辑拼接）
    ExternalController string // 样例: http://127.0.0.1:9090
    Secret             string // 样例: your_password
    MixedPort          string // 样例: 127.0.0.1:7890

    // 状态控制
    isReallyExiting bool
	isSystemInitializing bool
	tunErrorCounter      int
	onceSync             sync.Once
	mTun                 *systray.MenuItem
    hJob            windows.Handle
    hMutex          windows.Handle
    httpClient      = &http.Client{Timeout: 1 * time.Second}
    exePath, _      = os.Executable()
    baseDir         = filepath.Dir(exePath)
    configData      = make(map[string]string)
    configMu        sync.RWMutex
    lastState       = -1
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

	// 原子写入：写临时文件 -> 重命名
	configPath := filepath.Join(baseDir, CONFIG_FILE)
	tmpPath := configPath + ".tmp"

	// 先尝试写临时文件
	if err := os.WriteFile(tmpPath, content, 0644); err != nil {
		return 
	}

	// Windows 下需要先删除原文件才能 Rename 成功
	os.Remove(configPath)
	if err := os.Rename(tmpPath, configPath); err != nil {
		// 容错处理
		os.Rename(tmpPath, configPath)
	}
}

// --- 核心逻辑 ---

func syncConfigToKernel() {
	configMu.RLock()
	tun := configData["tun_enabled"] == "true"
	mode := configData["mode"]
	if mode == "" { mode = "rule" }
	configMu.RUnlock()

	// 保持原有核心逻辑：一次性对齐所有内核参数
	payload := map[string]interface{}{
		"mode": mode,
		"tun":  map[string]bool{"enable": tun},
	}
	sendAPIRequest("PATCH", "/configs", payload)
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
	// 手术点：探测使用 ExternalController
	req, _ := http.NewRequest("GET", ExternalController, nil)
	if Secret != "" {
		req.Header.Set("Authorization", "Bearer "+Secret)
	}
	
	resp, err := httpClient.Do(req)
	if err != nil {
		tunErrorCounter = 0
		return StateStop
	}
	defer resp.Body.Close()

	if isSystemInitializing {
		isSystemInitializing = false
	}

	onceSync.Do(func() {
		go syncConfigToKernel()
	})

	configMu.RLock()
	wantTun := configData["tun_enabled"] == "true"
	wantProxy := configData["system_proxy_enabled"] == "true"
	configMu.RUnlock()

	if wantTun {
		hasTunInterface := false
		ifaces, err := net.Interfaces()
		if err == nil {
			for _, i := range ifaces {
				name := strings.ToLower(i.Name)
				if strings.Contains(name, "mihomo") || strings.Contains(name, "meta") || 
				   strings.Contains(name, "clash") || strings.Contains(name, "sing-box") {
					hasTunInterface = true
					break
				}
			}
		}

		if hasTunInterface {
			tunErrorCounter = 0
			return StateTun
		} else {
			// 保持原有逻辑：连续 5 秒没找到网卡才变黄
			tunErrorCounter++
			if tunErrorCounter > 5 {
				return StateError
			}
			return StateTun
		}
	}

	if wantProxy {
		return StateProxy
	}
	return StateDefault
}

func watchTunState() {
	var (
		modiphlpapi          = syscall.NewLazyDLL("iphlpapi.dll")
		procNotifyAddrChange = modiphlpapi.NewProc("NotifyAddrChange")
		handle               syscall.Handle
		overlapped           syscall.Overlapped
	)

	for {
		// 1. 阻塞等待 Windows 系统发送网络地址变动信号
		// 当你手动开关网卡、拨号或内核创建 TUN 网卡时，此函数会返回
		procNotifyAddrChange.Call(uintptr(unsafe.Pointer(&handle)), uintptr(unsafe.Pointer(&overlapped)))
		
		// 给系统 500ms 响应时间，等待适配器列表刷新完成
		time.Sleep(500 * time.Millisecond)

		// 2. 检测物理网卡是否存在
		hasTun := false
		ifaces, err := net.Interfaces()
		if err == nil {
			for _, i := range ifaces {
				name := strings.ToLower(i.Name)
				if strings.Contains(name, "mihomo") || 
				   strings.Contains(name, "meta") || 
				   strings.Contains(name, "clash") || 
				   strings.Contains(name, "sing-box") {
					hasTun = true
					break
				}
			}
		}

		// 3. 【核心同步逻辑】
		// 只有在 UI 菜单已初始化，且当前不在“人工操作/启动缓冲”锁定期间，才执行同步
		if mTun != nil && !isSystemInitializing {
			if hasTun {
				mTun.Check()
			} else {
				mTun.Uncheck()
			}

			// 4. 将真实的网卡物理状态写回配置文件
			// 这样做可以确保：如果网卡因为意外（如内核崩溃）消失了，配置也会同步更新
			// 避免下次启动时因为网卡不存在而导致的一系列报错
			saveIniConfig("tun_enabled", fmt.Sprint(hasTun))
		}
		
		// 稍微喘息一下，防止极端情况下信号过于频繁导致 CPU 占用
		time.Sleep(100 * time.Millisecond)
	}
}

func reloadConfigFile() {
    // 自愈联动：先重新扫一次 YAML 到 INI
    sniffAndSolidifyConfig() 
    
    configPath := filepath.Join(baseDir, "config.yaml")
    body := map[string]string{"path": configPath}
    sendAPIRequest("PATCH", "/configs", body)
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
    loadIniConfigAll()
    // 初始状态强制对齐一次注册表
    setProxyRegistry(getIniConfig("system_proxy_enabled") == "true")
    updateIconByState(StateStop)

    // --- 1. 菜单项创建 ---
    mWeb := systray.AddMenuItem("进入 Web 面板", "")
    mProxy := systray.AddMenuItemCheckbox("系统代理", "", getIniConfig("system_proxy_enabled") == "true")
    mTun = systray.AddMenuItemCheckbox("TUN 模式", "", getIniConfig("tun_enabled") == "true")
    systray.AddSeparator()

    curMode := getIniConfig("mode")
    modeMenus := make(map[string]*systray.MenuItem)
    modeMenus["rule"] = systray.AddMenuItemCheckbox("规则模式", "", curMode == "rule" || curMode == "")
    modeMenus["global"] = systray.AddMenuItemCheckbox("全局模式", "", curMode == "global")
    modeMenus["direct"] = systray.AddMenuItemCheckbox("直连模式", "", curMode == "direct")
    systray.AddSeparator()

    mAuto := systray.AddMenuItemCheckbox("开机自动启动", "", getIniConfig("auto_start") == "true")
    mDir := systray.AddMenuItem("打开程序目录", "")
    mReloadYAML := systray.AddMenuItem("重载配置文件", "")
    mRestart := systray.AddMenuItem("重启内核", "")
    systray.AddSeparator()

    mExit := systray.AddMenuItem("退出程序", "")

    // --- 2. 事件循环 ---
    for {
        select {
        case <-mWeb.ClickedCh:
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(ExternalController+"/ui"), nil, nil, windows.SW_SHOWNORMAL)

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
            // 内部会触发 isSystemInitializing = true 保护 3 秒
            setTunMode(next) 
            if next {
                mTun.Check()
            } else {
                mTun.Uncheck()
            }

        case <-mProxy.ClickedCh:
            next := !mProxy.Checked()
            // 系统代理通常很快，不需要像 TUN 那样复杂的缓冲锁，但共用 saveIniConfig
            saveIniConfig("system_proxy_enabled", fmt.Sprint(next))
            setProxyRegistry(next)
            if next {
                mProxy.Check()
            } else {
                mProxy.Uncheck()
            }

        case <-mReloadYAML.ClickedCh:
            go reloadConfigFile()

        case <-mAuto.ClickedCh:
            next := !mAuto.Checked()
            toggleAutoStart(next)
            if next {
                mAuto.Check()
            } else {
                mAuto.Uncheck()
            }

        case <-mDir.ClickedCh:
            windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)

        case <-mRestart.ClickedCh:
            go func() {
                // 强制杀死进程，monitorKernelDaemon 会自动重启它
                killCmd := exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe")
                killCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
                _ = killCmd.Run()
                
                // 重置同步标志，让新内核启动后能重新接收配置
                configMu.Lock()
                onceSync = sync.Once{}
                configMu.Unlock()
            }()

        case <-mExit.ClickedCh:
            isReallyExiting = true
            systray.Quit()
            return
        }
    }
}

func sniffAndSolidifyConfig() {
    configPath := filepath.Join(baseDir, "config.yaml")
    file, err := os.Open(configPath)
    if err != nil {
        // 如果连 yaml 都没有，给一组默认成品，防止程序崩溃
        ExternalController = "http://127.0.0.1:9090"
        MixedPort = "127.0.0.1:7890"
        return
    }
    defer file.Close()

    // 预设“脏活”处理中转站
    tempPort := "7890" 
    rawController := "127.0.0.1:9090"
    rawSecret := ""

    // 预编译正则：只扫非注释行，精准抓取
    reController := regexp.MustCompile(`^external-controller:\s*['"]?([^'"]+?)['"]?`)
    reSecret     := regexp.MustCompile(`^secret:\s*['"]?([^'"]+?)['"]?`)
    reMixed      := regexp.MustCompile(`^mixed-port:\s*(\d+)`)
    rePort       := regexp.MustCompile(`^port:\s*(\d+)`)

    scanner := bufio.NewScanner(file)
    for scanner.Scan() {
        line := strings.TrimSpace(scanner.Text())
        // 跳过空行和注释，这是最基本的性能尊重
        if line == "" || strings.HasPrefix(line, "#") {
            continue
        }

        if m := reController.FindStringSubmatch(line); len(m) > 1 {
            rawController = m[1]
        } else if m := reSecret.FindStringSubmatch(line); len(m) > 1 {
            rawSecret = m[1]
        } else if m := reMixed.FindStringSubmatch(line); len(m) > 1 {
            tempPort = m[1]
        } else if m := rePort.FindStringSubmatch(line); len(m) > 1 {
            // 只有在没扫到 mixed-port 的情况下，才把普通 port 作为备选
            if tempPort == "7890" { tempPort = m[1] }
        }
    }

    // --- 开始“洗数据”：脏活一次干完 ---
    
    // 1. 处理 ExternalController：补全 IP 和协议头
    if strings.HasPrefix(rawController, ":") {
        rawController = "127.0.0.1" + rawController
    }
    if !strings.HasPrefix(rawController, "http") {
        rawController = "http://" + rawController
    }
    ExternalController = rawController

    // 2. 处理 Secret
    Secret = rawSecret

    // 3. 处理 MixedPort：直接拼接成品，程序以后拿来就用
    MixedPort = "127.0.0.1:" + tempPort

    // --- 固化阶段：写入 INI 快照 ---
    // 这样下次启动，程序直接走“最短路径”读这三个成品
    saveIniConfig("external-controller", ExternalController)
    saveIniConfig("secret", Secret)
    saveIniConfig("mixed-port", MixedPort)
    
    // 初始化时，如果 INI 是空的，默认给个 false 开关
    if getIniConfig("system_proxy_enabled") == "" {
        saveIniConfig("system_proxy_enabled", "false")
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
	sendAPIRequest("PATCH", "/configs", map[string]string{"mode": mode})
}

func setTunMode(enable bool) {
	// 保持原有核心逻辑：3 秒锁定保护
	isSystemInitializing = true 
	saveIniConfig("tun_enabled", fmt.Sprint(enable))
	
	sendAPIRequest("PATCH", "/configs", map[string]interface{}{
		"tun": map[string]bool{"enable": enable},
	})

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
	if err != nil { return }
	defer key.Close()

	if enable {
		_ = key.SetDWordValue("ProxyEnable", 1)
		// 手术点：MixedPort 已经是成品 "127.0.0.1:XXXX"，直接写入
		_ = key.SetStringValue("ProxyServer", MixedPort) 
	} else {
		_ = key.SetDWordValue("ProxyEnable", 0)
	}
}

func sendAPIRequest(method, path string, payload interface{}) {
	jsonBody, _ := json.Marshal(payload)
	// 手术点：使用解析好的 ExternalController 替代写死的 API_URL
	req, err := http.NewRequest(method, ExternalController+path, bytes.NewBuffer(jsonBody))
	if err != nil {
		return
	}

	// 手术点：自动注入从 YAML 扫出来的 Secret
	if Secret != "" {
		req.Header.Set("Authorization", "Bearer "+Secret)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err == nil {
		resp.Body.Close()
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

func main() {
    // 1. 基础环境检查：权限提升
    // 必须首先确保以管理员身份运行，否则后续创建网卡、修改注册表、写入文件都可能静默失败
    if !isAdmin() {
        runAsAdmin()
        return
    }

    // 2. 单实例锁：防止多个启动器互相抢占内核控制权
    // 使用全局 Mutex，确保即使在不同 Session 下也只能运行一个 Launcher
    hMutex, _ = windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(APP_MUTEX))
    if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
        os.Exit(0)
    }
    defer windows.CloseHandle(hMutex)

    // 3. 切换工作目录：环境锚定
    // 无论从哪里启动（命令行、快捷方式、注册表），都强制将工作目录设为程序所在目录
    // 这保证了 ./mihomo.exe 和 ./config.yaml 始终能被正确找到
    os.Chdir(baseDir)
    iniPath := filepath.Join(baseDir, CONFIG_FILE)

    // 4. 配置加载与变量固化 (核心手术点)
    // 逻辑：INI 存在则读快照（秒开）；不存在则扫 YAML 并生成快照（发现模式）
    if _, err := os.Stat(iniPath); os.IsNotExist(err) {
        // 进入自愈模式：扫描 YAML，提取外部控制地址、Secret 和端口
        sniffAndSolidifyConfig() 
    } else {
        // 进入高效模式：直接从 INI 填充成品变量
        loadIniConfigAll()
        ExternalController = getIniConfig("external-controller")
        Secret             = getIniConfig("secret")
        MixedPort          = getIniConfig("mixed-port")
    }

    // 5. 变量二次兜底：安全网逻辑
    // 防止 INI 文件损坏或 YAML 扫描失败导致的空变量
    if ExternalController == "" { ExternalController = "http://127.0.0.1:9090" }
    if MixedPort == "" { MixedPort = "127.0.0.1:7890" }

    // 6. 物理状态对齐：注册表同步
    // 在内核启动前，先把 Windows 代理指向 MixedPort。
    // 这确保了即便内核启动慢，用户的代理设置也是准确的。
    isProxyEnabled := getIniConfig("system_proxy_enabled") == "true"
    setProxyRegistry(isProxyEnabled)

    // 7. 进程树生命周期管理
    // 初始化 JobObject，将 Launcher 和 内核（mihomo.exe）绑定
    // 这样当 Launcher 被结束或崩溃时，Windows 会自动清理掉内核进程，防止孤儿进程占用端口
    initJobObject()

    // 8. 启动异步监控流水线
    // 这些协程现在消费的都是上面固化好的 ExternalController 和 Secret
    go monitorKernelDaemon() // 负责内核的保活与重启
    go monitorIconState()    // 负责根据 API 返回值切换托盘图标颜色
    go watchTunState()       // 负责实时监听系统网卡变动并同步给 UI

    // 9. UI 渲染与事件循环
    // 正式进入系统托盘模式，接管用户交互
    systray.Run(onReady, onExit)
}
