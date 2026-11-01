//go:build linux

package main

// term.go — the minimum of terminal control needed for a live view,
// using raw ioctls so the binary stays dependency-free.

import (
	"bufio"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

func isTerminal(f *os.File) bool {
	var t syscall.Termios
	return ioctl(int(f.Fd()), syscall.TCGETS, unsafe.Pointer(&t)) == nil
}

func termSize(f *os.File) (cols, rows int) {
	var ws struct{ Row, Col, X, Y uint16 }
	if ioctl(int(f.Fd()), syscall.TIOCGWINSZ, unsafe.Pointer(&ws)) != nil || ws.Col == 0 {
		return 0, 0
	}
	return int(ws.Col), int(ws.Row)
}

// rawInput turns off line buffering and echo so single keys arrive
// immediately. Signals (Ctrl-C) still work. Returns a restore func.
func rawInput(f *os.File) (func(), error) {
	fd := int(f.Fd())
	var old syscall.Termios
	if err := ioctl(fd, syscall.TCGETS, unsafe.Pointer(&old)); err != nil {
		return func() {}, err
	}
	t := old
	t.Lflag &^= syscall.ICANON | syscall.ECHO
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	if err := ioctl(fd, syscall.TCSETS, unsafe.Pointer(&t)); err != nil {
		return func() {}, err
	}
	return func() { _ = ioctl(fd, syscall.TCSETS, unsafe.Pointer(&old)) }, nil
}

// drawFrame repaints in place: home the cursor, overwrite each line, clear
// the remainder. No full-screen clear per tick, so there is no flicker.
func drawFrame(out *bufio.Writer, lines []string, cols, rows, width int) {
	margin := 0
	if cols > width {
		margin = (cols - width) / 2
	}
	pad := strings.Repeat(" ", margin)
	topPad := 0
	if rows > len(lines)+1 {
		topPad = 1
	}
	n := len(lines)
	if rows > 0 && n > rows-topPad {
		n = rows - topPad
	}
	out.WriteString("\x1b[H")
	if topPad == 1 {
		out.WriteString("\x1b[K\r\n")
	}
	for i := 0; i < n; i++ {
		out.WriteString(pad)
		out.WriteString(lines[i])
		out.WriteString("\x1b[K")
		if i < n-1 {
			out.WriteString("\r\n")
		}
	}
	out.WriteString("\x1b[J")
	out.Flush()
}
