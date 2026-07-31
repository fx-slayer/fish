package main

import (
	"encoding/json"
	"fmt"
	"io"
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
// Rendering works on visual lines: every logical line of the file is pre-folded to
// the terminal width (accounting for double-width characters), so one entry of
// Reader.index always occupies exactly one terminal row. Paging can therefore be
// exact, and long lines no longer overflow the screen and scroll the page away.
// Reader.currentLine indexes visual lines, while progress is persisted as a logical
// line number so it stays valid across terminal widths.
type Reader struct {
	f                 string
	data              string
	progressFile      string // progress file path
	progressFD        *os.File
	progress          map[string]int // map[abs-filepath]progress
	previousSavedLine int
	savedLogical      int // logical line restored from the progress file
	jumpBreakMark     int // visual line the break mark is drawn above
	lastPageLines     int // content rows emitted by the last renderPage
	displayBreakMark  bool
	lines             []string // logical lines, as split from the file
	index             []vLine  // visual lines, folded to the terminal width
	firstVis          []int    // logical line -> first visual line
	totalLine         int
	currentLine       int
	winHeight         int
	winWidth          int
	scrollingLine     int
	scrollingTk       *time.Ticker
	eventSignal       chan byte
	quitSignal        chan struct{}
}

// vLine is one terminal row: the text to print plus the logical line it came from.
type vLine struct {
	text    string
	logical int
}

// NewReader creates new reader, f must be absolute file path.
//
// All mutable render state (currentLine, winWidth, winHeight, scrollingLine) is owned
// exclusively by the Run event loop. Daemons must not touch it; they only emit commands
// on eventSignal. This keeps both the state and stdout free of concurrent writers.
func NewReader(f string) Reader {
	return Reader{
		f:            f,
		index:        []vLine{},
		progress:     make(map[string]int),
		scrollingTk:  time.NewTicker(time.Second),
		eventSignal:  make(chan byte),
		quitSignal:   make(chan struct{}),
		savedLogical: -1,
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
	// Progress is stored as a logical line so it survives a terminal resize.
	cur := r.logicalAt(r.currentLine)
	if r.previousSavedLine == cur {
		return
	}
	r.previousSavedLine = cur
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
		r.savedLogical = pos
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
	r.lines = strings.Split(r.data, "\n")
	return nil
}

// buildVisualIndex folds every logical line to the current terminal width so that
// one index entry maps to exactly one terminal row. It must be called after the
// window size is known and again whenever the width changes.
func (r *Reader) buildVisualIndex() {
	r.index = make([]vLine, 0, len(r.lines))
	r.firstVis = make([]int, len(r.lines))
	for li, l := range r.lines {
		r.firstVis[li] = len(r.index)
		for _, chunk := range foldLine(l, r.winWidth) {
			r.index = append(r.index, vLine{text: chunk, logical: li})
		}
	}
	r.totalLine = len(r.index)
}

// logicalAt returns the logical line shown at visual line v.
func (r *Reader) logicalAt(v int) int {
	if v < 0 || v >= len(r.index) {
		return 0
	}
	return r.index[v].logical
}

// visualAt returns the first visual line of logical line l.
func (r *Reader) visualAt(l int) int {
	if l < 0 || l >= len(r.firstVis) {
		return 0
	}
	return r.firstVis[l]
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

// daemonCatchSignal turns termination signals into CmdExit so the reader leaves
// through the normal path. Without this the process dies with the terminal still
// in raw mode, on the alternate screen and with the cursor hidden, which the
// user's shell has no way to recover from.
func (r *Reader) daemonCatchSignal() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	defer signal.Stop(sigCh)
	select {
	case <-sigCh:
		select {
		case r.eventSignal <- CmdExit:
		case <-r.quitSignal:
		}
	case <-r.quitSignal:
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
	cur, total := r.logicalAt(r.currentLine)+1, len(r.lines)
	var percentage float64
	if total > 0 {
		percentage = float64(cur) / float64(total) * 100
	}
	s := fmt.Sprintf("> %s %d/%d %.02f%% [Q]:Quit [A]:Scroll(%s)", path.Base(r.f), cur, total, percentage, r.scrollInfo())
	// The status line owns the last row; wrapping it would scroll the page.
	_, _ = fmt.Fprint(os.Stdout, truncateWidth(s, r.winWidth))
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

// hideCursor stops the cursor from blinking at the end of the status line and from
// jumping around while a page is redrawn (DECTCEM).
func (r *Reader) hideCursor() {
	_, _ = os.Stdout.Write([]byte("\x1b[?25l"))
}

func (r *Reader) showCursor() {
	_, _ = os.Stdout.Write([]byte("\x1b[?25h"))
}

// renderPage draws exactly winHeight-1 content rows plus the status line. Every
// emitted row is one terminal row, and the break mark consumes a row of the page
// budget, so the output can never exceed the screen and scroll the top away.
func (r *Reader) renderPage() {
	r.clearScreenRaw()
	pageLines := r.pageLines()
	used, shown := 0, 0
	for i := r.currentLine; i < len(r.index) && used < pageLines; i++ {
		if r.displayBreakMark && i == r.jumpBreakMark {
			if used+1 >= pageLines {
				break // no room for both the mark and its line
			}
			_, _ = fmt.Fprint(os.Stdout, strings.Repeat(">", r.winWidth*3/4)+"\r\n")
			used++
		}
		_, _ = fmt.Fprint(os.Stdout, r.index[i].text+"\r\n")
		used++
		shown++
	}
	// Paging advances by index entries, which is fewer than the rows drawn when the
	// break mark took one of them.
	r.lastPageLines = shown
	for i := used; i < pageLines; i++ {
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
	// Deferred in this order so the cursor is restored while the alternate screen is
	// still current; otherwise the main screen would keep a hidden cursor.
	r.hideCursor()
	defer r.showCursor()
	// Started before the setup work below, which can take long enough on a large
	// file that a signal arriving there would skip the cleanup above.
	go r.daemonCatchSignal()
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
	r.buildVisualIndex()
	// savedLogical stays -1 when the file has no stored progress. A saved line may
	// also exceed the file if it was edited since the last run.
	if r.savedLogical >= len(r.lines) {
		r.savedLogical = len(r.lines) - 1
	}
	if r.savedLogical >= 0 {
		r.currentLine = r.visualAt(r.savedLogical)
	}
	r.clampCurrentLine()
	rstore, e := r.enterRawMode()
	if e != nil {
		return e
	}
	defer rstore()
	go r.daemonUpdateWindowSize()
	go r.daemonScrolling()
	go r.daemonCatchInput()
	r.renderPage()
	for {
		switch <-r.eventSignal {
		case CmdResize:
			// Keep the reader anchored on the same logical line across a resize.
			anchor := r.logicalAt(r.currentLine)
			prevWidth := r.winWidth
			if e := r.updateWindowsSize(); e != nil {
				return e
			}
			if r.winWidth != prevWidth {
				r.buildVisualIndex()
				r.currentLine = r.visualAt(anchor)
				r.displayBreakMark = false
			}
			r.clampCurrentLine()
		case CmdScrollTick:
			if r.scrollingLine <= 0 {
				continue // scrolling is off, nothing changed, skip the redraw
			}
			r.currentLine += r.scrollingLine
			r.displayBreakMark = false
		case CmdSwitchScrolling:
			if r.scrollingLine == 2 {
				r.scrollingLine = 0
			} else {
				r.scrollingLine++
			}
		case CmdExit:
			return nil
		case CmdNextPage:
			// Advance by exactly what was drawn, so no line is skipped or repeated.
			// A full page shares no rows with the previous one, so there is no
			// boundary worth marking.
			r.currentLine += r.lastPageLines
			r.displayBreakMark = false
		case CmdPrevPage:
			r.currentLine -= r.pageLines()
			r.displayBreakMark = false
		case CmdNextLine:
			r.currentLine++
			r.displayBreakMark = false
		case CmdPrevLine:
			r.currentLine--
			r.displayBreakMark = false
		case CmdNextHalfPage:
			// Half a page is re-shown, so mark where unread content starts.
			r.setBreakMark(r.currentLine + r.pageLines())
			r.currentLine += r.pageLines() / 2
		}
		r.clampCurrentLine()
		r.renderPage()
	}
}

// pageLines is the number of content rows available above the status line.
func (r *Reader) pageLines() int {
	if r.winHeight < 2 {
		return 1
	}
	return r.winHeight - 1
}

func (r *Reader) clampCurrentLine() {
	if r.currentLine >= r.totalLine {
		r.currentLine = r.totalLine - 1
	}
	if r.currentLine < 0 {
		r.currentLine = 0
	}
}

// setBreakMark records the visual line where content new to this jump begins, so
// the reader can see where to resume. The mark is dropped on the next single-line
// move or resize.
func (r *Reader) setBreakMark(at int) {
	r.jumpBreakMark = at
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
