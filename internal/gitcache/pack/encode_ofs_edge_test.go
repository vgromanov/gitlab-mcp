package pack

import (
	"errors"
	"testing"
)

func TestEncodeOFSRejectsZero(t *testing.T) {
	if _, err := EncodeOFS(0); !errors.Is(err, ErrMalformed) {
		t.Fatalf("zero: %v", err)
	}
	got, err := EncodeOFS(1)
	if err != nil || len(got) == 0 {
		t.Fatalf("one: %v %v", got, err)
	}
}
