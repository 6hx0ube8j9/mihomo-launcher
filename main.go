package main

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"io"
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
	startMu         sync.Mutex
	lastStart       time.Time
	hJob            windows.Handle
	httpClient      = &http.Client{Timeout: 2 * time.Second}
)

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

func initPaths() error {
	p, _ := os.Executable()
	fullExeP = p
	baseDir = filepath.Dir(fullExeP)
	coreExe = filepath.Join(baseDir, "mihomo.exe")
	svcExe = filepath.Join(baseDir, "mihomo-service", "mihomo-service.exe")
	iniPath = filepath.Join(baseDir, "mihomo-launcher.ini")
	return nil
}

func tryStartCore() {
	startMu.Lock()
	defer startMu.Unlock()
	if time.Since(lastStart) < 5*time.Second { return }
	if _, err := os.Stat(coreExe); os.IsNotExist(err) { return }

	cmd := exec.Command(coreExe, "-d", baseDir)
	cmd.Dir = baseDir
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	if err := cmd.Start(); err == nil {
		lastStart = time.Now()
		if hJob != 0 {
			hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
			windows.AssignProcessToJobObject(hJob, hp)
			windows.CloseHandle(hp)
		}
		wg.Add(1)
		go func() { defer wg.Done(); _ = cmd.Wait() }()
	}
}

func engineKeeper() {
	defer wg.Done()
	ticker := time.NewTicker(3 * time.Second)
	for {
		select {
		case <-mainCtx.Done(): return
		case <-ticker.C:
			confMu.RLock()
			isSvc := conf.ServiceMode
			confMu.RUnlock()
			if !isSvc {
				if resp, err := httpClient.Get(API_URL + "/version"); err != nil {
					tryStartCore()
				} else {
					resp.Body.Close()
				}
			}
			syncStatus()
		}
	}
}

func syncStatus() {
	resp, err := httpClient.Get(API_URL + "/configs")
	if err != nil { return }
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	data := string(b)

	confMu.Lock()
	defer confMu.Unlock()
	
	// 从内核同步状态到托盘
	currTun := strings.Contains(data, `"tun":{"enable":true`)
	if conf.TunEnabled != currTun {
		conf.TunEnabled = currTun
		saveIni()
	}
	if isProxyInReg() != conf.SystemProxy {
		setProxyReg(conf.SystemProxy)
	}
}

func onReady() {
	confMu.RLock()
	systray.SetIcon(getIcon("default.ico"))
	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	mDir := systray.AddMenuItem("打开程序目录", "")
	systray.AddSeparator()
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", conf.SystemProxy)
	mTun := systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", conf.TunEnabled)
	systray.AddSeparator()
	mRule := systray.AddMenuItemCheckbox("规则模式", "", conf.Mode == "rule")
	mGlobal := systray.AddMenuItemCheckbox("全局模式", "", conf.Mode == "global")
	mDirect := systray.AddMenuItemCheckbox("直连模式", "", conf.Mode == "direct")
	systray.AddSeparator()
	mAuto := systray.AddMenuItemCheckbox("开机启动", "", conf.AutoStart)
	mRes := systray.AddMenuItem("重启内核", "")
	mFull := systray.AddMenuItem("彻底退出", "")
	systray.AddSeparator()
	mHide := systray.AddMenuItem("隐藏图标", "")
	confMu.RUnlock()

	go func() {
		for range time.Tick(2 * time.Second) {
			confMu.RLock()
			if conf.SystemProxy { mProxy.Check() } else { mProxy.Uncheck() }
			if conf.TunEnabled { mTun.Check() } else { mTun.Uncheck() }
			if conf.Mode == "rule" { mRule.Check(); mGlobal.Uncheck(); mDirect.Uncheck() }
			if conf.Mode == "global" { mRule.Uncheck(); mGlobal.Check(); mDirect.Uncheck() }
			if conf.Mode == "direct" { mRule.Uncheck(); mGlobal.Uncheck(); mDirect.Check() }
			confMu.RUnlock()
			refreshIcon()
		}
	}()

	for {
		select {
		case <-mWeb.ClickedCh: windows.ShellExecute(0, nil, syscall.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
		case <-mDir.ClickedCh: windows.ShellExecute(0, nil, syscall.StringToUTF16Ptr(baseDir), nil, nil, windows.SW_SHOWNORMAL)
		case <-mProxy.ClickedCh:
			confMu.Lock()
			conf.SystemProxy = !mProxy.Checked()
			confMu.Unlock()
			saveIni()
		case <-mTun.ClickedCh:
			confMu.Lock()
			newVal := !mTun.Checked()
			conf.TunEnabled = newVal
			sendPatch(fmt.Sprintf(`{"tun": {"enable": %v}}`, newVal))
			confMu.Unlock()
			saveIni()
		case <-mRule.ClickedCh: updateMode("rule")
		case <-mGlobal.ClickedCh: updateMode("global")
		case <-mDirect.ClickedCh: updateMode("direct")
		case <-mAuto.ClickedCh:
			confMu.Lock()
			conf.AutoStart = !mAuto.Checked()
			updateAutoStart(conf.AutoStart)
			confMu.Unlock()
			saveIni()
		case <-mRes.ClickedCh: 
			killProcess("mihomo.exe")
		case <-mFull.ClickedCh: cleanExit()
		case <-mHide.ClickedCh:
			confMu.Lock()
			conf.TrayHidden = true
			confMu.Unlock()
			saveIni()
			systray.Quit()
		}
	}
}

func updateMode(m string) {
	confMu.Lock()
	conf.Mode = m
	sendPatch(fmt.Sprintf(`{"mode": "%s"}`, m))
	confMu.Unlock()
	saveIni()
}

func cleanExit() {
	cancel()
	setProxyReg(false)
	killProcess("mihomo.exe")
	if hJob != 0 { windows.CloseHandle(hJob) }
	os.Exit(0)
}

func killProcess(name string) {
	cmd := exec.Command("taskkill", "/F", "/T", "/IM", name)
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	_ = cmd.Run()
}

func refreshIcon() {
	resp, err := httpClient.Get(API_URL + "/configs")
	if err != nil { systray.SetIcon(getIcon("stop.ico")); return }
	resp.Body.Close()
	confMu.RLock()
	defer confMu.RUnlock()
	if conf.TunEnabled { systray.SetIcon(getIcon("tun.ico"))
	} else if conf.SystemProxy { systray.SetIcon(getIcon("proxy.ico"))
	} else { systray.SetIcon(getIcon("default.ico")) }
}

func isProxyInReg() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil { return false }
	defer k.Close()
	v, _, _ := k.GetIntegerValue("ProxyEnable")
	return v == 1
}

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
	fmt.Fprintf(f, "auto_start=%v\ntray_hidden=%v\ntun_enabled=%v\nsystem_proxy=%v\nmode=%s\n", 
		conf.AutoStart, conf.TrayHidden, conf.TunEnabled, conf.SystemProxy, conf.Mode)
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
	if strings.Contains(s, "mode=global") { conf.Mode = "global" 
	} else if strings.Contains(s, "mode=direct") { conf.Mode = "direct" }
}

func main() {
	if !isAdmin() { runAsAdmin(); return }
	initPaths()
	initJobObject()

	_, err := windows.CreateMutex(nil, false, syscall.StringToUTF16Ptr(APP_MUTEX))
	if err != nil {
		conn, err := net.DialTimeout("tcp", IPC_PORT, 500*time.Millisecond)
		if err == nil {
			conn.Write([]byte("SHOW"))
			conn.Close()
		}
		os.Exit(0)
	}

	loadIni()
	isSilent := false
	for _, a := range os.Args { if a == "-silent" { isSilent = true } }
	if !isSilent { conf.TrayHidden = false }

	wg.Add(1)
	go func() {
		defer wg.Done()
		ln, _ := net.Listen("tcp", IPC_PORT)
		for {
			c, err := ln.Accept()
			if err != nil { return }
			buf := make([]byte, 4)
			c.Read(buf)
			if string(buf) == "SHOW" {
				// 收到 SHOW 指令，如果当前是隐藏状态，则退出当前并重启带 UI 的进程
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
