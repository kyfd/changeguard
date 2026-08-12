package experiment

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/liufengxi/dbguard/internal/model"
)

func TestValidateExperimentSQLRejectsTransactionEscape(t *testing.T) {
	cases := []string{
		"COMMIT; DROP TABLE orders;",
		"-- comment before command\nROLLBACK;",
		"/* comment */ START TRANSACTION;",
		"SAVEPOINT bypass;",
	}
	for _, input := range cases {
		if err := validateExperimentSQL(input, "SELECT 1;"); err == nil {
			t.Fatalf("expected transaction control to be rejected: %q", input)
		}
	}
}

func TestValidateExperimentSQLRejectsPSQLMetaCommands(t *testing.T) {
	for _, input := range []string{"\\! id", "SELECT 1; \\gexec", "\\copy orders TO PROGRAM 'id'"} {
		if err := validateExperimentSQL(input, "SELECT 1;"); err == nil {
			t.Fatalf("expected psql meta command to be rejected: %q", input)
		}
	}
}

func TestValidateExperimentSQLAllowsSafeQuotedText(t *testing.T) {
	input := "INSERT INTO orders(note) VALUES ('COMMIT; C:\\\\temp'); -- \\! only a comment"
	if err := validateExperimentSQL(input, "DELETE FROM orders WHERE note = 'COMMIT; C:\\\\temp';"); err != nil {
		t.Fatalf("safe quoted text should be allowed: %v", err)
	}
}

func TestUnsafeExperimentReturnsFailedReport(t *testing.T) {
	runner := &HybridRunner{mode: "simulated"}
	report := runner.Run(context.Background(), modelChange("COMMIT;", "ROLLBACK;"))
	if report.Status != "FAILED" || !strings.Contains(report.ExecutionError, "事务控制语句") {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func modelChange(sql, rollback string) model.ChangeRequest {
	return model.ChangeRequest{ID: "chg_security", SQL: sql, RollbackSQL: rollback}
}
func TestNormalizeShadowSQLKeepsProductionConcurrentIntentOutsideTransaction(t *testing.T) {
	input := "CREATE UNIQUE INDEX CONCURRENTLY ux_orders_request_id ON orders(request_id); DROP INDEX CONCURRENTLY ux_old;"
	output, changed := normalizeShadowSQL(input)
	if !changed {
		t.Fatal("concurrent index SQL should be normalized for shadow transaction")
	}
	if strings.Contains(strings.ToUpper(output), "CONCURRENTLY") {
		t.Fatalf("shadow SQL must not contain CONCURRENTLY: %s", output)
	}
	if !strings.Contains(strings.ToUpper(output), "CREATE UNIQUE INDEX") || !strings.Contains(strings.ToUpper(output), "DROP INDEX") {
		t.Fatalf("normalized DDL lost index intent: %s", output)
	}
}

func TestPostgresModeWithoutDSNFailsClosed(t *testing.T) {
	runner := &HybridRunner{mode: "postgres"}
	report := runner.Run(context.Background(), model.ChangeRequest{SQL: "SELECT 1;", RollbackSQL: "SELECT 1;"})
	if report.Status != "FAILED" || !strings.Contains(report.ExecutionError, "DBGUARD_SHADOW_DSN") {
		t.Fatalf("postgres mode must fail when DSN is missing: %+v", report)
	}
}

func TestExecutableStatementsIgnoreCommentsAndSemicolons(t *testing.T) {
	if statements := executableStatements("-- rollback note;\n/* no operation */; ;"); len(statements) != 0 {
		t.Fatalf("comment-only rollback must not count as verified: %#v", statements)
	}
	statements := executableStatements("-- explanation\nDROP INDEX IF EXISTS idx_orders;")
	if len(statements) != 1 || !strings.Contains(statements[0], "DROP INDEX") {
		t.Fatalf("expected one executable rollback statement, got %#v", statements)
	}
}

func TestSimulationIsExplicitlyNotRun(t *testing.T) {
	runner := &HybridRunner{mode: "demo_only"}
	report := runner.Run(context.Background(), modelChange("SELECT 1;", "SELECT 1;"))
	if report.Mode != "DEMO_ONLY" || report.Status != "NOT_RUN" || report.RollbackVerified {
		t.Fatalf("demo mode must not fabricate execution success: %+v", report)
	}
}

func TestClassifyShadowSQLErrorMapsTimeouts(t *testing.T) {
	cases := map[string]string{
		"ERROR: canceling statement due to lock timeout (SQLSTATE 55P03)":      "LOCK_TIMEOUT",
		"ERROR: canceling statement due to statement timeout (SQLSTATE 57014)": "STATEMENT_TIMEOUT",
		"ERROR: deadlock detected":                                   "DEADLOCK",
		"ERROR: could not serialize access due to concurrent update": "SERIALIZATION_FAILURE",
		"ERROR: relation does not exist":                             "SQL_EXEC_FAILED",
	}
	for input, want := range cases {
		got := classifyShadowSQLError(fmt.Errorf("%s", input))
		if !strings.Contains(got, want) {
			t.Fatalf("classify %q: got %q want prefix %q", input, got, want)
		}
	}
}
