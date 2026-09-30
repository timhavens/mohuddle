package chat

import (
	"testing"
	"time"
)

func TestNotificationKeyTracksCapacityWaitWithoutHeartbeatNoise(t *testing.T) {
	r := Room{Activities: map[Participant]ParticipantActivity{Codex: {State: SchedulerQueued, Transition: "manager_capacity", WaitReason: "Waiting for Room 78"}}}
	waiting := NotificationKey(r, nil)
	a := r.Activities[Codex]
	a.LastUpdateAt = time.Now()
	r.Activities[Codex] = a
	if waiting != NotificationKey(r, nil) {
		t.Fatal("heartbeat changed notification key")
	}
	a.State = SchedulerActive
	r.Activities[Codex] = a
	if waiting == NotificationKey(r, nil) {
		t.Fatal("resuming did not change notification key")
	}
}
