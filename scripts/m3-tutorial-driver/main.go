// Command m3-tutorial-driver is the fail-closed exit gate for the League
// tutorial server slice.
//
// Staged mode audits the private pack and reports unfinished runtime proofs as
// PENDING. Strict mode also starts a fresh production-composition scenario.
// The scenario writes a report bound to a one-time run id and the pack id. A
// checked-in fixture or a report from an earlier run therefore cannot satisfy
// the final gate.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/SarnautCore/server/internal/pack"
	"github.com/SarnautCore/server/internal/quests"
	"github.com/SarnautCore/server/internal/session"
)

const (
	reportSchema       = "sarnaut.m3-tutorial-acceptance/v1"
	productionMode     = "production"
	defaultScriptCount = 21
	packEnvironment    = "SARNAUT_M3_INTEGRATION_PACK"
	reportEnvironment  = "SARNAUT_M3_ACCEPTANCE_REPORT"
	runIDEnvironment   = "SARNAUT_M3_ACCEPTANCE_RUN_ID"
)

type mode uint8

const (
	modeStaged mode = iota
	modeStrict
)

func (value mode) String() string {
	if value == modeStrict {
		return "strict"
	}
	return "staged"
}

func parseMode(value string) (mode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "staged", "report":
		return modeStaged, nil
	case "strict":
		return modeStrict, nil
	default:
		return modeStaged, fmt.Errorf("mode %q is invalid; use staged, report, or strict", value)
	}
}

type options struct {
	mode              mode
	packPath          string
	reportPath        string
	expectedScripts   int
	expectedWarriorID string
	timeout           time.Duration
	scenario          []string
}

type checkState uint8

const (
	statePass checkState = iota
	statePending
	stateFail
)

func (state checkState) String() string {
	switch state {
	case statePass:
		return "PASS"
	case statePending:
		return "PENDING"
	default:
		return "FAIL"
	}
}

type check struct {
	id     string
	state  checkState
	detail string
}

type acceptanceReport struct {
	Schema      string          `json:"schema"`
	RunID       string          `json:"run_id"`
	PackID      string          `json:"pack_id"`
	BuildID     string          `json:"build_id"`
	Composition string          `json:"composition"`
	StartedAt   time.Time       `json:"started_at"`
	FinishedAt  time.Time       `json:"finished_at"`
	Proofs      []reportedProof `json:"proofs"`
}

type reportedProof struct {
	ID       string `json:"id"`
	Outcome  string `json:"outcome"`
	Observed int64  `json:"observed"`
	Detail   string `json:"detail"`
}

type proofSpec struct {
	id    string
	exact *int64
	min   int64
}

func exact(value int64) *int64 { return &value }

// requiredProofs is deliberately boring and explicit. A production scenario
// must account for every exit criterion, and a new criterion changes this list
// in review instead of disappearing into an unstructured log.
var requiredProofs = []proofSpec{
	{id: "production-composition", exact: exact(1)},
	{id: "real-warrior-chargen", exact: exact(1)},
	{id: "warrior-authored-action", min: 1},
	{id: "tutorial-quest-progress", exact: exact(defaultScriptCount)},
	{id: "tutorial-kills", min: 1},
	{id: "tutorial-item-events", min: 1},
	{id: "tutorial-equip-events", min: 1},
	{id: "player-death", min: 1},
	{id: "player-respawn", min: 1},
	{id: "experience-awards", min: 1},
	{id: "level-changes", min: 1},
	{id: "terrain-cues", min: 1},
	{id: "device-cues", min: 1},
	{id: "path-cues", min: 1},
	{id: "restart-safe-deferred-impacts", min: 1},
	{id: "skipped-impacts", exact: exact(0)},
	{id: "skipped-handlers", exact: exact(0)},
	{id: "fallbacks", exact: exact(0)},
	{id: "clean-shutdown", exact: exact(1)},
}

type runner struct {
	out io.Writer
	err io.Writer
}

func main() {
	modeFlag := flag.String("mode", "strict", "staged/report audits available evidence; strict launches and requires a complete fresh scenario")
	packPath := flag.String("pack", os.Getenv(packEnvironment), "private compiled tutorial pack directory; defaults to "+packEnvironment)
	reportPath := flag.String("report", "", "existing staged report to inspect; a strict scenario writes to a fresh temporary report")
	expectedScripts := flag.Int("expect-scripts", defaultScriptCount, "exact number of scripted tutorial quests")
	warriorID := flag.String("warrior", "chargen.league.warrior", "exact enabled League Warrior chargen option id")
	timeout := flag.Duration("timeout", 5*time.Minute, "maximum production scenario duration")
	flag.Parse()

	selectedMode, err := parseMode(*modeFlag)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "m3-tutorial-driver: %v\n", err)
		os.Exit(2)
	}
	if *expectedScripts <= 0 {
		_, _ = fmt.Fprintln(os.Stderr, "m3-tutorial-driver: -expect-scripts must be positive")
		os.Exit(2)
	}
	if *timeout <= 0 {
		_, _ = fmt.Fprintln(os.Stderr, "m3-tutorial-driver: -timeout must be positive")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	application := runner{out: os.Stdout, err: os.Stderr}
	exitCode := application.run(ctx, options{
		mode: selectedMode, packPath: *packPath, reportPath: *reportPath,
		expectedScripts: *expectedScripts, expectedWarriorID: *warriorID,
		timeout: *timeout, scenario: flag.Args(),
	})
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

func (runner runner) run(ctx context.Context, options options) int {
	checks := make([]check, 0, len(requiredProofs)+8)
	content, packChecks := auditPack(options.packPath, options.expectedScripts, options.expectedWarriorID, options.mode)
	checks = append(checks, packChecks...)

	var expectedPackID string
	if content != nil {
		expectedPackID = content.ID()
	}

	reportPath := options.reportPath
	expectedRunID := ""
	if len(options.scenario) > 0 {
		if options.mode != modeStrict {
			checks = append(checks, check{id: "scenario", state: stateFail, detail: "a production scenario may only be launched in strict mode"})
		} else if content == nil {
			checks = append(checks, check{id: "scenario", state: stateFail, detail: "the production scenario was not started because the pack audit failed"})
		} else {
			var cleanup func()
			var err error
			reportPath, expectedRunID, cleanup, err = runner.runScenario(ctx, options, expectedPackID)
			if cleanup != nil {
				defer cleanup()
			}
			if err != nil {
				checks = append(checks, check{id: "scenario", state: stateFail, detail: err.Error()})
			} else {
				checks = append(checks, check{id: "scenario", state: statePass, detail: "production scenario exited zero and wrote a fresh report"})
			}
		}
	} else if options.mode == modeStrict {
		checks = append(checks, check{
			id: "scenario", state: stateFail,
			detail: "strict mode requires a production scenario command after --; an existing report cannot close the gate",
		})
	}

	if reportPath == "" {
		checks = append(checks, pendingProofChecks("no acceptance report was supplied or produced")...)
	} else {
		report, err := readReport(reportPath)
		if err != nil {
			checks = append(checks, check{id: "report", state: stateFail, detail: err.Error()})
			checks = append(checks, pendingProofChecks("the acceptance report could not be read")...)
		} else {
			checks = append(checks, validateReport(report, expectedPackID, expectedRunID)...)
		}
	}

	failed, pending := runner.printChecks(checks)
	_, _ = fmt.Fprintf(runner.out, "SUMMARY mode=%s pass=%d pending=%d fail=%d\n",
		options.mode, len(checks)-pending-failed, pending, failed)
	if failed > 0 || options.mode == modeStrict && pending > 0 {
		return 1
	}
	return 0
}

func auditPack(path string, expectedScripts int, warriorID string, selectedMode mode) (*pack.Pack, []check) {
	if strings.TrimSpace(path) == "" {
		state := statePending
		if selectedMode == modeStrict {
			state = stateFail
		}
		return nil, []check{{id: "pack", state: state, detail: "no private pack path; set -pack or " + packEnvironment}}
	}
	content, err := pack.Load(path, pack.Options{})
	if err != nil {
		return nil, []check{{id: "pack", state: stateFail, detail: err.Error()}}
	}
	checks := []check{{id: "pack", state: statePass, detail: "loaded pack " + content.ID()}}

	scriptIDs := content.QuestScriptIDs()
	if len(scriptIDs) != expectedScripts {
		checks = append(checks, check{id: "script-rows", state: stateFail,
			detail: fmt.Sprintf("found %d scripted quests, want exactly %d", len(scriptIDs), expectedScripts)})
	} else {
		checks = append(checks, check{id: "script-rows", state: statePass,
			detail: fmt.Sprintf("all %d scripted quest rows are present", len(scriptIDs))})
	}
	for _, questID := range scriptIDs {
		row, ok := content.QuestScript(questID)
		if !ok || len(row.StartImpacts)+len(row.TriggerAgents)+len(row.Counters) == 0 {
			checks = append(checks, check{id: "script-row:" + questID, state: stateFail,
				detail: "script row is absent or has no activation, trigger, or counter content"})
		}
	}
	if err := content.ValidateQuestScriptCoverage(); err != nil {
		checks = append(checks, check{id: "script-coverage", state: stateFail, detail: err.Error()})
	} else {
		checks = append(checks, check{id: "script-coverage", state: statePass,
			detail: "every count-special objective has one stable compiled binding"})
	}

	catalog, err := quests.CatalogFromPack(content, quests.CatalogOptions{AllowCountSpecial: true})
	if err != nil {
		checks = append(checks, check{id: "quest-catalog", state: stateFail, detail: err.Error()})
	} else if skipped := catalog.SkippedUnsupportedQuests(); len(skipped) != 0 {
		checks = append(checks, check{id: "quest-catalog", state: stateFail,
			detail: fmt.Sprintf("catalog skipped %d quest(s): %v", len(skipped), skipped)})
	} else if catalog.Count() != len(content.QuestIDs()) {
		checks = append(checks, check{id: "quest-catalog", state: stateFail,
			detail: fmt.Sprintf("catalog admitted %d of %d pack quests", catalog.Count(), len(content.QuestIDs()))})
	} else {
		checks = append(checks, check{id: "quest-catalog", state: statePass,
			detail: fmt.Sprintf("admitted all %d quests with zero skips", catalog.Count())})
	}

	source := session.NewPackQuestScriptSource(content)
	if !source.HasDestinationIndex() {
		checks = append(checks, check{id: "map-locators", state: stateFail, detail: "pack has no strict map-locator index"})
	} else {
		checks = append(checks, check{id: "map-locators", state: statePass, detail: "strict map-locator index is present"})
	}

	warriorFound := false
	for _, option := range content.ChargenOptions() {
		if option.ID == warriorID && option.Enabled {
			warriorFound = true
			break
		}
	}
	if warriorFound {
		checks = append(checks, check{id: "warrior-pack-content", state: statePass,
			detail: "enabled chargen option " + warriorID + " is present"})
	} else {
		state := statePending
		if selectedMode == modeStrict {
			state = stateFail
		}
		checks = append(checks, check{id: "warrior-pack-content", state: state,
			detail: "enabled chargen option " + warriorID + " is not present"})
	}
	return content, checks
}

func (runner runner) runScenario(
	ctx context.Context,
	options options,
	packID string,
) (reportPath string, runID string, cleanup func(), err error) {
	if len(options.scenario) == 0 || strings.TrimSpace(options.scenario[0]) == "" {
		return "", "", nil, errors.New("scenario command is empty")
	}
	if options.reportPath != "" {
		return "", "", nil, errors.New("strict scenario output uses a fresh report; remove -report")
	}
	directory, err := os.MkdirTemp("", "sarnaut-m3-acceptance-")
	if err != nil {
		return "", "", nil, fmt.Errorf("create report directory: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(directory) }
	reportPath = filepath.Join(directory, "report.json")
	runID, err = newRunID()
	if err != nil {
		cleanup()
		return "", "", nil, err
	}

	command := exec.CommandContext(ctx, options.scenario[0], options.scenario[1:]...)
	command.Stdout = runner.out
	command.Stderr = runner.err
	command.Env = append(os.Environ(),
		packEnvironment+"="+options.packPath,
		reportEnvironment+"="+reportPath,
		runIDEnvironment+"="+runID,
		"SARNAUT_M3_ACCEPTANCE_PACK_ID="+packID,
	)
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return reportPath, runID, cleanup, fmt.Errorf("production scenario did not finish before %s: %w", options.timeout, ctx.Err())
		}
		return reportPath, runID, cleanup, fmt.Errorf("production scenario failed: %w", err)
	}
	if _, err := os.Stat(reportPath); err != nil {
		return reportPath, runID, cleanup, fmt.Errorf("production scenario wrote no report: %w", err)
	}
	return reportPath, runID, cleanup, nil
}

func newRunID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("create acceptance run id: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func readReport(path string) (acceptanceReport, error) {
	file, err := os.Open(path)
	if err != nil {
		return acceptanceReport{}, fmt.Errorf("open acceptance report: %w", err)
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var report acceptanceReport
	if err := decoder.Decode(&report); err != nil {
		return acceptanceReport{}, fmt.Errorf("decode acceptance report: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return acceptanceReport{}, errors.New("decode acceptance report: trailing JSON value")
		}
		return acceptanceReport{}, fmt.Errorf("decode acceptance report trailer: %w", err)
	}
	return report, nil
}

func validateReport(report acceptanceReport, packID, runID string) []check {
	checks := make([]check, 0, len(requiredProofs)+1)
	metadataProblems := make([]string, 0, 8)
	if report.Schema != reportSchema {
		metadataProblems = append(metadataProblems, fmt.Sprintf("schema=%q want %q", report.Schema, reportSchema))
	}
	if packID == "" || report.PackID != packID {
		metadataProblems = append(metadataProblems, fmt.Sprintf("pack_id=%q want %q", report.PackID, packID))
	}
	if runID != "" && report.RunID != runID {
		metadataProblems = append(metadataProblems, "run_id does not match this strict invocation")
	}
	if strings.TrimSpace(report.BuildID) == "" {
		metadataProblems = append(metadataProblems, "build_id is empty")
	}
	if report.Composition != productionMode {
		metadataProblems = append(metadataProblems, fmt.Sprintf("composition=%q want %q", report.Composition, productionMode))
	}
	if report.StartedAt.IsZero() || report.FinishedAt.IsZero() || report.FinishedAt.Before(report.StartedAt) {
		metadataProblems = append(metadataProblems, "started_at/finished_at do not describe a completed run")
	}
	if len(metadataProblems) > 0 {
		checks = append(checks, check{id: "report", state: stateFail, detail: strings.Join(metadataProblems, "; ")})
	} else {
		checks = append(checks, check{id: "report", state: statePass,
			detail: fmt.Sprintf("fresh production report for build %s, duration %s", report.BuildID, report.FinishedAt.Sub(report.StartedAt))})
	}

	specs := make(map[string]proofSpec, len(requiredProofs))
	for _, spec := range requiredProofs {
		specs[spec.id] = spec
	}
	seen := make(map[string]bool, len(report.Proofs))
	for _, proof := range report.Proofs {
		spec, known := specs[proof.ID]
		if !known {
			checks = append(checks, check{id: "proof:" + proof.ID, state: stateFail, detail: "unknown proof id"})
			continue
		}
		if seen[proof.ID] {
			checks = append(checks, check{id: "proof:" + proof.ID, state: stateFail, detail: "duplicate proof"})
			continue
		}
		seen[proof.ID] = true
		checks = append(checks, validateProof(spec, proof))
	}
	for _, spec := range requiredProofs {
		if !seen[spec.id] {
			checks = append(checks, check{id: "proof:" + spec.id, state: statePending, detail: "report omitted required proof"})
		}
	}
	return checks
}

func validateProof(spec proofSpec, proof reportedProof) check {
	id := "proof:" + spec.id
	detail := strings.TrimSpace(proof.Detail)
	switch strings.ToLower(strings.TrimSpace(proof.Outcome)) {
	case "pending":
		if detail == "" {
			detail = "production dependency has not supplied this proof"
		}
		return check{id: id, state: statePending, detail: detail}
	case "fail", "failed":
		if detail == "" {
			detail = "production scenario reported failure"
		}
		return check{id: id, state: stateFail, detail: detail}
	case "pass", "passed":
		if detail == "" {
			return check{id: id, state: stateFail, detail: "passing proof has no observation detail"}
		}
	default:
		return check{id: id, state: stateFail, detail: fmt.Sprintf("outcome %q is invalid", proof.Outcome)}
	}
	if spec.exact != nil && proof.Observed != *spec.exact {
		return check{id: id, state: stateFail,
			detail: fmt.Sprintf("observed=%d want exactly %d; %s", proof.Observed, *spec.exact, detail)}
	}
	if spec.exact == nil && proof.Observed < spec.min {
		return check{id: id, state: stateFail,
			detail: fmt.Sprintf("observed=%d want at least %d; %s", proof.Observed, spec.min, detail)}
	}
	return check{id: id, state: statePass, detail: fmt.Sprintf("observed=%d; %s", proof.Observed, detail)}
}

func pendingProofChecks(reason string) []check {
	checks := make([]check, 0, len(requiredProofs))
	for _, spec := range requiredProofs {
		checks = append(checks, check{id: "proof:" + spec.id, state: statePending, detail: reason})
	}
	return checks
}

func (runner runner) printChecks(checks []check) (failed, pending int) {
	for _, result := range checks {
		_, _ = fmt.Fprintf(runner.out, "%s %-38s %s\n", result.state, result.id, result.detail)
		switch result.state {
		case stateFail:
			failed++
		case statePending:
			pending++
		}
	}
	return failed, pending
}
