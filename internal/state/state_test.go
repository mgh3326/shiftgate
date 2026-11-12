package state_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mgh3326/shiftgate/internal/state"
)

func TestLoadOldStateWithoutLastOpID(t *testing.T) {
	dir := t.TempDir()
	st := state.Store{Dir: dir}
	svcDir := filepath.Join(dir, "web")
	if err := os.MkdirAll(svcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldJSON := `{
  "schema_version": 1,
  "service": "web",
  "generation": 1,
  "rollback_available": false,
  "last_op": "deploy",
  "last_result": "ok"
}
`
	if err := os.WriteFile(st.Path("web"), []byte(oldJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := st.Load("web")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if s.LastOpID != "" {
		t.Fatalf("expected empty LastOpID, got %q", s.LastOpID)
	}
	if s.LastOp != "deploy" || s.LastResult != "ok" {
		t.Fatalf("unexpected state: %+v", s)
	}

	// Saving and reloading round-trips correctly.
	s.LastOpID = "op_123"
	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}
	reloaded, err := st.Load("web")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.LastOpID != "op_123" {
		t.Fatalf("expected op_123, got %q", reloaded.LastOpID)
	}

	// Marshalling with empty LastOpID omits the field.
	s.LastOpID = ""
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["last_op_id"]; ok {
		t.Fatalf("last_op_id should be omitted when empty, got %s", string(b))
	}
}
