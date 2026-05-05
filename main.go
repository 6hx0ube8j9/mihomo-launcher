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

const (
	API_URL    = "http://127.0.0.1:9090"
	PROXY_ADDR = "127.0.0.1:7890"
	LOCK_FILE  = "tun_on.lock"
	TUN_NAME   = "Mihomo"
	LOCK_PORT  = "127.0.0.1:54321"
)

type Config struct {
	AutoStart   bool `json:"auto_start"`
	ServiceMode bool `json:"service_mode"`
}

var (
	conf        Config
	exePath, _  = os.Executable()
	baseDir     = filepath.Dir(exePath)
	coreExe     = filepath.Join(baseDir, "mihomo.exe")
	hJob        windows.Handle
)

// --- 系统权限与进程控制 ---

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

// --- 内核管理逻辑 ---

func startCoreLoop() {
	for {
		// 1. 彻底杀掉已存在的内核
		kill := exec.Command("taskkill", "/F", "/IM", "mihomo.exe", "/T")
		kill.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		kill.Run()
		time.Sleep(500 * time.Millisecond)

		// 2. 启动新内核 (必须指定 Dir 否则会找不到配置文件)
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
			
			// 只有存在 LOCK_FILE 时，才强制 PATCH TUN
			if _, err := os.Stat(filepath.Join(baseDir, LOCK_FILE)); err == nil {
				go func() {
					time.Sleep(2 * time.Second) // 等待内核初始化 API
					sendPatch(`{"tun": {"enable": true}}`)
				}()
			}
			cmd.Wait() // 阻塞直到内核退出，实现自动重启
		}
		time.Sleep(2 * time.Second)
	}
}

// --- 托盘 UI 逻辑 ---

func onReady() {
	systray.SetIcon(getIcon("tray_default.ico"))
	
	// --- 菜单定义 ---
	mWeb := systray.AddMenuItem("进入 Web 面板", "")
	systray.AddSeparator()
	mProxy := systray.AddMenuItemCheckbox("系统代理", "", false)
	mTun := systray.AddMenuItemCheckbox("虚拟网卡 (TUN)", "", false)
	systray.AddSeparator()
	mRule := systray.AddMenuItemCheckbox("规则模式", "", false)
	mGlobal := systray.AddMenuItemCheckbox("全局模式", "", false)
	mDirect := systray.AddMenuItemCheckbox("直连模式", "", false)
	systray.AddSeparator()

	// 启动设置 (一级菜单)
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

	// --- 状态同步轮询 ---
	go func() {
		for {
			syncStatus(mProxy, mTun, mRule, mGlobal, mDirect)
			time.Sleep(3 * time.Second)
		}
	}()

	// --- 事件监听 ---
	go func() {
		for {
			select {
			case <-mWeb.ClickedCh:
				// 无黑框打开浏览器
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
				saveConfig()
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
				// 这种方式运行 BAT 依然能隐藏 CMD 窗口
				cmd := exec.Command("cmd", "/c", "start", "", "cmd", "/c", bat)
				cmd.Dir = filepath.Dir(bat)
				cmd.SysProcAttr = &windows.SysProcAttr{HideWindow: true}
				cmd.Start()
			case <-mHide.ClickedCh:
				systray.Quit()
			}
		}
	}()
}

// --- 核心工具函数 ---

func syncStatus(mP, mT, mR, mG, mD *systray.MenuItem) {
	// 1. 代理注册表同步
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	isP := false
	if err == nil {
		v, _, _ := k.GetIntegerValue("ProxyEnable")
		k.Close()
		if v == 1 { mP.Check(); isP = true } else { mP.Uncheck() }
	}

	// 2. 网卡同步
	isT := false
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if strings.Contains(i.Name, TUN_NAME) && i.Flags&net.FlagUp != 0 {
			isT = true; break
		}
	}

	// 3. API 模式同步
	resp, err := http.Get(API_URL + "/configs")
	if err != nil {
		systray.SetIcon(getIcon("tray_stop.ico"))
		return
	}
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	data := string(body)

	// 图标切换逻辑
	isTunApi := strings.Contains(data, `"tun":{"enable":true`)
	if isTunApi && isT {
		mT.Check(); systray.SetIcon(getIcon("tray_tun.ico"))
	} else {
		mT.Uncheck()
		if isP { systray.SetIcon(getIcon("tray_proxy.ico")) } else { systray.SetIcon(getIcon("tray_default.ico")) }
	}

	// 模式勾选同步
	if strings.Contains(data, `"mode":"rule"`) { mR.Check(); mG.Uncheck(); mD.Uncheck() }
	if strings.Contains(data, `"mode":"global"`) { mR.Uncheck(); mG.Check(); mD.Uncheck() }
	if strings.Contains(data, `"mode":"direct"`) { mR.Uncheck(); mG.Uncheck(); mD.Check() }
}

func sendPatch(jsonStr string) {
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(jsonStr)))
	c := &http.Client{Timeout: 1 * time.Second}
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
	// 通知系统更新代理设置
	windows.NewLazySystemDLL("user32.dll").NewProc("UpdatePerUserSystemParameters").Call(0, 0, 0, 0)
}

func setAutoStart(enable bool) {
	k, _ := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.ALL_ACCESS)
	defer k.Close()
	if enable {
		k.SetStringValue("MihomoLauncher", "\""+exePath+"\" -minimized")
	} else {
		k.DeleteValue("MihomoLauncher")
	}
}

func getIcon(name string) []byte {
	data, _ := iconFs.ReadFile("icons/" + name)
	return data
}

func saveConfig() {
	d, _ := json.MarshalIndent(conf, "", "  ")
	ioutil.WriteFile(filepath.Join(baseDir, "config.json"), d, 0644)
}

func loadConfig() {
	d, err := ioutil.ReadFile(filepath.Join(baseDir, "config.json"))
	if err == nil { json.Unmarshal(d, &conf) }
}

func main() {
	if !isAdmin() { runAsAdmin(); return }
	initJob()

	// 单实例锁
	ln, err := net.Listen("tcp", LOCK_PORT)
	if err != nil { return }
	defer ln.Close()

	loadConfig()

	// 启动后台内核守护
	go startCoreLoop()

	systray.Run(onReady, func() {})
}
