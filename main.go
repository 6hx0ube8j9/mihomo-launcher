package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	APP_MUTEX    = "Global\\MihomoLauncherMutexV8"
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
	httpClient  = &http.Client{Timeout: 1 * time.Second}
)

type Config struct {
	TunEnabled  bool
	TrayHidden  bool
	SystemProxy bool
	Mode        string
}

// --- 内核管理 ---

func initJobObject() {
	h, _ := windows.CreateJobObject(nil, nil)
	if h != 0 {
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		windows.NewLazySystemDLL("kernel32.dll").NewProc("SetInformationJobObject").Call(
			uintptr(h), uintptr(windows.JobObjectExtendedLimitInformation),
			uintptr(unsafe.Pointer(&info)), uintptr(uint32(unsafe.Sizeof(info))),
		)
		hJob = h
	}
}

func monitorCore() {
	for {
		if isExiting { return }
		resp, err := httpClient.Get(API_URL)
		if err != nil {
			runCore()
		} else {
			resp.Body.Close()
		}
		time.Sleep(5 * time.Second)
	}
}

func runCore() {
	_ = exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
	time.Sleep(500 * time.Millisecond)

	if _, err := os.Stat(coreExe); err == nil {
		cmd := exec.Command(coreExe, "-d", baseDir)
		cmd.Dir = baseDir
		cmd.SysProcAttr = &windows.SysProcAttr{
			CreationFlags: windows.CREATE_NO_WINDOW | 0x00000008,
		}
		if err := cmd.Start(); err == nil && hJob != 0 {
			hp, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
			_ = windows.AssignProcessToJobObject(hJob, hp)
			windows.CloseHandle(hp)
		}
	}
}

// --- 托盘 UI ---

func onReady() {
	systray.SetIcon(getIcon("default.ico"))

	mWeb := systray.AddMenuItem("打开控制面板", "")
	systray.AddSeparator()
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", false)
	mTun := systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", false)
	systray.AddSeparator()
	mRule := systray.AddMenuItemCheckbox("规则模式", "", false)
	mGlobal := systray.AddMenuItemCheckbox("全局模式", "", false)
	mDirect := systray.AddMenuItemCheckbox("直连模式", "", false)
	systray.AddSeparator()
	mRes := systray.AddMenuItem("重启内核", "")
	mSvc := systray.AddMenuItem("管理服务", "")
	mHide := systray.AddMenuItem("隐藏图标 (后台运行)", "")
	mExit := systray.AddMenuItem("完全退出程序", "")

	go func() {
		for {
			if isExiting { return }
			loadIni()
			confMu.RLock()
			isHidden := conf.TrayHidden
			confMu.RUnlock()

			if isHidden {
				systray.SetIcon([]byte{})
			} else {
				refreshUI(mProxy, mTun, mRule, mGlobal, mDirect)
			}
			time.Sleep(2 * time.Second)
		}
	}()

	for {
		select {
		case <-mProxy.ClickedCh: toggleProxy()
		case <-mTun.ClickedCh: toggleTun()
		case <-mRule.ClickedCh: setMode("rule")
		case <-mGlobal.ClickedCh: setMode("global")
		case <-mDirect.ClickedCh: setMode("direct")
		case <-mWeb.ClickedCh: 
			windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
		case <-mRes.ClickedCh: runCore()
		case <-mSvc.ClickedCh:
			bat := filepath.Join(baseDir, "mihomo-service", "mihomo-service.bat")
			c := exec.Command("cmd", "/c", bat)
			c.SysProcAttr = &windows.SysProcAttr{HideWindow: true}
			_ = c.Start()
		case <-mHide.ClickedCh:
			confMu.Lock()
			conf.TrayHidden = true
			confMu.Unlock()
			saveIni()
		case <-mExit.ClickedCh:
			cleanExit()
		}
	}
}

func refreshUI(mProxy, mTun, mRule, mGlobal, mDirect *systray.MenuItem) {
	isP := false
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err == nil {
		if v, _, _ := k.GetIntegerValue("ProxyEnable"); v == 1 { isP = true }
		k.Close()
	}

	resp, err := httpClient.Get(API_URL + "/configs")
	if err == nil {
		var d struct {
			Mode string `json:"mode"`
			Tun  struct{ Enable bool } `json:"tun"`
		}
		json.NewDecoder(resp.Body).Decode(&d)
		resp.Body.Close()
		
		confMu.Lock()
		conf.Mode = strings.ToLower(d.Mode)
		conf.TunEnabled = d.Tun.Enable
		confMu.Unlock()

		if d.Tun.Enable {
			systray.SetIcon(getIcon("tun.ico"))
		} else if isP {
			systray.SetIcon(getIcon("proxy.ico"))
		} else {
			systray.SetIcon(getIcon("default.ico"))
		}
	} else {
		systray.SetIcon(getIcon("stop.ico"))
	}

	confMu.RLock()
	if isP { mProxy.Check() } else { mProxy.Uncheck() }
	if conf.TunEnabled { mTun.Check() } else { mTun.Uncheck() }
	mRule.Uncheck(); mGlobal.Uncheck(); mDirect.Uncheck()
	switch conf.Mode {
	case "rule": mRule.Check()
	case "global": mGlobal.Check()
	case "direct": mDirect.Check()
	}
	confMu.RUnlock()
}

// --- 基础工具 ---

func toggleProxy() {
	confMu.Lock()
	conf.SystemProxy = !conf.SystemProxy
	state := conf.SystemProxy
	confMu.Unlock()
	k, _, _ := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.ALL_ACCESS)
	if state {
		k.SetDWordValue("ProxyEnable", 1)
		k.SetStringValue("ProxyServer", PROXY_ADDR)
	} else {
		k.SetDWordValue("ProxyEnable", 0)
	}
	k.Close()
	windows.NewLazySystemDLL("user32.dll").NewProc("UpdatePerUserSystemParameters").Call(0, 0, 0, 0)
}

func toggleTun() {
	confMu.Lock()
	val := !conf.TunEnabled
	confMu.Unlock()
	body := fmt.Sprintf(`{"tun": {"enable": %v}}`, val)
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", strings.NewReader(body))
	if r, err := httpClient.Do(req); err == nil { r.Body.Close() }
}

func setMode(m string) {
	body := fmt.Sprintf(`{"mode": "%s"}`, m)
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", strings.NewReader(body))
	if r, err := httpClient.Do(req); err == nil { r.Body.Close() }
}

func loadIni() {
	f, err := os.ReadFile(iniPath)
	if err != nil { return }
	s := string(f)
	confMu.Lock()
	conf.TrayHidden = strings.Contains(s, "tray_hidden=true")
	conf.TunEnabled = strings.Contains(s, "tun_enabled=true")
	conf.SystemProxy = strings.Contains(s, "system_proxy=true")
	confMu.Unlock()
}

func saveIni() {
	confMu.RLock()
	defer confMu.RUnlock()
	f, _ := os.Create(iniPath)
	fmt.Fprintf(f, "tray_hidden=%v\ntun_enabled=%v\nsystem_proxy=%v\n", conf.TrayHidden, conf.TunEnabled, conf.SystemProxy)
	f.Close()
}

func getIcon(n string) []byte {
	b, _ := iconFs.ReadFile("icons/" + n)
	return b
}

func cleanExit() {
	isExiting = true
	k, _, _ := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.ALL_ACCESS)
	k.SetDWordValue("ProxyEnable", 0)
	k.Close()
	if hJob != 0 { windows.CloseHandle(hJob) }
	os.Exit(0)
}

func main() {
	p, _ := os.Executable()
	baseDir = filepath.Dir(p)
	coreExe = filepath.Join(baseDir, "mihomo.exe")
	iniPath = filepath.Join(baseDir, "mihomo-launcher.ini")

	mName := windows.StringToUTF16Ptr(APP_MUTEX)
	hM, err := windows.CreateMutex(nil, false, mName)
	// 修正：哪怕不使用 hM，也要处理 err 或者将其忽略以通过编译
	if err != nil || windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		if hM != 0 { windows.CloseHandle(hM) }
		loadIni()
		conf.TrayHidden = false
		saveIni()
		return 
	}

	initJobObject()
	loadIni()
	go monitorCore()
	systray.Run(onReady, nil)
}
