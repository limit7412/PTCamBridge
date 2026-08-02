package source

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"

	"github.com/limit7412/PTCamBridge/internal/core"
)

// AutoPort asks the driver to pick a port by USB vendor ID instead of naming
// one explicitly.
const AutoPort = "auto"

// DefaultSerialBaud is the rate Babble wired firmware runs at.
const DefaultSerialBaud = 3000000

// serialReadTimeout bounds a blocking read so the loop can notice that the
// context was cancelled. It is not a stall detector.
const serialReadTimeout = 200 * time.Millisecond

// serialStallTimeout is how long a port may stay silent before the driver
// treats the board as gone and reconnects.
const serialStallTimeout = 5 * time.Second

// knownCameraVIDs are the USB vendor IDs of the bridges and MCUs that Babble
// and OpenIris boards ship with: Espressif, Silicon Labs, QinHeng, FTDI and
// Raspberry Pi.
var knownCameraVIDs = map[string]string{
	"303A": "Espressif",
	"10C4": "Silicon Labs CP210x",
	"1A86": "QinHeng CH340",
	"0403": "FTDI",
	"2E8A": "Raspberry Pi",
}

// SerialConfig configures the wired Babble board driver.
type SerialConfig struct {
	// Port is a port name such as "COM5", or AutoPort to search by vendor ID.
	Port string
	// Baud defaults to DefaultSerialBaud when zero.
	Baud int
	// Header overrides the packet preamble for firmware that differs from the
	// documented 0xFF 0xA0 0xFF 0xA1.
	Header []byte
	// MaxFrameSize bounds a single JPEG; zero selects the core default.
	MaxFrameSize int
}

// Serial reads the OpenIris/ETVR wired packet stream from a serial port.
type Serial struct {
	cfg      SerialConfig
	parser   core.ETVRParser
	log      *slog.Logger
	reporter Reporter
}

// NewSerial builds the driver and validates the packet header up front, since
// a bad header would otherwise fail identically on every reconnect.
func NewSerial(cfg SerialConfig, log *slog.Logger, reporter Reporter) (*Serial, error) {
	if cfg.Baud <= 0 {
		cfg.Baud = DefaultSerialBaud
	}
	if strings.TrimSpace(cfg.Port) == "" {
		cfg.Port = AutoPort
	}
	parser, err := core.NewETVRParser(cfg.Header, cfg.MaxFrameSize)
	if err != nil {
		return nil, fmt.Errorf("serial: %w", err)
	}
	if reporter == nil {
		reporter = NopReporter{}
	}
	return &Serial{cfg: cfg, parser: parser, log: log, reporter: reporter}, nil
}

// Name implements Source.
func (s *Serial) Name() string { return "serial" }

// Run implements Source.
func (s *Serial) Run(ctx context.Context, out chan<- core.Frame) error {
	return runWithBackoff(ctx, s.log, s.Name(), s.reporter, func(ctx context.Context) error {
		return s.session(ctx, out)
	})
}

// session opens the port and reads until it fails or the context ends.
func (s *Serial) session(ctx context.Context, out chan<- core.Frame) error {
	name, err := s.resolvePort()
	if err != nil {
		return err
	}
	port, err := serial.Open(name, &serial.Mode{BaudRate: s.cfg.Baud})
	if err != nil {
		return fmt.Errorf("serial: open %s at %d baud: %w", name, s.cfg.Baud, err)
	}
	defer port.Close()

	if err := port.SetReadTimeout(serialReadTimeout); err != nil {
		return fmt.Errorf("serial: set read timeout: %w", err)
	}
	s.log.Info("serial port opened", "port", name, "baud", s.cfg.Baud)

	assembler := newFrameAssembler(s.splitPackets, s.cfg.MaxFrameSize)
	buf := make([]byte, readChunk)
	lastData := time.Now()
	var count uint64

	for {
		if ctx.Err() != nil {
			return nil
		}
		n, err := port.Read(buf)
		if err != nil {
			return fmt.Errorf("serial: read from %s: %w", name, err)
		}
		if n == 0 {
			// A read timeout, not an error. Only a long silence means the
			// board is gone.
			if time.Since(lastData) > serialStallTimeout {
				return fmt.Errorf("serial: %s produced no data for %s", name, serialStallTimeout)
			}
			continue
		}
		lastData = time.Now()

		for _, f := range assembler.feed(buf[:n]) {
			if count == 0 {
				s.reporter.Connected(s.Name())
			}
			count++
			if err := send(ctx, out, f); err != nil {
				return nil
			}
		}
	}
}

// splitPackets adapts the parser to the frameAssembler signature. The parser
// carries its own payload bound, so maxSize is already applied there.
func (s *Serial) splitPackets(buf []byte, _ int) ([][]byte, []byte) {
	return s.parser.Parse(buf)
}

// resolvePort returns the configured port, or searches for one when set to
// AutoPort.
func (s *Serial) resolvePort() (string, error) {
	if !strings.EqualFold(s.cfg.Port, AutoPort) {
		return s.cfg.Port, nil
	}
	ports, err := ListSerialPorts()
	if err != nil {
		return "", fmt.Errorf("serial: enumerate ports: %w", err)
	}
	for _, p := range ports {
		if p.Vendor != "" {
			s.log.Info("auto-selected serial port", "port", p.Name, "vendor", p.Vendor)
			return p.Name, nil
		}
	}
	if len(ports) == 1 {
		s.log.Info("auto-selected the only serial port available", "port", ports[0].Name)
		return ports[0].Name, nil
	}
	return "", errors.New("serial: no port matched a known camera vendor ID; set source.serial.port explicitly")
}

// SerialPort describes a port offered in the tray menu and over the management
// API.
type SerialPort struct {
	Name string `json:"name"`
	// Vendor is set when the USB vendor ID belongs to a board family Babble
	// firmware is known to ship on.
	Vendor  string `json:"vendor,omitempty"`
	VID     string `json:"vid,omitempty"`
	PID     string `json:"pid,omitempty"`
	Product string `json:"product,omitempty"`
}

// ListSerialPorts enumerates serial ports, with the recognised camera boards
// listed first so a caller can take the head of the list.
func ListSerialPorts() ([]SerialPort, error) {
	details, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil, err
	}
	var known, others []SerialPort
	for _, d := range details {
		p := SerialPort{Name: d.Name, Product: d.Product}
		if d.IsUSB {
			p.VID, p.PID = strings.ToUpper(d.VID), strings.ToUpper(d.PID)
			if vendor, ok := knownCameraVIDs[p.VID]; ok {
				p.Vendor = vendor
			}
		}
		if p.Vendor != "" {
			known = append(known, p)
		} else {
			others = append(others, p)
		}
	}
	return append(known, others...), nil
}
