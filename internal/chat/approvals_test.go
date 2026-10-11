package chat

import (
	"encoding/json"
	"testing"
)

func TestApprovalNotesCarryTheReason(t *testing.T) {
	parts := []json.RawMessage{
		json.RawMessage(`{"type":"tool-submit_plan","toolCallId":"c1","state":"approval-responded","approval":{"id":"a1","approved":true,"reason":"1. one\n2. two"}}`),
		json.RawMessage(`{"type":"tool-web_fetch","toolCallId":"c2","state":"approval-responded","approval":{"id":"a2","approved":false}}`),
		json.RawMessage(`{"type":"text","text":"hi"}`),
	}
	notes := ApprovalNotes(parts)
	if len(notes) != 1 || notes["c1"] != "1. one\n2. two" {
		t.Errorf("notes = %v", notes)
	}
	dec := ApprovalDecisions(parts)
	if !dec["c1"] || dec["c2"] {
		t.Errorf("decisions = %v", dec)
	}
}
