package chatgpt

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/timhavens/mohuddle/internal/api"
	roomguidance "github.com/timhavens/mohuddle/internal/chatgpt/skills/mohuddle-room"
)

type CreateRoomInput struct {
	JoinInput
	OperationID string `json:"operation_id" jsonschema:"Unique identifier for this creation. Reuse exactly for retries so only one room is created."`
}

func (b *Bridge) joinManaged(ctx context.Context, req *mcp.CallToolRequest, input JoinInput, operation string) (api.ChatGPTView, error) {
	var view api.ChatGPTView
	connection, err := b.activeConnection()
	if err != nil {
		return view, err
	}
	if connection.Version != api.ChatGPTManagerConnectionVersion {
		return view, fmt.Errorf("room creation requires the shared MoHuddle manager connection")
	}
	key := ""
	if req.Params.Meta != nil {
		if session, ok := req.Params.Meta["openai/session"].(string); ok && session != "" {
			key = session
		}
	}
	if key == "" {
		key = input.ConversationKey
		if len(key) < 16 || len(key) > 128 {
			return view, fmt.Errorf("provide a stable conversation_key of 16–128 characters for this conversation")
		}
	}
	// This stable hash is only a routing hint. The independently validated local
	// workspace grant and room participation capability supply authorization.
	hash := sha256.Sum256([]byte(key))
	method := "chatgpt.join"
	if operation != "" {
		method = "chatgpt.create_room"
	}
	err = b.Call(ctx, method, api.ManagedJoinRequest{ClientKey: fmt.Sprintf("%x", hash), Room: input.Room, OperationID: operation}, &view)
	view.InstructionVersion = roomguidance.Version
	view.Usage += "\n\n" + QuickGuide + "\n\n" + EffortGuide + "\n\n" + roomguidance.Brief
	return view, err
}
