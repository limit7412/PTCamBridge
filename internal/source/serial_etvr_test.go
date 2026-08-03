package source

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/limit7412/PTCamBridge/internal/core"
)

// autoSerial builds an AutoPort driver whose enumeration is fixed, so the
// candidate rotation can be observed without any hardware.
func autoSerial(t *testing.T, ports ...SerialPort) *Serial {
	t.Helper()
	s, err := NewSerial(SerialConfig{Port: AutoPort}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewSerial: %v", err)
	}
	s.listPorts = func() ([]SerialPort, error) { return ports, nil }
	return s
}

func resolve(t *testing.T, s *Serial) string {
	t.Helper()
	name, err := s.resolvePort()
	if err != nil {
		t.Fatalf("resolvePort: %v", err)
	}
	return name
}

// Two boards can carry a recognised vendor ID while only one of them is the
// camera. Handing back the same head of the list on every reconnect means the
// wrong one is opened forever and the real board is never reached.
func TestSerialAutoPortMovesOnAfterAFailedCandidate(t *testing.T) {
	s := autoSerial(t,
		SerialPort{Name: "COM3", Vendor: "Silicon Labs CP210x", VID: "10C4"},
		SerialPort{Name: "COM7", Vendor: "Espressif", VID: "303A"},
	)

	if got := resolve(t, s); got != "COM3" {
		t.Fatalf("first attempt = %q, want COM3", got)
	}
	if got := resolve(t, s); got != "COM7" {
		t.Errorf("second attempt = %q, want the other candidate", got)
	}
}

// Once every candidate has had a turn the search starts over rather than
// giving up: a board that was unplugged a minute ago can be back.
func TestSerialAutoPortRestartsTheRotationWhenExhausted(t *testing.T) {
	s := autoSerial(t,
		SerialPort{Name: "COM3", Vendor: "Silicon Labs CP210x", VID: "10C4"},
		SerialPort{Name: "COM7", Vendor: "Espressif", VID: "303A"},
	)

	want := []string{"COM3", "COM7", "COM3", "COM7"}
	for i, expected := range want {
		if got := resolve(t, s); got != expected {
			t.Fatalf("attempt %d = %q, want %q", i+1, got, expected)
		}
	}
}

// A port that delivered frames is the one to reopen first when it drops: a
// tugged cable is likelier than the board having moved. It gets that one
// attempt only, so a board that really is gone does not wedge the rotation.
func TestSerialAutoPortPrefersThePortThatWorked(t *testing.T) {
	s := autoSerial(t,
		SerialPort{Name: "COM3", Vendor: "Silicon Labs CP210x", VID: "10C4"},
		SerialPort{Name: "COM7", Vendor: "Espressif", VID: "303A"},
	)

	if got := resolve(t, s); got != "COM3" {
		t.Fatalf("first attempt = %q, want COM3", got)
	}
	// COM7 is opened next and is the one that produces frames.
	if got := resolve(t, s); got != "COM7" {
		t.Fatalf("second attempt = %q, want COM7", got)
	}
	s.proven = "COM7"

	if got := resolve(t, s); got != "COM7" {
		t.Errorf("after a drop = %q, want the port that was working", got)
	}
	if got := resolve(t, s); got != "COM3" {
		t.Errorf("after the retry failed = %q, want the rotation to continue", got)
	}
}

// A port that worked and has since been unplugged must not stall the search.
func TestSerialAutoPortSkipsAProvenPortThatIsGone(t *testing.T) {
	s := autoSerial(t, SerialPort{Name: "COM7", Vendor: "Espressif", VID: "303A"})
	s.proven = "COM4"

	if got := resolve(t, s); got != "COM7" {
		t.Errorf("resolvePort = %q, want the candidate that is still attached", got)
	}
}

// With nothing recognisable and nothing else it could be, the only port on the
// machine is worth a try; several unrecognised ports are not a guess worth
// making.
func TestSerialAutoPortCandidates(t *testing.T) {
	tests := []struct {
		name  string
		ports []SerialPort
		want  []string
	}{
		{
			name:  "the lone port is the fallback",
			ports: []SerialPort{{Name: "COM1"}},
			want:  []string{"COM1"},
		},
		{
			name:  "several unrecognised ports are not candidates",
			ports: []SerialPort{{Name: "COM1"}, {Name: "COM2"}},
			want:  nil,
		},
		{
			name:  "a recognised port wins over the fallback",
			ports: []SerialPort{{Name: "COM1"}, {Name: "COM2", Vendor: "FTDI"}},
			want:  []string{"COM2"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := autoCandidates(tc.ports)
			if len(got) != len(tc.want) {
				t.Fatalf("autoCandidates = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("autoCandidates = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestSerialAutoPortReportsWhenNothingMatches(t *testing.T) {
	s := autoSerial(t, SerialPort{Name: "COM1"}, SerialPort{Name: "COM2"})
	if _, err := s.resolvePort(); err == nil {
		t.Fatal("expected an error when no port matches a known vendor ID")
	}
}

// An explicit port is used as written, with none of the rotation.
func TestSerialExplicitPortIsUsedVerbatim(t *testing.T) {
	s, err := NewSerial(SerialConfig{Port: "COM9"}, discardLogger(), nil)
	if err != nil {
		t.Fatalf("NewSerial: %v", err)
	}
	s.listPorts = func() ([]SerialPort, error) { return nil, errors.New("enumeration must not be reached") }

	for i := 0; i < 3; i++ {
		if got := resolve(t, s); got != "COM9" {
			t.Fatalf("attempt %d = %q, want COM9", i+1, got)
		}
	}
}

// fakePort feeds a session a fixed stream and then goes quiet, which is what a
// board that talks nonsense and one that says nothing both look like from here.
type fakePort struct {
	chunks [][]byte
	closed bool
}

func (p *fakePort) Read(b []byte) (int, error) {
	if len(p.chunks) == 0 {
		// A read timeout, the way the real driver reports one.
		time.Sleep(time.Millisecond)
		return 0, nil
	}
	n := copy(b, p.chunks[0])
	p.chunks = p.chunks[1:]
	return n, nil
}

func (p *fakePort) SetReadTimeout(time.Duration) error { return nil }

func (p *fakePort) Close() error {
	p.closed = true
	return nil
}

// wiredSerial builds a driver whose port hands back the given chunks.
func wiredSerial(t *testing.T, log *slog.Logger, cfg SerialConfig, chunks ...[]byte) *Serial {
	t.Helper()
	if cfg.Port == "" {
		cfg.Port = "COM4"
	}
	s, err := NewSerial(cfg, log, nil)
	if err != nil {
		t.Fatalf("NewSerial: %v", err)
	}
	s.openPort = func(string, int) (serialPort, error) { return &fakePort{chunks: chunks}, nil }
	return s
}

// runUntilStall runs one session and returns why it ended.
func runUntilStall(t *testing.T, s *Serial) error {
	t.Helper()
	restore := serialStallTimeout
	serialStallTimeout = 50 * time.Millisecond
	t.Cleanup(func() { serialStallTimeout = restore })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.session(ctx, make(chan core.Frame, 8))
}

// A silent port and a port full of bytes nobody can parse need opposite fixes,
// and the stall alone does not tell them apart. The byte count does.
func TestSerialStallSaysHowMuchArrived(t *testing.T) {
	t.Run("nothing on the wire", func(t *testing.T) {
		s := wiredSerial(t, discardLogger(), SerialConfig{})
		err := runUntilStall(t, s)
		if err == nil {
			t.Fatal("session returned nil, want a stall")
		}
		if !strings.Contains(err.Error(), "0 bytes received") {
			t.Errorf("error = %v, want it to report that nothing arrived", err)
		}
	})

	t.Run("bytes but no packet", func(t *testing.T) {
		s := wiredSerial(t, discardLogger(), SerialConfig{}, bytes.Repeat([]byte{0x5A}, 64))
		err := runUntilStall(t, s)
		if err == nil {
			t.Fatal("session returned nil, want a stall")
		}
		if !strings.Contains(err.Error(), "64 bytes received") {
			t.Errorf("error = %v, want it to report the 64 bytes that arrived", err)
		}
	})
}

// The bytes themselves are what settles a header mismatch, so they have to
// reach the log along with the header that was being looked for.
func TestSerialStallLogsTheUnparsableStream(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// A preamble one byte different from the configured one: the case the
	// setting exists for, and the one a byte count alone cannot diagnose.
	stream := append([]byte{0xFF, 0xA0, 0xFF, 0xB1, 0x10, 0x00}, bytes.Repeat([]byte{0x42}, 32)...)
	s := wiredSerial(t, log, SerialConfig{}, stream)

	if err := runUntilStall(t, s); err == nil {
		t.Fatal("session returned nil, want a stall")
	}

	out := logged.String()
	for _, want := range []string{
		"ff a0 ff b1",     // what actually arrived
		"expected_header", // and what was being looked for
		"ff a0 ff a1",
		"baud",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log does not mention %q:\n%s", want, out)
		}
	}
	// Only the head of the stream, not a dump of everything that arrived.
	if strings.Contains(out, strings.Repeat("42 ", 20)) {
		t.Errorf("log carries more of the stream than the preview:\n%s", out)
	}
}

// The reconnect loop comes back every few seconds. A board that is wrong the
// same way every time must not fill the log with the same warning.
func TestSerialStallWarnsOncePerDistinctStream(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	junk := bytes.Repeat([]byte{0x5A}, 64)
	s := wiredSerial(t, log, SerialConfig{}, junk)
	if err := runUntilStall(t, s); err == nil {
		t.Fatal("session returned nil, want a stall")
	}
	// Same stream again, as a reconnect would see.
	s.openPort = func(string, int) (serialPort, error) { return &fakePort{chunks: [][]byte{junk}}, nil }
	if err := runUntilStall(t, s); err == nil {
		t.Fatal("second session returned nil, want a stall")
	}

	if warns := strings.Count(logged.String(), "level=WARN"); warns != 1 {
		t.Errorf("logged %d warnings for the same stream, want 1:\n%s", warns, logged.String())
	}

	// A stream that changed is news again: the board may have been reflashed
	// or a different device may now be on that port.
	logged.Reset()
	s.openPort = func(string, int) (serialPort, error) {
		return &fakePort{chunks: [][]byte{bytes.Repeat([]byte{0x77}, 64)}}, nil
	}
	if err := runUntilStall(t, s); err == nil {
		t.Fatal("third session returned nil, want a stall")
	}
	if warns := strings.Count(logged.String(), "level=WARN"); warns != 1 {
		t.Errorf("logged %d warnings for a changed stream, want 1:\n%s", warns, logged.String())
	}
}

// A board that worked and then went quiet is a different situation: there is
// no unparsable stream to show, so showing one would be misleading.
func TestSerialStallAfterWorkingSaysNothingAboutTheStream(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	parser, err := core.NewETVRParser(nil, 0)
	if err != nil {
		t.Fatalf("NewETVRParser: %v", err)
	}
	packet, err := parser.EncodePacket(testJPEG(t))
	if err != nil {
		t.Fatalf("EncodePacket: %v", err)
	}

	s := wiredSerial(t, log, SerialConfig{}, packet)
	if err := runUntilStall(t, s); err == nil {
		t.Fatal("session returned nil, want a stall once the board went quiet")
	}
	if strings.Contains(logged.String(), "no packet matched") {
		t.Errorf("a board that worked was reported as unparsable:\n%s", logged.String())
	}
}
