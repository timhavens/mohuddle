package room

import (
	"context"
	"strings"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
)

// Caller holds o.mu. Task mutability and human filesystem scope are distinct.
func (o *Orchestrator) turnAccessLocked(participant chat.Participant, spec turnSpec) (chat.AgentSettings, chat.TurnAccess) {
	configured := effectiveRoleSettings(participant, o.settings[participant])
	permission := configured.Permissions
	if spec.readOnly {
		configured.Permissions = chat.PermissionReadOnly
	} else if record, ok := o.room.Workflows[spec.workflowID]; ok && record.PermissionCeiling.Valid() && permissionRank(configured.Permissions) > permissionRank(record.PermissionCeiling) {
		configured.Permissions = record.PermissionCeiling
	}
	policy := chat.ResolveTurnAccess(permission, configured.Permissions == chat.PermissionReadOnly, spec.private || spec.noTools)
	if policy.ReadOnly {
		configured.Permissions = chat.PermissionReadOnly
	}
	return configured, policy
}

// A stale provider instruction must not turn an already-authorized read into a
// failed task. Correct it once, using the same context/deadline and permissions.
func continueAuthorizedRead(ctx context.Context, runner agent.Agent, request agent.TurnRequest, result agent.TurnResult, runErr error, emit func(agent.Event)) (agent.TurnResult, error) {
	if runErr != nil || ctx.Err() != nil || request.Access.NoTools || request.VoiceOnly || request.Access.ReadScope != chat.ReadScopeHost || result.AccessRequest == nil || result.AccessRequest.Mode != chat.AccessRead || strings.TrimSpace(result.AccessRequest.Path) == "" {
		return result, runErr
	}
	request.Prompt += "\n\nHOST PERMISSION CORRECTION: Your previous response requested filesystem read access that the human has already granted. You may read paths throughout this host, including outside the workspace. Continue the original task and provide its answer; do not request that read access again. All current read-only restrictions remain in effect."
	return runner.Run(ctx, request, emit)
}
