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
	IPC_PORT   = "127.0.0.1:54321" // 单实例唤醒端口
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
)

// --- 进程通讯与单实例逻辑 ---

func handleIPC() {
	ln, err := net.Listen("tcp", IPC_PORT)
	if err != nil {
		// 端口被占用，尝试唤醒旧进程
		conn, err := net.Dial("tcp", IPC_PORT)
		if err == nil {
			conn.Write([]byte("ACTIVATE_TRAY"))
			conn.Close()
		}
		os.Exit(0)
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil { continue }
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		if string(buf[:n]) == "ACTIVATE_TRAY" {
			conf.TrayHidden = false
			saveConfig()
			// 如果 systray 没在运行，这里无法直接重启，建议通过配置重启 Launcher
			// 简单处理：提示图标已激活（实际逻辑在 main 循环中通过读取配置判断更好）
			systray.ShowAppWindow("") // 某些平台唤醒尝试
		}
		conn.Close()
	}
}

// --- 系统底层控制 ---

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

// --- 内核守护与服务模式逻辑 ---

func engineLoop() {
	for {
		if conf.ServiceMode {
			// A 情况：服务模式，仅监测不直接拉起
			syncTunStateWithConfig()
		} else {
			// B 情况：便携模式，由 Launcher 亲自拉起并绑定 Job
			ensureCoreRunning()
		}
		time.Sleep(5 * time.Second)
	}
}

func ensureCoreRunning() {
	// 检查进程是否存在
	cmd := exec.Command("tasklist", "/FI", "IMAGENAME eq mihomo.exe", "/NH")
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	out, _ := cmd.Output()
	if !strings.Contains(string(out), "mihomo.exe") {
		startCoreBin()
	}
	syncTunStateWithConfig()
}

func startCoreBin() {
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

func syncTunStateWithConfig() {
	// 核心逻辑：如果内存要求开 TUN 但 API 说没开，就补发 PATCH
	resp, err := http.Get(API_URL + "/configs")
	if err != nil { return }
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	
	isTunApiOn := strings.Contains(string(body), `"tun":{"enable":true`)
	if conf.TunEnabled && !isTunApiOn {
		sendPatch(`{"tun": {"enable": true}}`)
	} else if !conf.TunEnabled && isTunApiOn {
		sendPatch(`{"tun": {"enable": false}}`)
	}
}

// --- 托盘 UI 与交互 ---

func onReady() {
	// 如果配置为隐藏，则图标不可见（注：systray 暂不支持彻底启动后动态移除图标，通常在 main 控制启动）
	systray.SetIcon(getIcon("default.ico"))

	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	systray.AddSeparator()
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", false)
	mTun := systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", false)
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

	// 菜单动态置灰逻辑
	go func() {
		for {
			if conf.ServiceMode {
				mInstallSvc.Disable()
				mUninstallSvc.Enable()
			} else {
				mInstallSvc.Enable()
				mUninstallSvc.Disable()
			}
			syncUI(mProxy, mTun, mRule, mGlobal, mDirect)
			time.Sleep(3 * time.Second)
		}
	}()

	// 事件监听
	go func() {
		for {
			select {
			case <-mWeb.ClickedCh:
				windows.ShellExecute(0, nil, syscall.StringToUTF16Ptr(API_URL+"/ui"), nil, nil, windows.SW_SHOWNORMAL)
			case <-mProxy.ClickedCh:
				toggleProxy(!mProxy.Checked())
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
				finalCleanup()
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

// --- 核心工具函数 ---

func syncUI(mP, mT, mR, mG, mD *systray.MenuItem) {
	// 优先级 1: 内核失联
	resp, err := http.Get(API_URL + "/configs")
	if err != nil {
		systray.SetIcon(getIcon("stop.ico"))
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	data := string(body)

	// 优先级 2: 配置冲突 (TUN 开启但无网卡)
	isTunApiOn := strings.Contains(data, `"tun":{"enable":true`)
	hasTunInterface := false
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if strings.Contains(i.Name, TUN_NAME) && i.Flags&net.FlagUp != 0 {
			hasTunInterface = true
			break
		}
	}

	if isTunApiOn && !hasTunInterface {
		systray.SetIcon(getIcon("error.ico"))
	} else if isTunApiOn && hasTunInterface {
		systray.SetIcon(getIcon("tun.ico"))
		mT.Check()
	} else {
		mT.Uncheck()
		// 优先级 4: 系统代理
		if isProxyOn() {
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

func finalCleanup() {
	toggleProxy(false)
	if conf.ServiceMode {
		runBat("stop")
	} else {
		exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
	}
}

func runBat(action string) {
	bat := filepath.Join(baseDir, "mihomo-service", "mihomo-service.bat")
	args := "/c start /d " + filepath.Dir(bat) + " " + filepath.Base(bat)
	if action != "" { args += " " + action }
	windows.ShellExecute(0, nil, syscall.StringToUTF16Ptr("cmd"), syscall.StringToUTF16Ptr(args), nil, windows.SW_HIDE)
}

func isProxyOn() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil { return false }
	defer k.Close()
	v, _, _ := k.GetIntegerValue("ProxyEnable")
	return v == 1
}

func toggleProxy(enable bool) {
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

func sendPatch(jsonStr string) {
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(jsonStr)))
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

	// 核心逻辑：如果配置为隐藏且是由自启拉起，则不启动 UI 线程
	go engineLoop()

	if conf.TrayHidden {
		// 潜伏模式：只运行协程，不进入 systray.Run
		select {} 
	} else {
		systray.Run(onReady, func() {})
	}
}
