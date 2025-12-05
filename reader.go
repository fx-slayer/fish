package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

const ProgressFile = ".cmdline-reader-progress"

const (
	CmdExit byte = iota
	CmdNextPage
	CmdPrevPage
	CmdNextLine
	CmdPrevLine
	CmdNextHalfPage
	CmdSwitchScrolling
	CmdNULL // CmdNULL is used to indicate no command received but call Reader.renderPage.
)

// Reader is a command-line reader designed for reading books/long-text file.
//
// Reader.pageFactor: Default 0.75 ,due to line wrapping of long single lines, the terminal height does not
// always match the number of text lines, so precise page turns cannot be achieved.
// To ensure that the number of lines turned is less than the actual terminal height,
// the actual NextPage/PrevPage commands use fewer lines than the ideal count.
type Reader struct {
	f                 string
	data              string
	progressFile      string // progress file path
	progressFD        *os.File
	progress          map[string]int // map[abs-filepath]progress
	previousSavedLine int
	jumpBreakMark     int
	pageFactor        float64 // see Reader doc.
	displayBreakMark  bool
	index             []string // line number:line content
	totalLine         int
	currentLine       int
	winHeight         int
	winWidth          int
	scrollingLine     int
	scrollingTk       <-chan time.Time
	renderSignal      chan struct{}
	eventSignal       chan byte
	quitSignal        chan struct{}
}

// NewReader creates new reader, f must be absolute file path.
func NewReader(f string) Reader {
	return Reader{
		f:            f,
		index:        []string{},
		progress:     make(map[string]int),
		scrollingTk:  time.Tick(time.Second),
		renderSignal: make(chan struct{}),
		eventSignal:  make(chan byte),
		quitSignal:   make(chan struct{}),
		pageFactor:   0.75,
	}
}

// daemonCatchInput 监听键盘输入并将用户命令转换为事件信号。
// 在RAW模式下直接捕获按键，无需用户按回车确认。
// 支持的快捷键：
// - 'q', Ctrl+C, Ctrl+D: 退出阅读器
// - 'a': 切换自动滚屏模式
// - Enter: 下一行
// - Space: 下半页
// - 上/下/左/右箭头: 上一行/下一行/上一页/下一页
// 通过 r.eventSignal channel 向主事件循环发送命令。
func (r *Reader) daemonCatchInput() {
	var b [3]byte
	for {
		select {
		case <-r.quitSignal:
			return
		default:
		}
		_, err := os.Stdin.Read(b[:])
		if err != nil {
			continue
		}
		switch b[0] {
		case 0x03, 0x04, 'q': // ctrl + c = 0x03 | ctrl + d = 0x04
			r.eventSignal <- CmdExit
		case 'a':
			r.eventSignal <- CmdSwitchScrolling
		case 0x0d: // key: enter
			r.eventSignal <- CmdNextLine
		case ' ':
			r.eventSignal <- CmdNextHalfPage
		case 0x1b:
			if b[1] != 0x5b {
				continue
			}
			switch b[2] {
			case 0x41: // up arrow
				r.eventSignal <- CmdPrevLine
			case 0x42: // down arrow
				r.eventSignal <- CmdNextLine
			case 0x43: // right arrow
				r.eventSignal <- CmdNextPage
			case 0x44: // left arrow
				r.eventSignal <- CmdPrevPage
			default:
				continue
			}
		}
	}
}

func (r *Reader) saveProgress() {
	// TODO exec when quit only?
	if r.previousSavedLine == r.currentLine {
		return
	}
	r.previousSavedLine = r.currentLine
	r.progress[r.f] = r.previousSavedLine
	pp, _ := json.MarshalIndent(r.progress, "", "  ")
	_ = r.progressFD.Truncate(0)
	_, _ = r.progressFD.Seek(0, 0)
	_, _ = r.progressFD.Write(pp)
}

func (r *Reader) loadProgress() error {
	u, e := os.UserHomeDir()
	if e != nil {
		return e
	}
	d := filepath.Join(u, ProgressFile)
	if _, e := os.Stat(d); os.IsNotExist(e) {
		if e := os.WriteFile(d, []byte("{}"), 0644); e != nil {
			return e
		}
	}
	f, e := os.OpenFile(d, os.O_RDWR, 0644)
	if e != nil {
		return e
	}
	r.progressFile = d
	r.progressFD = f
	pp, e := io.ReadAll(f)
	if e != nil {
		return e
	}
	if e := json.Unmarshal(pp, &r.progress); e != nil {
		return e
	}
	pos, ok := r.progress[r.f]
	if ok {
		r.currentLine = pos
		r.previousSavedLine = pos
	}
	return nil
}

func (r *Reader) createIndex() error {
	dd, e := os.ReadFile(r.f)
	if e != nil {
		return e
	}
	r.data = string(dd)
	r.index = strings.Split(r.data, "\n")
	r.totalLine = len(r.index)
	return nil
}

func (r *Reader) updateWindowsSize() {
	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		panic(err)
	}
	r.winWidth = width
	r.winHeight = height
	r.renderPage()
}

// daemonUpdateWindowSize 监听终端窗口大小变化信号(SIGWINCH)。
// 当用户调整终端窗口大小时，捕获该信号并立即重新计算窗口尺寸，
// 重新渲染页面以适应新的窗口大小。
// 这确保阅读器在任何时刻都与实际终端尺寸保持同步。
func (r *Reader) daemonUpdateWindowSize() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	go func() {
		for {
			select {
			case <-sigCh:
				r.updateWindowsSize()
			case <-r.quitSignal:
				return
			}
		}
	}()
}

func (r *Reader) enterRawMode() (restore func(), err error) {
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() {
		_ = term.Restore(fd, oldState)
	}, nil
}

func (r *Reader) printInfo() {
	var percentage float64
	if r.totalLine > 0 {
		percentage = float64(r.currentLine) / float64(r.totalLine) * 100
	}
	_, _ = fmt.Fprintf(os.Stdout, "> %s %d/%d %.02f%% [Q]:Quit [A]:Scroll(%s)", path.Base(r.f), r.currentLine, r.totalLine, percentage, r.scrollInfo())
}

func (r *Reader) scrollInfo() string {
	switch r.scrollingLine {
	case 0:
		return "off"
	case 1:
		return "1"
	case 2:
		return "2"
	default:
		return "?"
	}
}

func (r *Reader) clearScreenRaw() {
	_, _ = fmt.Fprint(os.Stdout, "\033[2J\033[H")
}

func (r *Reader) enterAltScreen() {
	_, _ = os.Stdout.Write([]byte("\x1b[?1049h"))
}

func (r *Reader) exitAltScreen() {
	_, _ = os.Stdout.Write([]byte("\x1b[?1049l"))
}

func (r *Reader) renderPage() {
	start := r.currentLine
	r.clearScreenRaw()
	pageLines := r.winHeight - 1
	end := start + pageLines
	if end > len(r.index) {
		end = len(r.index)
	}
	for i := start; i < end; i++ {
		if r.displayBreakMark && i == r.jumpBreakMark {
			br := strings.Repeat(">", r.winWidth*3/4)
			_, _ = fmt.Fprint(os.Stdout, br+"\r\n"+r.index[i]+"\r\n")
		} else {
			_, _ = fmt.Fprint(os.Stdout, r.index[i]+"\r\n")
		}
	}
	for i := end - start; i < pageLines; i++ {
		_, _ = fmt.Fprint(os.Stdout, "\r\n")
	}
	r.printInfo()
	r.saveProgress()
}

// daemonRenderPage 监听渲染信号并负责页面刷新。
// 当收到 r.renderSignal 信号时，调用 renderPage() 重新绘制当前页面。
// 这种基于信号的渲染方式避免了不必要的频繁刷新，提高了性能。
// 使用 channel 实现事件驱动的渲染机制，确保渲染和输入处理的并发安全。
func (r *Reader) daemonRenderPage() {
	for {
		select {
		case <-r.renderSignal:
			r.renderPage()
		case <-r.quitSignal:
			return
		}
	}
}

// daemonScrolling 实现自动滚屏功能，每秒触发一次(由 r.scrollingTk 时间ticker驱动)。
// 当启用自动滚屏时(scrollingLine > 0)，每秒向下自动翻滚指定行数。
// scrollingLine 的值为0(关闭)、1或2，用户可通过按'a'键循环切换。
// 每次自动滚屏后发送 CmdNULL 命令触发页面重新渲染，更新显示内容。
// 到达文末时停止滚屏。
func (r *Reader) daemonScrolling() {
	for {
		select {
		case <-r.scrollingTk:
			if r.scrollingLine > 0 {
				if r.currentLine < r.totalLine-1 {
					r.currentLine += r.scrollingLine
				}
				r.eventSignal <- CmdNULL
			}
		case <-r.quitSignal:
			return
		}
	}
}

func (r *Reader) Run() error {
	defer r.close()
	r.enterAltScreen()
	defer r.exitAltScreen()
	r.clearScreenRaw()
	if e := r.createIndex(); e != nil {
		return e
	}
	if e := r.loadProgress(); e != nil {
		return e
	}
	r.updateWindowsSize()
	rstore, e := r.enterRawMode()
	if e != nil {
		return e
	}
	defer rstore()
	go r.daemonUpdateWindowSize()
	go r.daemonScrolling()
	go r.daemonRenderPage()
	go r.daemonCatchInput()
	if r.currentLine > r.totalLine {
		r.currentLine = 0
	}
	r.renderPage()
	for {
		switch <-r.eventSignal {
		case CmdNULL:
			// no op.
		case CmdSwitchScrolling:
			if r.scrollingLine == 2 {
				r.scrollingLine = 0
			} else {
				r.scrollingLine++
			}
		case CmdExit:
			return nil
		case CmdNextPage: // actually set to next 0.75 page
			r.setBreakMark()
			off := int(math.Round(float64(r.winHeight) * r.pageFactor))
			if r.currentLine+off < r.totalLine {
				r.currentLine += off
			} else if r.currentLine < r.totalLine-1 {
				r.currentLine = r.totalLine - 1
			}
		case CmdPrevPage: // actually set to prev 0.75 page
			r.setBreakMark()
			off := int(math.Round(float64(r.winHeight) * r.pageFactor))
			if r.currentLine-off >= 0 {
				r.currentLine -= off
			} else {
				r.currentLine = 0
			}
		case CmdNextLine:
			if r.currentLine < r.totalLine-1 {
				r.currentLine++
			}
		case CmdPrevLine:
			if r.currentLine > 0 {
				r.currentLine--
			}
		case CmdNextHalfPage:
			r.setBreakMark()
			off := r.winHeight / 2
			if r.currentLine+off < r.totalLine {
				r.currentLine += off
			} else if r.currentLine < r.totalLine-1 {
				r.currentLine = r.totalLine - 1
			}
		}
		r.renderSignal <- struct{}{}
	}
}

func (r *Reader) setBreakMark() {
	r.jumpBreakMark = r.currentLine + r.winHeight - 1
	r.displayBreakMark = true
}

func (r *Reader) close() {
	if r.progressFD != nil {
		_ = r.progressFD.Close()
	}
	close(r.quitSignal)
}
