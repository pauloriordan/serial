// +build darwin linux freebsd openbsd netbsd

package serial

import (
	"errors"
	"fmt"
	"log"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// port implements Port interface.
type port struct {
	fd         int
	oldTermios *syscall.Termios

	timeout time.Duration
}

const (
	rs485Enabled      = 1 << 0
	rs485RTSOnSend    = 1 << 1
	rs485RTSAfterSend = 1 << 2
	rs485RXDuringTX   = 1 << 4
	rs485Tiocs        = 0x542f
)

// rs485_ioctl_opts is used to configure RS485 options in the driver
type rs485_ioctl_opts struct {
	flags                 uint32
	delay_rts_before_send uint32
	delay_rts_after_send  uint32
	padding               [5]uint32
}

// New allocates and returns a new serial port controller.
func New() Port {
	return &port{fd: -1}
}

// Open connects to the given serial port.
func (p *port) Open(c *Config) (err error) {
	// See man termios(3).
	// O_NOCTTY: no controlling terminal.
	// O_NDELAY: no data carrier detect.
	p.fd, err = syscall.Open(c.Address, syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_NDELAY|syscall.O_CLOEXEC, 0666)
	if err != nil {
		return
	}
	// Backup current termios to restore on closing.
	p.backupTermios()

	// Create a new Termios using the backup as defaults to avoid clobbering
	// the OS defaults
	termios, err := newTermios(c, p.oldTermios)
	if err != nil {
		return
	}

	if err = p.setTermios(termios); err != nil {
		// No need to restore termios
		syscall.Close(p.fd)
		p.fd = -1
		p.oldTermios = nil
		return err
	}
	if err = enableRS485(p.fd, &c.RS485); err != nil {
		p.Close()
		return err
	}

	//p.setDtr(c.Dsrdtr)
	//p.setRts(c.Rtscts)
	p.setRtsDtr(c.Rtscts, c.Dsrdtr)
	p.timeout = c.Timeout
	return
}

func (p *port) Close() (err error) {
	if p.fd == -1 {
		return
	}
	p.restoreTermios()
	err = syscall.Close(p.fd)
	p.fd = -1
	p.oldTermios = nil
	return
}

// Read reads from serial port. Port must be opened before calling this method.
// It is blocked until all data received or timeout after p.timeout.
func (p *port) Read(b []byte) (n int, err error) {
	var rfds syscall.FdSet

	fd := p.fd
	fdset(fd, &rfds)

	var tv *syscall.Timeval
	if p.timeout > 0 {
		timeout := syscall.NsecToTimeval(p.timeout.Nanoseconds())
		tv = &timeout
	}
	for {
		// If syscall.Select() returns EINTR (Interrupted system call), retry it
		if err = syscallSelect(fd+1, &rfds, nil, nil, tv); err == nil {
			break
		}
		if err != syscall.EINTR {
			err = fmt.Errorf("serial: could not select: %v", err)
			return
		}
	}
	if !fdisset(fd, &rfds) {
		// Timeout
		err = ErrTimeout
		return
	}
	n, err = syscall.Read(fd, b)

	if n < 1 {
		// Syscall reported ready, but read returned no data. That's an error
		err = syscall.EBADF
	}

	return
}

// Write writes data to the serial port.
func (p *port) Write(b []byte) (n int, err error) {
	n, err = syscall.Write(p.fd, b)
	return
}

func (p *port) setTermios(termios *syscall.Termios) (err error) {
	if err = tcsetattr(p.fd, termios); err != nil {
		err = fmt.Errorf("serial: could not set setting: %v", err)
	}
	return
}

func (p *port) setRtsDtr(rts bool, dtr bool) {
	var status int

	syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(p.fd),
		uintptr(syscall.TIOCMGET),
		uintptr(unsafe.Pointer(&status)))

	if rts {
		status |= syscall.TIOCM_RTS
	} else {
		status &^= syscall.TIOCM_RTS
	}

	if dtr {
		status |= syscall.TIOCM_DTR
	} else {
		status &^= syscall.TIOCM_DTR
	}

	syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(p.fd),
		uintptr(syscall.TIOCMSET),
		uintptr(unsafe.Pointer(&status)))
}

func (p *port) setDtr(dtr bool) {
	var status int

	syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(p.fd),
		uintptr(syscall.TIOCMGET),
		uintptr(unsafe.Pointer(&status)))

	if dtr {
		status |= syscall.TIOCM_DTR
	} else {
		status &^= syscall.TIOCM_DTR
	}

	syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(p.fd),
		uintptr(syscall.TIOCMSET),
		uintptr(unsafe.Pointer(&status)))
}

func (p *port) setRts(rts bool) {
	var status int

	// Get the modem bits status
	syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(p.fd),
		uintptr(syscall.TIOCMGET),
		uintptr(unsafe.Pointer(&status)))

	if rts {
		status |= syscall.TIOCM_RTS
	} else {
		status &^= syscall.TIOCM_RTS
	}

	// Update according to the conf.Rtscts setting
	syscall.Syscall(syscall.SYS_IOCTL,
		uintptr(p.fd),
		uintptr(syscall.TIOCMSET),
		uintptr(unsafe.Pointer(&status)))
}

// backupTermios saves current termios setting.
// Make sure that device file has been opened before calling this function.
func (p *port) backupTermios() {
	oldTermios := &syscall.Termios{}
	if err := tcgetattr(p.fd, oldTermios); err != nil {
		// Warning only.
		log.Printf("serial: could not get setting: %v\n", err)
		return
	}
	// Will be reloaded when closing.
	p.oldTermios = oldTermios
}

// restoreTermios restores backed up termios setting.
// Make sure that device file has been opened before calling this function.
func (p *port) restoreTermios() {
	if p.oldTermios == nil {
		return
	}
	if err := tcsetattr(p.fd, p.oldTermios); err != nil {
		// Warning only.
		log.Printf("serial: could not restore setting: %v\n", err)
		return
	}
	p.oldTermios = nil
}

// Helpers for termios

func newTermios(c *Config, originalTermios *syscall.Termios) (termios *syscall.Termios, err error) {
	termios = &syscall.Termios{}
	*termios = *originalTermios

	flag := termios.Cflag
	// Baud rate
	if c.BaudRate == 0 {
		// 19200 is the required default.
		flag = syscall.B19200
	} else {
		var ok bool
		flag, ok = baudRates[c.BaudRate]
		if !ok {
			err = fmt.Errorf("serial: unsupported baud rate %v", c.BaudRate)
			return
		}
	}

	termios.Iflag &^= 0x80000000
	// toggle the baud rate bits off and apply the configured baud rate (flag)
	termios.Cflag &^= (tcCbaud | tcCbaudEx)
	termios.Cflag |= flag

	// Input baud.
	cfSetIspeed(termios, flag)
	// Output baud.
	cfSetOspeed(termios, flag)
	// Character size.
	if c.DataBits == 0 {
		flag = syscall.CS8
	} else {
		var ok bool
		flag, ok = charSizes[c.DataBits]
		if !ok {
			err = fmt.Errorf("serial: unsupported character size %v", c.DataBits)
			return
		}
	}

	termios.Cflag |= flag

	// Stop bits
	switch c.StopBits {
	case 0, 1:
		// Default is one stop bit.
		termios.Cflag &^= syscall.CSTOPB
	case 2:
		// CSTOPB: Set two stop bits.
		termios.Cflag |= syscall.CSTOPB
	default:
		err = fmt.Errorf("serial: unsupported stop bits %v", c.StopBits)
		return
	}

	termios.Iflag &^= (syscall.INPCK | syscall.ISTRIP)

	switch c.Parity {
	case "N":
		// No parity
		termios.Cflag &^= (syscall.PARODD | syscall.PARENB | tcCmsPar)
	case "", "E":
		// As mentioned in the modbus spec, the default parity mode must be Even parity
		// PARENB: Enable parity generation on output.
		termios.Cflag &^= (syscall.PARODD | tcCmsPar)
		termios.Cflag |= syscall.PARENB
		// INPCK: Enable input parity checking. XXX
		termios.Iflag |= syscall.INPCK
	case "O":
		// PARODD: Parity is odd.
		termios.Cflag &^= tcCmsPar
		termios.Cflag |= (syscall.PARENB | syscall.PARODD)
	default:
		err = fmt.Errorf("serial: unsupported parity %v", c.Parity)
		return
	}

	// Set raw mode for the terminal (from pyserial)
	termios.Cflag |= syscall.CREAD | syscall.CLOCAL

	termios.Lflag &^= (syscall.ICANON | syscall.ECHO | syscall.ECHOE |
		syscall.ECHOK | syscall.ECHONL | syscall.ISIG | syscall.IEXTEN |
		syscall.ECHOCTL | syscall.ECHOKE)

	termios.Oflag &^= (syscall.OPOST | syscall.ONLCR | syscall.OCRNL)

	termios.Iflag &^= (syscall.INLCR | syscall.IGNCR | syscall.ICRNL |
		syscall.IGNBRK | syscall.PARMRK | syscall.IXON | syscall.IXOFF |
		syscall.IXANY)

	// Set both MIN and TIME to zero. Read always returns immediately with as many
	// characters as are available in the queue
	termios.Cc[syscall.VMIN] = 0
	termios.Cc[syscall.VTIME] = 0

	// Enable / disable rtscts flow control
	if c.Rtscts {
		termios.Cflag |= tcCrtsCts
	} else {
		termios.Cflag &^= tcCrtsCts
	}

	return
}

// enableRS485 enables RS485 functionality of driver via an ioctl if the config says so
func enableRS485(fd int, config *RS485Config) error {
	if !config.Enabled {
		return nil
	}
	rs485 := rs485_ioctl_opts{
		rs485Enabled,
		uint32(config.DelayRtsBeforeSend / time.Millisecond),
		uint32(config.DelayRtsAfterSend / time.Millisecond),
		[5]uint32{0, 0, 0, 0, 0},
	}

	if config.RtsHighDuringSend {
		rs485.flags |= rs485RTSOnSend
	}
	if config.RtsHighAfterSend {
		rs485.flags |= rs485RTSAfterSend
	}
	if config.RxDuringTx {
		rs485.flags |= rs485RXDuringTX
	}

	r, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		uintptr(rs485Tiocs),
		uintptr(unsafe.Pointer(&rs485)))
	if errno != 0 {
		return os.NewSyscallError("SYS_IOCTL (RS485)", errno)
	}
	if r != 0 {
		return errors.New("serial: unknown error from SYS_IOCTL (RS485)")
	}
	return nil
}

func (p *port) FlushInputBuffer() (err error) {
	return tcflush(p.fd, syscall.TCIFLUSH)
}

func (p *port) FlushOutputBuffer() (err error) {
	return tcflush(p.fd, syscall.TCOFLUSH)
}
