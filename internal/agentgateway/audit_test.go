package agentgateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditLogRecentReturnsVerifiedSanitizedEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	log, err := OpenAuditLog(path, []byte(strings.Repeat("r", 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	records := []AuditRecord{
		{Operation: "agent-ask_started", ChangeID: "chg_1", PrincipalHash: "principal-private", RequestSHA256: "request-private", Outcome: "started"},
		{Operation: "agent-ask", ChangeID: "chg_1", PrincipalHash: "principal-private", RequestSHA256: "request-private", Outcome: "success", HTTPStatus: 200, TraceID: "tr_safe"},
		{Operation: "submit-check", ChangeID: "chg_2", PrincipalHash: "principal-private", RequestSHA256: "request-private", Outcome: "rejected", HTTPStatus: 403},
	}
	for _, record := range records {
		if _, err := log.Append(record); err != nil {
			t.Fatal(err)
		}
	}
	page, err := log.Recent(2, false)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Verified || page.Total != 3 || len(page.Events) != 2 {
		t.Fatalf("unexpected page: %+v", page)
	}
	if page.Events[0].Operation != "submit-check" || page.Events[1].TraceID != "tr_safe" {
		t.Fatalf("events must be newest-first and skip started records: %+v", page.Events)
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, privateField := range []string{"principal_hash", "request_sha256", "prev_hash", `"hash"`, "principal-private", "request-private"} {
		if strings.Contains(string(encoded), privateField) {
			t.Fatalf("sanitized page leaked %q: %s", privateField, encoded)
		}
	}
}

func TestAuditLogRecentDetectsPostOpenTampering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	log, err := OpenAuditLog(path, []byte(strings.Repeat("q", 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if _, err := log.Append(AuditRecord{Operation: "agent-ask", Outcome: "success", HTTPStatus: 200}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content = []byte(strings.Replace(string(content), `"outcome":"success"`, `"outcome":"forged"`, 1))
	if err := os.WriteFile(path, content, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Recent(20, false); err == nil {
		t.Fatal("recent audit query must reject post-open tampering")
	}
	if state := log.State(); state.Verified {
		t.Fatalf("tampering must degrade audit state: %+v", state)
	}
}

func TestAuditLogDetectsTampering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	key := []byte(strings.Repeat("k", 32))
	log, err := OpenAuditLog(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(AuditRecord{Operation: "agent-ask", ChangeID: "chg_1", Outcome: "success", HTTPStatus: 200}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(AuditRecord{Operation: "submit-check", ChangeID: "chg_2", Outcome: "success", HTTPStatus: 200}); err != nil {
		t.Fatal(err)
	}
	if state := log.State(); !state.Verified || state.Events != 2 || state.LastHashPrefix == "" {
		t.Fatalf("unexpected state: %+v", state)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content = []byte(strings.Replace(string(content), `"outcome":"success"`, `"outcome":"forged"`, 1))
	if err := os.WriteFile(path, content, 0o640); err != nil {
		t.Fatal(err)
	}
	if reopened, err := OpenAuditLog(path, key); err == nil {
		_ = reopened.Close()
		t.Fatal("tampered audit chain must not reopen")
	}
}

func TestAuditLogContinuesVerifiedChainAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	key := []byte(strings.Repeat("s", 32))
	first, err := OpenAuditLog(path, key)
	if err != nil {
		t.Fatal(err)
	}
	record, err := first.Append(AuditRecord{Operation: "agent-ask_started", Outcome: "started"})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := OpenAuditLog(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	next, err := second.Append(AuditRecord{Operation: "agent-ask", Outcome: "success", HTTPStatus: 200})
	if err != nil {
		t.Fatal(err)
	}
	if next.Sequence != 2 || next.PrevHash != record.Hash {
		t.Fatalf("chain did not continue: first=%+v next=%+v", record, next)
	}
}

func TestAuditLogMarksChainUnverifiedAfterWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	log, err := OpenAuditLog(path, []byte(strings.Repeat("f", 32)))
	if err != nil {
		t.Fatal(err)
	}
	if err := log.file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(AuditRecord{Operation: "agent-ask", Outcome: "success"}); err == nil {
		t.Fatal("append through a closed descriptor must fail")
	}
	if state := log.State(); state.Verified {
		t.Fatalf("write failure must degrade readiness: %+v", state)
	}
	log.file = nil
}
