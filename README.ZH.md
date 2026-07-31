### README

[English](README.md) | 简体中文

`fish` 是一个用于阅读书籍的命令行阅读器，可能支持以下特性：

> 摸鱼用，终端环境让你看起来更像在工作。

- 记住阅读行号。✅

- 显示阅读进度。✅

- 上下翻页快捷键。✅

  - `space` 向下翻半页。
  - `↓`、`enter` 下一行。
  - `↑` 上一行。
  - `→` 下一页。
  - `←` 上一页。
  - `q`、`ctrl+c`、`ctrl+d` 退出。

- 自动滚屏。✅

  - `a` 切换滚屏模式，按 `off` → `1` → `2` 行每秒循环。

- 宽字符感知的折行。✅

  长行会按终端宽度预先折行，中日韩等双宽字符按两格计算。渲染出的一行始终
  对应终端的一行，因此翻页是精确的，页面也不会溢出屏幕。

#### 用法

```bash
fish <FILE>
```

阅读进度按文件分别保存在 `~/.cmdline-reader-progress`，下次运行时恢复。进度以文件中
的行号记录，因此调整终端大小不会改变你的阅读位置。

#### 安装

```bash
git clone https://github.com/fx-slayer/fish.git
cd fish
make install
```

将为 `darwin/arm64` 构建到 `~/go/bin/fish`。其他平台请调整 `Makefile` 中的
`GOOS`/`GOARCH`。
