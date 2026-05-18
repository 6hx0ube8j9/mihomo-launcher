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
	"runtime"
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
	APP_MUTEX    = "Mihomo_Unique_Mutex"
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
	hJob    windows.Handle
	hMutex  windows.Handle

	httpClient = &http.Client{Timeout: 1 * time.Second}
	exePath, _ = os.Executable()
	baseDir    = filepath.Dir(exePath)
	configData = make(map[string]string)
	configMu   sync.RWMutex

	isSystemInitializing         int32 = 1
	isSyncing                    int32
	globalOpID                   int32
	hasFirstSynced               int32
	isKernelActive               int32
	isFocusing                   int32
	manualUpdateTrigger          int32
	isReallyExiting              int32
	lastClickTime                int64
	isTunInterfaceCurrentlyAlive int32

	cachedWebUIHwnd uintptr

	exitOnce        sync.Once
	lastState int32 = -1
	tunErrorCounter = 0

	mTun *systray.MenuItem

	u32 = windows.NewLazySystemDLL("user32.dll")
	k32 = windows.NewLazySystemDLL("kernel32.dll")

	procEnumWindows      = u32.NewProc("EnumWindows")
	procGetClassName     = u32.NewProc("GetClassNameW")
	procIsWindowVisible  = u32.NewProc("IsWindowVisible")
	procGetWindowThread  = u32.NewProc("GetWindowThreadProcessId")
	procGetWindow        = u32.NewProc("GetWindow")
	procGetWindowText    = u32.NewProc("GetWindowTextW")

	procSetWindowPos     = u32.NewProc("SetWindowPos")
	procShowWindow       = u32.NewProc("ShowWindow")
	procSetForeground    = u32.NewProc("SetForegroundWindow")
	procBringToTop       = u32.NewProc("BringWindowToTop")
	procGetForeground    = u32.NewProc("GetForegroundWindow")
	procAttachThread     = u32.NewProc("AttachThreadInput")
	procGetCurrentThread = k32.NewProc("GetCurrentThreadId")

	procKeybdEvent = u32.NewProc("keybd_event")
)

const (
    // 物理层级常量
    HWND_TOPMOST   = ^uintptr(0) // -1
    HWND_NOTOPMOST = ^uintptr(1) // -2

    // 状态常量
    SW_RESTORE     = 9
    
    // 动作组合标志位
    SWP_NOSIZE     = 0x0001
    SWP_NOMOVE     = 0x0002
    SWP_SHOWWINDOW = 0x0040
    SWP_SILKY      = SWP_NOSIZE | SWP_NOMOVE | SWP_SHOWWINDOW 
    
    debugPort      = "52719"
)

func init() {
    u32 := windows.NewLazySystemDLL("user32.dll")
    procSetContext := u32.NewProc("SetProcessDpiAwarenessContext")
    if procSetContext.Find() == nil {
        _, _, _ = procSetContext.Call(uintptr(0xfffffffc))
    } else {
        procSetAware := u32.NewProc("SetProcessDPIAware")
        if procSetAware.Find() == nil {
            _, _, _ = procSetAware.Call()
        }
    }
}

func main() {
    atomic.StoreInt32(&isSystemInitializing, 1)
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

	// 2. 模拟 Alt 键按下（黑魔法前置）：提前向系统高呼“有键盘输入事件”，偷取 Windows 前台焦点控制权
	procKeybdEvent.Call(0x12, 0, 0, 0) // Alt down

	// 3. 线程关联：让当前进程拥有前台权限（增加防崩溃安全判定）
	if foreT != currT && foreT != 0 {
		procAttachThread.Call(foreT, currT, 1)
	}
	if targT != 0 && targT != currT {
		procAttachThread.Call(currT, targT, 1)
	}

	// 4. 强行恢复可能处于最小化状态的窗口 (9 = SW_RESTORE)
	procShowWindow.Call(targetHwnd, 9)

	// 5. 穿透多进程内核补刀：利用 SwitchToThisWindow 直接激活最底层的多进程 shell 外壳
	winuser := windows.NewLazySystemDLL("user32.dll")
	switchToThisWindow := winuser.NewProc("SwitchToThisWindow")
	_, _, _ = switchToThisWindow.Call(targetHwnd, 1)

	// 6. 执行窗口唤醒组合拳
	procSetForeground.Call(targetHwnd)
	procBringToTop.Call(targetHwnd)
	
	// ⚡ 方案 A 修复：在 64 位环境下，-1 的十六进制补码为 0xFFFFFFFFFFFFFFFF (HWND_TOPMOST)
	// (0x0040|0x0002|0x0001 代表 SWP_SHOWWINDOW|SWP_NOMOVE|SWP_NOSIZE)
	procSetWindowPos.Call(targetHwnd, uintptr(0xFFFFFFFFFFFFFFFF), 0, 0, 0, 0, 0x0040|0x0002|0x0001)

	// 7. 解除线程关联
	if targT != 0 && targT != currT {
		procAttachThread.Call(currT, targT, 0)
	}
	if foreT != currT && foreT != 0 {
		procAttachThread.Call(foreT, currT, 0)
	}

	// 8. 释放 Alt 键
	procKeybdEvent.Call(0x12, 0, 2, 0) // Alt up

	// 9. 延时 400 毫秒恢复普通层级，允许用户切走，不做流氓置顶
	time.AfterFunc(400*time.Millisecond, func() {
		// ⚡ 方案 A 修复：在 64 位环境下，-2 的十六进制补码为 0xFFFFFFFFFFFFFFFE (HWND_NOTOPMOST)
		procSetWindowPos.Call(targetHwnd, uintptr(0xFFFFFFFFFFFFFFFE), 0, 0, 0, 0, 0x0040|0x0002|0x0001)
	})
}

func launchWebUI() {
	apiAddr := getIniConfig("external-controller")
	secret := getIniConfig("secret")
	proxyAddr := getIniConfig("proxy_address")

	baseUI := strings.TrimRight(apiAddr, "/")
	if !strings.HasPrefix(baseUI, "http") {
		baseUI = "http://" + baseUI
	}
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(baseUI, "http://"), "https://"))
	if port == "" {
		host, port = "127.0.0.1", "9090"
	}
	finalURL := fmt.Sprintf("%s/ui/?hostname=%s&port=%s&secret=%s#/proxies", baseUI, host, port, secret)

	if cachedWebUIHwnd != 0 {
		if vis, _, _ := procIsWindowVisible.Call(cachedWebUIHwnd); vis != 0 {
			focusWindowSilky(cachedWebUIHwnd)
			return
		}
		cachedWebUIHwnd = 0
	}

	client := &http.Client{Timeout: 300 * time.Millisecond}
	isPortOccupied := false

	if resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/json", debugPort)); err == nil {
		isPortOccupied = true
		defer resp.Body.Close()
		var targets []map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&targets); err == nil {
			for _, t := range targets {
				pURL, _ := t["url"].(string)
				if strings.Contains(pURL, "/ui/") || strings.Contains(pURL, "setup") {
					id, _ := t["id"].(string)
					_, _ = client.Get(fmt.Sprintf("http://127.0.0.1:%s/json/activate/%s", debugPort, id))

					go func() {
						runtime.LockOSThread()
						defer runtime.UnlockOSThread()

						for i := 0; i < 20; i++ {
							var targetHwnd uintptr

							_, _, _ = procEnumWindows.Call(windows.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
								if vis, _, _ := procIsWindowVisible.Call(hwnd); vis != 0 {
									var buf [256]uint16
									_, _, _ = procGetClassName.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), 256)
									if windows.UTF16ToString(buf[:]) == "Chrome_WidgetWin_1" {
										
										childCount := 0
										child, _, _ := procGetWindow.Call(hwnd, 5) // 5 = GW_CHILD
										for child != 0 {
											childCount++
											if childCount > 5 {
												break
											}
											child, _, _ = procGetWindow.Call(child, 2) // 2 = GW_HWNDNEXT
										}

										if childCount <= 5 {
											var titleBuf [512]uint16
											_, _, _ = procGetWindowText.Call(hwnd, uintptr(unsafe.Pointer(&titleBuf[0])), 512)
											wndTitle := windows.UTF16ToString(titleBuf[:])

											if strings.Contains(wndTitle, "ui") || strings.Contains(wndTitle, "Dashboard") || strings.Contains(wndTitle, "Proxies") {
												targetHwnd = hwnd
												cachedWebUIHwnd = hwnd
												return 0
											}
										}
									}
								}
								return 1
							}), 0)

							if targetHwnd != 0 {
								focusWindowSilky(targetHwnd)
								break
							}
							time.Sleep(50 * time.Millisecond)
						}
					}()
					return
				}
			}
		}
	} else {
		conn, dialErr := net.DialTimeout("tcp", "127.0.0.1:"+debugPort, 50*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			isPortOccupied = true
		}
	}

	if isPortOccupied {
		killCmd := fmt.Sprintf("for /f \"tokens=5\" %%a in ('netstat -aon ^| findstr :%s ^| findstr LISTENING') do taskkill /F /PID %%a", debugPort)
		_ = exec.Command("cmd", "/c", killCmd).Run()
		time.Sleep(150 * time.Millisecond)
	}

	var browserPath string
	potentialPaths := []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("LocalAppData"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `BraveSoftware\Brave-Browser\Application\brave.exe`),
		filepath.Join(os.Getenv("LocalAppData"), `BraveSoftware\Brave-Browser\Application\brave.exe`),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `BraveSoftware\Brave-Browser\Application\brave.exe`),
	}

	for _, p := range potentialPaths {
		if _, err := os.Stat(p); err == nil {
			browserPath = p
			break
		}
	}

	if browserPath != "" {
		userDataDir := filepath.Join(baseDir, "webcache")
		_ = os.MkdirAll(userDataDir, 0755)

		args := []string{
			"--app=" + finalURL,
			"--remote-debugging-port=" + debugPort,
			"--user-data-dir=" + userDataDir,
			"--window-size=1280,768",
			"--proxy-server=" + proxyAddr,
			"--no-first-run",
			"--no-default-browser-check",
		}
		cmd := exec.Command(browserPath, args...)
		if err := cmd.Start(); err == nil {
			mainPid := uint32(cmd.Process.Pid)

			if hJob != 0 {
				if hp, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, mainPid); err == nil {
					_ = windows.AssignProcessToJobObject(hJob, hp)
					_ = windows.CloseHandle(hp)
				}
			}

			go func() {
				runtime.LockOSThread()
				defer runtime.UnlockOSThread()

				for i := 0; i < 20; i++ {
					var newHwnd uintptr

					_, _, _ = procEnumWindows.Call(windows.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
						if vis, _, _ := procIsWindowVisible.Call(hwnd); vis != 0 {
							var wndPid uint32
							_, _, _ = procGetWindowThread.Call(hwnd, uintptr(unsafe.Pointer(&wndPid)))

							var buf [256]uint16
							_, _, _ = procGetClassName.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), 256)
							className := windows.UTF16ToString(buf[:])

							if className == "Chrome_WidgetWin_1" {
								if wndPid == mainPid {
									newHwnd = hwnd
									cachedWebUIHwnd = hwnd
									return 0
								}

								childCount := 0
								child, _, _ := procGetWindow.Call(hwnd, 5) // 5 = GW_CHILD
								for child != 0 {
									childCount++
									if childCount > 5 {
										break
									}
									child, _, _ = procGetWindow.Call(child, 2) // 2 = GW_HWNDNEXT
								}

								if childCount <= 5 {
									var titleBuf [512]uint16
									_, _, _ = procGetWindowText.Call(hwnd, uintptr(unsafe.Pointer(&titleBuf[0])), 512)
									wndTitle := windows.UTF16ToString(titleBuf[:])

									if strings.Contains(wndTitle, "ui") || strings.Contains(wndTitle, "Dashboard") || strings.Contains(wndTitle, "Proxies") {
										newHwnd = hwnd
										cachedWebUIHwnd = hwnd
										return 0
									}
								}
							}
						}
						return 1
					}), 0)

					if newHwnd != 0 {
						focusWindowSilky(newHwnd)
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
			}()
		}
	} else {
		_ = exec.Command("cmd", "/c", "start", "", finalURL).Start()
	}
}

func onReady() {
    // 1. 【核心保护】一进入函数立即开启初始化锁
    atomic.StoreInt32(&isSystemInitializing, 1)

    // 基础环境初始化
    saveIniConfig("startup_enabled", fmt.Sprint(checkAutoStartStatus()))
    ensureDefaultConfig()
    sniffAndSolidifyConfig()
    setProxyRegistry(getIniConfig("system_proxy_enabled") == "true")
    updateIconByState(StateStop)

    systray.SetOnClick(func(menu systray.IMenu) {
        if atomic.LoadInt32(&isSystemInitializing) == 1 {
            return
        }
        now := time.Now().UnixNano()
        // 1000ms = 1秒
        if now - atomic.LoadInt64(&lastClickTime) < int64(1000 * time.Millisecond) {
            return
        }
        atomic.StoreInt64(&lastClickTime, now)
        go launchWebUI()
    })

    // 菜单项初始化
    mWeb := systray.AddMenuItem("进入 Web 面板", "")
    mWeb.Click(func() { 
        // 菜单点击通常也建议加上同样的防抖保护
        now := time.Now().UnixNano()
        if now - atomic.LoadInt64(&lastClickTime) < int64(1000 * time.Millisecond) {
            return
        }
        atomic.StoreInt64(&lastClickTime, now)

        go launchWebUI() 
    })

    systray.AddSeparator()

    // --- 系统代理 ---
    mProxy := systray.AddMenuItemCheckbox("系统代理", "", getIniConfig("system_proxy_enabled") == "true")
    mProxy.Click(func() {
        next := !mProxy.Checked()
        saveIniConfig("system_proxy_enabled", fmt.Sprint(next))
        setProxyRegistry(next)
        if next { mProxy.Check() } else { mProxy.Uncheck() }
    })

    // --- 虚拟网卡 (TUN) ---
    mTun = systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", getIniConfig("tun_enabled") == "true")
    mTun.Click(func() {
        next := !mTun.Checked()
        if next { mTun.Check() } else { mTun.Uncheck() }
        
        go func() {
            // 在 setTunMode 内部会自动处理 isSystemInitializing 锁
            setTunMode(next)
        }()
    })

    systray.AddSeparator()

    // --- 模式切换 (采用第二份的封装结构，更安全) ---
    mModeRoot := systray.AddMenuItem("模式切换", "")
    curMode := getIniConfig("mode")
    modeMenus := make(map[string]*systray.MenuItem)

    setupMode := func(key, label string) {
        modeMenus[key] = mModeRoot.AddSubMenuItemCheckbox(label, "", curMode == key)
        modeMenus[key].Click(func() {
            saveIniConfig("mode", key)
            // 立即刷新 UI 勾选状态
            for k, menu := range modeMenus {
                if k == key { menu.Check() } else { menu.Uncheck() }
            }
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
        sniffAndSolidifyConfig()
    })

    mReload := mMoreRoot.AddSubMenuItem("重载配置文件", "")
    mReload.Click(func() {
        atomic.StoreInt32(&isSystemInitializing, 1)
        sniffAndSolidifyConfig()
        reloadConfigFile()
        atomic.StoreInt32(&hasFirstSynced, 0)
    })
    mEditConfig := mMoreRoot.AddSubMenuItem("编辑 config.yaml", "")
    mEditConfig.Click(func() {
        now := time.Now().UnixNano()
        if now - atomic.LoadInt64(&lastClickTime) < int64(1000 * time.Millisecond) {
            return
        }
        atomic.StoreInt64(&lastClickTime, now)

        configPath := filepath.Join(baseDir, "config.yaml")
        windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(configPath), nil, nil, windows.SW_SHOWNORMAL)
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
        if atomic.LoadInt32(&isReallyExiting) == 1 { return }
        
        if !isProcessRunning("mihomo.exe") {
            // 1. 锁定状态
            atomic.StoreInt32(&isSystemInitializing, 1)
            atomic.StoreInt32(&hasFirstSynced, 0)
            atomic.StoreInt32(&isKernelActive, 0)
            
            KillProcessByName("mihomo.exe")
            time.Sleep(300 * time.Millisecond) // 稍微多给点时间让端口释放
            
            cmd := exec.Command(target, "-d", ".")
            cmd.Dir = absBaseDir
            cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
            
            if err := cmd.Start(); err == nil {
                atomic.StoreInt32(&isKernelActive, 1)
                sniffAndSolidifyConfig() 

                // 绑定 Job Object (略...)
                if hJob != 0 {
                    hp, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
                    if err == nil {
                        _ = windows.AssignProcessToJobObject(hJob, hp)
                        _ = windows.CloseHandle(hp)
                    }
                }
                
                go func(c *exec.Cmd) {
                    _ = c.Wait()
                    atomic.StoreInt32(&isKernelActive, 0)
                }(cmd)
                
                // 2. 核心调整：启动后强制对齐状态
                time.Sleep(1000 * time.Millisecond) 
                go syncConfigToKernel() // 这里面的 defer 会把 Initializing 设为 0
                
            } else {
                // 3. 兜底：如果连 Start 都失败了（如文件丢失），必须开门，让图标变红
                atomic.StoreInt32(&isSystemInitializing, 0)
            }
        } else {
            if atomic.LoadInt32(&isSystemInitializing) == 1 && atomic.LoadInt32(&isSyncing) == 0 {
                // 只有在没有进行同步任务时，才允许复位
                atomic.StoreInt32(&isSystemInitializing, 0)
            }
        }
        time.Sleep(2 * time.Second)
    }
}

func watchTunState() {
    ticker := time.NewTicker(3 * time.Second)
    defer ticker.Stop()

    // 初始探测逻辑保持不变...
    lastHasTun := false
    if ifaces, err := net.Interfaces(); err == nil {
        for _, i := range ifaces {
            if isTunInterfaceMatch(i.Name) {
                lastHasTun = true
                break
            }
        }
    }
    val := int32(0); if lastHasTun { val = 1 }
    atomic.StoreInt32(&isTunInterfaceCurrentlyAlive, val)

    confirmCount := 0
    for {
        select {
        case <-ticker.C:
            if atomic.LoadInt32(&isReallyExiting) == 1 { return }

            // 如果正在同步或初始化，保持静默，重置计数器
            if atomic.LoadInt32(&isSystemInitializing) == 1 || atomic.LoadInt32(&isSyncing) == 1 {
                confirmCount = 0
                continue
            }

            currentHasTun := false
            currentIfaces, err := net.Interfaces()
            if err != nil { continue }
            for _, i := range currentIfaces {
                if isTunInterfaceMatch(i.Name) {
                    currentHasTun = true
                    break
                }
            }

            // --- 核心调整：非对称确认逻辑 ---
            if currentHasTun != lastHasTun {
                if currentHasTun {
                    // 1. 如果是从“无”到“有”：瞬时更新，不等待
                    // 这样当你启动 TUN 时，图标能立刻变绿
                    lastHasTun = true
                    confirmCount = 0
                    atomic.StoreInt32(&isTunInterfaceCurrentlyAlive, 1)
                } else {
                    // 2. 如果是从“有”到“无”：维持 2 次确认（6秒）
                    // 防止 Windows 网卡重置瞬间的误报
                    confirmCount++
                    if confirmCount >= 2 {
                        lastHasTun = false
                        confirmCount = 0
                        atomic.StoreInt32(&isTunInterfaceCurrentlyAlive, 0)
                    }
                }
            } else {
                confirmCount = 0
            }
            // 无论如何，标记已经完成过至少一次探测
            atomic.StoreInt32(&hasFirstSynced, 1)
        }
    }
}
func checkSystemState() int32 {
    // 1. 进程检查
    if !isProcessRunning("mihomo.exe") {
        return int32(StateStop)
    }

    // 2. API 获取
    body, err := doAPIRequest("GET", "/configs", nil)
    if err != nil { return int32(StateStop) }

    var currentConf struct {
        Tun struct { Enable bool `json:"enable"` } `json:"tun"`
        Mode string `json:"mode"`
    }
    if err := json.Unmarshal(body, &currentConf); err != nil { return int32(StateStop) }

    // 3. 读取账本
    targetTunInIni := getIniConfig("tun_enabled") == "true"
    targetModeInIni := getIniConfig("mode")
    targetProxyInIni := getIniConfig("system_proxy_enabled") == "true"

    // 4. 对齐逻辑
    if atomic.LoadInt32(&isSystemInitializing) == 1 {
        if currentConf.Tun.Enable != targetTunInIni || (targetModeInIni != "" && currentConf.Mode != targetModeInIni) {
            go syncConfigToKernel()
        }
    } else {
		if currentConf.Tun.Enable != targetTunInIni {
			enabled := currentConf.Tun.Enable
			saveIniConfig("tun_enabled", fmt.Sprint(enabled))
			if mTun != nil {
				if enabled { mTun.Check() } else { mTun.Uncheck() }
			}
		}
		if currentConf.Mode != "" && currentConf.Mode != targetModeInIni {
			saveIniConfig("mode", currentConf.Mode)
		}
	}

    // 5. 事实反馈
    if currentConf.Tun.Enable {
        return int32(StateTun)
    }
    if targetProxyInIni {
        return int32(StateProxy)
    }
    return int32(StateDefault)
}

func monitorIconState() {
	var successCounter int

	for {
		if atomic.LoadInt32(&isReallyExiting) == 1 { return }

		// 1. 最高优先级：进程不在 (零宽容，第一时间 STOP)
		if !isProcessRunning("mihomo.exe") {
			tunErrorCounter = 0
			successCounter = 0
			if atomic.LoadInt32(&lastState) != int32(StateStop) {
				updateIconByState(StateStop)
				atomic.StoreInt32(&lastState, int32(StateStop))
			}
		} else {
			// 2. 状态采集
			curr := checkSystemState()
			isTunModeInConfig := (getIniConfig("tun_enabled") == "true")
			isPhysicalLost := (atomic.LoadInt32(&isTunInterfaceCurrentlyAlive) == 0)
			
			isInitializing := (atomic.LoadInt32(&isSystemInitializing) == 1)
			isSyncing := (atomic.LoadInt32(&isSyncing) == 1)

			// --- 核心手术：防闪烁拦截 ---
			// 如果处于启动或同步中，且 API 已经返回了中间态(黄/蓝)，但物理网卡还没出
			// 我们强制判定为“尚未就绪”，不进入下方的恢复逻辑，从而冻结在“灰色”
			if (isInitializing || isSyncing) && curr != int32(StateTun) {
				if isTunModeInConfig {
					// 这种情况下，我们认为系统还没“恢复”，继续等待
					goto nextLoop 
				}
			}

			// 3. 判定故障 (isBroken)
			isBroken := (curr == int32(StateStop)) ||
				(isTunModeInConfig && isPhysicalLost && !isInitializing && !isSyncing)

			if isBroken {
				successCounter = 0
				if tunErrorCounter < 5 { tunErrorCounter++ }

				// 连续 3 秒异常才生效 (防抖)
				if tunErrorCounter > 2 {
					targetState := int32(StateError)
					if curr == int32(StateStop) {
						targetState = int32(StateStop)
					}

					if atomic.LoadInt32(&lastState) != targetState {
						updateIconByState(int(targetState))
						atomic.StoreInt32(&lastState, targetState)
					}
				}
			} else {
				// 4. 重置/恢复逻辑
				successCounter++
				// 恢复判定：只有连续 3 秒稳定，或者还没变红(Error)前的状态切换
				if tunErrorCounter <= 2 || successCounter >= 3 {
					if successCounter >= 3 { tunErrorCounter = 0 }

					if atomic.LoadInt32(&lastState) != curr {
						updateIconByState(int(curr))
						atomic.StoreInt32(&lastState, curr)
					}
				}
			}
		}

	nextLoop:
		time.Sleep(1 * time.Second)
	}
}

func reloadConfigFile() {
    atomic.StoreInt32(&isSystemInitializing, 1) // 关门
    
    payload := map[string]string{"path": filepath.Join(baseDir, "config.yaml")}
    _, _ = doAPIRequest("PUT", "/configs?force=false", payload)
	
    go syncConfigToKernel() 
}

func syncConfigToKernel() {
    if !atomic.CompareAndSwapInt32(&isSyncing, 0, 1) { return }
    
    // 【精简点】统一在 defer 处理所有锁的释放
    defer func() {
        atomic.StoreInt32(&isSyncing, 0)
        // 关键：同步彻底结束（无论成败）后再允许 monitor 恢复工作
        atomic.StoreInt32(&isSystemInitializing, 0) 
    }()

    tunEnabled := getIniConfig("tun_enabled") == "true"
    currentMode := getIniConfig("mode")
    
    // 逻辑合并：如果是初始化，带上 mode
    payload := map[string]interface{}{"tun": map[string]bool{"enable": tunEnabled}}
    if atomic.LoadInt32(&isSystemInitializing) == 1 {
        payload["mode"] = currentMode
    }

    // 指数退避重试
    for i := 0; i < 5; i++ {
        if _, err := doAPIRequest("PATCH", "/configs", payload); err == nil {
            // 成功后更新 UI
            if mTun != nil {
                if tunEnabled { mTun.Check() } else { mTun.Uncheck() }
            }
            return // 成功直接返回，触发 defer 解锁
        }
        time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
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
	// 确保 Body 最终被关闭
	defer resp.Body.Close()

	// 6. 自动判定：无内容或标准心跳响应则跳过读取
	// 204 是标准的 "No Content" 成功码，ContentLength == 0 说明确实没数据
	if resp.StatusCode == 204 || resp.ContentLength == 0 {
		_, _ = io.Copy(io.Discard, resp.Body)
		// 如果状态码在 200 范围内，认为成功
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil, nil
		}
		return nil, fmt.Errorf("API Status Error: %d", resp.StatusCode)
	}

	// 7. 读取响应内容
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
			if val != "" {
			    saveIniConfig("secret", val)
			}	
		}
	}
}

func setMihomoMode(mode string) {
	saveIniConfig("mode", mode)
	_, _ = doAPIRequest("PATCH", "/configs", map[string]string{"mode": mode})
}

func setTunMode(enable bool) {
    newID := atomic.AddInt32(&globalOpID, 1)
    atomic.StoreInt32(&isSystemInitializing, 1) 
    saveIniConfig("tun_enabled", fmt.Sprint(enable))

    go func(opID int32) {
        _, err := doAPIRequest("PATCH", "/configs", map[string]interface{}{
            "tun": map[string]bool{"enable": enable},
        })

        if err != nil {
            if atomic.LoadInt32(&globalOpID) == opID {
                atomic.StoreInt32(&isSystemInitializing, 0) 
            }
            return
        }

        for i := 0; i < 15; i++ {
            if atomic.LoadInt32(&globalOpID) != opID {
                return
            }

            found := false
            ifaces, _ := net.Interfaces()
            for _, iface := range ifaces {
                if isTunInterfaceMatch(iface.Name) { 
                    found = true
                    break
                }
            }
            if found == enable {
                break
            }
            time.Sleep(200 * time.Millisecond)
        }
        if atomic.LoadInt32(&globalOpID) == opID {
            atomic.StoreInt32(&isSystemInitializing, 0)
        }
    }(newID)
}

func setProxyRegistry(enable bool) {
	if atomic.LoadInt32(&isReallyExiting) != 1 {
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
		_ = key.DeleteValue("AutoConfigURL")
	} else {
		_ = key.SetDWordValue("ProxyEnable", 0)
	}

	key.Close()

	wininet := windows.NewLazySystemDLL("wininet.dll")
	setOption := wininet.NewProc("InternetSetOptionW")
	_, _, _ = setOption.Call(0, 39, 0, 0)
	_, _, _ = setOption.Call(0, 42, 0, 0)
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

	if key != "" && val != "" {
		if old, ok := configData[key]; ok && old == val {
			configMu.Unlock()
			return
		}
		configData[key] = val
	}

	keys := []string{"mode", "tun_enabled", "system_proxy_enabled", "startup_enabled", "proxy_address", "tun_device_name", "external-controller", "secret"}
	var buf bytes.Buffer
	for _, k := range keys {
		if v, ok := configData[k]; ok {
			buf.WriteString(k + " = " + v + "\n")
		}
	}
	configMu.Unlock() 

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
