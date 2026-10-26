//
// Copyright 2014-2026 Cristian Maglie. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//

//go:build linux || darwin || freebsd || openbsd

package serial

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.bug.st/serial/unixutils"
	"golang.org/x/sys/unix"
)

type unixPort struct {
	handle int
	// modemLock makes SetDTR and SetRTS (a read, then a write, of the modem bits) atomic with
	// respect to each other.
	modemLock sync.Mutex

	readTimeout time.Duration
	closeLock   sync.RWMutex
	closeSignal *unixutils.Pipe
	opened      uint32
}

func (port *unixPort) Close() error {
	if !atomic.CompareAndSwapUint32(&port.opened, 1, 0) {
		return nil
	}

	if port.closeSignal != nil {
		// Wake every pending Read, Write and Break (if any)...
		port.closeSignal.Write([]byte{0})
	}

	// ...and wait until each of them has returned, so that none can touch the descriptor after
	// it is closed, when the number may already belong to another port.
	port.closeLock.Lock()
	defer port.closeLock.Unlock()

	port.releaseExclusiveAccess()
	err := unix.Close(port.handle)
	if port.closeSignal != nil {
		if cerr := port.closeSignal.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

// use marks the descriptor as in use until the returned function is called, so that Close waits
// for the caller. It fails with PortClosed once the port has been closed.
func (port *unixPort) use() (func(), error) {
	port.closeLock.RLock()
	if atomic.LoadUint32(&port.opened) != 1 {
		port.closeLock.RUnlock()
		return nil, &PortError{code: PortClosed}
	}
	return port.closeLock.RUnlock, nil
}

func (port *unixPort) Read(p []byte) (int, error) {
	port.closeLock.RLock()
	defer port.closeLock.RUnlock()
	if atomic.LoadUint32(&port.opened) != 1 {
		return 0, &PortError{code: PortClosed}
	}

	var deadline time.Time
	if port.readTimeout != NoTimeout {
		deadline = time.Now().Add(port.readTimeout)
	}

	fds := unixutils.NewFDSet(port.handle, port.closeSignal.ReadFD())
	for {
		timeout := time.Duration(-1)
		if port.readTimeout != NoTimeout {
			timeout = time.Until(deadline)
			if timeout < 0 {
				// a negative timeout means "no-timeout" in Select(...)
				timeout = 0
			}
		}
		res, err := unixutils.Select(fds, nil, fds, timeout)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if res.IsReadable(port.closeSignal.ReadFD()) {
			return 0, &PortError{code: PortClosed}
		}
		if !res.IsReadable(port.handle) {
			// Timeout happened
			return 0, nil
		}
		n, err := unix.Read(port.handle, p)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		// Linux: when the port is disconnected during a read operation
		// the port is left in a "readable with zero-length-data" state.
		// https://stackoverflow.com/a/34945814/1655275
		if n == 0 && err == nil {
			return 0, &PortError{code: PortClosed}
		}
		if n < 0 { // Do not return -1 unix errors
			n = 0
		}
		return n, err
	}
}

// Write writes all of p. It waits for room in the output queue (the other end may have stopped
// reading, or flow control may hold the line) and gives up with PortClosed when the port is
// closed meanwhile; n then says how much was written.
func (port *unixPort) Write(p []byte) (n int, err error) {
	release, err := port.use()
	if err != nil {
		return 0, err
	}
	defer release()
	for n < len(p) {
		m, err := unix.Write(port.handle, p[n:])
		if m > 0 {
			n += m
		}
		switch err {
		case nil, unix.EINTR:
			continue
		case unix.EAGAIN:
			// The queue is full: wait until the descriptor can take more, or the port is closed.
			res, err := unixutils.Select(unixutils.NewFDSet(port.closeSignal.ReadFD()), unixutils.NewFDSet(port.handle), nil, -1)
			if err == unix.EINTR {
				continue
			}
			if err != nil {
				return n, err
			}
			if res.IsReadable(port.closeSignal.ReadFD()) {
				return n, &PortError{code: PortClosed}
			}
		default:
			return n, err
		}
	}
	return n, nil
}

// Break holds the break condition for t. A Close during the break ends it at once; the break is
// always cleared before Break returns.
func (port *unixPort) Break(t time.Duration) error {
	release, err := port.use()
	if err != nil {
		return err
	}
	defer release()
	if err := unix.IoctlSetInt(port.handle, ioctlTiocsbrk, 0); err != nil {
		return err
	}

	res, selErr := unixutils.Select(unixutils.NewFDSet(port.closeSignal.ReadFD()), nil, nil, t)

	if err := unix.IoctlSetInt(port.handle, ioctlTioccbrk, 0); err != nil {
		return err
	}
	if selErr == nil && res.IsReadable(port.closeSignal.ReadFD()) {
		return &PortError{code: PortClosed}
	}
	return nil
}

func (port *unixPort) SetMode(mode *Mode) error {
	release, err := port.use()
	if err != nil {
		return err
	}
	defer release()
	settings, err := port.getTermSettings()
	if err != nil {
		return err
	}
	if err := setTermSettingsParity(mode.Parity, settings); err != nil {
		return err
	}
	if err := setTermSettingsDataBits(mode.DataBits, settings); err != nil {
		return err
	}
	if err := setTermSettingsStopBits(mode.StopBits, settings); err != nil {
		return err
	}
	requireSpecialBaudrate := false
	if err, special := setTermSettingsBaudrate(mode.BaudRate, settings); err != nil {
		return err
	} else if special {
		requireSpecialBaudrate = true
	}
	setTermSettingsFlowControl(mode.FlowControl, settings)
	if err := port.setTermSettings(settings); err != nil {
		return err
	}
	if requireSpecialBaudrate {
		// MacOSX require this one to be the last operation otherwise an
		// 'Invalid serial port' error is produced.
		if err := port.setSpecialBaudrate(uint32(mode.BaudRate)); err != nil {
			return err
		}
	}
	return nil
}

func (port *unixPort) SetDTR(dtr bool) error {
	release, err := port.use()
	if err != nil {
		return err
	}
	defer release()
	port.modemLock.Lock()
	defer port.modemLock.Unlock()
	status, err := port.getModemBitsStatus()
	if err != nil {
		return err
	}
	if dtr {
		status |= unix.TIOCM_DTR
	} else {
		status &^= unix.TIOCM_DTR
	}
	return port.setModemBitsStatus(status)
}

func (port *unixPort) SetRTS(rts bool) error {
	release, err := port.use()
	if err != nil {
		return err
	}
	defer release()
	port.modemLock.Lock()
	defer port.modemLock.Unlock()
	status, err := port.getModemBitsStatus()
	if err != nil {
		return err
	}
	if rts {
		status |= unix.TIOCM_RTS
	} else {
		status &^= unix.TIOCM_RTS
	}
	return port.setModemBitsStatus(status)
}

func (port *unixPort) SetReadTimeout(timeout time.Duration) error {
	if timeout < 0 && timeout != NoTimeout {
		return &PortError{code: InvalidTimeoutValue}
	}
	port.readTimeout = timeout
	return nil
}

func (port *unixPort) GetModemStatusBits() (*ModemStatusBits, error) {
	release, err := port.use()
	if err != nil {
		return nil, err
	}
	defer release()
	status, err := port.getModemBitsStatus()
	if err != nil {
		return nil, err
	}
	return &ModemStatusBits{
		CTS: (status & unix.TIOCM_CTS) != 0,
		DCD: (status & unix.TIOCM_CD) != 0,
		DSR: (status & unix.TIOCM_DSR) != 0,
		RI:  (status & unix.TIOCM_RI) != 0,
	}, nil
}

func nativeOpen(portName string, mode *Mode) (*unixPort, error) {
	h, err := unix.Open(portName, unix.O_RDWR|unix.O_NOCTTY|unix.O_NDELAY, 0)
	if err != nil {
		switch err {
		case unix.EBUSY:
			return nil, &PortError{code: PortBusy}
		case unix.EACCES:
			return nil, &PortError{code: PermissionDenied}
		}
		return nil, err
	}
	port := &unixPort{
		handle:      h,
		opened:      1,
		readTimeout: NoTimeout,
	}

	// Setup serial port
	settings, err := port.getTermSettings()
	if err != nil {
		port.Close()
		return nil, &PortError{code: InvalidSerialPort, causedBy: fmt.Errorf("error getting term settings: %w", err)}
	}

	// Set raw mode
	setRawMode(settings)

	// Explicitly disable RTS/CTS flow control
	setTermSettingsFlowControl(mode.FlowControl, settings)

	if err = port.setTermSettings(settings); err != nil {
		port.Close()
		return nil, &PortError{code: InvalidSerialPort, causedBy: fmt.Errorf("error setting term settings: %w", err)}
	}

	if mode.InitialStatusBits != nil {
		status, err := port.getModemBitsStatus()
		if err != nil {
			port.Close()
			return nil, &PortError{code: InvalidSerialPort, causedBy: fmt.Errorf("error getting modem bits status: %w", err)}
		}
		if mode.InitialStatusBits.DTR {
			status |= unix.TIOCM_DTR
		} else {
			status &^= unix.TIOCM_DTR
		}
		if mode.InitialStatusBits.RTS {
			status |= unix.TIOCM_RTS
		} else {
			status &^= unix.TIOCM_RTS
		}
		if err := port.setModemBitsStatus(status); err != nil {
			port.Close()
			return nil, &PortError{code: InvalidSerialPort, causedBy: fmt.Errorf("error setting modem bits status: %w", err)}
		}
	}

	// MacOSX require that this operation is the last one otherwise an
	// 'Invalid serial port' error is returned... don't know why...
	if err := port.SetMode(mode); err != nil {
		port.Close()
		return nil, &PortError{code: InvalidSerialPort, causedBy: fmt.Errorf("error configuring port: %w", err)}
	}

	// Non-blocking, so that a Write can wait for room in a way Close can interrupt.
	unix.SetNonblock(h, true)

	port.acquireExclusiveAccess()

	// This pipe is used as a signal to cancel blocking Read
	if pipe, err := unixutils.NewPipe(); err != nil {
		port.Close()
		return nil, &PortError{code: InvalidSerialPort, causedBy: fmt.Errorf("error opening signaling pipe: %w", err)}
	} else {
		port.closeSignal = pipe
	}

	return port, nil
}

func nativeGetPortsList() ([]string, error) {
	files, err := os.ReadDir(devFolder)
	if err != nil {
		return nil, err
	}

	ports := make([]string, 0, len(files))
	for _, f := range files {
		// Skip folders
		if f.IsDir() {
			continue
		}

		// Keep only devices with the correct name
		if !osPortFilter.MatchString(f.Name()) {
			continue
		}

		portName := devFolder + "/" + f.Name()

		// Check if serial port is real or is a placeholder serial port "ttySxx" or "ttyHSxx"
		if strings.HasPrefix(f.Name(), "ttyS") || strings.HasPrefix(f.Name(), "ttyHS") {
			port, err := nativeOpen(portName, &Mode{})
			if err != nil {
				continue
			} else {
				port.Close()
			}
		}

		// Save serial port in the resulting list
		ports = append(ports, portName)
	}

	return ports, nil
}

// termios manipulation functions

func setTermSettingsParity(parity Parity, settings *unix.Termios) error {
	switch parity {
	case NoParity:
		settings.Cflag &^= unix.PARENB
		settings.Cflag &^= unix.PARODD
		settings.Cflag &^= tcCMSPAR
		settings.Iflag &^= unix.INPCK
	case OddParity:
		settings.Cflag |= unix.PARENB
		settings.Cflag |= unix.PARODD
		settings.Cflag &^= tcCMSPAR
		settings.Iflag |= unix.INPCK
	case EvenParity:
		settings.Cflag |= unix.PARENB
		settings.Cflag &^= unix.PARODD
		settings.Cflag &^= tcCMSPAR
		settings.Iflag |= unix.INPCK
	case MarkParity:
		if tcCMSPAR == 0 {
			return &PortError{code: InvalidParity}
		}
		settings.Cflag |= unix.PARENB
		settings.Cflag |= unix.PARODD
		settings.Cflag |= tcCMSPAR
		settings.Iflag |= unix.INPCK
	case SpaceParity:
		if tcCMSPAR == 0 {
			return &PortError{code: InvalidParity}
		}
		settings.Cflag |= unix.PARENB
		settings.Cflag &^= unix.PARODD
		settings.Cflag |= tcCMSPAR
		settings.Iflag |= unix.INPCK
	default:
		return &PortError{code: InvalidParity}
	}
	return nil
}

func setTermSettingsDataBits(bits int, settings *unix.Termios) error {
	databits, ok := databitsMap[bits]
	if !ok {
		return &PortError{code: InvalidDataBits}
	}
	// Remove previous databits setting
	settings.Cflag &^= unix.CSIZE
	// Set requested databits
	settings.Cflag |= databits
	return nil
}

func setTermSettingsStopBits(bits StopBits, settings *unix.Termios) error {
	switch bits {
	case OneStopBit:
		settings.Cflag &^= unix.CSTOPB
	case OnePointFiveStopBits:
		return &PortError{code: InvalidStopBits}
	case TwoStopBits:
		settings.Cflag |= unix.CSTOPB
	default:
		return &PortError{code: InvalidStopBits}
	}
	return nil
}

func setTermSettingsFlowControl(f FlowControl, settings *unix.Termios) {
	setTermSettingsCtsRts(f == RTSCTSFlowControl, settings)
	if f == XONXOFFFlowControl {
		settings.Iflag |= unix.IXON | unix.IXOFF
	} else {
		settings.Iflag &^= unix.IXON | unix.IXOFF
	}
}

func setTermSettingsCtsRts(enable bool, settings *unix.Termios) {
	if enable {
		settings.Cflag |= tcCRTSCTS
	} else {
		settings.Cflag &^= tcCRTSCTS
	}
}

func setRawMode(settings *unix.Termios) {
	// Set local mode
	settings.Cflag |= unix.CREAD
	settings.Cflag |= unix.CLOCAL

	// Set raw mode
	settings.Lflag &^= unix.ICANON
	settings.Lflag &^= unix.ECHO
	settings.Lflag &^= unix.ECHOE
	settings.Lflag &^= unix.ECHOK
	settings.Lflag &^= unix.ECHONL
	settings.Lflag &^= unix.ECHOCTL
	settings.Lflag &^= unix.ECHOPRT
	settings.Lflag &^= unix.ECHOKE
	settings.Lflag &^= unix.ISIG
	settings.Lflag &^= unix.IEXTEN

	settings.Iflag &^= unix.IXON
	settings.Iflag &^= unix.IXOFF
	settings.Iflag &^= unix.IXANY
	settings.Iflag &^= unix.INPCK
	settings.Iflag &^= unix.IGNPAR
	settings.Iflag &^= unix.PARMRK
	settings.Iflag &^= unix.ISTRIP
	settings.Iflag &^= unix.IGNBRK
	settings.Iflag &^= unix.BRKINT
	settings.Iflag &^= unix.INLCR
	settings.Iflag &^= unix.IGNCR
	settings.Iflag &^= unix.ICRNL
	settings.Iflag &^= tcIUCLC

	settings.Oflag &^= unix.OPOST

	// Block reads until at least one char is available (no timeout)
	settings.Cc[unix.VMIN] = 1
	settings.Cc[unix.VTIME] = 0
}

// native syscall wrapper functions

func (port *unixPort) getTermSettings() (*unix.Termios, error) {
	return unix.IoctlGetTermios(port.handle, ioctlTcgetattr)
}

func (port *unixPort) setTermSettings(settings *unix.Termios) error {
	return unix.IoctlSetTermios(port.handle, ioctlTcsetattr, settings)
}

func (port *unixPort) getModemBitsStatus() (int, error) {
	return unix.IoctlGetInt(port.handle, unix.TIOCMGET)
}

func (port *unixPort) setModemBitsStatus(status int) error {
	return unix.IoctlSetPointerInt(port.handle, unix.TIOCMSET, status)
}

func (port *unixPort) acquireExclusiveAccess() error {
	return unix.IoctlSetInt(port.handle, unix.TIOCEXCL, 0)
}

func (port *unixPort) releaseExclusiveAccess() error {
	return unix.IoctlSetInt(port.handle, unix.TIOCNXCL, 0)
}
