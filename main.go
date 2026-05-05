package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	API_URL    = "http://127.0.0.1:9090"
	PROXY_ADDR = "127.0.0.1:7890"
	TUN_NAME   = "Mihomo"
	IPC_PORT   = "127.0.0.1:54321" // 用于唤醒隐藏实例
)

type Config struct {
	AutoStart   bool `json:"auto_start"`
	ServiceMode bool `json:"service_mode"`
	TunEnabled  bool `json:"tun_enabled"`
	TrayHidden  bool `json:"tray_hidden"`
}

var (
	conf       Config
	exePath, _ = os.Executable()
	baseDir    = filepath.Dir(exePath)
	coreExe    = filepath.Join(baseDir, "bin", "mihomo.exe")
	hJob       windows.Handle
	quitCh     = make(chan bool) // 用于通知 IPC 退出
)

// --- IPC 通讯：解决双击唤醒 ---

func handleIPC() {
	ln, err := net.Listen("tcp", IPC_PORT)
	if err != nil {
		// 端口占用：说明已有实例在后台
		conn, err := net.Dial("tcp", IPC_PORT)
		if err == nil {
			conn.Write([]byte("WAKE_UP"))
			conn.Close()
		}
		// 通知完后，新进程结束，让后台旧进程处理
		os.Exit(0)
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		if string(buf[:n]) == "WAKE_UP" {
			// 核心逻辑：修改配置并重启 Launcher 界面
			conf.TrayHidden = false
			saveConfig()
			// 强制重启自身以恢复托盘（这是最稳定的唤醒 UI 方法）
			restartSelf()
		}
		conn.Close()
	}
}

func restartSelf() {
	verb, _ := syscall.UTF16PtrFromString("open")
	exe, _ := syscall.UTF16PtrFromString(exePath)
	windows.ShellExecute(0, verb, exe, nil, nil, windows.SW_SHOWNORMAL)
	os.Exit(0)
}

// --- 系统底层控制 ---

func isAdmin() bool {
	var token windows.Token
	err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token)
	if err != nil {
		return false
	}
	defer token.Close()
	return token.IsElevated()
}

func runAsAdmin() {
	verb, _ := syscall.UTF16PtrFromString("runas")
	exe, _ := syscall.UTF16PtrFromString(exePath)
	cwd, _ := syscall.UTF16PtrFromString(baseDir)
	windows.ShellExecute(0, verb, exe, nil, cwd, windows.SW_HIDE)
}

func initJob() {
	hJob, _ = windows.CreateJobObject(nil, nil)
	if hJob != 0 {
		var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		windows.NewLazySystemDLL("kernel32.dll").NewProc("SetInformationJobObject").Call(
			uintptr(hJob),
			uintptr(windows.JobObjectExtendedLimitInformation),
			uintptr(unsafe.Pointer(&info)),
			uintptr(uint32(unsafe.Sizeof(info))),
		)
	}
}

// --- 内核守护逻辑 ---

func engineLoop() {
	for {
		if conf.ServiceMode {
			// 情况 A：服务模式，仅同步 TUN 状态
			syncTunState()
		} else {
			// 情况 B：托管模式，检查进程并绑定 Job
			checkAndStartCore()
			syncTunState()
		}
		time.Sleep(5 * time.Second)
	}
}

func checkAndStartCore() {
	cmd := exec.Command("tasklist", "/FI", "IMAGENAME eq mihomo.exe", "/NH")
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	out, _ := cmd.Output()
	if !strings.Contains(string(out), "mihomo.exe") {
		c := exec.Command(coreExe, "-d", baseDir)
		c.Dir = baseDir
		c.SysProcAttr = &windows.SysProcAttr{
			CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_BREAKAWAY_FROM_JOB,
		}
		if err := c.Start(); err == nil && hJob != 0 {
			hProc, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(c.Process.Pid))
			windows.AssignProcessToJobObject(hJob, hProc)
			windows.CloseHandle(hProc)
		}
	}
}

func syncTunState() {
	resp, err := http.Get(API_URL + "/configs")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	isApiOn := strings.Contains(string(body), `"tun":{"enable":true`)

	if conf.TunEnabled && !isApiOn {
		sendPatch(`{"tun": {"enable": true}}`)
	} else if !conf.TunEnabled && isApiOn {
		sendPatch(`{"tun": {"enable": false}}`)
	}
}

// --- 托盘 UI ---

func onReady() {
	systray.SetIcon(getIcon("default.ico"))

	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	systray.AddSeparator()
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", isProxyEnabled())
	mTun := systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", conf.TunEnabled)
	systray.AddSeparator()
	mRule := systray.AddMenuItemCheckbox("规则模式", "", false)
	mGlobal := systray.AddMenuItemCheckbox("全局模式", "", false)
	mDirect := systray.AddMenuItemCheckbox("直连模式", "", false)
	systray.AddSeparator()

	mStartSet := systray.AddMenuItem("启动设置", "")
	mAutoStart := mStartSet.AddSubMenuItemCheckbox("开机自动启动", "", conf.AutoStart)
	mInstallSvc := mStartSet.AddSubMenuItem("安装后台服务", "")
	mUninstallSvc := mStartSet.AddSubMenuItem("卸载后台服务", "")
	mRunBat := mStartSet.AddSubMenuItem("管理服务 (BAT)", "")
	mRestart := mStartSet.AddSubMenuItem("重启内核", "")
	mFullExit := mStartSet.AddSubMenuItem("彻底退出程序", "")

	systray.AddSeparator()
	mOpenDir := systray.AddMenuItem("打开程序目录", "")
	mHide := systray.AddMenuItem("隐藏托盘图标", "")

	// 菜单动态状态
	go func() {
		for {
			if conf.ServiceMode {
				mInstallSvc.Disable()
				mUninstallSvc.Enable()
			} else {
				mInstallSvc.Enable()
				mUninstallSvc.Disable()
			}
			updateStatus(mProxy, mTun, mRule, mGlobal, mDirect)
			time.Sleep(3 * time.Second)
		}
	}()

	// 交互逻辑
	go func() {
		for {
			select {
			case <-mWeb.ClickedCh:
				windows.ShellExecute(0, nil, syscall.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
			case <-mProxy.ClickedCh:
				setProxy(!isProxyEnabled())
			case <-mTun.ClickedCh:
				conf.TunEnabled = !mTun.Checked()
				saveConfig()
			case <-mRule.ClickedCh: sendPatch(`{"mode": "rule"}`)
			case <-mGlobal.ClickedCh: sendPatch(`{"mode": "global"}`)
			case <-mDirect.ClickedCh: sendPatch(`{"mode": "direct"}`)
			case <-mAutoStart.ClickedCh:
				conf.AutoStart = !mAutoStart.Checked()
				setAutoStart(conf.AutoStart)
				saveConfig()
			case <-mInstallSvc.ClickedCh:
				runBat("install")
				conf.ServiceMode = true
				saveConfig()
			case <-mUninstallSvc.ClickedCh:
				runBat("uninstall")
				conf.ServiceMode = false
				saveConfig()
			case <-mRestart.ClickedCh:
				if conf.ServiceMode {
					runBat("restart")
				} else {
					exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
				}
			case <-mFullExit.ClickedCh:
				setProxy(false)
				if conf.ServiceMode {
					runBat("stop")
				} else {
					exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
				}
				os.Exit(0)
			case <-mOpenDir.ClickedCh:
				exec.Command("explorer", baseDir).Run()
			case <-mRunBat.ClickedCh:
				runBat("")
			case <-mHide.ClickedCh:
				conf.TrayHidden = true
				saveConfig()
				systray.Quit()
			}
		}
	}()
}

// --- 辅助工具 ---

func updateStatus(mP, mT, mR, mG, mD *systray.MenuItem) {
	resp, err := http.Get(API_URL + "/configs")
	if err != nil {
		systray.SetIcon(getIcon("stop.ico"))
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	data := string(body)

	// 图标优先级判定
	isTunApi := strings.Contains(data, `"tun":{"enable":true`)
	hasTunIf := false
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if strings.Contains(i.Name, TUN_NAME) && i.Flags&net.FlagUp != 0 {
			hasTunIf = true
			break
		}
	}

	if isTunApi && !hasTunIf {
		systray.SetIcon(getIcon("error.ico"))
	} else if isTunApi && hasTunIf {
		systray.SetIcon(getIcon("tun.ico"))
		mT.Check()
	} else {
		mT.Uncheck()
		if isProxyEnabled() {
			systray.SetIcon(getIcon("proxy.ico"))
			mP.Check()
		} else {
			systray.SetIcon(getIcon("default.ico"))
			mP.Uncheck()
		}
	}

	// 模式同步
	if strings.Contains(data, `"mode":"rule"`) { mR.Check(); mG.Uncheck(); mD.Uncheck() }
	if strings.Contains(data, `"mode":"global"`) { mR.Uncheck(); mG.Check(); mD.Uncheck() }
	if strings.Contains(data, `"mode":"direct"`) { mR.Uncheck(); mG.Uncheck(); mD.Check() }
}

func runBat(action string) {
	bat := filepath.Join(baseDir, "mihomo-service", "mihomo-service.bat")
	args := "/c start /d " + filepath.Dir(bat) + " " + filepath.Base(bat)
	if action != "" { args += " " + action }
	windows.ShellExecute(0, nil, syscall.StringToUTF16Ptr("cmd"), syscall.StringToUTF16Ptr(args), nil, windows.SW_HIDE)
}

func isProxyEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil { return false }
	defer k.Close()
	v, _, _ := k.GetIntegerValue("ProxyEnable")
	return v == 1
}

func setProxy(enable bool) {
	k, _, _ := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.ALL_ACCESS)
	if enable {
		k.SetDWordValue("ProxyEnable", 1)
		k.SetStringValue("ProxyServer", PROXY_ADDR)
	} else {
		k.SetDWordValue("ProxyEnable", 0)
	}
	k.Close()
	windows.NewLazySystemDLL("user32.dll").NewProc("UpdatePerUserSystemParameters").Call(0, 0, 0, 0)
}

func setAutoStart(enable bool) {
	k, _ := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.ALL_ACCESS)
	defer k.Close()
	if enable {
		k.SetStringValue("MihomoLauncher", "\""+exePath+"\"")
	} else {
		k.DeleteValue("MihomoLauncher")
	}
}

func sendPatch(json string) {
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(json)))
	(&http.Client{Timeout: time.Second}).Do(req)
}

func getIcon(name string) []byte {
	data, _ := iconFs.ReadFile("icons/" + name)
	return data
}

func saveConfig() {
	d, _ := json.MarshalIndent(conf, "", "  ")
	os.WriteFile(filepath.Join(baseDir, "config.json"), d, 0644)
}

func loadConfig() {
	d, err := os.ReadFile(filepath.Join(baseDir, "config.json"))
	if err == nil {
		json.Unmarshal(d, &conf)
	} else {
		conf = Config{AutoStart: false, ServiceMode: false, TunEnabled: false, TrayHidden: false}
		saveConfig()
	}
}

func main() {
	if !isAdmin() { runAsAdmin(); return }
	loadConfig()
	initJob()
	go handleIPC()
	go engineLoop()

	if conf.TrayHidden {
		// 潜伏模式
		select {}
	} else {
		systray.Run(onReady, func() {})
	}
}
