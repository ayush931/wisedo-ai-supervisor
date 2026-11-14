package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestChatErrorClassifiesOnlyReceiptUniqueViolationAsConflict(t *testing.T) {
	for _, tc := range []struct {
		name, table, constraint string
		status                  int
		code                    string
	}{
		{"receipt idempotency", "chat_receipts", "chat_receipts_pkey", 409, "IDEMPOTENCY_CONFLICT"},
		{"other unique violation", "chat_messages", "chat_messages_tenant_id_site_id_conversation_id_request_id_key", 503, "STORAGE_UNAVAILABLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			chatError(w, &pgconn.PgError{Code: "23505", SchemaName: "supervisor", TableName: tc.table, ConstraintName: tc.constraint})
			var body struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.status || body.Code != tc.code {
				t.Fatalf("got HTTP %d code %q, want HTTP %d code %q", w.Code, body.Code, tc.status, tc.code)
			}
		})
	}
}
