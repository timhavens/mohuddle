package ui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/timhavens/mohuddle/internal/api"
	"github.com/timhavens/mohuddle/internal/store"
)

func (m *Model) ConfigureChatGPT(service *api.Service) { m.chatgpt = service }

func (m *Model) handleChatGPT(fields []string) {
	if m.chatgpt == nil {
		m.addNotice(errorStyle.Render("ChatGPT requires the private local API; start MoHuddle without --no-api on Linux or macOS."))
		return
	}
	action := "status"
	if len(fields) > 1 {
		action = strings.ToLower(fields[1])
	}
	if len(fields) > 3 || (len(fields) == 3 && action != "on") {
		m.addNotice(errorStyle.Render("usage: /chatgpt on [1m–24h]|off|status|resume"))
		return
	}
	switch action {
	case "on":
		ttl := 8 * time.Hour
		if len(fields) == 3 {
			var err error
			ttl, err = time.ParseDuration(fields[2])
			if err != nil {
				m.addNotice(errorStyle.Render("use a duration such as 30m or 8h"))
				return
			}
		}
		path, err := m.chatgpt.EnableChatGPT(ttl)
		if err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return
		}
		command := "mohuddle chatgpt serve"
		defaultDir, _ := store.DefaultStateDir()
		if filepath.Clean(filepath.Dir(path)) != filepath.Clean(defaultDir) {
			command += " --state-dir '" + strings.ReplaceAll(filepath.Dir(path), "'", "'\\''") + "'"
		}
		m.addNotice(fmt.Sprintf("ChatGPT room access enabled for %s. It may request peer feedback and assign work using each participant's current permissions.\nPrivate tunnel command: %s\nThe bridge finds the open, authorized room automatically. Connect this command through OpenAI Secure MCP Tunnel.\nIn ChatGPT: join the MoHuddle room, then open its live panel. /chatgpt off revokes access immediately.", ttl, command))
	case "off":
		if err := m.chatgpt.RevokeChatGPT(); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return
		}
		m.addNotice("ChatGPT access revoked and pending peer replies cancelled.")
	case "resume":
		if err := m.chatgpt.ResumeChatGPT(); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return
		}
		m.addNotice("ChatGPT may contribute again; eight further peer exchanges or work requests are available.")
	case "status":
		state, _ := m.chatgpt.ChatGPTStatus()
		m.addNotice(fmt.Sprintf("ChatGPT: enabled %t, connected %t, paused %t; %d peer exchanges or work requests remaining.\nGrant expiry: %s\nUse /chatgpt on [duration], /chatgpt off, or /chatgpt resume.", state.Enabled, state.Connected, state.Paused, state.ExchangesRemaining, state.ExpiresAt.Format(time.RFC3339)))
	default:
		m.addNotice(errorStyle.Render("usage: /chatgpt on [1m–24h]|off|status|resume"))
		return
	}
	m.syncRoomMetadata()
}
