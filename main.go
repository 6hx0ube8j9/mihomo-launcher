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
	APP_MUTEX   = "Global\\MihomoLauncher_Unique_Mutex"
	API_URL     = "http://127.0.0.1:9090"
	CONFIG_FILE = "mihomo-launcher.ini"
	REG_RUN     = `Software\Microsoft\Windows\CurrentVersion\Run`
	REG_PROXY   = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	APP_NAME    = "MihomoLauncher"
	DEFAULT_PROXY_ADDR = "127.0.0.1:7890"
	// --- 状态定义 (对应不同图标) ---
	StateStop    = 0 // 红色：内核进程不存在 或 API无法连接
	StateError   = 1 // 黄色：API正常 但 TUN模式开启失败（网卡未出现）
	StateTun     = 2 // 绿色：TUN模式正常运行中
	StateProxy   = 3 // 蓝色：系统代理模式开启且地址正确
	StateDefault = 4 // 灰色：API就绪 但未开启任何转发功能（或正在同步中）
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
    lastState       = -1
    tunErrorCounter = 0 
    onceSync        sync.Once
)

// --- 基础工具函数 ---

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

// --- 配置管理 ---

func loadIniConfigAll() {
    b, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
    
    configMu.Lock()
    defer configMu.Unlock()
    
    // 1. 正常解析已有配置
    configData = make(map[string]string)
    for _, line := range strings.Split(string(b), "\n") {
        line = strings.TrimSpace(line)
        if line == "" || strings.HasPrefix(line, "#") { continue }
        if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
            configData[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
        }
    }

    // 2. 核心补全：用一个 Map 定义好你的“世界观”
    defaults := map[string]string{
        "tun_enabled":           "false",
        "system_proxy_enabled":  "false",
        "mode":                  "rule",
        "auto_start":            "false",
    }

    // 3. 补全逻辑：只有缺失时才补，逻辑非常清晰
    needsSave := false
    for k, v := range defaults {
        if _, exists := configData[k]; !exists {
            configData[k] = v
            needsSave = true
        }
    }

    // 4. 如果有变动，顺手存一下
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
        // 修正：确保 key 不为空且每行都有换行符
        if k = strings.TrimSpace(k); k != "" {
            buf.WriteString(fmt.Sprintf("%s = %s\n", k, v))
        }
    }
    content := buf.Bytes() // 先把数据取出来
    configMu.Unlock()
    
    // 写入文件放在锁外面，减少锁占用的时间
    _ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), content, 0644)
}

// --- 核心逻辑：自动同步 ---

func syncConfigToKernel() {
    configMu.RLock()
    tun := configData["tun_enabled"] == "true"
    mode := configData["mode"]
    if mode == "" { mode = "rule" }
    proxy := configData["system_proxy_enabled"] == "true"
    configMu.RUnlock()

    payload := fmt.Sprintf(`{"mode": "%s", "tun": {"enable": %v}}`, mode, tun)
    // 修正：增加错误检查
    req, err := http.NewRequest("PATCH", API_URL+"/configs", strings.NewReader(payload))
    if err != nil { return } 
    
    req.Header.Set("Content-Type", "application/json")
    resp, err := httpClient.Do(req)
    if err == nil {
        defer resp.Body.Close() // 必须 defer 释放连接
        if (resp.StatusCode == 204 || resp.StatusCode == 200) && proxy {
            // 这里有个小细节：如果同步成功且配置里写了要开代理，再强刷一次注册表
            setProxyRegistry(true)
        }
    }
}

func monitorKernelDaemon() {
    target := filepath.Join(baseDir, "mihomo.exe")
    for {
        if isReallyExiting { return }

        // 检查内核是否在运行
        if !isProcessRunning("mihomo.exe") {
            // 重置同步锁，确保内核重启后能重新推送一次配置
            onceSync = sync.Once{}

            // 【关键】：启动前清理可能残留的僵尸进程或抢占端口的旧进程
            killCmd := exec.Command("taskkill", "/F", "/IM", "mihomo.exe", "/T")
            killCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
            _ = killCmd.Run()
            
            // 给系统一点时间释放网络端口
            time.Sleep(300 * time.Millisecond)

            // 启动内核
            cmd := exec.Command(target, "-d", baseDir)
            cmd.SysProcAttr = &windows.SysProcAttr{
                CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_BREAKAWAY_FROM_JOB,
            }

            if err := cmd.Start(); err == nil {
                // 将内核进程绑定到 JobObject，Launcher 崩溃时内核会跟着死
                if hJob != 0 {
                    hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
                    _ = windows.AssignProcessToJobObject(hJob, hp)
                    windows.CloseHandle(hp)
                }
                // 等待内核进程结束
                _ = cmd.Wait()
            }
        }
        // 检查频率不要太快，2秒一次即可
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
    // 1. API 连通性检测：这是唯一的客观物理事实，不通则一切无从谈起
    resp, err := httpClient.Get(API_URL)
    if err != nil {
        tunErrorCounter = 0
        return StateStop // API 不通，内核可能崩溃或启动中，显示红色
    }
    resp.Body.Close()

    // 2. 只要 API 通了，触发一次同步（利用 sync.Once 确保内核重启后只推一次）
    onceSync.Do(func() {
        go syncConfigToKernel()
    })

    // 3. 读取本地内存中的“真理”（.ini 的意志）
    configMu.RLock()
    wantTun := configData["tun_enabled"] == "true"
    wantProxy := configData["system_proxy_enabled"] == "true"
    configMu.RUnlock()

    // 4. TUN 模式判定：因为涉及虚拟网卡硬件，需要物理检测
    if wantTun {
        hasTunInterface := false
        ifaces, _ := net.Interfaces()
        for _, i := range ifaces {
            name := strings.ToLower(i.Name)
            // 匹配常见的 TUN 网卡关键词
            if strings.Contains(name, "mihomo") || strings.Contains(name, "meta") || strings.Contains(name, "clash") {
                hasTunInterface = true
                break
            }
        }

        if hasTunInterface {
            tunErrorCounter = 0
            return StateTun // 绿色：网卡已就绪
        } else {
            tunErrorCounter++
            if tunErrorCounter > 5 {
                return StateError // 黄色：超过 5 秒网卡还没出来，报错
            }
            return StateTun // 宽容期内直接给绿色，避免图标闪烁，这叫“预判成功”
        }
    }

    // 5. 系统代理判定：暴力信任逻辑
    // 不再去读注册表看地址对不对，直接相信启动时的 setProxyRegistry 已经扫平了障碍
    if wantProxy {
        tunErrorCounter = 0
        return StateProxy // 蓝色：配置说开了，我们就认为它开了
    }

    // 6. 兜底逻辑：啥也没开
    // 只有当 API 通着，但 TUN 和 Proxy 都是 false 时，才会走到这里
    tunErrorCounter = 0
    return StateDefault // 灰色：待命状态
}

func reloadConfigFile() {
    // 使用全局 baseDir 确保路径绝对可靠
    configPath := filepath.Join(baseDir, "config.yaml")
    
    // 检查文件是否存在，避免给内核发送不存在的路径导致 API 报错
    if _, err := os.Stat(configPath); os.IsNotExist(err) {
        return 
    }

    // Windows 路径转义：C:\Folder\config.yaml -> C:\\Folder\\config.yaml
    escapedPath := strings.ReplaceAll(configPath, "\\", "\\\\")
    jsonPayload := fmt.Sprintf(`{"path": "%s"}`, escapedPath)

    // 使用 PUT 接口， force=true 确保旧规则连接被强制切断
    url := API_URL + "/configs?force=true"
    req, err := http.NewRequest("PUT", url, strings.NewReader(jsonPayload))
    if err != nil {
        return
    }
    
    // 设置请求头
    req.Header.Set("Content-Type", "application/json")
    
    resp, err := httpClient.Do(req)
    if err != nil {
        return
    }
    defer resp.Body.Close()

    // 204 说明重载指令已接受
    if resp.StatusCode == http.StatusNoContent {
        // 成功后重新同步一次开关状态（如模式、TUN状态等）
        go syncConfigToKernel()
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
        if strings.EqualFold(pname, name) && pe.ProcessID != currPid {
            return true
        }
        // 关键：在循环体内重置 Size (某些版本的 Windows 下是必要的)
        pe.Size = uint32(unsafe.Sizeof(pe))
        if err := windows.Process32Next(h, &pe); err != nil { break }
    }
    return false
}

func onReady() {
    // 1. 暴力初始化：不管现状，启动即校准
    loadIniConfigAll()
    setProxyRegistry(getIniConfig("system_proxy_enabled") == "true")

    // 2. 初始图标状态
    updateIconByState(StateStop)

    // 3. 创建 UI 菜单
    mWeb := systray.AddMenuItem("进入控制面板", "打开 Web UI 界面")
    systray.AddSeparator()

    // 模式切换菜单：利用一个小 Map 简化排他逻辑
    curMode := getIniConfig("mode")
    modeMenus := make(map[string]*systray.MenuItem)
    modeMenus["rule"] = systray.AddMenuItemCheckbox("规则模式", "", curMode == "rule" || curMode == "")
    modeMenus["global"] = systray.AddMenuItemCheckbox("全局模式", "", curMode == "global")
    modeMenus["direct"] = systray.AddMenuItemCheckbox("直连模式", "", curMode == "direct")
    systray.AddSeparator()

    // 功能开关菜单
    mTun := systray.AddMenuItemCheckbox("TUN 模式", "虚拟网卡接接管", getIniConfig("tun_enabled") == "true")
    mProxy := systray.AddMenuItemCheckbox("系统代理", "修改注册表代理", getIniConfig("system_proxy_enabled") == "true")
    systray.AddSeparator()

    // 管理菜单
    mReloadYAML := systray.AddMenuItem("更新 YAML 配置", "热重载 config.yaml")
    mAuto := systray.AddMenuItemCheckbox("开机自启", "", getIniConfig("auto_start") == "true")
    mDir := systray.AddMenuItem("浏览本地文件", "打开程序所在目录")
    mRestart := systray.AddMenuItem("重启内核", "强制杀死并重启内核")
    systray.AddSeparator()
    
    mExit := systray.AddMenuItem("退出程序", "")

    // 4. 暴力事件循环
    for {
        select {
        case <-mWeb.ClickedCh:
            windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)

        // 模式切换的暴力排他：点哪个，哪个就 Check，其他的全部 Uncheck
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
            setTunMode(next) // 内部已包含 saveIniConfig 和 API 推送
            if next { mTun.Check() } else { mTun.Uncheck() }

        case <-mProxy.ClickedCh:
            next := !mProxy.Checked()
            // 暴力：先改真理，再改注册表
            saveIniConfig("system_proxy_enabled", fmt.Sprint(next))
            setProxyRegistry(next)
            if next { mProxy.Check() } else { mProxy.Uncheck() }

        case <-mReloadYAML.ClickedCh:
            go reloadConfigFile()

        case <-mAuto.ClickedCh:
            next := !mAuto.Checked()
            toggleAutoStart(next)
            if next { mAuto.Check() } else { mAuto.Uncheck() }

        case <-mDir.ClickedCh:
            // 直接打开 baseDir，省去不必要的路径拼接
            windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)

        case <-mRestart.ClickedCh:
            go func() {
                // 1. 无差别屠杀旧内核
                killCmd := exec.Command("cmd", "/c", "taskkill /F /T /IM mihomo.exe")
                killCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
                _ = killCmd.Run()

                // 2. 关键：重置同步锁，让 monitor 拉起后能强制推送配置
                configMu.Lock()
                onceSync = sync.Once{}
                configMu.Unlock()
            }()

        case <-mExit.ClickedCh:
            // 标记退出，触发 onExit 里的清理流程
            isReallyExiting = true
            systray.Quit()
            return
        }
    }
}
func onExit() {
    if isReallyExiting {
        // 1. 恢复系统代理 (优先执行，因为这是 I/O 操作)
        setProxyRegistry(false)
        
        // 给注册表写入一点点缓冲时间
        time.Sleep(50 * time.Millisecond)

        // 2. 利用 JobObject 释放所有子进程
        if hJob != 0 {
            windows.CloseHandle(hJob) // 这一步会瞬间杀死内核
            hJob = 0
        }

        // 3. 释放 Mutex
        if hMutex != 0 {
            windows.CloseHandle(hMutex)
            hMutex = 0
        }
    }
}

// --- 系统操作 ---

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
	if enable { key.SetStringValue(APP_NAME, exePath) } else { key.DeleteValue(APP_NAME) }
	key.Close()
}

func updateIconByState(state int) {
	files := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
	if state < 0 || state >= len(files) { return }
	b, err := iconFs.ReadFile("icons/" + files[state])
	if err == nil { systray.SetIcon(b) }
}

// --- 程序入口 ---


func main() {
    // 1. 单例检查：确保全局只有一个实例
    mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
    h, err := windows.CreateMutex(nil, false, mName)
    if err != nil && err.(windows.Errno) == windows.ERROR_ALREADY_EXISTS {
        if h != 0 { windows.CloseHandle(h) }
        return // 发现多开，直接静默退出
    }
    hMutex = h 

    // 2. 权限检查与提权
    if !isAdmin() {
        // 提权前必须关闭当前 Mutex 句柄，否则管理员权限的新进程会检测到它而无法启动
        if hMutex != 0 {
            windows.CloseHandle(hMutex)
            hMutex = 0
        }
        runAsAdmin()
        os.Exit(0)
    }

    // 3. 初始化工作环境
    p, _ := os.Executable()
    baseDir = filepath.Dir(p)
    os.Chdir(baseDir)
    
    // 初始化 JobObject (用于联动退出)
    initJobObject()

    // 4. 启动后台协程
    go monitorKernelDaemon()
    go monitorIconState()

    // 5. 运行托盘（注意：onReady 不要返回）
    systray.Run(onReady, onExit)
}
