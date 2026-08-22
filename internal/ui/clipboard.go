package ui

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os/exec"
	"strings"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/timhavens/mohuddle/internal/chat"
)

const maxClipboardImageBytes = 20 * 1024 * 1024

type clipboardContent struct {
	Image []byte
	Text  string
	Err   error
}

type clipboardReader interface {
	Read() clipboardContent
}

type systemClipboard struct{}

type clipboardMsg clipboardContent

func (systemClipboard) Read() clipboardContent {
	if path, err := exec.LookPath("powershell.exe"); err == nil {
		const script = `Add-Type -AssemblyName System.Drawing; $image = Get-Clipboard -Format Image -ErrorAction SilentlyContinue; if ($null -eq $image) { exit 3 }; $stream = New-Object System.IO.MemoryStream; $image.Save($stream, [System.Drawing.Imaging.ImageFormat]::Png); [Console]::Out.Write([Convert]::ToBase64String($stream.ToArray()))`
		output, imageErr := exec.Command(path, "-NoProfile", "-NonInteractive", "-STA", "-Command", script).Output()
		if imageErr == nil {
			data, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(output)))
			if decodeErr == nil && len(data) > 0 {
				return clipboardContent{Image: data}
			}
		}
	}
	text, err := clipboard.ReadAll()
	if err != nil {
		return clipboardContent{Err: fmt.Errorf("clipboard unavailable: %w", err)}
	}
	if text == "" {
		return clipboardContent{Err: fmt.Errorf("clipboard is empty")}
	}
	return clipboardContent{Text: text}
}

func readClipboard(reader clipboardReader) tea.Cmd {
	return func() tea.Msg { return clipboardMsg(reader.Read()) }
}

func (m *Model) acceptClipboardImage(data []byte) error {
	if len(data) > maxClipboardImageBytes {
		return fmt.Errorf("clipboard image exceeds the 20 MiB limit")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("clipboard does not contain a valid image: %w", err)
	}
	if m.composerStore == nil {
		return fmt.Errorf("room attachment storage is unavailable")
	}
	attachment, err := m.composerStore.SaveAttachment(m.room.ID, chat.Attachment{
		Kind: chat.AttachmentImage, Name: "clipboard.png", MIMEType: "image/png",
		Width: config.Width, Height: config.Height,
	}, data)
	if err != nil {
		return fmt.Errorf("save clipboard image: %w", err)
	}
	m.attachments = append(m.attachments, attachment)
	m.resize()
	return nil
}
