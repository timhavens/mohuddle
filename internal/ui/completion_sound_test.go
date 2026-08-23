package ui

import (
	"bytes"
	"testing"
)

func TestTerminalBellWritesBEL(t *testing.T) {
	var output bytes.Buffer
	if err := (terminalBell{writer: &output}).Notify(); err != nil {
		t.Fatal(err)
	}
	if got, want := output.Bytes(), []byte{'\a'}; !bytes.Equal(got, want) {
		t.Fatalf("bell bytes=%v want=%v", got, want)
	}
}
