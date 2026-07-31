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
	CmdScrollTick // CmdScrollTick is emitted by daemonScrolling on every tick.
	CmdResize     // CmdResize is emitted by daemonUpdateWindowSize on SIGWINCH.
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
	scrollingTk       *time.Ticker
	eventSignal       chan byte
	quitSignal        chan struct{}
}

// NewReader creates new reader, f must be absolute file path.
//
// All mutable render state (currentLine, winWidth, winHeight, scrollingLine) is owned
// exclusively by the Run event loop. Daemons must not touch it; they only emit commands
// on eventSignal. This keeps both the state and stdout free of concurrent writers.
func NewReader(f string) Reader {
	return Reader{
		f:           f,
		index:       []string{},
		progress:    make(map[string]int),
		scrollingTk: time.NewTicker(time.Second),
		eventSignal: make(chan byte),
		quitSignal:  make(chan struct{}),
		pageFactor:  0.75,
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
	// A single Read may return several keypresses at once, or split one escape
	// sequence across calls, so an incomplete tail stays in the buffer and is
	// re-parsed once the remaining bytes arrive.
	var buf [64]byte
	fill := 0
	for {
		select {
		case <-r.quitSignal:
			return
		default:
		}
		if fill == len(buf) {
			fill = 0 // unreachable for the sequences handled here; avoids a zero-length Read
		}
		n, err := os.Stdin.Read(buf[fill:])
		if err != nil || n == 0 {
			continue
		}
		fill += n
		i := 0
		for i < fill {
			cmd, size, ok := decodeKey(buf[i:fill])
			if size == 0 {
				break // incomplete sequence, wait for more input
			}
			i += size
			if !ok {
				continue
			}
			select {
			case r.eventSignal <- cmd:
			case <-r.quitSignal:
				return
			}
		}
		fill = copy(buf[:], buf[i:fill])
	}
}

// decodeKey maps the leading keypress in b to a command. size is the number of
// bytes consumed, or 0 when b holds an incomplete escape sequence that should be
// retained until more input arrives. ok reports whether cmd is meaningful.
func decodeKey(b []byte) (cmd byte, size int, ok bool) {
	switch b[0] {
	case 0x03, 0x04, 'q': // ctrl + c = 0x03 | ctrl + d = 0x04
		return CmdExit, 1, true
	case 'a', 'A':
		return CmdSwitchScrolling, 1, true
	case 0x0d: // key: enter
		return CmdNextLine, 1, true
	case ' ':
		return CmdNextHalfPage, 1, true
	case 0x1b:
		if len(b) < 2 {
			return 0, 0, false
		}
		if b[1] != 0x5b { // not a CSI sequence, drop the ESC only
			return 0, 1, false
		}
		if len(b) < 3 {
			return 0, 0, false
		}
		switch b[2] {
		case 0x41: // up arrow
			return CmdPrevLine, 3, true
		case 0x42: // down arrow
			return CmdNextLine, 3, true
		case 0x43: // right arrow
			return CmdNextPage, 3, true
		case 0x44: // left arrow
			return CmdPrevPage, 3, true
		}
		return 0, 3, false
	}
	return 0, 1, false
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
	// A corrupted or truncated progress file must not block reading, and a literal
	// "null" unmarshals into a nil map that would panic on the next write.
	if len(pp) > 0 {
		if e := json.Unmarshal(pp, &r.progress); e != nil {
			r.progress = nil
		}
	}
	if r.progress == nil {
		r.progress = make(map[string]int)
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

func (r *Reader) updateWindowsSize() error {
	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return err
	}
	r.winWidth = width
	r.winHeight = height
	return nil
}

// daemonUpdateWindowSize 监听终端窗口大小变化信号(SIGWINCH)，
// 并向主事件循环投递 CmdResize，由主循环重新取尺寸并渲染。
// 该 goroutine 自身不读写任何渲染状态。
func (r *Reader) daemonUpdateWindowSize() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)
	for {
		select {
		case <-sigCh:
			select {
			case r.eventSignal <- CmdResize:
			case <-r.quitSignal:
				return
			}
		case <-r.quitSignal:
			return
		}
	}
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

// daemonScrolling 每秒向主事件循环投递一次 CmdScrollTick。
// 是否真的滚屏、滚几行由主循环依据 scrollingLine 决定，
// 该 goroutine 自身不读写任何渲染状态。
func (r *Reader) daemonScrolling() {
	for {
		select {
		case <-r.scrollingTk.C:
			select {
			case r.eventSignal <- CmdScrollTick:
			case <-r.quitSignal:
				return
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
	if e := r.updateWindowsSize(); e != nil {
		return e
	}
	rstore, e := r.enterRawMode()
	if e != nil {
		return e
	}
	defer rstore()
	go r.daemonUpdateWindowSize()
	go r.daemonScrolling()
	go r.daemonCatchInput()
	// The saved progress may exceed the file if it was edited since the last run,
	// and a hand-edited progress file may hold a negative value.
	if r.currentLine >= r.totalLine {
		r.currentLine = r.totalLine - 1
	}
	if r.currentLine < 0 {
		r.currentLine = 0
	}
	r.renderPage()
	for {
		switch <-r.eventSignal {
		case CmdResize:
			if e := r.updateWindowsSize(); e != nil {
				return e
			}
		case CmdScrollTick:
			if r.scrollingLine <= 0 {
				continue
			}
			if r.currentLine < r.totalLine-1 {
				r.currentLine += r.scrollingLine
			}
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
		r.renderPage()
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
	if r.scrollingTk != nil {
		r.scrollingTk.Stop()
	}
	close(r.quitSignal)
}
