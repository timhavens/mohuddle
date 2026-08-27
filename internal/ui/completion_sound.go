package ui

import (
	"io"
	"os"
)

type completionNotifier interface {
	Notify() error
}

type terminalBell struct {
	writer io.Writer
}

func defaultCompletionNotifier() completionNotifier {
	return terminalBell{writer: os.Stderr}
}

func (b terminalBell) Notify() error {
	if b.writer == nil {
		return nil
	}
	_, err := b.writer.Write([]byte{'\a'})
	return err
}

// notifyRequestFinished rings once when a request is finished or has stopped
// producing responses. It is deliberately not per agent: a round of five
// agents is one request and gets one bell.
func (m *Model) notifyRequestFinished() {
	if !m.completionSound || m.completionNotifier == nil {
		return
	}
	if err := m.completionNotifier.Notify(); err != nil && !m.completionSoundError {
		m.completionSoundError = true
		m.addNotice(errorStyle.Render("Could not play the request-finished terminal sound: " + err.Error()))
	}
}
