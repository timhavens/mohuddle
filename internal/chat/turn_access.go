package chat

// TurnAccess preserves the human's filesystem authorization when a task adds
// a read-only ceiling. It contains no paths or credentials and is safe for UI.
type TurnAccess struct {
	Configured PermissionProfile `json:"configured"`
	ReadScope  string            `json:"read_scope"`
	ReadOnly   bool              `json:"read_only"`
	NoTools    bool              `json:"no_tools,omitempty"`
}

const (
	ReadScopeGranted = "granted"
	ReadScopeHost    = "host"
	ReadScopeNone    = "none"
)

func ResolveTurnAccess(configured PermissionProfile, readOnly, noTools bool) TurnAccess {
	if !configured.Valid() {
		configured = PermissionWorkspace
	}
	value := TurnAccess{Configured: configured, ReadScope: ReadScopeGranted, ReadOnly: readOnly || configured == PermissionReadOnly, NoTools: noTools}
	if configured == PermissionFull {
		value.ReadScope = ReadScopeHost
	}
	if noTools {
		value.ReadScope, value.ReadOnly = ReadScopeNone, true
	}
	return value
}

func (a TurnAccess) Label() string {
	if a.NoTools {
		return "transcript only"
	}
	if a.ReadScope == ReadScopeHost {
		if a.ReadOnly {
			return "full-machine reads · read-only task"
		}
		return "full-machine reads · writable task"
	}
	if a.ReadOnly {
		return "granted roots · read-only task"
	}
	return "granted roots · writable task"
}
