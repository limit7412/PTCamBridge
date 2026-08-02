package core

import (
	"errors"
	"testing"
)

func TestValidateJPEG(t *testing.T) {
	valid := encodeJPEG(t, 16, 16)

	cases := []struct {
		name string
		data []byte
		max  int
		want error
	}{
		{"real image", valid, 0, nil},
		{"minimal SOI/EOI", []byte{0xFF, 0xD8, 0xFF, 0xD9}, 0, nil},
		{"empty", nil, 0, ErrFrameTooSmall},
		{"two bytes", []byte{0xFF, 0xD8}, 0, ErrFrameTooSmall},
		{"missing SOI", []byte{0x00, 0x00, 0xFF, 0xD9}, 0, ErrMissingSOI},
		{"missing EOI", []byte{0xFF, 0xD8, 0x00, 0x00}, 0, ErrMissingEOI},
		{"over the limit", valid, 8, ErrFrameTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateJPEG(tc.data, tc.max)
			if !errors.Is(err, tc.want) {
				t.Errorf("ValidateJPEG = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestFrameSize(t *testing.T) {
	f := Frame{Data: []byte{0xFF, 0xD8, 0xFF, 0xD9}}
	if f.Size() != 4 {
		t.Errorf("Size() = %d, want 4", f.Size())
	}
}
