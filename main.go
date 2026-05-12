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
	// --- 系统句柄 ---
	hJob    windows.Handle
	hMutex  windows.Handle
	
	// --- 网络与路径 ---
	httpClient      = &http.Client{Timeout: 1 * time.Second}
	exePath, _      = os.Executable()
	baseDir         = filepath.Dir(exePath)

	// --- 状态控制 (使用 int32 确保 atomic 安全) ---
	// 初始值为 1，确保启动时的探测逻辑优先执行
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

	// --- 逻辑跟踪 (在此处安全整合补丁) ---
	lastState        = -1
	tunErrorCounter  = 0
	mTun             *systray.MenuItem
	
	// 【新增补丁变量】仅用于解决打勾同步逻辑，不影响原有指针
	globalLastHasTun bool 

	// --- 动态库载入 (User32 / Kernel32) ---
	u32 = windows.NewLazySystemDLL("user32.dll")
	k32 = windows.NewLazySystemDLL("kernel32.dll")

	// --- User32 过程调用 ---
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
	procKeybdEvent       = u32.NewProc("keybd_event")

	// --- Kernel32 过程调用 ---
	procGetCurrentThread = k32.NewProc("GetCurrentThreadId")
)

const (
	HWND_TOPMOST   = ^uintptr(0)
	HWND_NOTOPMOST = ^uintptr(1)
	SW_RESTORE     = 9
	SWP_SILKY      = 0x0043
	debugPort      = "52719"
)

func checkSystemState() int {
    // 1. 获取物理现实 (网卡)
    currentHasTun := isTunInterfaceExist()
    globalLastHasTun = currentHasTun // 更新共识，供剑客 2/3 使用

    // 2. 获取配置意愿 (INI)
    iniEnabled := getIniConfig("tun_enabled") == "true"

    // 3. 排除法：如果正在同步或系统初始化，直接进入“装傻”模式
    // 借鉴版逻辑：不报警、不改配置、不 PATCH
    if atomic.LoadInt32(&isSyncing) == 1 || isSystemInitializing {
        tunErrorCounter = 0 // 状态变更中，重置计数器
        return StateStop    // 返回一个中间态，UI 维持原样
    }

    // --- 核心纠偏逻辑开始 ---

    // 逻辑 A：配置要开，但网卡没了
    if iniEnabled && !currentHasTun {
        // 借鉴版缓冲：给 8 秒考察期 (假设 checkSystemState 每秒执行一次)
        tunErrorCounter++
        if tunErrorCounter <= 8 {
            return StateStop // 缓冲期内，不报错，不纠偏
        }

        // 灵魂拷问：探测内核真相 (你的核心思路)
        yamlState, err := fetchRemoteYaml()
        if err == nil {
            // 情况 A1：内核在线，且 YAML 也是 false
            // 【你的定夺】：说明是外部 Dashboard 关的，承认现实，修改账本
            if !yamlState.TunEnabled {
                saveIniConfig("tun_enabled", "false")
                tunErrorCounter = 0
                return StateStop
            }
            
            // 情况 A2：内核在线，YAML 也是 true，但网卡就是没了
            // 【骨架版暴力纠偏】：内核没丢配置，系统网卡层崩了，立即拉起
            go syncConfigToKernel()
            return StateError // 暂时亮红灯，等待拉起成功
        } else {
            // 情况 A3：API 探测失败 (内核可能正在重启或已崩溃)
            // 维持红灯，但不改配置，等待内核自愈
            return StateError 
        }
    }

    // 逻辑 B：配置是关，但网卡出现了 (外部开启)
    if !iniEnabled && currentHasTun {
        // 再次探测 API 确认
        yamlState, err := fetchRemoteYaml()
        if err == nil && yamlState.TunEnabled {
            // 【你的逻辑】：发现外部开启了 TUN，修改 INI 追随现实
            saveIniConfig("tun_enabled", "true")
            tunErrorCounter = 0
            return StateRunning
        }
    }

    // 逻辑 C：正常态对齐
    if iniEnabled && currentHasTun {
        tunErrorCounter = 0 // 状态完美，重置计数
        return StateRunning
    }

    tunErrorCounter = 0
    return StateStop
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

func syncUIAppearance(state int) {
	// 1. 图标变色
	updateIconByState(state)

	// 2. 菜单打勾逻辑：只有处于 StateTun (绿色) 时，TUN 菜单才打勾
	if mTun != nil {
		if state == StateTun {
			mTun.Check()
		} else {
			mTun.Uncheck()
		}
	}
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

    // 托盘图标点击直接打开 WebUI
    systray.SetOnClick(func(menu systray.IMenu) {
        go launchWebUI()
    })

    mWeb := systray.AddMenuItem("进入 Web 面板", "")
    mWeb.Click(func() { go launchWebUI() })

    systray.AddSeparator()

    // 1. 系统代理 (保持即时反馈)
    mProxy := systray.AddMenuItemCheckbox("系统代理", "", getIniConfig("system_proxy_enabled") == "true")
    mProxy.Click(func() {
        next := !mProxy.Checked()
        saveIniConfig("system_proxy_enabled", fmt.Sprint(next))
        setProxyRegistry(next)
        if next { mProxy.Check() } else { mProxy.Uncheck() }
    })

    // 2. 虚拟网卡 (合并修正点：配置驱动 + 异步刷新)
    mTun = systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", getIniConfig("tun_enabled") == "true")
    mTun.Click(func() {
        // A. 依靠配置判断而非 UI
        next := getIniConfig("tun_enabled") != "true"
        saveIniConfig("tun_enabled", fmt.Sprint(next))
        
        // B. 预刷新 UI (增强灵敏度)
        if next { mTun.Check() } else { mTun.Uncheck() }
        
        // C. 执行逻辑并强制最终校准
        go func() {
            setTunMode(next)
            // 确保在内核可能重启后，勾选状态能准确同步
            syncUIAppearance(checkSystemState())
        }()
    })

    systray.AddSeparator()

    // 3. 模式切换 (合并修正点：持久化 + 联动取消勾选)
    mModeRoot := systray.AddMenuItem("模式切换", "")
    curMode := getIniConfig("mode")
    modeMenus := make(map[string]*systray.MenuItem)

    setupMode := func(key, label string) {
        modeMenus[key] = mModeRoot.AddSubMenuItemCheckbox(label, "", curMode == key)
        modeMenus[key].Click(func() {
            // 保存意图到 INI
            saveIniConfig("mode", key)
            // 立即切换所有菜单勾选 (排他性勾选)
            for k, menu := range modeMenus {
                if k == key { menu.Check() } else { menu.Uncheck() }
            }
            // 异步同步内核
            go setMihomoMode(key)
        })
    }

    setupMode("rule", "规则模式")
    setupMode("global", "全局模式")
    setupMode("direct", "直连模式")

    systray.AddSeparator()

    mDir := systray.AddMenuItem("打开目录", "")
    mDir.Click(func() {
        windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
    })

    // 4. 更多选项 (增加自愈触发)
    mMoreRoot := systray.AddMenuItem("更多", "")
    
    mAuto := mMoreRoot.AddSubMenuItemCheckbox("开机自启动", "", checkAutoStartStatus())
    mAuto.Click(func() {
        next := !mAuto.Checked()
        toggleAutoStart(next)
        saveIniConfig("startup_enabled", fmt.Sprint(next)) // 同步配置
        if next { mAuto.Check() } else { mAuto.Uncheck() }
    })

    mRestart := mMoreRoot.AddSubMenuItem("重启内核", "")
    mRestart.Click(func() {
        // 触发 monitorKernelDaemon 的重启逻辑
        atomic.StoreInt32(&isSystemInitializing, 1)
        atomic.StoreInt32(&hasFirstSynced, 0)
        KillProcessByName("mihomo.exe")
        // 重启后 monitorKernelDaemon 会通过探针自动调用 syncUIAppearance 补齐状态
    })

    mReload := mMoreRoot.AddSubMenuItem("重载配置文件", "")
    mReload.Click(func() {
        sniffAndSolidifyConfig()
        reloadConfigFile()
        // 重载后强制刷新一次 UI
        syncUIAppearance(checkSystemState())
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
        // 1. 退出检查
        if atomic.LoadInt32(&isReallyExiting) == 1 {
            return
        }

        // 2. 检查内核进程是否存活
        if !isProcessRunning("mihomo.exe") {
            // --- A. 启动保护区 ---
            atomic.StoreInt32(&isSystemInitializing, 1)
            atomic.StoreInt32(&hasFirstSynced, 0)
            atomic.StoreInt32(&isKernelActive, 0)

            // 清理残留进程
            KillProcessByName("mihomo.exe")
            time.Sleep(500 * time.Millisecond)

            // 3. 构造启动命令
            cmd := exec.Command(target, "-d", ".")
            cmd.Dir = absBaseDir
            cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}

            if err := cmd.Start(); err == nil {
                atomic.StoreInt32(&isKernelActive, 1)

                // --- B. 资源绑定 (防止内核孤儿化) ---
                if hJob != 0 {
                    hp, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
                    if err == nil {
                        _ = windows.AssignProcessToJobObject(hJob, hp)
                        _ = windows.CloseHandle(hp)
                    }
                }

                // --- C. 核心自愈与 UI 强同步逻辑 ---
                go func() {
                    success := false
                    // 给予 6 秒启动窗口 (500ms * 12)
                    for i := 0; i < 12; i++ {
                        time.Sleep(500 * time.Millisecond)
                        
                        // 探测内核 API 是否响应
                        resp, err := doAPIRequest("GET", "/configs", nil)
                        if err == nil && len(resp) > 200 {
                            // 1. 立即灌入 INI 配置到内核
                            syncConfigToKernel()
                            
                            // 2. 获取当前系统真实状态
                            state := checkSystemState()
                            
                            // 3. 【关键修复】更新全局变量 globalLastHasTun
                            // 确保 watchTunState 在解锁后不会因为“没察觉到变化”而不打勾
                            globalLastHasTun = (state == StateTun)

                            // 4. 【强制刷新】执行菜单打勾和图标变色
                            syncUIAppearance(state)
                            
                            success = true
                            break
                        }
                    }

                    // 5. 任务完成或超时，解除锁定
                    atomic.StoreInt32(&isSystemInitializing, 0)
                    
                    // 6. 兜底处理：如果内核启动失败，UI 也要归位
                    if !success {
                        syncUIAppearance(checkSystemState())
                    }
                }()

                // 4. 监听内核进程退出
                go func(c *exec.Cmd) {
                    _ = c.Wait()
                    atomic.StoreInt32(&isKernelActive, 0)
                }(cmd)
            } else {
                // 启动失败立刻解锁保护区
                atomic.StoreInt32(&isSystemInitializing, 0)
            }
        }
        
        // 5. 循环检查间隔
        time.Sleep(2 * time.Second)
    }
}
func monitorIconState() {
    // 1. 获取决策中心的最终状态
    // 此调用包含了：网卡检测、8秒缓冲、API 探测、以及必要时的暴力拉起
    state := checkSystemState()

    // 2. 根据状态切换 UI 表现
    switch state {
    case StateRunning:
        // 状态：一切正常，TUN 已就绪
        systray.SetIcon(iconGreen)
        systray.SetTooltip(APP_NAME + " - 运行中 (TUN 已开启)")
        
        // 确保菜单勾选状态与事实一致
        if mTun != nil {
            mTun.Check()
            mTun.Enable() // 允许用户操作
        }

    case StateStop:
        // 状态：已停止 或 处于 8 秒缓冲期/同步中
        // 如果是正在同步 (isSyncing)，我们可以让图标变成灰色或黄色（如果有的话）
        if atomic.LoadInt32(&isSyncing) == 1 || isSystemInitializing {
            systray.SetIcon(iconGray)
            systray.SetTooltip(APP_NAME + " - 状态切换中...")
            if mTun != nil {
                mTun.Disabled() // 正在处理时，禁用菜单防止用户连续点击
            }
        } else {
            systray.SetIcon(iconGray)
            systray.SetTooltip(APP_NAME + " - 未运行")
            if mTun != nil {
                mTun.Uncheck()
                mTun.Enable()
            }
        }

    case StateError:
        // 状态：超过 8 秒缓冲后，API 确认失败或网卡依然缺失
        systray.SetIcon(iconRed)
        systray.SetTooltip(APP_NAME + " - 异常: TUN 网卡未就绪")
        
        // 报错时依然允许用户尝试手动点击开关进行“暴力重置”
        if mTun != nil {
            mTun.Enable()
        }

    default:
        // 保底处理
        systray.SetIcon(iconGray)
    }

    // 3. 联动执行“后勤存档”
    // watchTunState 会根据 globalLastHasTun 和 tunErrorCounter 决定是否更新 INI
    watchTunState()
}

func watchTunState() {
    // 1. 只有当状态发生明确变化时，才考虑操作磁盘，避免频繁 IO
    // currentHasTun 已经在 checkSystemState 中通过全局变量 globalLastHasTun 更新
    currentHasTun := globalLastHasTun
    
    // 2. 核心拦截：如果正在同步、初始化或处于 8 秒缓冲期内，观察员保持静默
    // 理由：防止在内核重启、网卡还没稳住时，误把配置改掉
    if atomic.LoadInt32(&isSyncing) == 1 || isSystemInitializing || tunErrorCounter > 0 {
        return
    }

    // 3. 获取当前账本状态
    iniEnabled := getIniConfig("tun_enabled") == "true"

    // --- 逻辑分叉：对齐账本 ---

    if currentHasTun && !iniEnabled {
        // 【你的逻辑 3】：物理网卡有了，但账本写着 false
        // 此时 checkSystemState 已经确认过 API 为 true，观察员直接补单
        log.Println("[Watch] 发现外部开启的 TUN 网卡，同步更新本地配置...")
        saveIniConfig("tun_enabled", "true")
        
        // 联动 UI：确保菜单勾选状态同步
        if mTun != nil {
            mTun.Check()
        }

    } else if !currentHasTun && iniEnabled {
        // 【你的逻辑 2】：网卡没了，但账本写着 true
        // 能够走到这一步，说明 checkSystemState 已经确认过 API 变为了 false（外部关闭）
        // 且已经度过了 8 秒缓冲期
        log.Println("[Watch] 检测到外部关闭行为，账本回退为 false...")
        saveIniConfig("tun_enabled", "false")
        
        // 联动 UI
        if mTun != nil {
            mTun.Uncheck()
        }
    }
    
    // 如果两者一致（True-True 或 False-False），则什么都不做，保持静默
}
func syncConfigToKernel() {
    // 1. 抢锁：防止多个纠偏任务同时跑
    if !atomic.CompareAndSwapInt32(&isSyncing, 0, 1) {
        return
    }
    // 2. 释放：确保最终解锁，并把系统初始化状态设为 false
    defer func() {
        atomic.StoreInt32(&isSyncing, 0)
        isSystemInitializing = false 
    }()

    isSystemInitializing = true
    // 3. 强力保护：防止网络请求意外卡死导致 UI 永远无法操作
    timer := time.AfterFunc(10*time.Second, func() { 
        isSystemInitializing = false 
    })
    defer timer.Stop()

    // ... 获取配置和拼接 payload 的逻辑 ...

    success := false
    for i := 0; i < 3; i++ {
        _, err := doAPIRequest("PATCH", "/configs", payload)
        if err == nil {
            success = true
            break 
        }
        time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
    }

    if success {
        // --- 关键修补点 ---
        // 同步成功了，立即清空大脑的错误计数器，让图标瞬间变绿
        tunErrorCounter = 0 
        
        // 给内核一点点时间创建网卡接口
        time.Sleep(500 * time.Millisecond)
    }
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
    configMu.Lock()
    
    // 1. 只有当 key 不为空时才处理逻辑
    if key != "" {
        old, ok := configData[key]
        // 如果值没变，直接解锁退出，保护磁盘寿命
        if ok && old == val {
            configMu.Unlock()
            return
        }
        // 值变了，更新内存缓存
        configData[key] = val
    }

    // 2. 准备数据（在锁内快速拷贝）
    keys := []string{"mode", "tun_enabled", "system_proxy_enabled", "startup_enabled", "proxy_address", "tun_device_name", "external-controller", "secret"}
    var buf bytes.Buffer
    for _, k := range keys {
        if v, ok := configData[k]; ok {
            buf.WriteString(k + " = " + v + "\n")
        }
    }
    configMu.Unlock() // 锁内逻辑到此为止

    // 3. 磁盘 IO（锁外执行，保证 UI 线程不卡顿）
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
