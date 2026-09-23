package ui

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/timhavens/mohuddle/internal/api"
	"github.com/timhavens/mohuddle/internal/chat"
	"github.com/timhavens/mohuddle/internal/settings"
	"github.com/timhavens/mohuddle/internal/store"
	"github.com/timhavens/mohuddle/internal/tunnel"
)

func (m *Model) ConfigureChatGPT(service *api.Service) { m.chatgpt = service }

func (m *Model) ConfigureChatGPTTunnel(manager *tunnel.Manager, preferences *settings.Store, roomKey string) {
	m.chatgptTunnel, m.chatgptPreferences, m.chatgptRoomKey = manager, preferences, roomKey
	if m.chatgpt != nil && preferences != nil {
		if err := m.chatgpt.SetChatGPTLimits(preferences.ChatGPTLimits(roomKey)); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
		}
	}
}

type chatGPTAutoConnectMsg struct{}

const chatGPTUsage = "usage: /chatgpt on [1m–24h]|off|status|restart|resume|renew [duration]|manual [duration]|profile NAME|auto on|off|monitor start|status|stop|resume|limits [exchanges N|followups N|duration 1h|repeats N|reset]"

func (m Model) chatGPTProfile() string {
	if m.chatgptPreferences != nil {
		return m.chatgptPreferences.ChatGPTProfile()
	}
	return tunnel.DefaultProfile
}

func (m *Model) handleChatGPT(fields []string) {
	if m.chatgpt == nil {
		m.addNotice(errorStyle.Render("ChatGPT requires the private local API; start MoHuddle without --no-api on Linux or macOS."))
		return
	}
	action := "status"
	if len(fields) > 1 {
		action = strings.ToLower(fields[1])
	}
	if action == "monitor" {
		m.handleCoordination(fields[2:])
		return
	}
	if action == "limits" {
		m.handleChatGPTLimits(fields[2:])
		m.syncRoomMetadata()
		m.resize()
		return
	}
	if len(fields) > 3 || (len(fields) == 3 && action != "on" && action != "manual" && action != "renew" && action != "profile" && action != "auto") {
		m.addNotice(errorStyle.Render(chatGPTUsage))
		return
	}
	switch action {
	case "on", "manual", "renew":
		ttl := 8 * time.Hour
		if len(fields) == 3 {
			var err error
			ttl, err = time.ParseDuration(fields[2])
			if err != nil {
				m.addNotice(errorStyle.Render("use a duration such as 30m or 8h"))
				return
			}
		}
		var path string
		var err error
		if action == "renew" {
			path, err = m.chatgpt.EnableChatGPT(ttl)
		} else {
			path, err = m.chatgpt.EnsureChatGPT(ttl)
		}
		if err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return
		}
		state, _ := m.chatgpt.ChatGPTStatus()
		if m.chatgptTunnel != nil && action != "manual" {
			m.chatgptTunnel.Start(m.chatGPTProfile(), path)
			m.addNotice(fmt.Sprintf("ChatGPT access is enabled until %s. MoHuddle manages the tunnel in the background; watch the CHATGPT agent row. Ask ChatGPT to join/read the room when ready. Use /chatgpt restart to repair the tunnel, or /leave @chatgpt to stop it.", state.ExpiresAt.Local().Format("15:04 MST")))
			break
		}
		if m.chatgptTunnel != nil {
			m.chatgptTunnel.Stop()
		}
		command := "mohuddle chatgpt serve"
		defaultDir, _ := store.DefaultStateDir()
		if filepath.Clean(filepath.Dir(path)) != filepath.Clean(defaultDir) {
			command += " --state-dir '" + strings.ReplaceAll(filepath.Dir(path), "'", "'\\''") + "'"
		}
		m.addNotice(fmt.Sprintf("ChatGPT room access enabled until %s, using an externally managed tunnel.\nPrivate tunnel command: %s\nIn ChatGPT: join the MoHuddle room, then open its live panel. /chatgpt off revokes access immediately.", state.ExpiresAt.Local().Format("15:04 MST"), command))
	case "off":
		m.chatgptAutoSuppressed = true
		if m.chatgptTunnel != nil {
			m.chatgptTunnel.Stop()
		}
		if m.chatgptPreferences != nil {
			if err := m.chatgptPreferences.SetChatGPTAutoConnect(m.chatgptRoomKey, false); err != nil {
				m.addNotice(errorStyle.Render("Could not save auto-connect OFF. Access will still be revoked now; disable auto-connect before reopening this room: " + err.Error()))
			}
		}
		if err := m.chatgpt.RevokeChatGPT(); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return
		}
		m.addNotice("ChatGPT access revoked; its managed tunnel is stopping and pending peer replies are cancelled.")
	case "restart":
		state, path := m.chatgpt.ChatGPTStatus()
		if !state.Enabled {
			m.addNotice("ChatGPT access is off or expired; use /join @chatgpt first.")
			break
		}
		if m.chatgptTunnel == nil {
			m.addNotice("This connection uses an externally managed tunnel; restart it in its terminal.")
			break
		}
		m.chatgptTunnel.Restart(m.chatGPTProfile(), path)
		m.addNotice("Restarting the ChatGPT tunnel and bridge. Room work, participation pause, and the existing access lifetime are preserved. Ask ChatGPT to read/rejoin once its agent row is ready.")
	case "profile":
		if len(fields) != 3 || m.chatgptPreferences == nil {
			m.addNotice(errorStyle.Render("usage: /chatgpt profile NAME"))
			break
		}
		if err := m.chatgptPreferences.SetChatGPTProfile(fields[2]); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			break
		}
		m.addNotice("Tunnel profile selected. Use /join @chatgpt to start it, or /chatgpt restart to switch the active tunnel.")
	case "auto":
		if len(fields) != 3 || (fields[2] != "on" && fields[2] != "off") || m.chatgptPreferences == nil {
			m.addNotice(errorStyle.Render("usage: /chatgpt auto on|off (remembered for this room only)"))
			break
		}
		if fields[2] == "off" {
			m.chatgptAutoSuppressed = true
		}
		if err := m.chatgptPreferences.SetChatGPTAutoConnect(m.chatgptRoomKey, fields[2] == "on"); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			break
		}
		m.chatgptAutoSuppressed = fields[2] == "off"
		m.addNotice("ChatGPT auto-connect is " + fields[2] + " for this room. When on, opening this room authorizes a fresh eight-hour grant and starts its tunnel. /leave @chatgpt disables it.")
	case "resume":
		if err := m.chatgpt.ResumeChatGPT(); err != nil {
			m.addNotice(errorStyle.Render(err.Error()))
			return
		}
		state, _ := m.chatgpt.ChatGPTStatus()
		m.addNotice(fmt.Sprintf("ChatGPT may contribute again; %d further peer exchanges or work requests are available. Re-enable live follow-ups in the panel when ready.", state.ExchangesRemaining))
	case "status":
		m.showCoordination()
		state, _ := m.chatgpt.ChatGPTStatus()
		transport := "externally managed tunnel"
		if m.chatgptTunnel != nil {
			status := m.chatgptTunnel.Status()
			profile := status.Profile
			if profile == "" {
				profile = m.chatGPTProfile()
			}
			transport = fmt.Sprintf("tunnel %s · profile %s · restarts %d\n%s", status.State, profile, status.Restarts, status.Detail)
		}
		auto := m.chatgptPreferences != nil && m.chatgptPreferences.ChatGPTAutoConnect(m.chatgptRoomKey)
		m.addNotice(fmt.Sprintf("ChatGPT: enabled %t, connected %t, paused %t; %d/%d exchanges remaining.\n%s\n%s\nGrant expiry: %s · auto-connect %t\n/chatgpt restart repairs transport; /chatgpt resume authorizes more participation; /leave @chatgpt revokes access and stops the managed tunnel.", state.Enabled, state.Connected, state.Paused, state.ExchangesRemaining, state.Limits.Exchanges, chatGPTLimitsDescription(state.Limits), transport, state.ExpiresAt.Format(time.RFC3339), auto))
		if reason := chatGPTPauseDescription(state.PauseReason); reason != "" {
			m.addNotice(reason)
		}
	default:
		m.addNotice(errorStyle.Render(chatGPTUsage))
		return
	}
	m.syncRoomMetadata()
	m.resize()
}

func chatGPTLimitsDescription(limits chat.ChatGPTLimits) string {
	return fmt.Sprintf("Room limits: %d exchanges per authorization; %d automatic follow-ups over %s; pause after %d identical requests without progress. /chatgpt limits changes these settings.", limits.Exchanges, limits.FollowUps, (time.Duration(limits.FollowUpSeconds) * time.Second).String(), limits.RepeatedRequests)
}

func chatGPTPauseDescription(reason string) string {
	switch reason {
	case "host_paused":
		return "Paused by host · /chatgpt resume"
	case "exchange_limit":
		return "Exchange budget reached · accepted work continues · /chatgpt resume"
	case "no_progress":
		return "Repeated requests without progress · accepted work continues · inspect results, then /chatgpt resume"
	}
	return ""
}

func (m *Model) handleChatGPTLimits(fields []string) {
	state, _ := m.chatgpt.ChatGPTStatus()
	limits := state.Limits
	if len(fields) == 0 {
		m.addNotice(chatGPTLimitsDescription(limits))
		return
	}
	if m.chatgptPreferences == nil || m.chatgptRoomKey == "" {
		m.addNotice(errorStyle.Render("Room preferences are unavailable; cannot save ChatGPT limits."))
		return
	}
	if len(fields) == 1 && fields[0] == "reset" {
		limits = chat.DefaultChatGPTLimits()
	} else if len(fields) == 2 {
		if fields[0] == "duration" {
			duration, err := time.ParseDuration(fields[1])
			if err != nil || duration < time.Minute || duration > 24*time.Hour || duration%time.Second != 0 {
				m.addNotice(errorStyle.Render("ChatGPT follow-up duration must be 1m–24h in whole seconds (for example 1h or 90m)."))
				return
			}
			limits.FollowUpSeconds = int(duration / time.Second)
		} else {
			value, err := strconv.Atoi(fields[1])
			if err != nil {
				m.addNotice(errorStyle.Render("ChatGPT limits require a whole number."))
				return
			}
			switch fields[0] {
			case "exchanges":
				limits.Exchanges = value
			case "followups":
				limits.FollowUps = value
			case "repeats":
				limits.RepeatedRequests = value
			default:
				m.addNotice(errorStyle.Render(chatGPTUsage))
				return
			}
		}
	} else {
		m.addNotice(errorStyle.Render(chatGPTUsage))
		return
	}
	if err := m.chatgptPreferences.SetChatGPTLimits(m.chatgptRoomKey, limits); err != nil {
		m.addNotice(errorStyle.Render(err.Error()))
		return
	}
	if err := m.chatgpt.SetChatGPTLimits(limits); err != nil {
		m.addNotice(errorStyle.Render(err.Error()))
		return
	}
	m.addNotice(chatGPTLimitsDescription(limits) + " Saved for this room. Usage is preserved; /chatgpt resume refreshes the exchange budget. Panel changes apply on its next update; paused follow-ups require re-enabling.")
}

func (m Model) chatGPTVisible() bool {
	return m.chatgpt != nil || m.chatgptTunnel != nil || m.room.ChatGPT != nil
}

// Transport readiness and the website's renewable participation lease are
// different states. A healthy tunnel never implies ChatGPT has joined.
func (m Model) chatGPTActivity() participantActivity {
	now := m.now
	if now.IsZero() {
		now = time.Now()
	}
	var transport tunnel.Status
	if m.chatgptTunnel != nil {
		transport = m.chatgptTunnel.Status()
	}
	return chatGPTConnectionActivity(m.room.ChatGPT, transport, now)
}

func chatGPTConnectionActivity(state *chat.ChatGPTState, transport tunnel.Status, now time.Time) participantActivity {
	result := participantActivity{Phase: phaseAway, Detail: "disconnected · /join @chatgpt"}
	if state == nil {
		return result
	}
	if !state.ExpiresAt.IsZero() && !now.Before(state.ExpiresAt) {
		result.Detail = "access expired · /join @chatgpt"
		return result
	}
	if !state.Enabled {
		return result
	}
	result = participantActivity{Phase: phaseWaiting, Detail: "waiting for ChatGPT to join"}
	connected := state.Connected && (state.LeaseUntil.IsZero() || now.Before(state.LeaseUntil))
	if connected {
		result.Phase, result.Detail = phaseIdle, "connected · ChatGPT website"
	}
	if state.Paused {
		result.Phase, result.Detail = phaseBlocked, "paused · /chatgpt resume"
	}
	switch transport.State {
	case tunnel.Starting:
		// A successful join is direct evidence that ChatGPT reached the room.
		// The independent periodic health check may still be catching up.
		if connected || state.Paused {
			result.Detail += " · tunnel health pending"
		} else {
			result.Detail = "connecting · " + transport.Detail
		}
	case tunnel.Recovering, tunnel.Failed:
		result.Phase, result.Detail = phaseWaiting, "reconnecting · "+transport.Detail
		if transport.State == tunnel.Failed {
			result.Phase, result.Detail = phaseError, "tunnel error · "+transport.Detail
		}
		// Keep transport trouble visible: an unexpired lease can outlive a lost
		// tunnel. Retain the participation state without claiming it is live.
		if state.Paused {
			result.Detail = "paused · " + result.Detail
		} else if connected {
			result.Detail = "joined · " + result.Detail
		}
	case tunnel.Ready:
		if !connected && !state.Paused {
			result.Detail = "tunnel ready · waiting for ChatGPT to join"
		}
	}
	if state.Limits.Exchanges > 0 {
		result.Detail += fmt.Sprintf(" · %d/%d exchanges", state.ExchangesRemaining, state.Limits.Exchanges)
	}
	if !state.Paused && (state.PauseReason == "exchange_limit" || state.PauseReason == "no_progress") {
		if result.Phase != phaseError {
			result.Phase = phaseBlocked
		}
		result.Detail = chatGPTPauseDescription(state.PauseReason) + " · " + result.Detail
	}
	return result
}

func (m Model) chatGPTActivityLine() string {
	activity := m.chatGPTActivity()
	if m.coordinationSummary != "" {
		activity.Detail = m.coordinationSummary + " · " + activity.Detail
	}
	icon, style := "○", dimStyle
	if activity.Phase == phaseWaiting {
		icon, style = activitySpinner[m.spinnerFrame%len(activitySpinner)], waitStyle
	}
	if activity.Phase == phaseError {
		icon, style = "!", errorStyle
	}
	if activity.Phase == phaseBlocked {
		icon, style = "Ⅱ", waitStyle
	}
	if activity.Phase == phaseIdle {
		icon, style = "●", busyStyle
	}
	return style.Render(icon) + " " + m.participantLabel(chat.ChatGPT, 7) + " " + style.Render(truncateActivityDetail(activity.Detail, max(20, m.width-12)))
}

func (m *Model) handleCoordination(fields []string) {
	if len(fields) != 1 {
		m.addNotice("usage: /chatgpt monitor start|status|stop|resume")
		return
	}
	if fields[0] == "status" {
		m.showCoordination()
		return
	}
	v, err := m.chatgpt.ControlCoordination(fields[0])
	if err != nil {
		m.addNotice(errorStyle.Render(err.Error()))
		return
	}
	m.addNotice(formatCoordination(v))
	m.coordinationNotice = ""
}
func formatCoordination(v *chat.CoordinationView) string {
	if v == nil {
		return "Coordination monitoring disabled"
	}
	notification, outcome := "unknown", "unknown"
	for _, e := range v.Events {
		if e.Kind == "notification_attempted" {
			notification = e.ID + " at " + monitorTime(e.At)
			outcome = "unknown"
		}
		if strings.HasPrefix(e.Kind, "host_") && strings.HasPrefix(notification, e.ID+" at ") {
			outcome = e.Kind + " (panel report)"
		}
	}
	return fmt.Sprintf("Monitor %s · %s · %s\nIdle %ds · no successful operation %ds · result %s · acknowledgement %s · assignment %s.\nNotification %s · website outcome %s. Completion counts are operations, not verified backlog items.", v.ID, v.State, v.Summary, v.IdleSeconds, v.NoSuccessSeconds, monitorTime(v.LastResultAt), monitorTime(v.AcknowledgedAt), monitorTime(v.LastAssignmentAt), notification, outcome)
}
func monitorTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Local().Format(time.RFC3339)
}
func (m *Model) showCoordination() {
	v, err := m.chatgpt.CoordinationStatus(time.Now().UTC())
	if err != nil {
		m.addNotice(errorStyle.Render(err.Error()))
		return
	}
	m.addNotice(formatCoordination(v))
}
func (m *Model) tickCoordination(now time.Time) {
	if m.chatgpt == nil || now.Sub(m.coordinationTick) < 5*time.Second {
		return
	}
	m.coordinationTick = now
	v, err := m.chatgpt.CoordinationStatus(now)
	if err != nil {
		if m.coordinationNotice != "error" {
			m.addNotice(errorStyle.Render("Coordination monitor: " + err.Error()))
			m.coordinationNotice = "error"
		}
		return
	}
	m.coordinationSummary = ""
	if v == nil {
		m.coordinationNotice = ""
		return
	}
	m.coordinationSummary = v.Summary
	key := ""
	if v.ActionNeeded || v.NoCompletion {
		key = fmt.Sprintf("%s:%t:%t:%s:%s", v.ID, v.ActionNeeded, v.NoCompletion, v.LastResultAt, v.LastAssignmentAt)
	}
	if key != "" && key != m.coordinationNotice {
		m.addNotice(formatCoordination(v))
	}
	m.coordinationNotice = key
}
