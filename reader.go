package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
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
)

type Reader struct {
	f             string
	data          string
	progressFile  string         // progress file path
	index         []string       // line number:line content
	progress      map[string]int // hex-md5:line number
	totalLine     int
	currentLine   int
	winHeight     int
	winWidth      int
	scrollingLine int
	scrollingTk   <-chan time.Time
}

func NewReader(f string) Reader {
	return Reader{
		f:           f,
		index:       []string{},
		progress:    make(map[string]int),
		scrollingTk: time.Tick(time.Second),
	}
}

func (r *Reader) catchExit() chan byte {
	inputCh := make(chan byte, 1)
	go func() {
		var b [3]byte
		for {
			_, err := os.Stdin.Read(b[:])
			if err != nil {
				continue
			}
			switch b[0] {
			case 'a':
				inputCh <- CmdSwitchScrolling
			case 0x0d:
				if b[1] == 0x00 && b[2] == 0x00 {
					inputCh <- CmdNextLine
				}
			case 'q':
				inputCh <- CmdExit
			case ' ':
				inputCh <- CmdNextHalfPage
			case 0x1b:
				if b[1] != 0x5b {
					continue
				}
				switch b[2] {
				case 0x41: // up arrow
					inputCh <- CmdPrevLine
				case 0x42: // down arrow
					inputCh <- CmdNextLine
				case 0x43: // right arrow
					inputCh <- CmdNextPage
				case 0x44: // left arrow
					inputCh <- CmdPrevPage
				default:
					continue
				}
			}
		}
	}()
	return inputCh
}

func (r *Reader) saveProgress() {
	//r.mu.Lock()
	//defer r.mu.Unlock()
	r.progress[r.f] = r.currentLine
	pp, _ := json.MarshalIndent(r.progress, "", "  ")
	_ = os.WriteFile(r.progressFile, pp, 0644)
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
	r.progressFile = d
	pp, e := os.ReadFile(d)
	if e != nil {
		return e
	}
	if e := json.Unmarshal(pp, &r.progress); e != nil {
		return e
	}
	pos, ok := r.progress[r.f]
	if ok {
		r.currentLine = pos
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

func (r *Reader) clearScreen() {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("cmd", "/c", "cls")
		cmd.Stdout = os.Stdout
		_ = cmd.Run()
	} else {
		cmd := exec.Command("clear")
		cmd.Stdout = os.Stdout
		_ = cmd.Run()
	}
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

func (r *Reader) daemonUpdateWindowSize() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	go func() {
		for range sigCh {
			r.updateWindowsSize()
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
	f := float64(r.currentLine) / float64(r.totalLine)
	//_, _ = fmt.Fprintf(os.Stdout, "> %s %d*%d %.02f%% %d/%d", path.Base(r.f), r.winWidth, r.winHeight, float64(r.currentLine)/float64(r.totalLine), r.currentLine, r.totalLine)
	_, _ = fmt.Fprintf(os.Stdout, "> %s %d/%d %.02f%% [Q]:Quit [A]:Scroll(%s)", path.Base(r.f), r.currentLine, r.totalLine, f*100, r.scrollInfo())
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
		_, _ = fmt.Fprint(os.Stdout, r.index[i]+"\r\n")
	}
	for i := end - start; i < pageLines; i++ {
		_, _ = fmt.Fprint(os.Stdout, "\r\n")
	}
	r.printInfo()
	r.saveProgress()
}

func (r *Reader) daemonScrolling() {
	for range r.scrollingTk {
		if r.scrollingLine > 0 {
			if r.currentLine < r.totalLine-1 {
				r.currentLine += r.scrollingLine
			}
			r.renderPage()
		}
	}
}

func (r *Reader) Run() error {
	r.enterAltScreen()
	defer r.exitAltScreen()
	r.clearScreen()
	if e := r.createIndex(); e != nil {
		return e
	}
	if e := r.loadProgress(); e != nil {
		return e
	}
	r.updateWindowsSize()
	go r.daemonUpdateWindowSize()
	go r.daemonScrolling()
	rstore, e := r.enterRawMode()
	if e != nil {
		return e
	}
	defer rstore()
	eventQ := r.catchExit()
	if r.currentLine > r.totalLine {
		r.currentLine = 0
	}
	r.renderPage()
	for {
		var c byte
		select {
		case c = <-eventQ:
		}
		switch c {
		case CmdSwitchScrolling:
			if r.scrollingLine == 2 {
				r.scrollingLine = 0
			} else {
				r.scrollingLine++
			}
		case CmdExit:
			return nil
		case CmdNextPage:
			off := r.winHeight - 1
			if r.currentLine+off < r.totalLine {
				r.currentLine += off
			}
		case CmdPrevPage:
			off := r.winHeight - 1
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
			off := r.winHeight / 2
			if r.currentLine+r.winHeight-1 < r.totalLine {
				r.currentLine += off
			}
		}
		r.renderPage()
	}
}
