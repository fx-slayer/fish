### README

English | [简体中文](README.ZH.md)

`fish` is a command-line reader designed for reading books, may support the following features:

> for slacking off, terminal env makes you look more like you're working.

- Remember reading line number.✅

- Display reading progress.✅

- Shortcut for next/prev page.✅

  - `space` for next half page.
  - `↓`,`enter` for next line.
  - `↑` for previous line.
  - `→` for next page.
  - `←` for previous page.
  - `q`,`ctrl+c`,`ctrl+d` to quit.

- Auto page scrolling.✅

  - `a` for switching scrolling mode, cycling through `off` → `1` → `2` lines per second.

- Wide character aware line wrapping.✅

  Long lines are pre-folded to the terminal width, counting CJK and other
  double-width characters as two cells. One rendered row is always one terminal
  row, so paging is exact and a page never overflows the screen.

#### Usage

```bash
fish <FILE>
```

Reading progress is stored per file in `~/.cmdline-reader-progress` and restored on
the next run. It is recorded as a line number in the file, so resizing the terminal
does not shift your position.

#### Install

```bash
git clone https://github.com/fx-slayer/fish.git
cd fish
make install
```

Builds for `darwin/arm64` into `~/go/bin/fish`. Adjust `GOOS`/`GOARCH` in the
`Makefile` for other platforms.
