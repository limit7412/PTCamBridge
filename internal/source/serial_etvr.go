package source

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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

// serialStallTimeout is how long a port may go without producing a frame
// before the driver treats the board as gone and reconnects. A var so tests
// do not have to spend it: what they check is what the stall reports, and
// waiting five seconds for each one says nothing extra.
var serialStallTimeout = 5 * time.Second

// previewBytes is how much of a stream nothing could be parsed out of is put
// in the log: enough to see a preamble and the start of a length field,
// not so much that the line turns into a hex dump.
const previewBytes = 16

// maxWarnedStreams bounds how many different unparsable streams one port is
// warned about before the rest go to debug.
//
// A cap is needed because "the bytes changed" is not always news: a port
// carrying a live stream hands back different first bytes on every open,
// depending on where the reader happened to join it, so keying the warning on
// the stream alone would warn on every reconnect -- the flooding the memory
// exists to prevent. A few tellings is enough to see that the bytes are
// varying, and the debug line still carries every one of them.
const maxWarnedStreams = 3

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

	// tried remembers which ports AutoPort has already handed out and been
	// brought back from, so the search moves on instead of returning the same
	// candidate on every reconnect. proven is the last port that actually
	// produced a frame, which earns it one retry ahead of the rotation.
	//
	// Only Run touches either, and Run is single threaded.
	tried  map[string]struct{}
	proven string

	// warned remembers, per port, which unparsable streams have already been
	// reported, so the same complaint is not made on every reconnect.
	//
	// Per port and per stream, because both change independently: "auto"
	// rotates between candidates, so remembering only the last stream would
	// warn again every time the rotation came back round; and a firmware
	// update or a different device on the same COM number is a new thing to
	// say. An entry is dropped when that port produces a frame, so a port that
	// works and later breaks is news again.
	warned map[string][]string

	// listPorts is ListSerialPorts, replaced in tests: the rotation is the
	// part worth checking and it cannot be reached without control over what
	// enumeration returns.
	listPorts func() ([]SerialPort, error)
	// openPort is serial.Open, replaced in tests for the same reason: what a
	// stalled session says about the wire cannot be checked without deciding
	// what comes off it.
	openPort func(name string, baud int) (serialPort, error)
}

// serialPort is the part of go.bug.st/serial.Port this driver uses.
type serialPort interface {
	Read(p []byte) (int, error)
	SetReadTimeout(t time.Duration) error
	Close() error
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
	return &Serial{
		cfg:       cfg,
		parser:    parser,
		log:       log,
		reporter:  reporter,
		tried:     map[string]struct{}{},
		warned:    map[string][]string{},
		listPorts: ListSerialPorts,
		openPort: func(name string, baud int) (serialPort, error) {
			return serial.Open(name, &serial.Mode{BaudRate: baud})
		},
	}, nil
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
	port, err := s.openPort(name, s.cfg.Baud)
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
	lastFrame := time.Now()
	var count uint64
	// Bytes since the last frame, over the same window the stall is measured
	// in, and the head of the stream while nothing has parsed out of it. See
	// stalled for what they are for.
	var sinceFrame int64
	var preview []byte

	for {
		if ctx.Err() != nil {
			return nil
		}
		// Frames, not bytes, and checked on every pass rather than only when a
		// read times out. "auto" can land on some other serial device that
		// chatters away without ever forming a packet; keying on arrival would
		// hold that port open forever and never re-run the search, so the real
		// board plugged in later would never be found.
		if time.Since(lastFrame) > serialStallTimeout {
			return s.stalled(name, count, sinceFrame, preview)
		}
		n, err := port.Read(buf)
		if err != nil {
			return fmt.Errorf("serial: read from %s: %w", name, err)
		}
		if n == 0 {
			// A read timeout, not an error.
			continue
		}
		sinceFrame += int64(n)
		if count == 0 && len(preview) < previewBytes {
			take := previewBytes - len(preview)
			if take > n {
				take = n
			}
			preview = append(preview, buf[:take]...)
		}

		frames := assembler.feed(buf[:n])
		for _, f := range frames {
			lastFrame = time.Now()
			if count == 0 {
				// This port has proved itself, so the auto search should come
				// back to it first rather than rotating past it.
				s.proven = name
				// It works now. If it stops parsing later that is news again,
				// whatever was said about it before.
				delete(s.warned, name)
				s.reporter.Connected(s.Name())
			}
			count++
			if err := send(ctx, out, f); err != nil {
				return nil
			}
		}
		if len(frames) > 0 {
			// Not zero: a read can carry a frame and the start of the next one
			// together, and those trailing bytes arrived after the frame the
			// stall is now measured from. Dropping them would report a silent
			// port for a board that is in fact part way through a packet.
			sinceFrame = int64(assembler.pending())
		}
	}
}

// stalled explains a session that stopped producing frames, and says what was
// on the wire when nothing could be made of it.
//
// The stall is counted in frames rather than bytes (see the loop), so on its
// own it cannot tell a silent port from a talkative one whose packets this
// parser does not recognise -- and those two need opposite fixes: the first is
// a board that is not streaming, the second is a header or a baud rate that
// does not match the firmware. The byte total separates them. For the second,
// the bytes themselves are what settles it, which is why they are logged: the
// packet header is configurable precisely because firmware revisions differ,
// and the value to configure is sitting in that line.
func (s *Serial) stalled(name string, frames uint64, sinceFrame int64, preview []byte) error {
	if frames > 0 {
		// A board that worked and then stopped. The count is a lower bound
		// here and says so: bytes arriving after a frame in that same read are
		// only still countable while the parser is holding them, and it
		// discards what cannot begin a packet. Which way that lands does not
		// change the reading -- nothing arrived, or something did -- but the
		// number should not be quoted as a total when it is not one.
		//
		// Nothing is shown of the stream either: this port was producing
		// frames a moment ago, so its packets are not the problem.
		return fmt.Errorf("serial: %s produced no frame for %s (at least %d bytes received since the last frame)", name, serialStallTimeout, sinceFrame)
	}

	// Nothing has ever parsed on this port, so no frame has reset the count:
	// this total is every byte the session saw.
	err := fmt.Errorf("serial: %s produced no frame for %s (%d bytes received in that time)", name, serialStallTimeout, sinceFrame)
	if sinceFrame == 0 {
		// A silent port. The error says so and there is nothing to show.
		return err
	}

	head := hexPreview(preview)
	// Said once per stream, and only so many times per port. The reconnect
	// loop comes back every few seconds, and repeating a complaint already
	// made adds nothing; the bytes are still in the debug line for anyone
	// watching a port whose stream keeps moving.
	seen := s.warned[name]
	if slices.Contains(seen, head) || len(seen) >= maxWarnedStreams {
		s.log.Debug("still nothing that parses on the serial port", "port", name, "first_bytes", head)
		return err
	}
	s.warned[name] = append(seen, head)
	s.log.Warn("bytes are arriving on the serial port but no packet matched; check the firmware's preamble against source.serial.header, and the baud rate",
		"port", name,
		"baud", s.cfg.Baud,
		"expected_header", hexPreview(s.parser.Header()),
		"first_bytes", head,
		"bytes_received", sinceFrame,
	)
	return err
}

// hexPreview renders bytes the way a protocol document writes them, so what is
// logged can be compared with the header in the settings by eye.
func hexPreview(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, c := range b {
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%02x", c)
	}
	return sb.String()
}

// splitPackets adapts the parser to the frameAssembler signature. The parser
// carries its own payload bound, so maxSize is already applied there.
func (s *Serial) splitPackets(buf []byte, _ int) ([][]byte, []byte) {
	return s.parser.Parse(buf)
}

// resolvePort returns the configured port, or searches for one when set to
// AutoPort.
//
// The search does not just take the head of the list. Two known-vendor boards
// can be plugged in at once -- a Babble board and an unrelated CP210x dongle,
// say -- and always returning the first one means the wrong device is opened,
// stalled out and reopened forever while the camera sitting next to it is
// never tried. So each candidate is used once, and the next reconnect moves
// on to the one after it.
func (s *Serial) resolvePort() (string, error) {
	if !strings.EqualFold(s.cfg.Port, AutoPort) {
		return s.cfg.Port, nil
	}
	ports, err := s.listPorts()
	if err != nil {
		return "", fmt.Errorf("serial: enumerate ports: %w", err)
	}

	candidates := autoCandidates(ports)
	if len(candidates) == 0 {
		return "", errors.New("serial: no port matched a known camera vendor ID; set source.serial.port explicitly")
	}

	// A port that has already delivered frames goes first after a drop: a
	// tugged cable is far more likely than the board having moved. It only
	// gets the one attempt, so if it really is gone the rotation continues.
	if s.proven != "" {
		name := s.proven
		s.proven = ""
		if slices.Contains(candidates, name) {
			clear(s.tried)
			s.tried[name] = struct{}{}
			s.log.Info("reopening the serial port that was working", "port", name)
			return name, nil
		}
	}

	name, ok := s.firstUntried(candidates)
	if !ok {
		// Every candidate has had a turn. Start the rotation again rather than
		// giving up: a board can be unplugged and put back, and the port that
		// failed a minute ago may be the right one now.
		s.log.Info("every candidate serial port has been tried, starting over", "ports", len(candidates))
		clear(s.tried)
		name, _ = s.firstUntried(candidates)
	}
	s.tried[name] = struct{}{}
	s.log.Info("auto-selected serial port", "port", name, "candidates", len(candidates))
	return name, nil
}

// firstUntried returns the first candidate this driver has not opened yet.
func (s *Serial) firstUntried(candidates []string) (string, bool) {
	for _, name := range candidates {
		if _, seen := s.tried[name]; !seen {
			return name, true
		}
	}
	return "", false
}

// autoCandidates lists the ports AutoPort is willing to open, best first.
//
// A recognised vendor ID is the only positive evidence available, so those
// come first and in the order ListSerialPorts put them. The lone port on the
// machine is the fallback: with nothing else it could be, it is worth a try.
func autoCandidates(ports []SerialPort) []string {
	var names []string
	for _, p := range ports {
		if p.Vendor != "" {
			names = append(names, p.Name)
		}
	}
	if len(names) == 0 && len(ports) == 1 {
		names = append(names, ports[0].Name)
	}
	return names
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
