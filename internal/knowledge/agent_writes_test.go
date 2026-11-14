package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestCreateEntityTxIsAgentOnly (WIS-612): only the server agent identity may create entities
// inside a caller's transaction. A person, anything carrying a browser session, a non-UUID run
// subject or a missing transaction is refused before anything is read or written.
func TestCreateEntityTxIsAgentOnly(t *testing.T) {
	s := New(nil)
	sc := Scope{TenantID: uuid.NewString(), SiteID: uuid.NewString()}
	tx := &sql.Tx{} // never used: every case is refused before the transaction is touched
	run := uuid.NewString()
	for name, c := range map[string]struct {
		tx *sql.Tx
		a  Actor
	}{
		"human issuer":            {tx, Actor{Issuer: "https://idp.example/realms/plant", Subject: run}},
		"agent with a session":    {tx, Actor{Issuer: "supervisor:agent", Subject: run, SessionHash: "abc"}},
		"agent with a bad run id": {tx, Actor{Issuer: "supervisor:agent", Subject: "run-1"}},
		"no transaction":          {nil, Actor{Issuer: "supervisor:agent", Subject: run}},
	} {
		if _, err := s.CreateEntityTx(context.Background(), c.tx, c.a, sc, contractEntity(), "key", 0); !errors.Is(err, ErrDenied) {
			t.Errorf("%s: %v, want ErrDenied", name, err)
		}
	}
}
