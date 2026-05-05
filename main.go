package main

import (
	"bytes"
	"embed"
	"encoding/json"
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
	conf       Config
	exePath, _ = os.Executable()
	baseDir    = filepath.Dir(exePath)
	coreExe    = filepath.Join(baseDir, "mihomo.exe")
	hJob       windows.Handle
)

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

// --- 内核守护逻辑 ---

func startCoreLoop() {
	for {
		kill := exec.Command("taskkill", "/F", "/IM", "mihomo.exe", "/T")
		kill.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		kill.Run()
		time.Sleep(500 * time.Millisecond)

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
			
			if _, err := os.Stat(filepath.Join(baseDir, LOCK_FILE)); err == nil {
				go func() {
					for i := 0; i < 10; i++ {
						time.Sleep(2 * time.Second)
						if sendPatch(`{"tun": {"enable": true}}`) { break }
					}
				}()
			}
			cmd.Wait()
		} else {
			systray.SetIcon(getIcon("error.ico"))
		}
		time.Sleep(2 * time.Second)
	}
}

// --- 托盘 UI 逻辑 ---

func onReady() {
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

	// 启动设置 (一级菜单)
	mStartSet := systray.AddMenuItem("启动设置", "")
	mAutoStart := mStartSet.AddSubMenuItemCheckbox("开机自动启动", "", conf.AutoStart)
	mInstallSvc := mStartSet.AddSubMenuItem("安装后台服务", "")
	mUninstallSvc := mStartSet.AddSubMenuItem("卸载后台服务", "")
	mRunBat := mStartSet.AddSubMenuItem("管理服务 (BAT)", "")
	// 注意：MenuItem 没有 AddSeparator 方法，所以这里去掉了子菜单的分隔符
	mRestart := mStartSet.AddSubMenuItem("重启内核", "")
	mFullExit := mStartSet.AddSubMenuItem("彻底退出程序", "")

	systray.AddSeparator()
	mOpenDir := systray.AddMenuItem("打开程序目录", "")
	mHide := systray.AddMenuItem("隐藏托盘图标", "")

	go func() {
		for {
			syncStatus(mProxy, mTun, mRule, mGlobal, mDirect)
			time.Sleep(3 * time.Second)
		}
	}()

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
				saveConfig()
				if conf.AutoStart { mAutoStart.Check() } else { mAutoStart.Uncheck() }

			case <-mInstallSvc.ClickedCh:
				// 处理服务安装逻辑
				runServiceBat("install")
			case <-mUninstallSvc.ClickedCh:
				// 处理服务卸载逻辑
				runServiceBat("uninstall")

			case <-mRestart.ClickedCh:
				systray.SetIcon(getIcon("stop.ico"))
				exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
			case <-mFullExit.ClickedCh:
				toggleProxy(false)
				exec.Command("taskkill", "/F", "/T", "/IM", "mihomo.exe").Run()
				os.Exit(0)
			case <-mOpenDir.ClickedCh:
				exec.Command("explorer", baseDir).Run()
			case <-mRunBat.ClickedCh:
				runServiceBat("")
			case <-mHide.ClickedCh:
				systray.Quit()
			}
		}
	}()
}

// 提取出的通用运行 BAT 函数
func runServiceBat(action string) {
	bat := filepath.Join(baseDir, "mihomo-service", "mihomo-service.bat")
	args := "/c start /d " + filepath.Dir(bat) + " " + filepath.Base(bat)
	if action != "" {
		args += " " + action
	}
	windows.ShellExecute(0, nil, syscall.StringToUTF16Ptr("cmd"), syscall.StringToUTF16Ptr(args), nil, windows.SW_HIDE)
}

// --- 辅助函数 ---

func syncStatus(mP, mT, mR, mG, mD *systray.MenuItem) {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	isP := false
	if err == nil {
		v, _, _ := k.GetIntegerValue("ProxyEnable")
		k.Close()
		if v == 1 { mP.Check(); isP = true } else { mP.Uncheck() }
	}

	isT := false
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if strings.Contains(i.Name, TUN_NAME) && i.Flags&net.FlagUp != 0 {
			isT = true; break
		}
	}

	resp, err := http.Get(API_URL + "/configs")
	if err != nil {
		systray.SetIcon(getIcon("stop.ico"))
		return
	}
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	data := string(body)

	isTunApi := strings.Contains(data, `"tun":{"enable":true`)
	if isTunApi && isT {
		mT.Check(); systray.SetIcon(getIcon("tun.ico"))
	} else {
		mT.Uncheck()
		if isP { systray.SetIcon(getIcon("proxy.ico")) } else { systray.SetIcon(getIcon("default.ico")) }
	}

	if strings.Contains(data, `"mode":"rule"`) { mR.Check(); mG.Uncheck(); mD.Uncheck() }
	if strings.Contains(data, `"mode":"global"`) { mR.Uncheck(); mG.Check(); mD.Uncheck() }
	if strings.Contains(data, `"mode":"direct"`) { mR.Uncheck(); mG.Uncheck(); mD.Check() }
}

func sendPatch(jsonStr string) bool {
	req, _ := http.NewRequest("PATCH", API_URL+"/configs", bytes.NewBuffer([]byte(jsonStr)))
	c := &http.Client{Timeout: 1 * time.Second}
	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
		return resp.StatusCode == 204 || resp.StatusCode == 200
	}
	return false
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

	ln, err := net.Listen("tcp", LOCK_PORT)
	if err != nil { return }
	defer ln.Close()

	loadConfig()
	go startCoreLoop()
	systray.Run(onReady, func() {})
}
