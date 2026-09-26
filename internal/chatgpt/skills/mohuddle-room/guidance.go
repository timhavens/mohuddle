// Package roomguidance owns the versioned instructions delivered to the
// coordinator and participants. The skill and runtime read the same source.
package roomguidance

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"strings"
)

//go:embed SKILL.md
var Skill string

//go:embed references/coordination.md
var Coordination string

var Version = fmt.Sprintf("coordination-v1-%x", sha256.Sum256([]byte(Skill+Coordination)))[:28]
var Brief = section("Operating reminder")
var Participant = section("Participant handoff")

func section(name string) string {
	_, rest, _ := strings.Cut(Coordination, "## "+name+"\n")
	body, _, _ := strings.Cut(rest, "\n## ")
	return strings.TrimSpace(body)
}
