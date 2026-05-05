package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"flag"
	"io/ioutil"
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

// --- 常量与全局变量 ---
const (
	API_URL    = "http://127.0.0.1:9090"
	PROXY_ADDR = "127.0.0.1:7890"
	LOCK_FILE  = "tun_on.lock"
	TUN_NAME   = "Mihomo" // 对应你 tray 代码中的网卡名检测
	LOCK_PORT  = "127.0.0.1:54321"
)

type Config struct {
	SystemProxy bool   `json:"system_proxy"`
	TunEnabled  bool   `json:"tun_enabled"`
	ProxyMode   string `json:"proxy_mode"`
	AutoStart   bool   `json:"auto_start"`
	ServiceMode bool   `json:"service_mode"`
}

var (
	conf       Config
	exePath, _ = os.Executable()
	baseDir    = filepath.Dir(exePath)
	coreExe    = filepath.Join(baseDir, "mihomo.exe")
	hJob       windows.Handle
)

// --- 基础工具逻辑 (来自 run) ---

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

func initJobObject() {
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

// --- 核心拉起逻辑 (整合 run 的 Wait 循环与 tray 的 lock 检测) ---

func runMihomoCore() {
	if _, err := os.Stat(coreExe); os.IsNotExist(err) { return }

	for {
		// 暴力清场
		killCmd := exec.Command("taskkill", "/F", "/IM", "mihomo.exe", "/T")
		killCmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		killCmd.Run()
		time.Sleep(500 * time.Millisecond)

		// 启动内核
		cmd := exec.Command(coreExe, "-d", baseDir)
		cmd.Dir = baseDir
		cmd.SysProcAttr = &windows.SysProcAttr{
			CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_BREAKAWAY_FROM_JOB,
		}

		if err := cmd.Start(); err == nil {
			if hJob != 0 {
				hProc, _ := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
				windows.AssignProcessToJobObject(hJob, hProc)
				windows.CloseHandle(hProc)
			}

			// TUN 锁检测强制开启 (来自你的 run 逻辑)
			if _, err := os.Stat(filepath.Join(baseDir, LOCK_FILE)); err == nil {
				go forceEnableTun()
			}
			cmd.Wait()
		}
		time.Sleep(time.Second) // 崩溃重启间隔
	}
}

func forceEnableTun() {
	c := &http.Client{Timeout: 2 * time.Second}
	body := []byte(`{"tun":{"enable":true}}`)
	for i := 0; i < 20; i++ {
		req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer(body))
		if resp, err := c.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 204 || resp.StatusCode == 200 { return }
		}
		time.Sleep(time.Second)
	}
}

// --- UI 交互与状态同步 (整合 tray) ---

func onReady() {
	systray.SetIcon(getIcon("tray_default.ico"))
	systray.SetTitle("Mihomo Launcher")

	// 菜单构建
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
	mStartSet.AddSeparator()
	mRestart := mStartSet.AddSubMenuItem("重启内核", "")
	mFullExit := mStartSet.AddSubMenuItem("彻底退出程序", "")

	systray.AddSeparator()
	mOpenDir := systray.AddMenuItem("打开程序目录", "")
	mHide := systray.AddMenuItem("隐藏托盘图标", "")

	// 实时状态同步协程 (来自 tray 的 syncLogic)
	go func() {
		client := &http.Client{Timeout: 1 * time.Second}
		for {
			syncStatus(client, mProxy, mTun, mRule, mGlobal, mDirect)
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
				toggleProxy(!mProxy.Checked())
			case <-mTun.ClickedCh:
				if mTun.Checked() {
					os.Remove(filepath.Join(baseDir, LOCK_FILE))
					sendPatch(`{"tun": {"enable": false}}`)
				} else {
					f, _ := os.Create(filepath.Join(baseDir, LOCK_FILE))
					if f != nil { f.Close() }
					sendPatch(`{"tun": {"enable": true}}`)
				}
			case <-mRule.ClickedCh: sendPatch(`{"mode": "rule"}`)
			case <-mGlobal.ClickedCh: sendPatch(`{"mode": "global"}`)
			case <-mDirect.ClickedCh: sendPatch(`{"mode": "direct"}`)
			
			case <-mAutoStart.ClickedCh:
				conf.AutoStart = !mAutoStart.Checked()
				setAutoStart(conf.AutoStart)
				if conf.AutoStart { mAutoStart.Check() } else { mAutoStart.Uncheck() }
			case <-mRestart.ClickedCh:
				exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
			case <-mFullExit.ClickedCh:
				toggleProxy(false)
				exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
				os.Exit(0)
			case <-mOpenDir.ClickedCh:
				exec.Command("explorer", baseDir).Run()
			case <-mRunBat.ClickedCh:
				bat := filepath.Join(baseDir, "mihomo-service", "mihomo-service.bat")
				exec.Command("cmd", "/c", "start", "", "cmd", "/c", bat).Run()
			case <-mHide.ClickedCh:
				systray.Quit()
			}
		}
	}()
}

func syncStatus(c *http.Client, mProxy, mTun, mR, mG, mD *systray.MenuItem) {
	// 1. 系统代理检测
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	isProxyOn := false
	if err == nil {
		regVal, _, _ := k.GetIntegerValue("ProxyEnable")
		k.Close()
		if regVal == 1 { mProxy.Check(); isProxyOn = true } else { mProxy.Uncheck() }
	}

	// 2. 网卡检测
	isPhysicalUp := false
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if strings.Contains(i.Name, TUN_NAME) && i.Flags&net.FlagUp != 0 {
			isPhysicalUp = true
			break
		}
	}

	// 3. API 同步内容
	resp, err := c.Get(API_URL + "/configs")
	if err != nil {
		systray.SetIcon(getIcon("tray_stop.ico"))
		return
	}
	defer resp.Body.Close()
	data, _ := ioutil.ReadAll(resp.Body)
	res := string(data)

	// 图标与 TUN 逻辑
	isTunApi := strings.Contains(res, `"tun":{"enable":true`)
	if isTunApi && isPhysicalUp {
		mTun.Check()
		systray.SetIcon(getIcon("tray_tun.ico"))
	} else {
		mTun.Uncheck()
		if isProxyOn { systray.SetIcon(getIcon("tray_proxy.ico")) } else { systray.SetIcon(getIcon("tray_default.ico")) }
	}

	// 模式检测
	if strings.Contains(res, `"mode":"rule"`) { mR.Check(); mG.Uncheck(); mD.Uncheck() }
	if strings.Contains(res, `"mode":"global"`) { mR.Uncheck(); mG.Check(); mD.Uncheck() }
	if strings.Contains(res, `"mode":"direct"`) { mR.Uncheck(); mG.Uncheck(); mD.Check() }
}

// --- 通用工具函数 ---

func getIcon(name string) []byte {
	data, _ := iconFs.ReadFile("icons/" + name)
	return data
}

func sendPatch(jsonStr string) {
	c := &http.Client{Timeout: 1 * time.Second}
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(jsonStr)))
	if resp, err := c.Do(req); err == nil { resp.Body.Close() }
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
	if enable { k.SetStringValue("MihomoLauncher", "\""+exePath+"\" -minimized") } else { k.DeleteValue("MihomoLauncher") }
}

func main() {
	if !isAdmin() { runAsAdmin(); return }
	initJobObject()

	// 端口占位防止多开
	ln, err := net.Listen("tcp", LOCK_PORT)
	if err != nil { return }
	defer ln.Close()

	loadConfig()
	go runMihomoCore()
	systray.Run(onReady, func() {})
}

func loadConfig() {
	data, err := ioutil.ReadFile(filepath.Join(baseDir, "config.json"))
	if err == nil { json.Unmarshal(data, &conf) }
}
