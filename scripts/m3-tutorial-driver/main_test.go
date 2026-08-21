package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SarnautCore/server/internal/pack"
)

func TestParseMode(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"staged", "report", "STRICT"} {
		if _, err := parseMode(name); err != nil {
			t.Errorf("parseMode(%q) error = %v", name, err)
		}
	}
	if _, err := parseMode("permissive"); err == nil {
		t.Fatal("parseMode(permissive) error = nil")
	}
}

func TestStagedModeReportsMissingRuntimeProofsWithoutHidingThem(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	exit := (runner{out: &output, err: &output}).run(t.Context(), options{
		mode: modeStaged, expectedScripts: defaultScriptCount,
		expectedWarriorID: "chargen.league.warrior",
	})
	if exit != 0 {
		t.Fatalf("exit = %d, output:\n%s", exit, output.String())
	}
	if !strings.Contains(output.String(), "PENDING pack") ||
		!strings.Contains(output.String(), "PENDING proof:clean-shutdown") {
		t.Fatalf("staged output omitted pending gates:\n%s", output.String())
	}
}

func TestStrictModeRequiresFreshProductionScenario(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	exit := (runner{out: &output, err: &output}).run(t.Context(), options{
		mode: modeStrict, expectedScripts: defaultScriptCount,
		expectedWarriorID: "chargen.league.warrior",
	})
	if exit == 0 {
		t.Fatalf("strict exit = 0, output:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "strict mode requires a production scenario command") {
		t.Fatalf("strict output omitted scenario refusal:\n%s", output.String())
	}
}

func TestReportIsBoundToPackAndStrictRun(t *testing.T) {
	t.Parallel()
	report := completeReport("pack-a", "run-a")
	checks := validateReport(report, "pack-b", "run-b")
	assertFailedCheckContains(t, checks, "report", "pack_id")
	assertFailedCheckContains(t, checks, "report", "run_id")
}

func TestCompleteReportPassesEveryProof(t *testing.T) {
	t.Parallel()
	checks := validateReport(completeReport("pack-a", "run-a"), "pack-a", "run-a")
	for _, result := range checks {
		if result.state != statePass {
			t.Errorf("check %s = %s: %s", result.id, result.state, result.detail)
		}
	}
}

func TestAnySkipHandlerOrFallbackFails(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"skipped-impacts", "skipped-handlers", "fallbacks"} {
		t.Run(id, func(t *testing.T) {
			report := completeReport("pack-a", "run-a")
			for index := range report.Proofs {
				if report.Proofs[index].ID == id {
					report.Proofs[index].Observed = 1
				}
			}
			checks := validateReport(report, "pack-a", "run-a")
			assertFailedCheckContains(t, checks, "proof:"+id, "want exactly 0")
		})
	}
}

func TestPendingAndMissingProofsStayVisible(t *testing.T) {
	t.Parallel()
	report := completeReport("pack-a", "run-a")
	report.Proofs[0].Outcome = "pending"
	report.Proofs = report.Proofs[:len(report.Proofs)-1]
	checks := validateReport(report, "pack-a", "run-a")
	var pending int
	for _, result := range checks {
		if result.state == statePending {
			pending++
		}
	}
	if pending != 2 {
		t.Fatalf("pending checks = %d, want 2: %#v", pending, checks)
	}
}

func TestDuplicateAndUnknownProofsFail(t *testing.T) {
	t.Parallel()
	report := completeReport("pack-a", "run-a")
	report.Proofs = append(report.Proofs, report.Proofs[0], reportedProof{
		ID: "invented-proof", Outcome: "pass", Observed: 1, Detail: "not in the contract",
	})
	checks := validateReport(report, "pack-a", "run-a")
	assertFailedCheckContains(t, checks, "proof:"+report.Proofs[0].ID, "duplicate")
	assertFailedCheckContains(t, checks, "proof:invented-proof", "unknown")
}

func TestReportParserRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	unknown := filepath.Join(directory, "unknown.json")
	if err := os.WriteFile(unknown, []byte(`{"schema":"x","surprise":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readReport(unknown); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("readReport(unknown) error = %v", err)
	}

	trailing := filepath.Join(directory, "trailing.json")
	if err := os.WriteFile(trailing, []byte(`{} {}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readReport(trailing); err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("readReport(trailing) error = %v", err)
	}
}

func TestScenarioGetsFreshRunAndPackBindings(t *testing.T) {
	t.Setenv("SARNAUT_M3_TEST_SCENARIO_HELPER", "1")
	var output bytes.Buffer
	application := runner{out: &output, err: &output}
	reportPath, runID, cleanup, err := application.runScenario(t.Context(), options{
		packPath: "private-pack", timeout: 30 * time.Second,
		scenario: []string{os.Args[0], "-test.run=TestM3ScenarioHelperProcess", "-test.count=1"},
	}, "pack-a")
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("runScenario() error = %v, output:\n%s", err, output.String())
	}
	if runID == "" {
		t.Fatal("runScenario() returned an empty run id")
	}
	report, err := readReport(reportPath)
	if err != nil {
		t.Fatalf("readReport() error = %v", err)
	}
	for _, result := range validateReport(report, "pack-a", runID) {
		if result.state != statePass {
			t.Errorf("scenario check %s = %s: %s", result.id, result.state, result.detail)
		}
	}
}

func TestM3ScenarioHelperProcess(t *testing.T) {
	if os.Getenv("SARNAUT_M3_TEST_SCENARIO_HELPER") != "1" {
		return
	}
	report := completeReport(
		os.Getenv("SARNAUT_M3_ACCEPTANCE_PACK_ID"),
		os.Getenv(runIDEnvironment),
	)
	payload, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(reportEnvironment), payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRealPrivatePackHasTwentyOneCompleteScriptRows(t *testing.T) {
	path := os.Getenv(packEnvironment)
	if path == "" {
		t.Skip("set " + packEnvironment + " to the private compiled M3 pack")
	}
	content, checks := auditPack(path, defaultScriptCount, "chargen.league.warrior", modeStaged)
	if content == nil {
		t.Fatalf("private pack did not load: %#v", checks)
	}
	for _, result := range checks {
		if result.state == stateFail {
			t.Errorf("private pack check %s failed: %s", result.id, result.detail)
		}
	}
	if got := len(content.QuestScriptIDs()); got != defaultScriptCount {
		t.Fatalf("script rows = %d, want %d", got, defaultScriptCount)
	}
	if err := content.ValidateQuestScriptCoverage(); err != nil {
		t.Fatalf("ValidateQuestScriptCoverage() error = %v", err)
	}
}

func TestFixturePackAuditParsesProductionPackReader(t *testing.T) {
	t.Parallel()
	content, err := pack.Load(filepath.Join("..", "..", "testdata", "packs", "demo"), pack.Options{})
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	_, checks := auditPack(content.Directory(), len(content.QuestScriptIDs()), "chargen.league.warrior", modeStaged)
	for _, result := range checks {
		if result.id == "pack" && result.state != statePass {
			t.Fatalf("fixture pack parse = %s: %s", result.state, result.detail)
		}
	}
}

func completeReport(packID, runID string) acceptanceReport {
	now := time.Date(2026, time.August, 22, 0, 0, 0, 0, time.UTC)
	report := acceptanceReport{
		Schema: reportSchema, RunID: runID, PackID: packID, BuildID: "test-build",
		Composition: productionMode, StartedAt: now, FinishedAt: now.Add(time.Minute),
	}
	for _, spec := range requiredProofs {
		observed := spec.min
		if spec.exact != nil {
			observed = *spec.exact
		}
		report.Proofs = append(report.Proofs, reportedProof{
			ID: spec.id, Outcome: "pass", Observed: observed, Detail: "test observation for " + spec.id,
		})
	}
	return report
}

func assertFailedCheckContains(t *testing.T, checks []check, id, fragment string) {
	t.Helper()
	for _, result := range checks {
		if result.id == id && result.state == stateFail && strings.Contains(result.detail, fragment) {
			return
		}
	}
	t.Fatalf("no failed %s check containing %q: %#v", id, fragment, checks)
}
