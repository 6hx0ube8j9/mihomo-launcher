package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
	
	"github.com/energye/systray"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed icons/*.ico
var iconFs embed.FS

const (
	APP_MUTEX    = "Global\\MihomoLauncher_Unique_Mutex"
	CONFIG_FILE  = "mihomo-launcher.ini"
	REG_RUN      = `Software\Microsoft\Windows\CurrentVersion\Run`
	REG_PROXY    = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	APP_NAME     = "MihomoLauncher"
	StateStop    = 0
	StateError   = 1
	StateTun     = 2
	StateProxy   = 3
	StateDefault = 4
)

var (
	hJob                 windows.Handle
	hMutex               windows.Handle
	httpClient           = &http.Client{Timeout: 1 * time.Second}
	exePath, _           = os.Executable()
	baseDir              = filepath.Dir(exePath)
	isSystemInitializing = true
	isSyncing            int32
	isReallyExiting      bool
	hasFirstSynced       int32
	exitOnce             sync.Once
	configMu             sync.RWMutex
	configData           = make(map[string]string)
	lastState            = -1
	tunErrorCounter      = 0
	mTun                 *systray.MenuItem
	isKernelActive       int32
    user32           = windows.NewLazySystemDLL("user32.dll")
    kernel32         = windows.NewLazySystemDLL("kernel32.dll")
    // 动态库载入
    u32 = windows.NewLazySystemDLL("user32.dll")
    k32 = windows.NewLazySystemDLL("kernel32.dll")

    // 1. 窗口寻找与属性 (用于在桌面上“揪”出浏览器)
    procEnumWindows      = u32.NewProc("EnumWindows")
    procGetClassName     = u32.NewProc("GetClassNameW")
    procIsWindowVisible  = u32.NewProc("IsWindowVisible")
    procGetWindowThread  = u32.NewProc("GetWindowThreadProcessId")
    
    // 2. 焦点与状态夺取 (置顶的核心手术刀)
    procSetWindowPos     = u32.NewProc("SetWindowPos")
    procShowWindow        = u32.NewProc("ShowWindow")
    procSetForeground    = u32.NewProc("SetForegroundWindow")
    procBringToTop       = u32.NewProc("BringWindowToTop")
    procGetForeground    = u32.NewProc("GetForegroundWindow")
    procAttachThread     = u32.NewProc("AttachThreadInput")
    procGetCurrentThread = k32.NewProc("GetCurrentThreadId")
    
    // 3. 辅助黑魔法 (刷新焦点)
    procKeybdEvent       = u32.NewProc("keybd_event")

    // 4. 状态控制 (防止疯狂连点)
    isFocusing           int32 
)

const (
    // 物理层级常量
    HWND_TOPMOST   = ^uintptr(0) // -1: 强制置顶
    HWND_NOTOPMOST = ^uintptr(1) // -2: 解除置顶

    // 状态常量
    SW_RESTORE     = 9           // 恢复窗口（最小化时有效）
    
    // 动作组合标志位: NOSIZE(1) | NOMOVE(2) | SHOWWINDOW(64) = 0x0043
    SWP_SILKY      = 0x0043 
	debugPort = "52719"
)

func main() {
	var err error
	exePath, err = os.Executable()
	if err != nil {
		return
	}
	baseDir = filepath.Dir(exePath)
	_ = os.Chdir(baseDir)

	mName, _ := windows.UTF16PtrFromString(APP_MUTEX)
	h, err := windows.CreateMutex(nil, false, mName)
	if err != nil || windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		if h != 0 {
			windows.CloseHandle(h)
		}
		return
	}
	hMutex = h

	isAutostart := false
	for _, arg := range os.Args {
		if arg == "---autostart" {
			isAutostart = true
			break
		}
	}

	if !isAdmin() && !isAutostart {
		runAsAdmin()
		return
	}

	ensureDefaultConfig()
	setProxyRegistry(getIniConfig("system_proxy_enabled") == "true")

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		systray.Quit()
	}()

	KillProcessByName("mihomo.exe")
	time.Sleep(200 * time.Millisecond)

	initJobObject()
	sniffAndSolidifyConfig()

	go func() {
		time.Sleep(1 * time.Second)
		go monitorKernelDaemon()
		go monitorIconState()
		go watchTunState()
	}()

	systray.Run(onReady, onExit)
	onExit()
}
func focusWindowSilky(targetHwnd uintptr) {
    // 1. 原子锁控制，防止短时间内多次触发导致置顶冲突
    if !atomic.CompareAndSwapInt32(&isFocusing, 0, 1) {
        return
    }
    defer atomic.StoreInt32(&isFocusing, 0)

    // 获取当前、前台以及目标窗口的线程 ID
    currT, _, _ := procGetCurrentThread.Call()
    foreH, _, _ := procGetForeground.Call()
    foreT, _, _ := procGetWindowThread.Call(foreH, 0)
    targT, _, _ := procGetWindowThread.Call(targetHwnd, 0)

    // 2. 线程关联（黑魔法）：让当前进程拥有前台权限
    if foreT != currT {
        procAttachThread.Call(foreT, currT, 1)
    }
    procAttachThread.Call(currT, targT, 1)

    // 3. 执行窗口唤醒组合拳
    procShowWindow.Call(targetHwnd, SW_RESTORE)
    procSetForeground.Call(targetHwnd)
    procBringToTop.Call(targetHwnd)
    // 物理置顶：设置为 HWND_TOPMOST
    procSetWindowPos.Call(targetHwnd, HWND_TOPMOST, 0, 0, 0, 0, SWP_SILKY)

    // 4. 解除线程关联
    procAttachThread.Call(currT, targT, 0)
    if foreT != currT {
        procAttachThread.Call(foreT, currT, 0)
    }

    // 5. 模拟 Alt 键：强制 Windows 刷新输入焦点到目标窗口
    procKeybdEvent.Call(0x12, 0, 0, 0) // Alt down
    procKeybdEvent.Call(0x12, 0, 2, 0) // Alt up

    // 6. 延时解除物理置顶，防止窗口“流氓”，允许用户切走
    time.AfterFunc(400*time.Millisecond, func() {
        procSetWindowPos.Call(targetHwnd, HWND_NOTOPMOST, 0, 0, 0, 0, SWP_SILKY)
    })
}

func launchWebUI() {

    apiAddr := getIniConfig("external-controller")
    secret := getIniConfig("secret")
    proxyAddr := getIniConfig("proxy_address")
    
    // URL 构造
    baseUI := strings.TrimRight(apiAddr, "/")
    if !strings.HasPrefix(baseUI, "http") {
        baseUI = "http://" + baseUI
    }
    host, port, _ := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(baseUI, "http://"), "https://"))
    if port == "" { host, port = "127.0.0.1", "9090" }
    finalURL := fmt.Sprintf("%s/ui/?hostname=%s&port=%s&secret=%s#/proxies", baseUI, host, port, secret)

    // 1. 探测阶段：通过 CDP 端口寻找是否已经有打开的 UI
    client := &http.Client{Timeout: 300 * time.Millisecond}
    if resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/json", debugPort)); err == nil {
        defer resp.Body.Close()
        var targets []map[string]interface{}
        if err := json.NewDecoder(resp.Body).Decode(&targets); err == nil {
            for _, t := range targets {
                pURL, _ := t["url"].(string)
                // 识别特征：URL 中包含 /ui/ 或 setup
                if strings.Contains(pURL, "/ui/") || strings.Contains(pURL, "setup") {
                    id, _ := t["id"].(string)
                    // 激活该标签页（前端跳转）
                    client.Get(fmt.Sprintf("http://127.0.0.1:%s/json/activate/%s", debugPort, id))
                    
                    // 异步置顶窗口（后台寻找 HWND）
                    go func() {
                        time.Sleep(100 * time.Millisecond)
                        var targetHwnd uintptr
                        procEnumWindows.Call(windows.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
                            var buf [256]uint16
                            procGetClassName.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), 256)
                            if windows.UTF16ToString(buf[:]) == "Chrome_WidgetWin_1" {
                                if vis, _, _ := procIsWindowVisible.Call(hwnd); vis != 0 {
                                    targetHwnd = hwnd
                                    return 0 // 找到即停止
                                }
                            }
                            return 1
                        }), 0)
                        if targetHwnd != 0 {
                            focusWindowSilky(targetHwnd)
                        }
                    }()
                    return // 激活成功，直接退出函数
                }
            }
        }
    }

    // 2. 查找可用浏览器阶段
    var browserPath string
    potentialPaths := []string{
        filepath.Join(os.Getenv("ProgramFiles(x86)"), `Microsoft\Edge\Application\msedge.exe`),
        filepath.Join(os.Getenv("ProgramFiles"), `Microsoft\Edge\Application\msedge.exe`),
        filepath.Join(os.Getenv("ProgramFiles(x86)"), `Google\Chrome\Application\chrome.exe`),
        filepath.Join(os.Getenv("ProgramFiles"), `Google\Chrome\Application\chrome.exe`),
        filepath.Join(os.Getenv("LocalAppData"), `Google\Chrome\Application\chrome.exe`),
    }

    for _, p := range potentialPaths {
        if _, err := os.Stat(p); err == nil {
            browserPath = p
            break
        }
    }

    // 3. 启动执行阶段
    if browserPath != "" {
        // 使用独立的用户数据目录，防止污染用户日常使用的浏览器
        userDataDir := filepath.Join(baseDir, "WebCache") // 建议通用命名
        _ = os.MkdirAll(userDataDir, 0755)

        args := []string{
            "--app=" + finalURL,
            "--remote-debugging-port=" + debugPort,
            "--user-data-dir=" + userDataDir,
            "--window-size=1280,768",
            "--proxy-server=" + proxyAddr,
            "--no-first-run",
        }
        cmd := exec.Command(browserPath, args...)
		if err := cmd.Start(); err == nil {
		    if hJob != 0 {
			    hp, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
			    if err == nil {
				    _ = windows.AssignProcessToJobObject(hJob, hp)
					_ = windows.CloseHandle(hp)
				}
			}
		}	
    } else {
        // 兜底：用系统默认浏览器直接打开
        _ = exec.Command("cmd", "/c", "start", "", finalURL).Start()
    }
}
func onReady() {
	saveIniConfig("startup_enabled", fmt.Sprint(checkAutoStartStatus()))
	ensureDefaultConfig()
	sniffAndSolidifyConfig()
	setProxyRegistry(getIniConfig("system_proxy_enabled") == "true")
	updateIconByState(StateStop)
    systray.SetOnClick(func(menu systray.IMenu) {
        go launchWebUI()
    })	

    mWeb := systray.AddMenuItem("进入 Web 面板", "")
    mWeb.Click(func() {
        go launchWebUI()
    })

    systray.AddSeparator()

    mProxy := systray.AddMenuItemCheckbox("系统代理", "", getIniConfig("system_proxy_enabled") == "true")
    mProxy.Click(func() {
        next := !mProxy.Checked()
        saveIniConfig("system_proxy_enabled", fmt.Sprint(next))
        setProxyRegistry(next)
        if next { mProxy.Check() } else { mProxy.Uncheck() }
    })

    mTun = systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", getIniConfig("tun_enabled") == "true")
    mTun.Click(func() {
        next := !mTun.Checked()
        if next { mTun.Check() } else { mTun.Uncheck() }
        go setTunMode(next)
    })

    systray.AddSeparator()

    mModeRoot := systray.AddMenuItem("模式切换", "")
    curMode := getIniConfig("mode")
    modeMenus := make(map[string]*systray.MenuItem)
    
    modeMenus["rule"] = mModeRoot.AddSubMenuItemCheckbox("规则模式", "", curMode == "rule")
    modeMenus["rule"].Click(func() {
        setMihomoMode("rule")
        modeMenus["rule"].Check(); modeMenus["global"].Uncheck(); modeMenus["direct"].Uncheck()
    })

    modeMenus["global"] = mModeRoot.AddSubMenuItemCheckbox("全局模式", "", curMode == "global")
    modeMenus["global"].Click(func() {
        setMihomoMode("global")
        modeMenus["rule"].Uncheck(); modeMenus["global"].Check(); modeMenus["direct"].Uncheck()
    })

    modeMenus["direct"] = mModeRoot.AddSubMenuItemCheckbox("直连模式", "", curMode == "direct")
    modeMenus["direct"].Click(func() {
        setMihomoMode("direct")
        modeMenus["rule"].Uncheck(); modeMenus["global"].Uncheck(); modeMenus["direct"].Check()
    })

    systray.AddSeparator()

    mDir := systray.AddMenuItem("打开目录", "")
    mDir.Click(func() {
        windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
    })

    mMoreRoot := systray.AddMenuItem("更多", "")
    mAuto := mMoreRoot.AddSubMenuItemCheckbox("开机自启动", "", checkAutoStartStatus())
    mAuto.Click(func() {
        next := !mAuto.Checked()
        toggleAutoStart(next)
        if next { mAuto.Check() } else { mAuto.Uncheck() }
    })

    mRestart := mMoreRoot.AddSubMenuItem("重启内核", "")
    mRestart.Click(func() {
        isSystemInitializing = true
        atomic.StoreInt32(&hasFirstSynced, 0)
        KillProcessByName("mihomo.exe")
    })

    mReload := mMoreRoot.AddSubMenuItem("重载配置文件", "")
    mReload.Click(func() {
        sniffAndSolidifyConfig()
        reloadConfigFile()
    })

    systray.AddSeparator()

    mExit := systray.AddMenuItem("关闭程序", "")
    mExit.Click(func() {
        isReallyExiting = true
        systray.Quit()
    })
}

func onExit() {
    exitOnce.Do(func() {
        atomic.StoreInt32(&isReallyExiting, 1)

        // 1. 尝试优雅关闭浏览器标签 (CDP)
        // 只要是 --app 模式启动的，这一步通常能生效
        client := &http.Client{Timeout: 200 * time.Millisecond}
        apiURL := fmt.Sprintf("http://127.0.0.1:%s/json", debugPort)
        if resp, err := client.Get(apiURL); err == nil {
            var targets []map[string]interface{}
            if json.NewDecoder(resp.Body).Decode(&targets) == nil {
                for _, t := range targets {
                    if id, ok := t["id"].(string); ok {
                        _, _ = client.Get(fmt.Sprintf("http://127.0.0.1:%s/json/close/%s", debugPort, id))
                    }
                }
            }
            resp.Body.Close()
        }

        // 2. 关键：先撤代理，再退程序
        setProxyRegistry(false)

        // 3. 退出托盘图标
        systray.Quit()

        // 4. 关闭 Job Object 句柄
        // 这一步会瞬间触发内核强杀还在运行的 mihomo.exe 和 --app 浏览器进程
        if hJob != 0 {
            windows.CloseHandle(hJob)
        }
        
        if hMutex != 0 {
            windows.CloseHandle(hMutex)
        }

        os.Exit(0)
    })
}

func monitorKernelDaemon() {
	target := filepath.Join(baseDir, "mihomo.exe")
	absBaseDir, _ := filepath.Abs(baseDir)
	for {
		if isReallyExiting {
			return
		}
		if !isProcessRunning("mihomo.exe") {
			isSystemInitializing = true
			atomic.StoreInt32(&hasFirstSynced, 0)
			atomic.StoreInt32(&isKernelActive, 0)
			KillProcessByName("mihomo.exe")
			time.Sleep(500 * time.Millisecond)
			cmd := exec.Command(target, "-d", ".")
			cmd.Dir = absBaseDir
			cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
			if err := cmd.Start(); err == nil {
				atomic.StoreInt32(&isKernelActive, 1)
				if hJob != 0 {
					hp, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
					if err == nil {
						err = windows.AssignProcessToJobObject(hJob, hp)
						if err != nil {
						}
						_ = windows.CloseHandle(hp)
					}
				}
				go func(c *exec.Cmd) {
					_ = c.Wait()
					atomic.StoreInt32(&isKernelActive, 0)
				}(cmd)
				time.Sleep(1500 * time.Millisecond)
				isSystemInitializing = false
			} else {
				isSystemInitializing = false
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

func watchTunState() {
	// 使用 Ticker 替代阻塞 API。每 3-5 秒检查一次网卡状态对性能几乎无影响
	// 但能保证程序退出的灵活性
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	var lastHasTun bool

	// 初始化状态，先查一次防止漏掉启动时的状态
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if isTunInterfaceMatch(i.Name) {
			lastHasTun = true
			break
		}
	}

	for {
		select {
		case <-ticker.C:
			// 1. 检查退出标志
			if isReallyExiting {
				return
			}

			// 2. 检查系统是否正在忙碌（初始化或正在同步中则跳过本次循环）
			if isSystemInitializing || atomic.LoadInt32(&isSyncing) == 1 {
				continue
			}

			// 3. 获取当前网卡列表，检查 TUN 是否存在
			currentHasTun := false
			currentIfaces, err := net.Interfaces()
			if err != nil {
				// 获取失败可能是系统繁忙，跳过本次等待下一次 ticker
				continue
			}

			for _, i := range currentIfaces {
				if isTunInterfaceMatch(i.Name) {
					currentHasTun = true
					break
				}
			}

			// 4. 只有当网卡状态发生“变化”时，才触发逻辑
			if currentHasTun != lastHasTun {
				// 记录日志或更新状态
				lastHasTun = currentHasTun

				// 只有内核处于活动状态时，才去同步 UI 和配置
				if atomic.LoadInt32(&isKernelActive) == 1 {
					// 更新托盘菜单的勾选状态
					if mTun != nil {
						if currentHasTun {
							mTun.Check()
						} else {
							mTun.Uncheck()
						}
					}

					// 自动持久化当前状态到 INI 配置
					// 这样如果用户在外部手动关了 TUN，Launcher 也能记住
					saveIniConfig("tun_enabled", fmt.Sprint(currentHasTun))
					
					// 如果你希望在网卡丢失时通知内核，可以在这里调用 API
					// _, _ = doAPIRequest("PATCH", "/configs", map[string]interface{}{"tun": map[string]bool{"enable": currentHasTun}})
				}
			}
			
		}
	}
}
func syncConfigToKernel() {
    if !atomic.CompareAndSwapInt32(&isSyncing, 0, 1) {
        return
    }
    defer atomic.StoreInt32(&isSyncing, 0)

    isSystemInitializing = true
    // 保护：如果函数因为意外卡死，10秒后强制解除初始化状态
    timer := time.AfterFunc(10*time.Second, func() { isSystemInitializing = false })
    defer timer.Stop()

    tunEnabled := getIniConfig("tun_enabled") == "true"
    payload := map[string]interface{}{
        "mode": getIniConfig("mode"),
        "tun":  map[string]bool{"enable": tunEnabled},
    }

    success := false
    for i := 0; i < 3; i++ {
        _, err := doAPIRequest("PATCH", "/configs", payload)
        if err == nil {
            success = true
            break // <--- 关键修改：成功了就别再试了
        }
        // 如果失败，等待一段时间重试
        time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
    }

    if success {
        if mTun != nil {
            if tunEnabled { mTun.Check() } else { mTun.Uncheck() }
        }
        // 同步成功后稍微稳一下状态
        time.Sleep(500 * time.Millisecond)
    }

    isSystemInitializing = false
}

func doAPIRequest(method, path string, payload interface{}) ([]byte, error) {
	// 1. 获取并格式化 API 地址
	apiAddr := strings.TrimSuffix(getIniConfig("external-controller"), "/")
	if apiAddr == "" {
		return nil, fmt.Errorf("api address is empty")
	}
	url := apiAddr + "/" + strings.TrimPrefix(path, "/")

	// 2. 处理请求 Body
	var bodyReader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal payload failed: %v", err)
		}
		bodyReader = bytes.NewBuffer(b)
	}

	// 3. 创建请求
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, err
	}

	// 4. 设置 Header
	req.Header.Set("Content-Type", "application/json")
	if secret := getIniConfig("secret"); secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}

	// 5. 执行请求
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	// 确保 Body 最终被关闭，防止连接泄漏
	defer resp.Body.Close()

	// 6. 性能优化：心跳检测逻辑
	// 如果是 GET 请求且 path 为空（说明来自 checkSystemState 的存活检查）
	// 我们只关心状态码，不关心内容，直接丢弃 Body 以节省内存分配
	if method == "GET" && (path == "" || path == "/") {
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("API Heartbeat Error: %d", resp.StatusCode)
		}
		return nil, nil
	}

	// 7. 读取响应内容
	// 对于配置更新、状态获取等请求，我们需要读取完整的响应体
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body failed: %v", err)
	}

	// 8. 错误状态码处理
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, fmt.Errorf("API Error: %d, Response: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

func ensureDefaultConfig() {
	configMu.Lock()
	b, _ := os.ReadFile(filepath.Join(baseDir, CONFIG_FILE))
	lines := strings.Split(string(b), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") { continue }
		if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
			configData[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	defaults := [][]string{
		{"mode", "rule"}, {"tun_enabled", "false"}, {"system_proxy_enabled", "false"},
		{"startup_enabled", "false"}, {"proxy_address", "127.0.0.1:7890"},
		{"tun_device_name", "Mihomo"}, {"external-controller", "http://127.0.0.1:9090"}, {"secret", ""},
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
	// 读取同目录下的 config.yaml
	data, err := os.ReadFile(filepath.Join(baseDir, "config.yaml"))
	if err != nil {
		return
	}

	lines := strings.Split(string(data), "\n")
	inTunSection := false
	foundMixed := false // 优先级锁：确保 mixed-port 不会被后续的 port 覆盖

	for _, line := range lines {
		// 去除首尾空格，跳过空行和注释
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// --- 1. 端口嗅探模块 (带优先级逻辑) ---
		// 优先级：mixed-port > port (HTTP)
		if strings.HasPrefix(trimmed, "mixed-port:") {
			if parts := strings.SplitN(trimmed, ":", 2); len(parts) == 2 {
				port := strings.Trim(parts[1], " \"'")
				if port != "" {
					saveIniConfig("proxy_address", "127.0.0.1:"+port)
					foundMixed = true // 锁定，不再允许 port: 修改 proxy_address
				}
			}
		} else if !foundMixed && strings.HasPrefix(trimmed, "port:") {
			// 只有在没找到 mixed-port 时才记录普通端口
			if parts := strings.SplitN(trimmed, ":", 2); len(parts) == 2 {
				port := strings.Trim(parts[1], " \"'")
				if port != "" {
					saveIniConfig("proxy_address", "127.0.0.1:"+port)
				}
			}
		}

		// --- 2. TUN 模块 (嵌套逻辑) ---
		if strings.HasPrefix(trimmed, "tun:") {
			inTunSection = true
			continue
		}
		// 如果碰到不带缩进的新行，说明退出了 tun 区域
		if inTunSection && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			inTunSection = false
		}
		// 在 tun 区域内寻找设备名
		if inTunSection && strings.Contains(trimmed, "device:") {
			if parts := strings.SplitN(trimmed, ":", 2); len(parts) == 2 {
				devName := strings.Trim(parts[1], " \"'")
				if devName != "" {
					saveIniConfig("tun_device_name", devName)
				}
			}
		}

		// --- 3. 基础信息嗅探 (用于 Web 面板访问) ---
		// 提取 API 控制地址
		if strings.HasPrefix(trimmed, "external-controller:") {
			addr := strings.Trim(strings.TrimPrefix(trimmed, "external-controller:"), " \"'")
			// 如果是 ":9090" 这种格式，补全 IP
			if strings.HasPrefix(addr, ":") {
				addr = "127.0.0.1" + addr
			}
			if addr != "" {
				// 统一补全协议头
				if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
					addr = "http://" + addr
				}
				saveIniConfig("external-controller", addr)
			}
		}

		// 提取 API 密钥 (Secret)
		if strings.HasPrefix(trimmed, "secret:") {
			val := strings.Trim(strings.TrimPrefix(trimmed, "secret:"), " \"'")
			saveIniConfig("secret", val)
		}
	}
}

func setMihomoMode(mode string) {
	saveIniConfig("mode", mode)
	_, _ = doAPIRequest("PATCH", "/configs", map[string]string{"mode": mode})
}

func setTunMode(enable bool) {
	isSystemInitializing = true
	saveIniConfig("tun_enabled", fmt.Sprint(enable))
	_, _ = doAPIRequest("PATCH", "/configs", map[string]interface{}{"tun": map[string]bool{"enable": enable}})
	time.Sleep(3 * time.Second)
	isSystemInitializing = false
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
		_ = key.SetStringValue("ProxyServer", getIniConfig("proxy_address"))
	} else {
		_ = key.SetDWordValue("ProxyEnable", 0)
	}
}

func toggleAutoStart(enable bool) {
	const taskName = "MihomoLauncherTask"
	if key, err := registry.OpenKey(registry.CURRENT_USER, REG_RUN, registry.SET_VALUE); err == nil {
		_ = key.DeleteValue(APP_NAME)
		key.Close()
	}
	success := false
	if enable {
		cleanExe := strings.ReplaceAll(exePath, "'", "''")
		cleanDir := strings.ReplaceAll(baseDir, "'", "''")
		psScript := fmt.Sprintf(
			"$trigger = New-ScheduledTaskTrigger -AtLogOn; $trigger.Delay = 'PT8S'; "+
				"$action = New-ScheduledTaskAction -Execute '%s' -Argument '---autostart' -WorkingDirectory '%s'; "+
				"$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero); "+
				"Register-ScheduledTask -TaskName '%s' -Trigger $trigger -Action $action -Settings $settings -User $env:USERNAME -RunLevel Highest -Force",
			cleanExe, cleanDir, taskName,
		)
		cmd := exec.Command("powershell", "-Command", psScript)
		cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		if err := cmd.Run(); err == nil { success = true }
	} else {
		cmd := exec.Command("schtasks", "/Delete", "/TN", taskName, "/F")
		cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		if err := cmd.Run(); err == nil || !checkAutoStartStatus() { success = true }
	}
	if success { saveIniConfig("startup_enabled", fmt.Sprint(enable)) }
}

func reloadConfigFile() {
    isSystemInitializing = true
    _, err := doAPIRequest("PUT", "/configs?force=false", map[string]string{"path": filepath.Join(baseDir, "config.yaml")})
    if err != nil {
        isSystemInitializing = false
        return
    }
    go syncConfigToKernel()
}

func checkSystemState() int {
	// 1. 尝试连接内核 API
	// 传入空 path 会触发 doAPIRequest 内部的 io.Discard 优化，不产生内存开销
	_, err := doAPIRequest("GET", "", nil) 
	if err != nil {
		tunErrorCounter = 0
		return StateStop // 连不上 API，内核可能在重启或已崩溃
	}

	// 2. API 连接成功，确保初始化标志被重置
	if isSystemInitializing {
		isSystemInitializing = false
	}

	// 3. 核心修复：首次启动/重启内核后的第一次配置同步
	// 使用 CompareAndSwap 保证全局只触发一次同步，且不会因为 onceSync 导致并发隐患
	if atomic.CompareAndSwapInt32(&hasFirstSynced, 0, 1) {
		go syncConfigToKernel()
	}

	// 4. 检查网卡/代理状态
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
			return StateTun // 正常：TUN 已开启且网卡存在
		}

		// 容错缓冲：防止内核启动 TUN 时网卡创建太慢导致图标闪烁
		tunErrorCounter++
		if tunErrorCounter > 8 { 
			return StateError // 超过 8 秒还没看到网卡，确实出错了
		}
		return StateStop // 缓冲中
	}

	// 5. 检查系统代理状态
	if getIniConfig("system_proxy_enabled") == "true" {
		return StateProxy
	}

	return StateDefault
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
	_ = windows.ShellExecute(0, verb, exe, nil, cwd, windows.SW_SHOWNORMAL)
}

func isProcessRunning(name string) bool {
	h, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil { return false }
	defer windows.CloseHandle(h)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(h, &pe); err != nil { return false }
	for {
		if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), name) {
			if pe.ProcessID != uint32(os.Getpid()) { return true }
		}
		if err := windows.Process32Next(h, &pe); err != nil { break }
	}
	return false
}

func KillProcessByName(name string) {
    snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
    if err != nil { return }
    defer windows.CloseHandle(snapshot)
    
    var pe windows.ProcessEntry32
    pe.Size = uint32(unsafe.Sizeof(pe))
    
    if err := windows.Process32First(snapshot, &pe); err != nil { return }
    
    for {
        if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), name) {
            pid := pe.ProcessID
            if pid != uint32(os.Getpid()) {
                h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_TERMINATE, false, pid)
                if err == nil {
                    // 检查路径（可选）：确保只杀掉本程序目录下的内核
                    // path, _ := getProcessPath(h) 
                    // if strings.Contains(path, baseDir) { ... }
                    
                    _ = windows.TerminateProcess(h, 9)
                    windows.CloseHandle(h)
                }
            }
        }
        if err := windows.Process32Next(snapshot, &pe); err != nil { break }
    }
}

func checkAutoStartStatus() bool {
	cmd := exec.Command("schtasks", "/Query", "/TN", "MihomoLauncherTask")
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	return cmd.Run() == nil
}

func updateIconByState(state int) {
	files := []string{"stop.ico", "error.ico", "tun.ico", "proxy.ico", "default.ico"}
	if state >= 0 && state < len(files) {
		if b, err := iconFs.ReadFile("icons/" + files[state]); err == nil {
			systray.SetIcon(b)
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
    // 1. 变化检测：如果不动，则不写
    if old, ok := configData[key]; ok && old == val && key != "" {
        configMu.Unlock()
        return
    }
    if key != "" {
        configData[key] = val
    }

    // 2. 准备数据（在锁内快速完成或拷贝）
    keys := []string{"mode", "tun_enabled", "system_proxy_enabled", "startup_enabled", "proxy_address", "tun_device_name", "external-controller", "secret"}
    var buf bytes.Buffer
    for _, k := range keys {
        if v, ok := configData[k]; ok {
            buf.WriteString(k + " = " + v + "\n") // 使用字符串拼接比 Sprintf 更快
        }
    }
    configMu.Unlock() // 尽早释放锁，不要带着锁去写磁盘

    // 3. 磁盘 IO（锁外执行）
    _ = os.WriteFile(filepath.Join(baseDir, CONFIG_FILE), buf.Bytes(), 0644)
}

func isTunInterfaceMatch(ifaceName string) bool {
	name := strings.ToLower(ifaceName)
	target := strings.ToLower(getIniConfig("tun_device_name"))
	if target != "" && strings.Contains(name, target) { return true }
	for _, kw := range []string{"mihomo", "meta", "clash", "sing-box", "wintun"} {
		if strings.Contains(name, kw) { return true }
	}
	return false
}

func initJobObject() {
    h, err := windows.CreateJobObject(nil, nil)
    if err != nil {
        return
    }
    
    // 设置：当 Job Object 句柄关闭时，自动杀掉所有关联进程
    info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
        BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
            LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
        },
    }
    
    _, err = windows.SetInformationJobObject(
        h,
        windows.JobObjectExtendedLimitInformation,
        uintptr(unsafe.Pointer(&info)),
        uint32(unsafe.Sizeof(info)),
    )
    if err != nil {
        windows.CloseHandle(h)
        return
    }
    hJob = h
}
