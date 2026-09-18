package agent

import (
	"github.com/timhavens/mohuddle/internal/access"
	"github.com/timhavens/mohuddle/internal/chat"
)

// EnforceTurnAccess keeps native provider options consistent with the host's
// resolved policy. Legacy callers without a policy retain their old settings.
func EnforceTurnAccess(request TurnRequest) TurnRequest {
	if !request.Access.Configured.Valid() {
		return request
	}
	if request.Access.ReadOnly {
		request.Settings.Permissions = chat.PermissionReadOnly
		request.WriteRoots = nil
	}
	if request.Access.ReadScope == chat.ReadScopeHost {
		request.ReadRoots = access.HostRoots(request.Workspace)
	}
	if request.Access.NoTools || request.NoTools || request.VoiceOnly {
		request.NoTools = true
		request.ReadRoots, request.WriteRoots = nil, nil
	}
	return request
}
