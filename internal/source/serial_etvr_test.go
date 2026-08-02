package source

import (
	"errors"
	"testing"
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
