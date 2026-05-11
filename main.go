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

	// --- 状态控制 (使用 int32 确保 atomic 安全) ---
	isSystemInitializing int32 = 1
	isSyncing            int32
	isReallyExiting      int32
	hasFirstSynced       int32
	isKernelActive       int32
	isFocusing           int32
	manualUpdateTrigger  int32

	// --- 并发同步 ---
	exitOnce   sync.Once
	configMu   sync.RWMutex
	configData = make(map[string]string)

	// --- 逻辑跟踪 ---
	lastState       = -1
	tunErrorCounter = 0
	mTun            *systray.MenuItem

	// --- 动态库载入 ---
	u32 = windows.NewLazySystemDLL("user32.dll")
	k32 = windows.NewLazySystemDLL("kernel32.dll")

	procEnumWindows      = u32.NewProc("EnumWindows")
	procGetClassName     = u32.NewProc("GetClassNameW")
	procIsWindowVisible  = u32.NewProc("IsWindowVisible")
	procGetWindowThread  = u32.NewProc("GetWindowThreadProcessId")
	procSetWindowPos     = u32.NewProc("SetWindowPos")
	procShowWindow       = u32.NewProc("ShowWindow")
	procSetForeground    = u32.NewProc("SetForegroundWindow")
	procBringToTop       = u32.NewProc("BringWindowToTop")
	procGetForeground    = u32.NewProc("GetForegroundWindow")
	procAttachThread     = u32.NewProc("AttachThreadInput")
	procGetCurrentThread = k32.NewProc("GetCurrentThreadId")
	procKeybdEvent       = u32.NewProc("keybd_event")
)

const (
	HWND_TOPMOST   = ^uintptr(0)
	HWND_NOTOPMOST = ^uintptr(1)
	SW_RESTORE     = 9
	SWP_SILKY      = 0x0043
	debugPort      = "52719"
)

func checkSystemState() int {
    // 1. 获取物理网卡事实
    hasTunOnSystem := false
    ifaces, _ := net.Interfaces()
    for _, i := range ifaces {
        if isTunInterfaceMatch(i.Name) {
            hasTunOnSystem = true
            break
        }
    }

    // 2. 尝试连接内核 API 获取实时配置
    resp, err := doAPIRequest("GET", "/configs", nil)
    if err != nil { 
        return StateStop // API 连不上，说明内核正在重启或挂了
    }

    // --- 【修正语法错误点】 ---
    respStr := string(resp)
    if !strings.Contains(respStr, `"port"`) { // 连 port 都没有，说明内核没准备好
        return StateStop
    }

    // --- 【核心增强逻辑】 ---
    
    // 判定当前是否允许校准 (非初始化中、非手动同步中)
    isBusy := atomic.LoadInt32(&isSystemInitializing) == 1 || atomic.LoadInt32(&isSyncing) == 1
    
    if !isBusy {
        // 解析内核真实状态
        kernelTunEnabled := strings.Contains(respStr, `"tun":`) && strings.Contains(respStr, `"enable":true`)
        localTunConfig := (getIniConfig("tun_enabled") == "true")

        // 场景：本地以为开着，但 API 说关了
        if !kernelTunEnabled && localTunConfig {
            /* 
               双重校验保险：
               只有当 (API说关了) 且 (物理网卡也真的消失了) 时，才确信是 Web 端的操作。
            */
            if !hasTunOnSystem {
                // 确认是人为在 Web 关闭
                if len(resp) > 100 { 
                    saveIniConfig("tun_enabled", "false")
                    if mTun != nil { mTun.Uncheck() }
                    localTunConfig = false // 更新局部变量供后续判断
                }
            } else {
                // 关键点：API说关了但网卡还在，判定为“重载中”或“数据不同步”
                // 返回 StateStop 保护 ini 不被误改
                return StateStop 
            }
        }
        
        // 反向校准：如果 Web 开了，本地没开
        if kernelTunEnabled && !localTunConfig {
            saveIniConfig("tun_enabled", "true")
            if mTun != nil { mTun.Check() }
            localTunConfig = true
        }
    }

    // 3. 最终状态判定 (基于校准后的 localTunConfig)
    // 必须重新获取一遍最新的配置值，确保返回的图标状态是正确的
    currentTunPlan := (getIniConfig("tun_enabled") == "true")
    if currentTunPlan {
        if hasTunOnSystem {
            return StateTun // 只有配置要开且网卡真在，才亮蓝色
        }
        return StateStop // 配置要开但网卡还没出来，变灰
    }

    if getIniConfig("system_proxy_enabled") == "true" {
        return StateProxy
    }

    return StateDefault
}

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
        atomic.StoreInt32(&isSystemInitializing, 1)
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
        atomic.StoreInt32(&isReallyExiting, 1)
        systray.Quit()
    })
}

func onExit() {
    exitOnce.Do(func() {
        atomic.StoreInt32(&isReallyExiting, 1)

        // 1. 【高级清理】先通过 CDP 优雅关闭浏览器窗口
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

        // 2. 【系统恢复】恢复代理设置和图标
        setProxyRegistry(false)
        systray.Quit()

        // 3. 【关键停顿】给 100ms 让信号传递
        time.Sleep(100 * time.Millisecond)

        // 4. 【强制兜底】即便 CDP 失败了，这行也能确保子进程（浏览器/内核）彻底消失
        if hJob != 0 { windows.CloseHandle(hJob) }
        
        // 5. 【门锁释放】确保下次启动不会提示“程序已在运行”
        if hMutex != 0 { windows.CloseHandle(hMutex) }

        // 6. 【彻底退出】
        os.Exit(0)
    })
}

func monitorKernelDaemon() {
    target := filepath.Join(baseDir, "mihomo.exe")
    absBaseDir, _ := filepath.Abs(baseDir)

    for {
        // 1. 退出前置检查
        if atomic.LoadInt32(&isReallyExiting) == 1 {
            return
        }

        // 2. 检查进程是否存在
        if !isProcessRunning("mihomo.exe") {
            // --- 【严父模式：强制锁死禁区】 ---
            // 在杀进程和重载前，先立起“初始化”大旗
            // 这会导致 saveIniConfig 和 watchTunState 的写入逻辑失效
            atomic.StoreInt32(&isSystemInitializing, 1) 
            atomic.StoreInt32(&hasFirstSynced, 0)      // 重置同步状态
            atomic.StoreInt32(&isKernelActive, 0)      // 标记内核当前离线

            // 清理可能残留的进程
            KillProcessByName("mihomo.exe")
            time.Sleep(500 * time.Millisecond) // 给系统回收句柄留点时间

            // 3. 启动内核进程
            cmd := exec.Command(target, "-d", ".")
            cmd.Dir = absBaseDir
            // Windows 下隐藏 CMD 黑窗口
            cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}

            if err := cmd.Start(); err == nil {
                atomic.StoreInt32(&isKernelActive, 1)

                // 将进程绑定到 Job Object，确保托盘关掉时内核跟着陪葬
                if hJob != 0 {
                    hp, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
                    if err == nil {
                        _ = windows.AssignProcessToJobObject(hJob, hp)
                        _ = windows.CloseHandle(hp)
                    }
                }

                // 异步等待进程结束，进程一旦结束立刻标记离线
                go func(c *exec.Cmd) {
                    _ = c.Wait()
                    atomic.StoreInt32(&isKernelActive, 0)
                    // 如果进程异常退出，下个 2s 循环会自动触发重启并再次上锁
                }(cmd)

                // --- 【智能动态解锁逻辑】 ---
                // 不再硬睡 1.5s，而是启动一个状态探针
                go func() {
                    // 最多轮询 12 次 (约 6-7 秒)，防止内核启动失败导致永久锁死
                    for i := 0; i < 12; i++ {
                        time.Sleep(500 * time.Millisecond)
                        
                        // 探测 API 是否不仅连通，而且数据完整
                        resp, err := doAPIRequest("GET", "/configs", nil)
                        if err == nil && len(resp) > 200 && strings.Contains(string(resp), `"port"`) {
                            // API 已经能返回正常的业务数据，说明加载稳了
                            // 此时再额外缓冲 500ms，等 Windows 网卡创建彻底完成
                            time.Sleep(500 * time.Millisecond)
                            break
                        }
                    }
                    // 此时解开禁制，checkSystemState 和 watchTunState 恢复磁盘写入权限
                    atomic.StoreInt32(&isSystemInitializing, 0)
                }()

            } else {
                // 启动失败的容错：解开锁，让 checkSystemState 能报告 StateError(红色)
                atomic.StoreInt32(&isSystemInitializing, 0)
            }
        }

        // 4. 巡检频率
        time.Sleep(2 * time.Second)
    }
}

func monitorIconState() {
	var failCount int

	for {
		if atomic.LoadInt32(&isReallyExiting) == 1 { return }

		// 1. 物理层判定：进程不在，直接灰色
		if !isProcessRunning("mihomo.exe") {
			failCount = 0
			if lastState != StateStop {
				updateIconByState(StateStop)
				lastState = StateStop
			}
		} else {
			// --- 第一步：执行联动函数 ---
			// 它会更新 isSystemInitializing 的状态
			curr := checkSystemState()

			// --- 第二步：物理网卡真实状态捕捉 ---
			isTunMode := (getIniConfig("tun_enabled") == "true")
			hasTun := false
			ifaces, _ := net.Interfaces()
			for _, i := range ifaces {
				if isTunInterfaceMatch(i.Name) {
					hasTun = true
					break
				}
			}

			if isTunMode && !hasTun {
                actualState := checkSystemState()
				if actualState != StateTun && actualState != StateStop {
				    failCount = 0
					lastState = actualState
					updateIconByState(actualState)
					if mTun != nil { mTun.Uncheck() }
					time.Sleep(1 * time.Second)
					continue
				}	
				if atomic.LoadInt32(&isSystemInitializing) == 1 {
					// 启动中，网卡没出来很正常，交给 failCount 处理（保持灰色）
					goto UseFailCountLogic 
				} else {
					// 锁已经开了，说明是“运行中”。此时网卡没了，必然是重载或故障！
					failCount = 0 
					if lastState != StateError {
						updateIconByState(StateError)
						lastState = StateError
					}
					// 这种情况下我们不往下走了，直接等待下一秒看它会不会恢复绿
					time.Sleep(1 * time.Second)
					continue
				}
			}

		UseFailCountLogic:
			// 以下是原有的 5 秒容错逻辑
			if curr == StateStop {
				failCount++
				if failCount > 5 {
					if lastState != StateError {
						updateIconByState(StateError)
						lastState = StateError
					}
				}
			} else {
				failCount = 0
				if curr != lastState {
					updateIconByState(curr)
					lastState = curr
				}
			}
		}

		time.Sleep(1 * time.Second)
	}
}

func watchTunState() {
    // 3秒一次轮询（配合 ticker）
    ticker := time.NewTicker(3 * time.Second)
    defer ticker.Stop()

    var lastHasTun bool

    // 初始状态获取
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
            if atomic.LoadInt32(&isReallyExiting) == 1 {
                return
            }

            // 1. 获取物理事实
            currentHasTun := false
            currentIfaces, err := net.Interfaces()
            if err != nil {
                continue
            }
            for _, i := range currentIfaces {
                if isTunInterfaceMatch(i.Name) {
                    currentHasTun = true
                    break
                }
            }

            // 2. 只有当网卡状态发生“变化”时才进入逻辑
            if currentHasTun != lastHasTun {
                
                // --- 【严父模式：三重审查关卡】 ---
                
                // 第一关：系统忙碌审查 (初始化中、杀进程中、同步中均锁死)
                isBusy := atomic.LoadInt32(&isSystemInitializing) == 1 || atomic.LoadInt32(&isSyncing) == 1
                
                // 第二关：内核活跃审查 (内核必须在线)
                isKernelActive := atomic.LoadInt32(&isKernelActive) == 1
                
                // 第三关：程序退出审查
                isExiting := atomic.LoadInt32(&isReallyExiting) == 1

                if isKernelActive && !isExiting && !isBusy {
                    
                    // --- 【核心安全判定：防误报纠偏】 ---
                    
                    // 如果网卡消失了，我们需要判断是“真关了”还是“正在重启导致的瞬连”
                    if !currentHasTun {
                        // 询问内核 API：你的主观意图是要开启吗？
                        resp, err := doAPIRequest("GET", "/configs", nil)
                        // 如果 API 还能通，且内容显示 tun.enable = true
                        // 说明网卡消失只是暂时的（比如重载），绝对不能更新配置！
                        if err == nil && strings.Contains(string(resp), `"enable":true`) {
                            continue // 拒绝承认网卡消失的事实，跳过本次更新
                        }
                        
                        // 如果 API 报错（连不上），说明内核正在重启或崩溃
                        if err != nil {
                            continue // 重启期间，网卡消失是正常的，不准改 ini
                        }
                    }

                    // --- 【通过审查，执行同步】 ---
                    
                    lastHasTun = currentHasTun

                    // A. 标记已同步，防止 checkSystemState 再次触发反向覆盖
                    atomic.StoreInt32(&hasFirstSynced, 1)

                    // B. 持久化到磁盘 (因为进入了此逻辑，saveIniConfig 内部的 isSystemInitializing 校验也会通过)
                    saveIniConfig("tun_enabled", fmt.Sprint(currentHasTun))

                    // C. 立即联动 UI 状态
                    // 直接调用纠偏后的 checkSystemState，确保图标准确
                    newState := checkSystemState() 
                    updateIconByState(newState)
                    lastState = newState 

                    // D. 更新菜单勾选状态
                    if mTun != nil {
                        if currentHasTun {
                            mTun.Check()
                        } else {
                            mTun.Uncheck()
                        }
                    }
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

    atomic.StoreInt32(&isSystemInitializing, 1)
    // 保护：如果函数因为意外卡死，10秒后强制解除初始化状态
    timer := time.AfterFunc(10*time.Second, func() { atomic.StoreInt32(&isSystemInitializing, 0) })
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

    atomic.StoreInt32(&isSystemInitializing, 0)
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
	atomic.StoreInt32(&isSystemInitializing, 1)
	saveIniConfig("tun_enabled", fmt.Sprint(enable))
	_, _ = doAPIRequest("PATCH", "/configs", map[string]interface{}{"tun": map[string]bool{"enable": enable}})
	time.Sleep(3 * time.Second)
	atomic.StoreInt32(&isSystemInitializing, 0)
}

func setProxyRegistry(enable bool) {
	if atomic.LoadInt32(&isReallyExiting) == 0 {
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
    atomic.StoreInt32(&isSystemInitializing, 1)
    _, err := doAPIRequest("PUT", "/configs?force=false", map[string]string{"path": filepath.Join(baseDir, "config.yaml")})
    if err != nil {
        atomic.StoreInt32(&isSystemInitializing, 0)
        return
    }
    go syncConfigToKernel()
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
    if key == "tun_enabled" {
        if atomic.LoadInt32(&isSystemInitializing) == 1 {
            // 处于静默期，拒绝写入磁盘，保护用户原始意图
            return 
        }
    }
    // --- 【政审结束】 ---

    configMu.Lock()
    
    // 1. 只有当 key 不为空时才处理逻辑
    if key != "" {
        old, ok := configData[key]
        // 如果值没变，直接解锁退出
        if ok && old == val {
            configMu.Unlock()
            return
        }
        // 值变了，更新内存缓存
        configData[key] = val
    }

    // 2. 准备数据
    keys := []string{"mode", "tun_enabled", "system_proxy_enabled", "startup_enabled", "proxy_address", "tun_device_name", "external-controller", "secret"}
    var buf bytes.Buffer
    for _, k := range keys {
        if v, ok := configData[k]; ok {
            buf.WriteString(k + " = " + v + "\n")
        }
    }
    configMu.Unlock() 

    // 3. 磁盘 IO
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
