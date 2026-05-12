func monitorIconState() {
    // 局部复用/声明必要变量，确保 goto 不会跨越声明
    var (
        curr      int
        isTunMode bool
        hasTun    bool
        ifaces    []net.Interface
        err       error
        backState int
    )

    for {
        if atomic.LoadInt32(&isReallyExiting) == 1 {
            return
        }

        // --- 1. 物理层判定 ---
        if !isProcessRunning("mihomo.exe") {
            tunErrorCounter = 0 // 进程没了，重置计数器
            if lastState != StateStop {
                updateIconByState(StateStop)
                lastState = StateStop
            }
            goto LoopSleep
        }

        // --- 2. 获取业务与网卡事实 ---
        curr = checkSystemState()
        isTunMode = (getIniConfig("tun_enabled") == "true")
        hasTun = false

        ifaces, err = net.Interfaces()
        if err == nil {
            for _, i := range ifaces {
                if isTunInterfaceMatch(i.Name) {
                    if (i.Flags & net.FlagUp) != 0 {
                        hasTun = true
                        break
                    }
                }
            }
        }

        // --- 3. 核心判定逻辑 ---
        
        // 判定条件：TUN开启但网卡没影，或者 API 连不上 (StateStop)
        if (isTunMode && !hasTun) || curr == StateStop {
            tunErrorCounter++ // 使用你现有的变量

            if tunErrorCounter <= 5 {
                // 【宽限期内】：尝试回退到降级状态 (Proxy/Default)
                backState = curr
                if backState == StateStop {
                    if getIniConfig("system_proxy_enabled") == "true" {
                        backState = StateProxy
                    } else {
                        backState = StateDefault
                    }
                }
                
                if lastState != backState {
                    updateIconByState(backState)
                    lastState = backState
                }
            } else {
                // 【宽限期满】：执行最终裁决
                
                // 情况 A：内核通讯正常(API通) 但网卡持续缺失 -> 业务违和 (Error红)
                if curr != StateStop && isTunMode && !hasTun {
                    if lastState != StateError {
                        updateIconByState(StateError)
                        lastState = StateError
                    }
                } else {
                    // 情况 B：压倒性 Stop (灰)
                    // API 依然不通，或者不符合红灯条件
                    if lastState != StateStop {
                        updateIconByState(StateStop)
                        lastState = StateStop
                    }
                }
            }
        } else {
            // --- 4. 一切正常 ---
            tunErrorCounter = 0 // 重置计数器
            if curr != lastState {
                updateIconByState(curr)
                lastState = curr
            }
        }

    LoopSleep:
        time.Sleep(1 * time.Second)
    }
}
