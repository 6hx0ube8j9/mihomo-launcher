package main

import (
	"bytes"
	"context"
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
	IPC_PORT     = "127.0.0.1:54321"
	SERVICE_NAME = "Mihomo"
	APP_MUTEX    = "Global\\MihomoLauncherMutex"
)

type Config struct {
	AutoStart   bool
	ServiceMode bool
	TunEnabled  bool
	TrayHidden  bool
	SystemProxy bool
	Mode        string
}

var (
	conf        Config
	confMu      sync.RWMutex
	fullExeP    string
	baseDir     string
	coreExe     string
	svcExe      string
	iniPath     string

	mainCtx, cancel = context.WithCancel(context.Background())
	wg              sync.WaitGroup
	hJob            windows.Handle
	httpClient      = &http.Client{Timeout: 2 * time.Second}
)

// --- 基础工具 ---

func isAdmin() bool {
	var t windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &t); err != nil {
		return false
	}
	defer t.Close()
	return t.IsElevated()
}

func runAsAdmin() {
	verb, _ := syscall.UTF16PtrFromString("runas")
	exe, _ := os.Executable()
	exePtr, _ := syscall.UTF16PtrFromString(exe)
	cwd, _ := os.Getwd()
	cwdPtr, _ := syscall.UTF16PtrFromString(cwd)
	var argsPtr *uint16
	if len(os.Args) > 1 {
		argsPtr, _ = syscall.UTF16PtrFromString(strings.Join(os.Args[1:], " "))
	}
	_ = windows.ShellExecute(0, verb, exePtr, argsPtr, cwdPtr, windows.SW_SHOWNORMAL)
}

func initJobObject() {
	h, _ := windows.CreateJobObject(nil, nil)
	if h != 0 {
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		windows.NewLazySystemDLL("kernel32.dll").NewProc("SetInformationJobObject").Call(uintptr(h), uintptr(windows.JobObjectExtendedLimitInformation), uintptr(unsafe.Pointer(&info)), uintptr(uint32(unsafe.Sizeof(info))))
		hJob = h
	}
}

// --- 原版 BAT 逻辑集成 ---

func manageService(action string) {
	switch action {
	case "install":
		// 模拟原版 BAT: 先清理后安装
		_ = exec.Command("sc", "stop", SERVICE_NAME).Run()
		_ = exec.Command("sc", "delete", SERVICE_NAME).Run()
		time.Sleep(500 * time.Millisecond)
		
		// 创建服务并配置自动启动
		cmd := exec.Command("sc", "create", SERVICE_NAME, "binPath=", svcExe, "start=", "auto", "DisplayName=", "Mihomo Service")
		cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		_ = cmd.Run()
		
		_ = exec.Command("sc", "description", SERVICE_NAME, "Mihomo Kernel Service Managed by Launcher").Run()
		_ = exec.Command("sc", "start", SERVICE_NAME).Run()
		
	case "uninstall":
		// 模拟原版 BAT: 停止并删除
		_ = exec.Command("sc", "stop", SERVICE_NAME).Run()
		_ = exec.Command("sc", "delete", SERVICE_NAME).Run()
	}
	
	// 更新本地配置中的服务状态
	query := exec.Command("sc", "query", SERVICE_NAME)
	query.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	out, _ := query.Output()
	
	confMu.Lock()
	conf.ServiceMode = strings.Contains(string(out), "RUNNING")
	confMu.Unlock()
	saveIni()
}

// --- 核心调度逻辑 ---

func engineKeeper() {
	defer wg.Done()
	ticker := time.NewTicker(3 * time.Second)
	for {
		select {
		case <-mainCtx.Done():
			return
		case <-ticker.C:
			confMu.RLock()
			isSvc := conf.ServiceMode
			confMu.RUnlock()
			
			if !isSvc {
				if resp, err := httpClient.Get(API_URL + "/version"); err != nil {
					startCore()
				} else {
					resp.Body.Close()
				}
			}
			syncStateFromCore()
		}
	}
}

func startCore() {
	if _, err := os.Stat(coreExe); os.IsNotExist(err) { return }
	cmd := exec.Command(coreExe, "-d", baseDir)
	cmd.Dir = baseDir
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	if err := cmd.Start(); err == nil {
		if hJob != 0 {
			hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
			windows.AssignProcessToJobObject(hJob, hp)
			windows.CloseHandle(hp)
		}
	}
}

func syncStateFromCore() {
	resp, err := httpClient.Get(API_URL + "/configs")
	if err != nil { return }
	defer resp.Body.Close()

	var data struct {
		Mode string `json:"mode"`
		Tun  struct { Enable bool `json:"enable"` } `json:"tun"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err == nil {
		confMu.Lock()
		conf.TunEnabled = data.Tun.Enable
		conf.Mode = strings.ToLower(data.Mode)
		confMu.Unlock()
	}
}

// --- UI 逻辑 ---

func onReady() {
	confMu.RLock()
	systray.SetIcon(getIcon("default.ico"))
	
	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	systray.AddSeparator()
	
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", conf.SystemProxy)
	mTun := systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", conf.TunEnabled)
	systray.AddSeparator()
	
	mRule := systray.AddMenuItemCheckbox("规则模式", "", conf.Mode == "rule")
	mGlobal := systray.AddMenuItemCheckbox("全局模式", "", conf.Mode == "global")
	mDirect := systray.AddMenuItemCheckbox("直连模式", "", conf.Mode == "direct")
	systray.AddSeparator()
	
	// 高级管理子菜单
	mSet := systray.AddMenuItem("高级管理", "")
	mAuto := mSet.AddSubMenuItemCheckbox("开机启动", "", conf.AutoStart)
	mSvcInst := mSet.AddSubMenuItem("安装/修复服务模式", "")
	mSvcUninst := mSet.AddSubMenuItem("停止/卸载服务模式", "")
	mRes := mSet.AddSubMenuItem("重启内核进程", "")
	mExit := mSet.AddSubMenuItem("完全退出", "") // 完全退出并入此处
	
	systray.AddSeparator()
	mDir := systray.AddMenuItem("打开程序目录", "") // 隐藏图标上方
	mHide := systray.AddMenuItem("隐藏托盘图标", "")
	confMu.RUnlock()

	// 状态更新协程
	go func() {
		for range time.Tick(2 * time.Second) {
			confMu.RLock()
			if conf.TunEnabled { mTun.Check() } else { mTun.Uncheck() }
			if conf.SystemProxy { mProxy.Check() } else { mProxy.Uncheck() }
			mRule.Uncheck(); mGlobal.Uncheck(); mDirect.Uncheck()
			switch conf.Mode {
			case "rule": mRule.Check()
			case "global": mGlobal.Check()
			case "direct": mDirect.Check()
			}
			confMu.RUnlock()
			updateTrayIcon()
		}
	}()

	for {
		select {
		case <-mWeb.ClickedCh: windows.ShellExecute(0, nil, syscall.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
		case <-mDir.ClickedCh: windows.ShellExecute(0, nil, syscall.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		case <-mProxy.ClickedCh:
			confMu.Lock()
			conf.SystemProxy = !conf.SystemProxy
			setProxyReg(conf.SystemProxy)
			confMu.Unlock()
			saveIni()
		case <-mTun.ClickedCh:
			confMu.Lock()
			newVal := !conf.TunEnabled
			sendPatch(fmt.Sprintf(`{"tun": {"enable": %v}}`, newVal))
			conf.TunEnabled = newVal
			confMu.Unlock()
			saveIni()
		case <-mRule.ClickedCh: setMode("rule")
		case <-mGlobal.ClickedCh: setMode("global")
		case <-mDirect.ClickedCh: setMode("direct")
		case <-mAuto.ClickedCh:
			confMu.Lock()
			conf.AutoStart = !conf.AutoStart
			updateAutoStart(conf.AutoStart)
			confMu.Unlock()
			saveIni()
		case <-mSvcInst.ClickedCh: manageService("install")
		case <-mSvcUninst.ClickedCh: manageService("uninstall")
		case <-mRes.ClickedCh: 
			_ = exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
		case <-mHide.ClickedCh:
			confMu.Lock()
			conf.TrayHidden = true
			confMu.Unlock()
			saveIni()
			systray.Quit()
		case <-mExit.ClickedCh:
			cleanExit()
		}
	}
}

func setMode(m string) {
	confMu.Lock()
	conf.Mode = m
	sendPatch(fmt.Sprintf(`{"mode": "%s"}`, m))
	confMu.Unlock()
	saveIni()
}

func updateTrayIcon() {
	confMu.RLock()
	defer confMu.RUnlock()
	if conf.TunEnabled { systray.SetIcon(getIcon("tun.ico"))
	} else if conf.SystemProxy { systray.SetIcon(getIcon("proxy.ico"))
	} else { systray.SetIcon(getIcon("default.ico")) }
}

func cleanExit() {
	cancel()
	setProxyReg(false)
	_ = exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
	if hJob != 0 { windows.CloseHandle(hJob) }
	os.Exit(0)
}

// --- 注册表与 API ---

func setProxyReg(e bool) {
	k, _, _ := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.ALL_ACCESS)
	defer k.Close()
	if e {
		k.SetDWordValue("ProxyEnable", 1)
		k.SetStringValue("ProxyServer", PROXY_ADDR)
	} else {
		k.SetDWordValue("ProxyEnable", 0)
	}
	windows.NewLazySystemDLL("user32.dll").NewProc("UpdatePerUserSystemParameters").Call(0, 0, 0, 0)
}

func updateAutoStart(e bool) {
	k, _, _ := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.ALL_ACCESS)
	defer k.Close()
	if e { k.SetStringValue("MihomoLauncher", "\""+fullExeP+"\" -silent")
	} else { k.DeleteValue("MihomoLauncher") }
}

func sendPatch(j string) {
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(j)))
	if resp, err := httpClient.Do(req); err == nil { resp.Body.Close() }
}

func getIcon(n string) []byte {
	d, _ := iconFs.ReadFile("icons/" + n)
	return d
}

func saveIni() {
	f, _ := os.Create(iniPath)
	defer f.Close()
	fmt.Fprintf(f, "auto_start=%v\ntray_hidden=%v\ntun_enabled=%v\nsystem_proxy=%v\nmode=%s\nservice_mode=%v\n", 
		conf.AutoStart, conf.TrayHidden, conf.TunEnabled, conf.SystemProxy, conf.Mode, conf.ServiceMode)
}

func loadIni() {
	conf.Mode = "rule"
	f, err := os.ReadFile(iniPath)
	if err != nil { return }
	s := string(f)
	conf.AutoStart = strings.Contains(s, "auto_start=true")
	conf.TrayHidden = strings.Contains(s, "tray_hidden=true")
	conf.TunEnabled = strings.Contains(s, "tun_enabled=true")
	conf.SystemProxy = strings.Contains(s, "system_proxy=true")
	conf.ServiceMode = strings.Contains(s, "service_mode=true")
	if strings.Contains(s, "mode=global") { conf.Mode = "global" 
	} else if strings.Contains(s, "mode=direct") { conf.Mode = "direct" }
}

func main() {
	if !isAdmin() { runAsAdmin(); return }

	p, _ := os.Executable()
	fullExeP = p
	baseDir = filepath.Dir(fullExeP)
	coreExe = filepath.Join(baseDir, "mihomo.exe")
	svcExe = filepath.Join(baseDir, "mihomo-service", "mihomo-service.exe")
	iniPath = filepath.Join(baseDir, "mihomo-launcher.ini")
	
	initJobObject()

	_, err := windows.CreateMutex(nil, false, syscall.StringToUTF16Ptr(APP_MUTEX))
	if err != nil {
		conn, err := net.DialTimeout("tcp", IPC_PORT, 500*time.Millisecond)
		if err == nil {
			conn.Write([]byte("WAKE"))
			conn.Close()
		}
		os.Exit(0)
	}

	loadIni()
	for _, a := range os.Args { if a == "-silent" { conf.TrayHidden = true } }

	wg.Add(1)
	go func() {
		defer wg.Done()
		ln, err := net.Listen("tcp", IPC_PORT)
		if err != nil { return }
		defer ln.Close()
		for {
			c, err := ln.Accept()
			if err != nil { return }
			buf := make([]byte, 4)
			c.Read(buf)
			if string(buf) == "WAKE" {
				windows.ShellExecute(0, nil, syscall.StringToUTF16Ptr(fullExeP), nil, nil, windows.SW_SHOWNORMAL)
				os.Exit(0)
			}
			c.Close()
		}
	}()

	wg.Add(1)
	go engineKeeper()

	if conf.TrayHidden {
		select {}
	} else {
		systray.Run(onReady, nil)
	}
}
