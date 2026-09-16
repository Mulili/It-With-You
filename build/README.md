# 构建目录（build）

`build` 目录用于存放应用构建所需的全部文件与资源。

目录结构：

* `bin` - 构建产物输出目录
* `darwin` - macOS 专用文件
* `windows` - Windows 专用文件

## macOS

`darwin` 目录存放 macOS 构建专用的文件，可以按需定制并参与构建。
若想恢复为默认状态，直接把这些文件删除，然后重新执行 `wails build` 即可。

该目录包含以下文件：

- `Info.plist` - macOS 构建使用的主 plist 文件，执行 `wails build` 时生效。
- `Info.dev.plist` - 内容与主 plist 相同，但仅在执行 `wails dev` 时生效。

## Windows

`windows` 目录存放使用 `wails build` 构建 Windows 版本时所需的清单文件（manifest）与资源文件（rc）。
这些文件可以针对你的应用进行定制；若要恢复默认状态，直接删除它们再执行 `wails build` 即可重新生成。

- `icon.ico` - 应用程序图标，执行 `wails build` 时使用。若想换图标，直接用你自己的文件替换即可。
  如果该文件缺失，构建时会根据 `build` 目录下的 `appicon.png` 自动生成一个新的 `icon.ico`。
- `installer/*` - 用于生成 Windows 安装包的文件，执行 `wails build` 时使用。
- `info.json` - Windows 构建使用的应用信息。这里的数据会同时被安装包和应用本身使用
  （右键 exe → 属性 → 详细信息）。
- `wails.exe.manifest` - 应用程序主清单文件。

> 📌 **本项目的额外约定**
>
> `icon.ico` 除了作为程序图标，还被 `main.go` 通过 `//go:embed build/windows/icon.ico`
> 复用为**系统托盘图标**（见 `internal/ui/tray.go`）。
> 因此**不要删除该文件**：不仅程序图标会变，托盘图标也会一起失效。
> 换图标时记得托盘用的是 `.ico` 格式——systray 在 Windows 下加载 png 会失败。
