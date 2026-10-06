package auth

import (
	"testing"

	"github.com/google/uuid"
)

// A session principal may do anything its user may; an API key only
// what its scopes say.
func TestHasScope(t *testing.T) {
	var nilP *Principal
	if nilP.HasScope(ScopeChat) {
		t.Error("nil principal has scopes")
	}
	session := &Principal{UserID: uuid.New()}
	if !session.HasScope(ScopeChat) || !session.HasScope(ScopeMCP) {
		t.Error("session principal should carry every scope")
	}
	key := &Principal{UserID: uuid.New(), APIKeyID: uuid.NullUUID{UUID: uuid.New(), Valid: true}, Scopes: []string{ScopeChat}}
	if !key.HasScope(ScopeChat) || key.HasScope(ScopeMCP) {
		t.Errorf("chat-only key: chat=%v mcp=%v", key.HasScope(ScopeChat), key.HasScope(ScopeMCP))
	}
	key.Scopes = append(key.Scopes, ScopeMCP)
	if !key.HasScope(ScopeMCP) {
		t.Error("mcp scope not honoured")
	}
	key.Scopes = nil
	if key.HasScope(ScopeChat) {
		t.Error("a key with no scopes should have none")
	}
}
