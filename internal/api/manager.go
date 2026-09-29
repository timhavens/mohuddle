package api

import (
	"context"
	"time"
)

const ChatGPTManagerConnectionVersion = "mohuddle.chatgpt.manager.v1"

// Manager endpoints accept only a dedicated workspace grant, never local admin
// or federation credentials. Configure before starting their private listener.
type ChatGPTManager interface {
	Authenticate(HelloRequest) *Session
	Handle(context.Context, *Session, Request) HandleResult
}

func (s *Service) ConfigureManager(manager ChatGPTManager) { s.manager = manager }

func WriteChatGPTConnection(path string, value ChatGPTConnection) error {
	return writeChatGPTConnection(path, value)
}
func NewChatGPTToken() (string, error)                       { return randomToken() }
func ManagerSuccess(request Request, value any) HandleResult { return succeeded(request, value) }
func ManagerFailure(request Request, code, message string) HandleResult {
	return failed(request, code, message)
}

// Detach releases presence without cancelling already accepted operations.
// Used only by the trusted manager on an explicit room switch.
func (s *Service) DetachChatGPT(participation string) {
	s.chatgptMu.Lock()
	defer s.chatgptMu.Unlock()
	if s.chatgpt.participation != participation {
		return
	}
	s.chatgpt.participation, s.chatgpt.clientKey = "", ""
	s.chatgpt.lease = time.Time{}
	s.updateChatGPTStateLocked()
}

type ManagedRoomView struct {
	RoomID    string    `json:"room_id"`
	RoomName  string    `json:"room_name"`
	Objective string    `json:"objective,omitempty"`
	Status    string    `json:"status"`
	Available bool      `json:"available"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ManagedJoinRequest struct {
	ReplaceExisting bool   `json:"replace_existing,omitempty"`
	ClientKey       string `json:"client_key"`
	Room            string `json:"room,omitempty"`
	OperationID     string `json:"operation_id,omitempty"`
}
