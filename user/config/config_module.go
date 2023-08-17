package config

import (
    "errors"
    "fmt"
    "os"
    "regexp"
    "stackplz/user/util"
    "strconv"
    "strings"
    "unsafe"

    "github.com/cilium/ebpf"
)

type StackUprobeConfig struct {
    LibName string
    LibPath string
    Points  []UprobeArgs
}

func (this *StackUprobeConfig) IsEnable() bool {
    return len(this.Points) > 0
}

func (this *StackUprobeConfig) ParseConfig(configs []string) (err error) {
    // strstr+0x0[str,str] 命中 strstr + 0x0 时将x0和x1读取为字符串
    // write[int,buf:128,int] 命中 write 时将x0读取为int、x1读取为字节数组、x2读取为int
    // 0x89ab[buf:64,int] 命中hook点时读取 x0 处64字节数据 读取 x1 值
    // 0x89ab[buf:64:sp+0x20-0x8] 命中hook点时读取 sp+0x20-0x8 处64字节数据
    // 0x89ab[buf:x1:sp+0x20-0x8] 命中hook点时读取 sp+0x20-0x8 处x1寄存器大小字节数据
    for point_index, config_str := range configs {
        reg := regexp.MustCompile(`(\w+)(\+0x[[:xdigit:]]+)?(\[.+?\])?`)
        match := reg.FindStringSubmatch(config_str)

        if len(match) > 0 {
            hook_point := UprobeArgs{}
            hook_point.Index = uint32(point_index)
            hook_point.Offset = 0x0
            hook_point.LibPath = this.LibPath
            sym_or_off := match[1]
            hook_point.PointName = sym_or_off
            if strings.HasPrefix(sym_or_off, "0x") {
                offset, err := strconv.ParseUint(strings.TrimPrefix(sym_or_off, "0x"), 16, 64)
                if err != nil {
                    return errors.New(fmt.Sprintf("parse for %s failed, sym_or_off:%s err:%v", config_str, sym_or_off, err))
                }
                hook_point.Offset = offset
                hook_point.Symbol = ""
            } else {
                hook_point.Symbol = sym_or_off
            }
            off := match[2]
            if off != "" {
                if strings.HasPrefix(off, "+0x") {
                    offset, err := strconv.ParseUint(strings.TrimPrefix(off, "+0x"), 16, 64)
                    if err != nil {
                        return errors.New(fmt.Sprintf("parse for %s failed, off:%s err:%v", config_str, off, err))
                    }
                    hook_point.Offset = offset
                }
            }
            if match[3] != "" {
                hook_point.ArgsStr = match[3][1 : len(match[3])-1]
                args := strings.Split(hook_point.ArgsStr, ",")
                for arg_index, arg_str := range args {
                    arg_name := fmt.Sprintf("arg_%d", arg_index)
                    arg := PointArg{arg_name, UPROBE_ENTER_READ, INT, "???"}
                    if arg_str == "str" {
                        arg.ArgType = STRING
                    } else if arg_str == "int" {
                        arg.ArgType = INT
                    } else if arg_str == "pattr" {
                        arg.ArgType = PTHREAD_ATTR
                    } else if arg_str == "pattr*" {
                        arg.ArgType = PTHREAD_ATTR.ToPointer()
                    } else if strings.HasPrefix(arg_str, "buf") {
                        var read_index uint32 = READ_INDEX_SKIP
                        var read_count uint32 = 256
                        items := strings.Split(arg_str, ":")
                        if len(items) == 1 {
                            // read_count = 256
                        } else if len(items) == 2 {
                            var size uint64
                            if strings.HasPrefix(items[1], "0x") {
                                size, err = strconv.ParseUint(strings.TrimPrefix(items[1], "0x"), 16, 32)
                                if err != nil {
                                    return errors.New(fmt.Sprintf("parse for %s failed, arg_str:%s, err:%v", config_str, arg_str, err))
                                }
                                read_count = uint32(size)
                            } else {
                                // 尝试当作纯数字字符串转换
                                size, err = strconv.ParseUint(items[1], 10, 32)
                                if err != nil {
                                    // 尝试视为寄存器字符串转换
                                    read_index, err = ParseAsReg(items[1])
                                    if err != nil {
                                        return err
                                    }
                                } else {
                                    read_count = uint32(size)
                                }
                            }
                        } else {
                            return errors.New(fmt.Sprintf("parse for %s failed, unexpected config, arg_str:%s", config_str, arg_str))
                        }
                        arg.ArgType = AT(TYPE_BUFFER_T, TYPE_POINTER, read_count)
                        if read_index != READ_INDEX_SKIP {
                            arg.ArgType.SetIndex(read_index)
                        }
                    } else {
                        return errors.New(fmt.Sprintf("parse for %s failed, unknown type, arg_str:%s", config_str, arg_str))
                    }
                    hook_point.Args = append(hook_point.Args, arg)
                }
            }
            this.Points = append(this.Points, hook_point)
        } else {
            return errors.New(fmt.Sprintf("parse for %s failed", config_str))
        }
    }
    return nil
}

func (this *StackUprobeConfig) UpdatePointArgsMap(UprobePointArgsMap *ebpf.Map) error {
    for _, uprobe_point := range this.Points {
        err := UprobePointArgsMap.Update(unsafe.Pointer(&uprobe_point.Index), unsafe.Pointer(uprobe_point.GetConfig()), ebpf.UpdateAny)
        if err != nil {
            return err
        }
    }
    return nil
}

func (this *StackUprobeConfig) Check() error {
    if len(this.Points) == 0 {
        return fmt.Errorf("need hook point count is 0 :(")
    }
    _, err := os.Stat(this.LibPath)
    if err != nil {
        return err
    }
    parts := strings.Split(this.LibPath, "/")
    this.LibName = parts[len(parts)-1]
    return nil
}

type SyscallConfig struct {
    SConfig
    HookALL                bool
    Enable                 bool
    syscall_mask           uint32
    syscall                [MAX_COUNT]uint32
    syscall_blacklist_mask uint32
    syscall_blacklist      [MAX_COUNT]uint32
}

func NewSyscallConfig() *SyscallConfig {
    config := &SyscallConfig{}
    config.Enable = false
    return config
}

func (this *SyscallConfig) GetSyscallFilter() SyscallFilter {
    filter := SyscallFilter{}
    filter.SetArch(this.Is32Bit)
    filter.SetHookALL(this.HookALL)
    this.FillFilter(&filter)
    return filter
}

func (this *SyscallConfig) FillFilter(filter *SyscallFilter) {
    filter.syscall = this.syscall
    filter.syscall_mask = this.syscall_mask
    filter.syscall_blacklist = this.syscall_blacklist
    filter.syscall_blacklist_mask = this.syscall_blacklist_mask
}

func (this *SyscallConfig) UpdatePointArgsMap(SyscallPointArgsMap *ebpf.Map) error {
    // 取 syscall 参数配置 syscall_point_args_map
    points := GetAllWatchPoints()
    for nr_name, point := range points {
        nr_point, ok := (point).(*SysCallArgs)
        if !ok {
            panic(fmt.Sprintf("cast [%s] point to SysCallArgs failed", nr_name))
        }
        // 这里可以改成只更新追踪的syscall以加快速度
        err := SyscallPointArgsMap.Update(unsafe.Pointer(&nr_point.NR), unsafe.Pointer(nr_point.GetConfig()), ebpf.UpdateAny)
        if err != nil {
            return err
        }
    }
    return nil
}

func (this *SyscallConfig) SetUp(is_32bit bool) error {
    for i := 0; i < len(this.syscall); i++ {
        this.syscall[i] = MAGIC_SYSCALL
    }
    for i := 0; i < len(this.syscall); i++ {
        this.syscall_blacklist[i] = MAGIC_SYSCALL
    }
    return nil
}

const (
    SYSCALL_GROUP_ALL uint32 = iota
    SYSCALL_GROUP_KILL
    SYSCALL_GROUP_EXIT
)

func (this *SyscallConfig) SetSysCall(syscall string) error {
    this.Enable = true
    if syscall == "all" {
        this.HookALL = true
        return nil
    }
    items := strings.Split(syscall, ",")
    if len(items) > MAX_COUNT {
        return fmt.Errorf("max syscall whitelist count is %d, provided count:%d", MAX_COUNT, len(items))
    }
    for i, v := range items {
        point := GetWatchPointByName(v)
        nr_point, ok := (point).(*SysCallArgs)
        if !ok {
            return errors.New(fmt.Sprintf("cast [%s] watchpoint to SysCallArgs failed", v))
        }
        this.syscall[i] = uint32(nr_point.NR)
        this.syscall_mask |= (1 << i)
    }
    return nil
}

func (this *SyscallConfig) SetSysCallBlacklist(syscall_blacklist string) error {
    items := strings.Split(syscall_blacklist, ",")
    if len(items) > MAX_COUNT {
        return fmt.Errorf("max syscall blacklist count is %d, provided count:%d", MAX_COUNT, len(items))
    }
    for i, v := range items {
        point := GetWatchPointByName(v)
        nr_point, ok := (point).(*SysCallArgs)
        if !ok {
            panic(fmt.Sprintf("cast [%s] watchpoint to SysCallArgs failed", v))
        }
        this.syscall_blacklist[i] = uint32(nr_point.NR)
        this.syscall_blacklist_mask |= (1 << i)
    }
    return nil
}

func (this *SyscallConfig) IsEnable() bool {
    return this.Enable
}

func (this *SyscallConfig) Check() error {

    return nil
}

func (this *SyscallConfig) Info() string {
    var watchlist []string
    for _, v := range this.syscall {
        if v == MAGIC_SYSCALL {
            continue
        }
        point := GetWatchPointByNR(v)
        nr_point, ok := (point).(*SysCallArgs)
        if !ok {
            panic(fmt.Sprintf("cast [%d] watchpoint to SysCallArgs failed", v))
        }
        watchlist = append(watchlist, nr_point.Name())
    }
    return fmt.Sprintf("watch:%s", strings.Join(watchlist, ","))
}

type ModuleConfig struct {
    SConfig
    TidsBlacklistMask uint32
    TidsBlacklist     [MAX_COUNT]uint32
    PidsBlacklistMask uint32
    PidsBlacklist     [MAX_COUNT]uint32
    TNamesWhitelist   []string
    TNamesBlacklist   []string
    Name              string
    StackUprobeConf   StackUprobeConfig
    SysCallConf       SyscallConfig
}

func NewModuleConfig() *ModuleConfig {
    config := &ModuleConfig{}
    config.SelfPid = uint32(os.Getpid())
    config.FilterMode = util.UNKNOWN_MODE
    // 首先把 logger 设置上
    // config.SetLogger(logger)
    // 虽然会通过全局配置进程覆盖 但是还是做好在初始化时就进行默认赋值
    config.Uid = MAGIC_UID
    config.Pid = MAGIC_PID
    config.Tid = MAGIC_TID
    for i := 0; i < len(config.PidsBlacklist); i++ {
        config.PidsBlacklist[i] = MAGIC_PID
    }
    for i := 0; i < len(config.TidsBlacklist); i++ {
        config.PidsBlacklist[i] = MAGIC_TID
    }
    return config
}

func (this *ModuleConfig) Check() error {

    return nil
}

func (this *ModuleConfig) Info() string {
    // 调用号信息
    return fmt.Sprintf("-")
}

func (this *ModuleConfig) SetTidsBlacklist(tids_blacklist string) error {
    if tids_blacklist == "" {
        return nil
    }
    this.TidsBlacklistMask = 0
    items := strings.Split(tids_blacklist, ",")
    if len(items) > MAX_COUNT {
        return fmt.Errorf("max tid blacklist count is %d, provided count:%d", MAX_COUNT, len(items))
    }
    for i, v := range items {
        value, _ := strconv.ParseUint(v, 10, 32)
        this.TidsBlacklist[i] = uint32(value)
        this.TidsBlacklistMask |= (1 << i)
    }
    return nil
}

func (this *ModuleConfig) SetPidsBlacklist(pids_blacklist string) error {
    if pids_blacklist == "" {
        return nil
    }
    this.PidsBlacklistMask = 0
    items := strings.Split(pids_blacklist, ",")
    if len(items) > MAX_COUNT {
        return fmt.Errorf("max pid blacklist count is %d, provided count:%d", MAX_COUNT, len(items))
    }
    for i, v := range items {
        value, _ := strconv.ParseUint(v, 10, 32)
        this.PidsBlacklist[i] = uint32(value)
        this.PidsBlacklistMask |= (1 << i)
    }
    return nil
}
func (this *ModuleConfig) SetTNamesBlacklist(t_names_blacklist string) error {
    if t_names_blacklist == "" {
        return nil
    }
    items := strings.Split(t_names_blacklist, ",")
    if len(items) > MAX_COUNT {
        return fmt.Errorf("max thread name blacklist count is %d, provided count:%d", MAX_COUNT, len(items))
    }
    for _, v := range items {
        this.TNamesBlacklist = append(this.TNamesBlacklist, v)
    }
    return nil
}
func (this *ModuleConfig) SetTNamesWhitelist(t_names_blacklist string) error {
    if t_names_blacklist == "" {
        return nil
    }
    items := strings.Split(t_names_blacklist, ",")
    if len(items) > MAX_COUNT {
        return fmt.Errorf("max thread name whitelist count is %d, provided count:%d", MAX_COUNT, len(items))
    }
    for _, v := range items {
        this.TNamesWhitelist = append(this.TNamesWhitelist, v)
    }
    return nil
}

func (this *ModuleConfig) UpdateRevFilter(rev_filter *ebpf.Map) (err error) {
    // ./stackplz -n com.starbucks.cn --iso -s newfstatat,openat,faccessat --hide-root -o tmp.log -q
    var rev_list []string = []string{
        "/sbin/su",
        "/sbin/.magisk/",
        "/dev/.magisk",
        "/system/bin/magisk",
        "/system/bin/su",
        "/system/xbin/su",
        // "ro.debuggable",
        "/proc/mounts",
        "which su",
        "mount",
    }

    for _, v := range rev_list {
        if len(v) > 32 {
            panic(fmt.Sprintf("[%s] rev string max len is 32", v))
        }
        key_value := 1
        filter := RevFilter{}
        copy(filter.RevString[:], v)
        err = rev_filter.Update(unsafe.Pointer(&filter), unsafe.Pointer(&key_value), ebpf.UpdateAny)
        if err != nil {
            return err
        }
    }
    return err
}

func (this *ModuleConfig) UpdateThreadFilter(thread_filter *ebpf.Map) (err error) {
    var thread_blacklist []string = []string{
        "RenderThread",
        "FinalizerDaemon",
        "RxCachedThreadS",
        "mali-cmar-backe",
        "mali-utility-wo",
        "mali-mem-purge",
        "mali-hist-dump",
        "hwuiTask0",
        "hwuiTask1",
        "NDK MediaCodec_",
    }

    for _, v := range thread_blacklist {
        if len(v) > 16 {
            panic(fmt.Sprintf("[%s] thread name max len is 16", v))
        }
        thread_value := 1
        filter := ThreadFilter{}
        copy(filter.ThreadName[:], v)
        err = thread_filter.Update(unsafe.Pointer(&filter), unsafe.Pointer(&thread_value), ebpf.UpdateAny)
        if err != nil {
            return err
        }
    }
    for _, v := range this.TNamesBlacklist {
        if len(v) > 16 {
            panic(fmt.Sprintf("[%s] thread name max len is 16", v))
        }
        thread_value := 1
        filter := ThreadFilter{}
        copy(filter.ThreadName[:], v)
        err = thread_filter.Update(unsafe.Pointer(&filter), unsafe.Pointer(&thread_value), ebpf.UpdateAny)
        if err != nil {
            return err
        }
    }
    for _, v := range this.TNamesWhitelist {
        if len(v) > 16 {
            panic(fmt.Sprintf("[%s] thread name max len is 16", v))
        }
        thread_value := 2
        filter := ThreadFilter{}
        copy(filter.ThreadName[:], v)
        err = thread_filter.Update(unsafe.Pointer(&filter), unsafe.Pointer(&thread_value), ebpf.UpdateAny)
        if err != nil {
            return err
        }
    }
    return err
}
func (this *ModuleConfig) GetCommonFilter() unsafe.Pointer {
    filter := CommonFilter{}
    filter.is_32bit = 0
    filter.uid = this.Uid
    filter.pid = this.Pid
    filter.tid = this.Tid
    // 这些暂时硬编码
    for i := 0; i < MAX_COUNT; i++ {
        filter.pid_list[i] = MAGIC_PID
    }
    for i := 0; i < MAX_COUNT; i++ {
        filter.blacklist_pids[i] = this.PidsBlacklist[i]
    }
    for i := 0; i < MAX_COUNT; i++ {
        filter.blacklist_tids[i] = this.TidsBlacklist[i]
    }
    filter.thread_name_whitelist = 0
    if len(this.TNamesWhitelist) > 0 {
        filter.thread_name_whitelist = 1
    }
    filter.trace_isolated = 0
    if this.TraceIsolated {
        filter.trace_isolated = 1
    }
    filter.signal = this.UprobeSignal
    if this.Debug {
        this.logger.Printf("CommonFilter{uid=%d, pid=%d, tid=%d, is_32bit=%d, whitelist:%d}", filter.uid, filter.pid, filter.tid, filter.is_32bit, filter.thread_name_whitelist)
    }
    return unsafe.Pointer(&filter)
}

func (this *ModuleConfig) GetConfigMap() ConfigMap {
    config := ConfigMap{}
    config.stackplz_pid = this.SelfPid
    config.filter_mode = this.FilterMode
    if this.Debug {
        this.logger.Printf("ConfigMap{stackplz_pid=%d}", config.stackplz_pid)
    }
    return config
}
