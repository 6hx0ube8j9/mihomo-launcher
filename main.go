func checkSystemState() int {
    // 1. 状态快照：一次性读取所有配置，避免后续多次磁盘 IO
    iniTunEnabled := getIniConfig("tun_enabled") == "true"
    iniProxyEnabled := getIniConfig("system_proxy_enabled") == "true"
    isInit := atomic.LoadInt32(&isSystemInitializing) == 1

    // 2. 内核通信
    body, err := doAPIRequest("GET", "/configs", nil)
    if err != nil {
        return StateStop
    }

    // 3. 状态对齐：仅在非初始化时处理
    if !isInit {
        var currentConf struct {
            Tun struct { Enable bool `json:"enable"` } `json:"tun"`
            Mode string `json:"mode"`
        }
        if err := json.Unmarshal(body, &currentConf); err == nil {
            // 对齐逻辑... (此处保持你原来的 saveIniConfig 即可)
            // 注意：如果这里更新了配置，建议同步更新局部变量 iniTunEnabled
            if currentConf.Tun.Enable != iniTunEnabled {
                saveIniConfig("tun_enabled", fmt.Sprint(currentConf.Tun.Enable))
                iniTunEnabled = currentConf.Tun.Enable 
            }
        }
    }

    // 4. 异步同步触发
    if atomic.CompareAndSwapInt32(&hasFirstSynced, 0, 1) {
        go syncConfigToKernel()
    }

    // --- 关键修改点：丝滑切换的分流路径 ---

    // 路径 A：如果配置已经关闭了 TUN，【立刻】走 Proxy/Default 判定，绝不向下运行
    if !iniTunEnabled {
        // 重置初始化状态（既然已经关闭 TUN，说明已经过了一次完整判定）
        if isInit { atomic.StoreInt32(&isSystemInitializing, 0) }
        
        if iniProxyEnabled {
            return StateProxy
        }
        return StateDefault
    }

    // 路径 B：开启了 TUN 模式
    // 初始化期间，直接返回 TUN，不扫网卡（这是你重启精准的关键）
    if isInit {
        // 这里不要急着重置 isSystemInitializing，交给外部计时器或启动完成回调
        return StateTun
    }

    // 路径 C：TUN 已稳定运行，进行物理网卡校验
    hasTun := false
    ifaces, _ := net.Interfaces()
    for _, i := range ifaces {
        if isTunInterfaceMatch(i.Name) {
            hasTun = true
            break
        }
    }

    if hasTun {
        return StateTun
    }

    return StateTun // 保留你的容错返回
}

func monitorIconState() {
    var failCount int
    var lastState int // 建议初始化一个不可能的状态

    for {
        if atomic.LoadInt32(&isReallyExiting) == 1 {
            return
        }

        // --- 1. 唯一事实来源 ---
        // 所有的 net.Interfaces()、isInitializing、INI 读取全部在 check 内部完成
        // 不要在这里二次调用 getIniConfig 或 net.Interfaces()
        curr := StateStop
        if isProcessRunning("mihomo.exe") {
            curr = checkSystemState()
        }

        // --- 2. 纯粹的容错计算 (不参与 UI 更新) ---
        // 如果 check 已经很精准了，这里其实只需要处理一种极端情况：
        // 内核在跑，但 API 报错 (StateStop) 且确实没网卡。
        if curr == StateStop && isProcessRunning("mihomo.exe") {
            failCount++
            if failCount > 5 {
                curr = StateError
            } else {
                // 在报错达到阈值前，为了丝滑，我们“假装”它还是上一个状态
                // 这样图标就不会闪烁
                curr = lastState
            }
        } else {
            failCount = 0
        }

        // --- 3. 唯一的 UI 更新出口 ---
        // 只有最终确定的 curr 变了，才动一下图标
        if curr != lastState {
            updateIconByState(curr)
            lastState = curr
        }

        time.Sleep(1 * time.Second)
    }
}

func watchTunState() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	var lastHasTun bool
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if isTunInterfaceMatch(i.Name) {
			lastHasTun = true
			break
		}
	}

	for {
		select {
		case <-ticker.C:
			if atomic.LoadInt32(&isReallyExiting) == 1 {
				return
			}

			if atomic.LoadInt32(&isSystemInitializing) == 1 || atomic.LoadInt32(&isSyncing) == 1 {
				continue
			}

			currentHasTun := false
			currentIfaces, err := net.Interfaces()
			if err != nil {
				continue
			}

			for _, i := range currentIfaces {
				if isTunInterfaceMatch(i.Name) {
					currentHasTun = true
					break
				}
			}

			if currentHasTun != lastHasTun {
				if atomic.LoadInt32(&isKernelActive) == 1 && atomic.LoadInt32(&isReallyExiting) == 0 {
					
					lastHasTun = currentHasTun
					atomic.StoreInt32(&hasFirstSynced, 1)
					saveIniConfig("tun_enabled", fmt.Sprint(currentHasTun))
					newState := checkSystemState()
					updateIconByState(newState)
					atomic.StoreInt32(&lastState, int32(newState))
					if mTun != nil {
						if currentHasTun {
							mTun.Check()
						} else {
							mTun.Uncheck()
						}
					}
					
				}
			}
		}
	}
}

func setProxyRegistry(enable bool) {
    if atomic.LoadInt32(&isReallyExiting) == 1 { return }
    
    // 1. 写入配置
    saveIniConfig("system_proxy_enabled", fmt.Sprint(enable))
    
    // 2. 修改注册表
    key, err := registry.OpenKey(registry.CURRENT_USER, REG_PROXY, registry.SET_VALUE)
    if err == nil {
        defer key.Close()
        if enable {
            _ = key.SetDWordValue("ProxyEnable", 1)
            _ = key.SetStringValue("ProxyServer", getIniConfig("proxy_address"))
        } else {
            _ = key.SetDWordValue("ProxyEnable", 0)
        }
    }

    // 3. 核心优化：异步刷新系统网络栈
    go func() {
        wininet := syscall.NewLazyDLL("wininet.dll")
        setOption := wininet.NewProc("InternetSetOptionW")
        _, _, _ = setOption.Call(0, 39, 0, 0)
        _, _, _ = setOption.Call(0, 37, 0, 0)
    }()

    // 4. 【丝滑关键】：立即手动调用一次状态检查，强制同步图标
    // 这样不需要等 monitor 的 1 秒轮询，UI 会立刻闪烁变色
    go func() {
        // 给系统 50ms 时间完成 IO 写入
        time.Sleep(50 * time.Millisecond)
        curr := checkSystemState()
        updateIconByState(curr)
        // 这里的 atomic 更新能确保 monitor 协程下一秒跑的时候不会覆盖我们
        // 注意：lastState 变量如果是全局的，需要在这里同步更新
        atomic.StoreInt32(&lastStateGlobal, int32(curr)) 
    }()
}

