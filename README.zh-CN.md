# FindRelay

[English](README.md) | 简体中文

FindRelay 让 Finder 照常运行，把它的两个入口交给你选择的文件管理器：点击 Dock 上的 Finder，以及在桌面上双击文件夹。桌面和文件关联仍由 Finder 管理。

- 已在 macOS 27.0、Apple Silicon 上测试。
- MIT 许可。

## 转发哪些操作

**Dock**（`--dock`，默认开启）。左键单击 Dock 上的 Finder 图标，会打开或激活目标应用。长按、拖拽、右键和按住修饰键的点击保持原样。目标应用打开失败时，FindRelay 改为打开一次 Finder。

**桌面**（`--desktop`，默认关闭）。在原生桌面上双击文件夹，会在目标应用里打开这个文件夹。第一击照常交给 Finder；只有第二击落在同一个、且是唯一选中的文件夹图标上时，FindRelay 才会转发。

以下情况仍由 Finder 处理：

- 符号链接、替身、应用包和其他文件包、堆叠、多选、网络卷和外置卷，以及 iCloud 项目。
- Finder 窗口和桌面空白处。
- 单击、拖拽、框选、重命名、右键、按住修饰键的点击，以及连击中的第三击及之后。

文件夹通过系统标准的“打开”事件送达，与 `open -a <应用> <文件夹>` 相同。有些应用收到这个事件后只把文件夹加进侧边栏或最近项目，并不打开它。遇到这类应用，请改用 `--open-command`（见下文）。

## 环境要求

- Xcode Command Line Tools（`xcode-select --install`）
- Go 1.27.3 或更高版本

## 构建

```bash
make build              # 生成 dist/findrelay
./dist/findrelay version
```

## 权限

打开 **系统设置 → 隐私与安全性 → 辅助功能**，允许运行 `findrelay` 的程序：在前台运行时是你的终端，作为登录服务运行时是 `findrelay` 可执行文件本身。如果事件 tap 仍然创建失败，再检查 **输入监控**。

桌面入口需要读取“桌面”文件夹来判断点中的是什么。系统可能会弹出“文件与文件夹”访问提示，也可以在 **系统设置 → 隐私与安全性 → 文件与文件夹** 里允许。没有这项权限时，桌面双击仍由 Finder 处理，日志里出现 `desktop_folder_permission_denied`，Dock 入口不受影响。

## 使用

每次运行的参数就是全部配置，FindRelay 不写配置文件。

```bash
./dist/findrelay run --app "/Applications/Bloom.app"                        # Dock 入口（默认开启）
./dist/findrelay run --app "/Applications/Bloom.app" --desktop              # Dock 和桌面入口
./dist/findrelay run --app "/Applications/Bloom.app" --dock=false --desktop  # 只开桌面入口
./dist/findrelay run --observe                                              # 只观察，不需要目标应用
```

| 参数 | 含义 |
| --- | --- |
| `--app <path>` | 目标 `.app` 应用包。除 `--observe` 外必填。不接受 Finder 和 FindRelay 自身。 |
| `--dock` | 转发 Dock 上的 Finder 点击。默认开启，`--dock=false` 关闭。 |
| `--desktop` | 转发桌面上的文件夹双击。默认关闭。 |
| `--open-command <template>` | 通过一条 shell 命令把文件夹交给目标应用（见下文）。 |
| `--dock-intercept=up\|down` | 在什么时候接管 Dock 点击：抬起时（`up`，默认）或按下时（`down`）。 |
| `--desktop-intercept=up\|down` | 在什么时候接管桌面的第二击：抬起时（`up`，默认）或按下时（`down`）。 |
| `--observe` | 只监听，输出脱敏的命中样本，不拦截任何事件。 |
| `--debug` | 等同于 `--log-level=debug`。 |
| `--log-level=debug\|info\|warn\|error` | 日志级别，默认 `warn`。 |

按 `Ctrl+C` 停止，再按一次立即退出。两个入口都关闭时，FindRelay 直接退出，不创建事件 tap。完整用法见 `findrelay run --help`。

开启 `--debug` 后，每次按下和抬起各输出一行 `gesture_event`。看三个字段就能知道发生了什么：

- `source=` 说明结论是怎么得出的。`system` 是系统级命中；`off_dock` 表示只问了 Dock，那里没有元素；`covered` 表示指针下面是其他应用的窗口；`same` 和 `changed` 是手势过程中的复核。
- `class=` 是脱敏后的类别，例如 `dock_finder`、`desktop_icon`、`off_dock`、`covered`、`target_changed`。
- `decision=` 是处理结果。`pass` 表示放行。默认的 `up` 模式下，转发成功记为 `cancel_up+activate_app`（Dock）或 `cancel_desktop_up+open_directory`（桌面）；`down` 模式下记为 `consume+activate_app` 或 `consume+open_directory`。

日志不包含文件名、路径、URL、坐标，也不记录被点击的是哪个应用。退出前的最后一行是回调耗时汇总。

### 检查配置

```bash
./dist/findrelay doctor                                  # 检查已安装服务里记录的目标应用
./dist/findrelay doctor --app "/Applications/Bloom.app"  # 检查指定的目标应用
```

`doctor` 会报告系统版本、GUI 会话、目标应用、已安装的服务和权限预检结果。

### 自定义打开命令

如果目标应用不响应标准的“打开”事件，可以用 `--open-command` 给出一条命令模板。模板经 `/bin/sh` 执行，文件夹路径放在环境变量 `folder` 里，所以要写成带引号的 `"$folder"`：

```bash
./dist/findrelay run --app "/Applications/FileOS.app" --desktop --open-command 'open -a FileOS "$folder"'
./dist/findrelay run --app "/Applications/FileOS.app" --desktop --open-command '/path/to/open_in_fileos.sh "$folder"'
./dist/findrelay run --app "/Applications/FileOS.app" --desktop --open-command 'shortcuts run "在 FileOS 中打开" --input-path "$folder"'
```

- 模板必须是一行，不超过 500 个字符，并且引用 `$folder` 或 `${folder}`。
- 命令以 FindRelay 自身的权限运行，输出一律丢弃，只看退出状态，最长等待 15 秒。
- 失败或超时时，日志记录 `open_command_failed` 或 `open_command_timeout`，并在 Finder 里打开一次该文件夹。
- 先在终端里用真实路径试一遍命令。这个参数不影响 Dock 入口。

### 登录服务

想在关闭终端后继续运行、并在登录时自动启动，先把可执行文件放到固定位置，再安装用户级 LaunchAgent：

```bash
mkdir -p ~/.local/bin && cp dist/findrelay ~/.local/bin/
~/.local/bin/findrelay service install --app "/Applications/Bloom.app" --desktop   # 写入参数，不启动
~/.local/bin/findrelay service start       # 立即启动，并允许登录时启动
~/.local/bin/findrelay service status      # plist、登录启动、进程状态、转发是否生效
~/.local/bin/findrelay service restart     # 重启以换用新参数
~/.local/bin/findrelay service stop        # 停止并关闭登录启动，保留 plist
~/.local/bin/findrelay service uninstall   # 停止并移除 LaunchAgent
```

- `install` 写入 `~/Library/LaunchAgents/io.findrelay.agent.plist`，服务保持禁用；执行 `start` 之后才会在登录时启动。这些命令都不需要 root。
- `install` 接受与 `run` 相同的参数。再次安装只替换参数；正在运行的实例要 `restart` 后才换用新参数。
- LaunchAgent 记录的是可执行文件的绝对路径。移动文件后要重新安装；用新构建替换后，要重新确认它的辅助功能权限。
- 辅助功能权限要授予 `findrelay` 可执行文件本身，而不是终端。没有授权时服务以退出码 4 退出，`status` 会提示。
- 服务不读取 shell 配置，`--open-command` 里请写绝对路径。
- 前台的 `run` 和服务在同一个 GUI 会话里共用一把锁，只有一个能运行。锁被其他实例持有时，`status` 会说明。
- 服务退出后不会自动重启，运行 `service start` 即可恢复。

服务把脱敏日志写入系统统一日志：

```bash
log show --predicate 'subsystem == "io.findrelay.agent"' --last 10m
```

## 退出码

| 退出码 | 含义 |
| --- | --- |
| `0` | 正常结束，或两个入口都已关闭 |
| `2` | 用法错误 |
| `3` | 缺少目标应用，或目标应用无效 |
| `4` | 缺少辅助功能或输入监控权限 |
| `5` | 服务管理失败，例如未安装、对已停止的服务执行 `restart`、launchctl 出错，或记录的可执行文件不存在 |
| `6` | 运行期故障，或同一 GUI 会话里已有实例在运行 |

## 开发

```bash
make check   # gofmt、go vet、go build 和 CGO race 测试，与 CI 相同
make fmt     # 格式化 Go 源码
```

会接触真实系统的测试默认跳过：

```bash
FINDRELAY_AX_INTEGRATION=1 CGO_ENABLED=1 go test -count=1 ./internal/darwin -run Integration -v              # 只读的 AX 命中测试，需要已授权且屏幕解锁
FINDRELAY_LAUNCHCTL_INTEGRATION=1 CGO_ENABLED=1 go test -count=1 ./internal/service -run TestLaunchctlIntegration -v   # 真实 launchctl 生命周期；已安装服务时跳过
```

## 许可

[MIT](LICENSE)
