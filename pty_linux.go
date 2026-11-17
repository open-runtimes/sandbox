package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// openPTY allocates a pseudo-terminal pair through /dev/ptmx and returns the
// controlling (master) side and the terminal (slave) side.
func openPTY() (*os.File, *os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}

	var unlock int32
	if err := ioctl(master, syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("unlock pty: %w", err)
	}

	var n uint32
	if err := ioctl(master, syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("pty number: %w", err)
	}

	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("open pty: %w", err)
	}

	return master, slave, nil
}

// resizePTY sets the terminal size, which the kernel signals to the
// foreground process group as SIGWINCH.
func resizePTY(f *os.File, cols, rows int) error {
	size := struct{ rows, cols, x, y uint16 }{rows: uint16(rows), cols: uint16(cols)}
	return ioctl(f, syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&size)))
}

func ioctl(f *os.File, request, arg uintptr) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := conn.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, request, arg)
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}
