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
	API_URL      = "http://127.0.0.1:9090"
	PROXY_ADDR   = "127.0.0.1:7890"
	APP_MUTEX    = "Global\\MihomoLauncherMutexV3"
	TUN_ADAPTER  = "Mihomo" 
)

var (
	conf        Config
	confMu      sync.RWMutex
	baseDir     string
	coreExe     string
	iniPath     string
	hJob        windows.Handle
	isExiting   bool
	httpClient  = &http.Client{Timeout: 1500 * time.Millisecond}
)

type Config struct {
	AutoStart   bool
	ServiceMode bool
	TunEnabled  bool
	TrayHidden  bool
	SystemProxy bool
	Mode        string
}

// --- 系统工具 ---

func isAdmin() bool {
	var t windows.Token
	err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &t)
	if err != nil { return false }
	defer t.Close()
	return t.IsElevated()
}

func runAsAdmin() {
	verb, _ := syscall.UTF16PtrFromString("runas")
	exe, _ := os.Executable()
	exePtr, _ := syscall.UTF16PtrFromString(exe)
	cwd, _ := os.Getwd()
	cwdPtr, _ := syscall.UTF16PtrFromString(cwd)
	_ = windows.ShellExecute(0, verb, exePtr, nil, cwdPtr, windows.SW_SHOWNORMAL)
}

// --- 进程树守护 (骨架1精华) ---

func initJobObject() {
	h, _ := windows.CreateJobObject(nil, nil)
	if h != 0 {
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		windows.NewLazySystemDLL("kernel32.dll").NewProc("SetInformationJobObject").Call(
			uintptr(h),
			uintptr(windows.JobObjectExtendedLimitInformation),
			uintptr(unsafe.Pointer(&info)),
			uintptr(uint32(unsafe.Sizeof(info))),
		)
		hJob = h
	}
}

func monitorCore() {
	for {
		if isExiting { return }
		confMu.RLock()
		sMode := conf.ServiceMode
		confMu.RUnlock()

		if !sMode {
			// 直接拉起内核，不使用 cmd /c 避免黑窗
			runCoreDirectly()
		}
		time.Sleep(3 * time.Second)
	}
}

func runCoreDirectly() {
	// 彻底清理旧内核
	_ = exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
	time.Sleep(500 * time.Millisecond)

	if _, err := os.Stat(coreExe); os.IsNotExist(err) { return }

	cmd := exec.Command(coreExe, "-d", baseDir)
	cmd.Dir = baseDir
	// 核心修复：CREATE_NO_WINDOW 杜绝黑框
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_BREAKAWAY_FROM_JOB,
	}

	if err := cmd.Start(); err == nil {
		if hJob != 0 {
			hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
			_ = windows.AssignProcessToJobObject(hJob, hp)
			windows.CloseHandle(hp)
		}
		go patchTunAfterStart()
		_ = cmd.Wait()
	}
}

func patchTunAfterStart() {
	for i := 0; i < 15; i++ {
		time.Sleep(1 * time.Second)
		confMu.RLock()
		enabled := conf.TunEnabled
		confMu.RUnlock()
		if !enabled { break }

		body := `{"tun": {"enable": true}}`
		req, _ := http.NewRequest("PATCH", API_URL+"/configs", strings.NewReader(body))
		if resp, err := httpClient.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 { break }
		}
	}
}

// --- UI 逻辑与同步 ---

func syncLoop(mProxy, mTun, mRule, mGlobal, mDirect *systray.MenuItem) {
	for {
		if isExiting { return }
		loadIni() 

		// 代理检测
		isProxyOn := false
		k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
		if err == nil {
			v, _, _ := k.GetIntegerValue("ProxyEnable")
			isProxyOn = v == 1
			k.Close()
		}

		// 物理 TUN 网卡检测
		isTunUp := false
		ifaces, _ := net.Interfaces()
		for _, i := range ifaces {
			if strings.Contains(i.Name, TUN_ADAPTER) && i.Flags&net.FlagUp != 0 {
				isTunUp = true
				break
			}
		}

		// API 状态与图标切换修复
		resp, err := httpClient.Get(API_URL + "/configs")
		if err == nil {
			var data struct {
				Mode string `json:"mode"`
				Tun  struct { Enable bool `json:"enable"` } `json:"tun"`
			}
			if json.NewDecoder(resp.Body).Decode(&data) == nil {
				confMu.Lock()
				conf.Mode = strings.ToLower(data.Mode)
				conf.TunEnabled = data.Tun.Enable
				confMu.Unlock()
			}
			resp.Body.Close()
			
			// 根据状态设置图标
			if isTunUp {
				systray.SetIcon(getIcon("tray_tun.ico"))
			} else if isProxyOn {
				systray.SetIcon(getIcon("tray_proxy.ico"))
			} else {
				systray.SetIcon(getIcon("tray_default.ico"))
			}
		} else {
			systray.SetIcon(getIcon("tray_stop.ico"))
		}

		// 同步勾选
		if mProxy != nil {
			updateUI(mProxy, mTun, mRule, mGlobal, mDirect, isProxyOn, isTunUp)
		}
		
		time.Sleep(2 * time.Second)
	}
}

func updateUI(mProxy, mTun, mRule, mGlobal, mDirect *systray.MenuItem, proxy, tun bool) {
	if proxy { mProxy.Check() } else { mProxy.Uncheck() }
	if tun { mTun.Check() } else { mTun.Uncheck() }

	confMu.RLock()
	mRule.Uncheck(); mGlobal.Uncheck(); mDirect.Uncheck()
	switch conf.Mode {
	case "rule": mRule.Check()
	case "global": mGlobal.Check()
	case "direct": mDirect.Check()
	}
	confMu.RUnlock()
}

func onReady() {
	systray.SetIcon(getIcon("tray_default.ico"))
	
	mWeb := systray.AddMenuItem("打开控制面板", "")
	systray.AddSeparator()
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", conf.SystemProxy)
	mTun := systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", conf.TunEnabled)
	systray.AddSeparator()
	mRule := systray.AddMenuItemCheckbox("规则模式", "", conf.Mode == "rule")
	mGlobal := systray.AddMenuItemCheckbox("全局模式", "", conf.Mode == "global")
	mDirect := systray.AddMenuItemCheckbox("直连模式", "", conf.Mode == "direct")
	systray.AddSeparator()
	mRes := systray.AddMenuItem("重启内核", "")
	mSvc := systray.AddMenuItem("管理服务", "")
	mHide := systray.AddMenuItem("隐藏图标 (后台运行)", "")
	mExit := systray.AddMenuItem("退出程序", "")

	// 开启同步回路
	go syncLoop(mProxy, mTun, mRule, mGlobal, mDirect)

	for {
		select {
		case <-mProxy.ClickedCh:
			confMu.Lock()
			conf.SystemProxy = !conf.SystemProxy
			setProxyReg(conf.SystemProxy)
			confMu.Unlock()
			saveIni()
		case <-mTun.ClickedCh:
			confMu.Lock()
			conf.TunEnabled = !conf.TunEnabled
			sendPatch(fmt.Sprintf(`{"tun": {"enable": %v}}`, conf.TunEnabled))
			confMu.Unlock()
			saveIni()
		case <-mRule.ClickedCh: setMode("rule")
		case <-mGlobal.ClickedCh: setMode("global")
		case <-mDirect.ClickedCh: setMode("direct")
		case <-mWeb.ClickedCh: exec.Command("rundll32", "url.dll,FileProtocolHandler", API_URL+"/ui").Start()
		case <-mRes.ClickedCh: go runCoreDirectly()
		case <-mSvc.ClickedCh:
			serviceBat := filepath.Join(baseDir, "mihomo-service", "mihomo-service.bat")
			c := exec.Command("cmd", "/c", "start", "", "cmd", "/c", serviceBat)
			c.Dir = filepath.Dir(serviceBat)
			c.SysProcAttr = &windows.SysProcAttr{HideWindow: true}
			_ = c.Start()
		case <-mHide.ClickedCh:
			confMu.Lock()
			conf.TrayHidden = true
			confMu.Unlock()
			saveIni()
			systray.Quit()
			return
		case <-mExit.ClickedCh:
			cleanExit()
		}
	}
}

// --- 基础工具 ---

func setProxyReg(e bool) {
	k, _, _ := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.ALL_ACCESS)
	if e {
		k.SetDWordValue("ProxyEnable", 1)
		k.SetStringValue("ProxyServer", PROXY_ADDR)
	} else {
		k.SetDWordValue("ProxyEnable", 0)
	}
	k.Close()
	windows.NewLazySystemDLL("user32.dll").NewProc("UpdatePerUserSystemParameters").Call(0, 0, 0, 0)
}

func sendPatch(j string) {
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(j)))
	if resp, err := httpClient.Do(req); err == nil { resp.Body.Close() }
}

func setMode(m string) {
	confMu.Lock()
	conf.Mode = m
	sendPatch(fmt.Sprintf(`{"mode": "%s"}`, m))
	confMu.Unlock()
	saveIni()
}

func loadIni() {
	f, err := os.ReadFile(iniPath)
	if err != nil { return }
	s := string(f)
	confMu.Lock()
	conf.TrayHidden = strings.Contains(s, "tray_hidden=true")
	conf.TunEnabled = strings.Contains(s, "tun_enabled=true")
	conf.SystemProxy = strings.Contains(s, "system_proxy=true")
	conf.ServiceMode = strings.Contains(s, "service_mode=true")
	if strings.Contains(s, "mode=rule") { conf.Mode = "rule" }
	if strings.Contains(s, "mode=global") { conf.Mode = "global" }
	if strings.Contains(s, "mode=direct") { conf.Mode = "direct" }
	confMu.Unlock()
}

func saveIni() {
	confMu.RLock()
	defer confMu.RUnlock()
	f, _ := os.Create(iniPath)
	defer f.Close()
	fmt.Fprintf(f, "tray_hidden=%v\ntun_enabled=%v\nsystem_proxy=%v\nmode=%s\nservice_mode=%v\n",
		conf.TrayHidden, conf.TunEnabled, conf.SystemProxy, conf.Mode, conf.ServiceMode)
}

func cleanExit() {
	isExiting = true
	setProxyReg(false)
	if hJob != 0 { windows.CloseHandle(hJob) }
	os.Exit(0)
}

func getIcon(n string) []byte {
	// 兼容之前的图标命名
	data, _ := iconFs.ReadFile("icons/" + n)
	return data
}

// --- 入口 ---

func main() {
	if !isAdmin() { runAsAdmin(); return }

	p, _ := os.Executable()
	baseDir = filepath.Dir(p)
	coreExe = filepath.Join(baseDir, "mihomo.exe")
	iniPath = filepath.Join(baseDir, "mihomo-launcher.ini")

	// 1. 核心 Mutex 修复：必须在一切逻辑之前执行
	mName := windows.StringToUTF16Ptr(APP_MUTEX)
	hM, err := windows.CreateMutex(nil, false, mName)
	if err != nil || windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		// 救活逻辑：只改配置，然后闪退
		loadIni()
		conf.TrayHidden = false
		saveIni()
		if hM != 0 { windows.CloseHandle(hM) }
		os.Exit(0)
	}

	loadIni()
	initJobObject()
	go monitorCore()

	// 2. 托盘显示监听回路
	for {
		if isExiting { break }
		loadIni()
		if !conf.TrayHidden {
			systray.Run(onReady, nil)
		}
		time.Sleep(2 * time.Second)
	}
}
