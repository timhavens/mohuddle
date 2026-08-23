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

func (m *Model) notifyTurnFinished() {
	if !m.completionSound || m.completionNotifier == nil {
		return
	}
	if err := m.completionNotifier.Notify(); err != nil && !m.completionSoundError {
		m.completionSoundError = true
		m.addNotice(errorStyle.Render("Could not play the AI-finished terminal sound: " + err.Error()))
	}
}
