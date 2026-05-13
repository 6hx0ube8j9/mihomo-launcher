func checkSystemState() int {
    // 1. 状态快照
    iniTunEnabled := getIniConfig("tun_enabled") == "true"
    iniProxyEnabled := getIniConfig("system_proxy_enabled") == "true"
    isInit := atomic.LoadInt32(&isSystemInitializing) == 1

    // 2. 内核通信
    body, err := doAPIRequest("GET", "/configs", nil)
    if err != nil {
        return StateStop
    }

    // 3. 状态对齐
    if !isInit {
        var currentConf struct {
            Tun struct { Enable bool `json:"enable"` } `json:"tun"`
            Mode string `json:"mode"`
        }
        if err := json.Unmarshal(body, &currentConf); err == nil {
            if currentConf.Tun.Enable != iniTunEnabled {
                saveIniConfig("tun_enabled", fmt.Sprint(currentConf.Tun.Enable))
                iniTunEnabled = currentConf.Tun.Enable 
            }
            // ... 其他对齐逻辑
        }
    }

    // 4. 异步同步触发
    if atomic.CompareAndSwapInt32(&hasFirstSynced, 0, 1) {
        go syncConfigToKernel()
    }

    // --- 分流路径 ---

    // 路径 A：关闭 TUN
    if !iniTunEnabled {
        if isInit { atomic.StoreInt32(&isSystemInitializing, 0) }
        if iniProxyEnabled { return StateProxy }
        return StateDefault
    }

    // 路径 B：开启了 TUN 且正在初始化
    if isInit {
        return StateTun
    }

    // 路径 C：TUN 稳定期，执行物理网卡校验
    // 【修复点】：在这里确保 hasTun 被正确定义和使用
    hasTun := false 
    ifaces, err := net.Interfaces()
    if err == nil {
        for _, i := range ifaces {
            if isTunInterfaceMatch(i.Name) {
                hasTun = true
                break
            }
        }
    }

    if hasTun {
        return StateTun
    }

    // 如果走到这里，说明配置开启了 TUN，但没搜到网卡，且已过初始化期
    return StateError 
}
