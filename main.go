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
	APP_MUTEX    = "Global\\MihomoLauncherMutex"
	TUN_ADAPTER  = "Mihomo" // 物理网卡关键字
)

var (
	conf        Config
	confMu      sync.RWMutex
	baseDir     string
	coreExe     string
	iniPath     string
	hJob        windows.Handle
	isExiting   bool
	httpClient  = &http.Client{Timeout: 1 * time.Second}
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
	_ = windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &t)
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

// --- 核心守护逻辑 (骨架1精华) ---

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
			runCoreAndExclude()
		}
		time.Sleep(2 * time.Second) // 守护频率，防止闪退死循环
	}
}

func runCoreAndExclude() {
	// 强制清理残余
	_ = exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
	time.Sleep(500 * time.Millisecond)

	if _, err := os.Stat(coreExe); os.IsNotExist(err) { return }

	cmd := exec.Command(coreExe, "-d", baseDir)
	cmd.Dir = baseDir
	// 关键：不显示窗口，允许脱离当前 Job 后重新分配
	cmd.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_BREAKAWAY_FROM_JOB,
	}

	if err := cmd.Start(); err == nil {
		if hJob != 0 {
			hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
			_ = windows.AssignProcessToJobObject(hJob, hp)
			windows.CloseHandle(hp)
		}
		
		// 启动后根据 INI 状态补齐 TUN 配置 (骨架1补丁)
		go patchTunOnStart()

		_ = cmd.Wait() // 阻塞点：内核死，此处才会继续执行重启循环
	}
}

func patchTunOnStart() {
	time.Sleep(2 * time.Second)
	confMu.RLock()
	needTun := conf.TunEnabled
	confMu.RUnlock()
	if needTun {
		for i := 0; i < 10; i++ { // 尝试 10 次直到 API 就绪
			body := `{"tun": {"enable": true}}`
			req, _ := http.NewRequest("PATCH", API_URL+"/configs", strings.NewReader(body))
			if resp, err := httpClient.Do(req); err == nil {
				resp.Body.Close()
				if resp.StatusCode == 204 || resp.StatusCode == 200 { break }
			}
			time.Sleep(1 * time.Second)
		}
	}
}

// --- 状态同步与 UI (骨架2精华) ---

func syncStateLoop(mProxy, mTun, mRule, mGlobal, mDirect *systray.MenuItem) {
	for {
		if isExiting { return }
		loadIni() // 实时加载，支持双击救活时的配置变更

		// 1. 系统代理检测
		isProxyOn := false
		k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
		if err == nil {
			v, _, _ := k.GetIntegerValue("ProxyEnable")
			isProxyOn = v == 1
			k.Close()
		}

		// 2. 物理网卡检测 (TUN 状态最准判据)
		isTunUp := false
		ifaces, _ := net.Interfaces()
		for _, i := range ifaces {
			if strings.Contains(i.Name, TUN_ADAPTER) && i.Flags&net.FlagUp != 0 {
				isTunUp = true
				break
			}
		}

		// 3. API 状态拉取
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
			systray.SetIcon(getIcon("default.ico"))
		} else {
			systray.SetIcon(getIcon("stop.ico")) // 内核不通变色
		}

		// 更新 UI 勾选
		updateUI(mProxy, mTun, mRule, mGlobal, mDirect, isProxyOn, isTunUp)
		time.Sleep(3 * time.Second)
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

// --- 托盘事件控制 ---

func onReady() {
	systray.SetIcon(getIcon("default.ico"))
	
	mWeb := systray.AddMenuItem("控制面板", "")
	systray.AddSeparator()
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", conf.SystemProxy)
	mTun := systray.AddMenuItemCheckbox("TUN 模式", "", conf.TunEnabled)
	systray.AddSeparator()
	mRule := systray.AddMenuItemCheckbox("规则模式", "", conf.Mode == "rule")
	mGlobal := systray.AddMenuItemCheckbox("全局模式", "", conf.Mode == "global")
	mDirect := systray.AddMenuItemCheckbox("直连模式", "", conf.Mode == "direct")
	systray.AddSeparator()
	mSet := systray.AddMenuItem("高级管理", "")
	mRes := mSet.AddSubMenuItem("手动拉起/重启内核", "")
	mSvc := mSet.AddSubMenuItem("管理系统服务 (BAT)", "")
	mHide := mSet.AddSubMenuItem("后台运行 (隐藏托盘)", "")
	mExit := mSet.AddSubMenuItem("完全退出", "")

	go syncStateLoop(mProxy, mTun, mRule, mGlobal, mDirect)

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
		case <-mWeb.ClickedCh: windows.ShellExecute(0, nil, syscall.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
		case <-mRes.ClickedCh: go runCoreAndExclude()
		case <-mHide.ClickedCh:
			confMu.Lock()
			conf.TrayHidden = true
			confMu.Unlock()
			saveIni()
			os.Exit(0) // 隐藏即退出 UI 进程，依靠守护进程存活
		case <-mExit.ClickedCh:
			cleanExit()
		}
	}
}

// --- 基础工具 (Registry, INI, Exit) ---

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
	conf.AutoStart = strings.Contains(s, "auto_start=true")
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
	fmt.Fprintf(f, "auto_start=%v\ntray_hidden=%v\ntun_enabled=%v\nsystem_proxy=%v\nmode=%s\nservice_mode=%v\n",
		conf.AutoStart, conf.TrayHidden, conf.TunEnabled, conf.SystemProxy, conf.Mode, conf.ServiceMode)
}

func cleanExit() {
	isExiting = true
	setProxyReg(false)
	if hJob != 0 { windows.CloseHandle(hJob) } // 关闭 Job 句柄，内核自动陪葬
	os.Exit(0)
}

func getIcon(n string) []byte {
	d, _ := iconFs.ReadFile("icons/" + n)
	return d
}

// --- 入口 ---

func main() {
	if !isAdmin() { runAsAdmin(); return }

	p, _ := os.Executable()
	baseDir = filepath.Dir(p)
	coreExe = filepath.Join(baseDir, "mihomo.exe")
	iniPath = filepath.Join(baseDir, "mihomo-launcher.ini")

	// 1. 单实例与救活逻辑
	mutex, err := windows.CreateMutex(nil, false, syscall.StringToUTF16Ptr(APP_MUTEX))
	if err != nil || windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		// 救活：新进程写配置，旧进程轮询会发现 TrayHidden 变 false 并显形
		loadIni()
		conf.TrayHidden = false
		saveIni()
		os.Exit(0)
	}
	defer windows.CloseHandle(mutex)

	loadIni()
	initJobObject()

	// 2. 独立协程启动守护回路
	go monitorCore()

	// 3. UI 显隐逻辑
	if conf.TrayHidden {
		// 如果隐藏模式启动，主线程仅保持同步
		for {
			loadIni()
			if !conf.TrayHidden { break } // 被救活了，跳出循环进入托盘
			time.Sleep(2 * time.Second)
		}
	}

	systray.Run(onReady, nil)
}
