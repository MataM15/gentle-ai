package sddstatus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeAttemptClassesReplayAndIsolateBudgets(t *testing.T) {
	for _, class := range []AttemptClass{AttemptClassEnvironment, AttemptClassHarness, AttemptClassAcceptance} {
		t.Run(string(class), func(t *testing.T) {
			repo := initRuntimeLedgerRepo(t)
			store := mustRuntimeStore(t, repo, "class-"+string(class))
			first := beginClass(t, store, "first", class)
			finishClass(t, store, first, "first", class)
			status, err := store.Status()
			if err != nil {
				t.Fatal(err)
			}
			if status.ClassAttempts[class] != 1 || !status.ClassTerminal[class].DecisionRequired {
				t.Fatalf("class state = %#v", status)
			}
			_, err = store.Begin(context.Background(), classRequest(status.Revision, "again", class))
			if !errors.Is(err, ErrRuntimeBudgetExhausted) {
				t.Fatalf("exhausted %s error = %v", class, err)
			}
			other := AttemptClassAcceptance
			if class == other {
				other = AttemptClassEnvironment
			}
			if _, err := store.Begin(context.Background(), classRequest(status.Revision, "other", other)); err != nil {
				t.Fatalf("%s exhausted %s: %v", class, other, err)
			}
		})
	}
}

// TestRuntimeAttemptClassAcceptancePassCompletesObjectiveForReset documents
// and pins one interpretation of an otherwise-unspecified question: which
// class, if any, should flip the objective-wide Complete/NextAction that
// runtimeObjectiveResetAdmissible and runtimeObjectiveAdvanceAdmissible key
// off. This test takes acceptance as that terminal gate (environment and
// harness precede it per the orchestrator docs) and asserts Reset then uses
// the Complete shortcut instead of falling back to the candidate-drift check
// — a fully-passed classified objective with zero further code changes must
// still be resettable. This is a judgment call pending sign-off from whoever
// specified attempt classes, not a confirmed requirement.
func TestRuntimeAttemptClassAcceptancePassCompletesObjectiveForReset(t *testing.T) {
	repo := initRuntimeLedgerRepo(t)
	store := mustRuntimeStore(t, repo, "class-acceptance-complete")
	revision := currentRuntimeRevision(t, store)
	nonTerminal := []AttemptClass{AttemptClassEnvironment, AttemptClassHarness}
	for _, class := range nonTerminal {
		begun, err := store.Begin(context.Background(), BeginAttemptRequest{
			ExpectedRevision: revision, RequestID: string(class) + "-begin", WorkUnit: "class-proof",
			EvidenceGoal: "prove acceptance alone completes the objective", MaxAttempts: 1, MaxChangedLines: 20, Class: class,
		})
		if err != nil {
			t.Fatalf("begin %s: %v", class, err)
		}
		finished, err := store.Finish(context.Background(), FinishAttemptRequest{
			ExpectedRevision: begun.Revision, RequestID: string(class) + "-finish", Outcome: AttemptPassed,
			EvidenceRevision: runtimeTestHash('a'), Diagnosis: "passed", HarnessDisposition: HarnessReused,
			CleanupEvidence: "cleanup complete", ProcessEvidence: "no descendants", Class: class,
		})
		if err != nil {
			t.Fatalf("finish %s: %v", class, err)
		}
		revision = finished.Revision
		// This is the assertion that actually discriminates the fix from
		// R2-001/R3-001: against the pre-fix unconditional assignment, the
		// very first (environment) pass here would already have flipped
		// Complete to true. Checking only after the full sequence (as an
		// earlier version of this test did) cannot tell the two apart.
		status, err := store.Status()
		if err != nil {
			t.Fatal(err)
		}
		if status.Complete || status.NextAction == RuntimeActionComplete {
			t.Fatalf("objective completed after %s alone = %#v", class, status)
		}
		if !status.ClassTerminal[class].Complete {
			t.Fatalf("%s class terminal not marked complete = %#v", class, status.ClassTerminal)
		}
	}
	begun, err := store.Begin(context.Background(), BeginAttemptRequest{
		ExpectedRevision: revision, RequestID: "acceptance-begin", WorkUnit: "class-proof",
		EvidenceGoal: "prove acceptance alone completes the objective", MaxAttempts: 1, MaxChangedLines: 20, Class: AttemptClassAcceptance,
	})
	if err != nil {
		t.Fatalf("begin acceptance: %v", err)
	}
	finished, err := store.Finish(context.Background(), FinishAttemptRequest{
		ExpectedRevision: begun.Revision, RequestID: "acceptance-finish", Outcome: AttemptPassed,
		EvidenceRevision: runtimeTestHash('a'), Diagnosis: "passed", HarnessDisposition: HarnessReused,
		CleanupEvidence: "cleanup complete", ProcessEvidence: "no descendants", Class: AttemptClassAcceptance,
	})
	if err != nil {
		t.Fatalf("finish acceptance: %v", err)
	}
	revision = finished.Revision
	status, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if !status.Complete || status.NextAction != RuntimeActionComplete {
		t.Fatalf("status after full classified pass = %#v", status)
	}
	if !status.ClassTerminal[AttemptClassEnvironment].Complete || !status.ClassTerminal[AttemptClassHarness].Complete ||
		!status.ClassTerminal[AttemptClassAcceptance].Complete {
		t.Fatalf("per-class terminal state = %#v", status.ClassTerminal)
	}
	reset, err := store.Reset(context.Background(), ResetObjectiveRequest{
		ExpectedRevision: revision, RequestID: "class-reset", Reason: "prove complete-objective reset shortcut", Actor: "test",
	})
	if err != nil {
		t.Fatalf("reset after complete classified objective: %v", err)
	}
	if reset.Objective != nil {
		t.Fatalf("reset left an objective open: %#v", reset.Objective)
	}
}

func TestRuntimeAttemptClassRejectsMismatchedFinishAndPreservesLegacy(t *testing.T) {
	repo := initRuntimeLedgerRepo(t)
	store := mustRuntimeStore(t, repo, "class-binding")
	legacy, err := store.Begin(context.Background(), classRequest("", "legacy", ""))
	if err != nil {
		t.Fatal(err)
	}
	if legacy.ActiveAttempt == nil || legacy.ActiveAttempt.Class != AttemptClassLegacy {
		t.Fatalf("legacy begin = %#v", legacy)
	}
	if _, err := store.Finish(context.Background(), finishRequest(legacy.Revision, "legacy", AttemptClassEnvironment)); err == nil {
		t.Fatal("mismatched class finish succeeded")
	}
	if _, err := store.Finish(context.Background(), finishRequest(legacy.Revision, "legacy", "")); err != nil {
		t.Fatal(err)
	}
	status, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.ClassAttempts[AttemptClassLegacy] != 1 {
		t.Fatalf("legacy replay = %#v", status)
	}
}

func TestRuntimeAttemptClassDigestAndChangedLineSafety(t *testing.T) {
	repo := initRuntimeLedgerRepo(t)
	store := mustRuntimeStore(t, repo, "class-integrity")
	started, err := store.Begin(context.Background(), BeginAttemptRequest{
		RequestID: "environment-begin", WorkUnit: "class-proof", EvidenceGoal: "preserve global line safety",
		MaxAttempts: 2, MaxChangedLines: 1, Class: AttemptClassEnvironment,
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.loadRecord(started.Revision)
	if err != nil {
		t.Fatal(err)
	}
	record.Begin.Class = AttemptClassHarness
	if err := validateRuntimeRecordShape(record); err == nil {
		t.Fatal("class-mutated begin record passed digest validation")
	}

	appendRuntimeLedgerFile(t, repo, "line\n")
	if _, err := store.Finish(context.Background(), FinishAttemptRequest{
		ExpectedRevision: started.Revision, RequestID: "environment-finish", Outcome: AttemptFailed,
		EvidenceRevision: runtimeTestHash('b'), Diagnosis: "global line charge", HarnessDisposition: HarnessReused,
		CleanupEvidence: "cleanup complete", ProcessEvidence: "no descendants", Class: AttemptClassEnvironment,
	}); err != nil {
		t.Fatal(err)
	}
	status, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Begin(context.Background(), BeginAttemptRequest{
		ExpectedRevision: status.Revision, RequestID: "acceptance-begin", WorkUnit: "class-proof",
		EvidenceGoal: "preserve global line safety", MaxAttempts: 2, MaxChangedLines: 1, Class: AttemptClassAcceptance,
	}); !errors.Is(err, ErrRuntimeBudgetExhausted) {
		t.Fatalf("acceptance ignored global changed-line budget: %v", err)
	}
}

func beginClass(t *testing.T, store RuntimeStore, id string, class AttemptClass) RuntimeStatus {
	t.Helper()
	status, err := store.Begin(context.Background(), classRequest(currentRuntimeRevision(t, store), id, class))
	if err != nil {
		t.Fatal(err)
	}
	return status
}

func finishClass(t *testing.T, store RuntimeStore, status RuntimeStatus, id string, class AttemptClass) {
	t.Helper()
	if _, err := store.Finish(context.Background(), finishRequest(status.Revision, id, class)); err != nil {
		t.Fatal(err)
	}
}

// TestRuntimeAttemptClassBeginRefusesReopeningAPassedClass covers R4-002: a
// non-legacy class that already passed (ClassTerminal[class].Complete ==
// true) must refuse a same-class Begin even while ClassAttempts[class] is
// still under MaxAttempts, and must not block an independent, not-yet-
// terminal class on the same objective. Before the fix, only
// ClassTerminal[class].DecisionRequired was checked, so this exact case
// silently reopened the passed class.
func TestRuntimeAttemptClassBeginRefusesReopeningAPassedClass(t *testing.T) {
	repo := initRuntimeLedgerRepo(t)
	store := mustRuntimeStore(t, repo, "class-reopen-refused")
	started, err := store.Begin(context.Background(), BeginAttemptRequest{
		RequestID: "environment-begin", WorkUnit: "class-proof", EvidenceGoal: "prove reopen refusal",
		MaxAttempts: 2, MaxChangedLines: 20, Class: AttemptClassEnvironment,
	})
	if err != nil {
		t.Fatal(err)
	}
	finished, err := store.Finish(context.Background(), FinishAttemptRequest{
		ExpectedRevision: started.Revision, RequestID: "environment-finish", Outcome: AttemptPassed,
		EvidenceRevision: runtimeTestHash('a'), Diagnosis: "passed", HarnessDisposition: HarnessReused,
		CleanupEvidence: "cleanup complete", ProcessEvidence: "no descendants", Class: AttemptClassEnvironment,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Begin(context.Background(), BeginAttemptRequest{
		ExpectedRevision: finished.Revision, RequestID: "environment-reopen", WorkUnit: "class-proof",
		EvidenceGoal: "prove reopen refusal", MaxAttempts: 2, MaxChangedLines: 20, Class: AttemptClassEnvironment,
	}); !errors.Is(err, ErrRuntimeObjectiveDone) {
		t.Fatalf("reopening a passed class error = %v, want ErrRuntimeObjectiveDone", err)
	}
	if _, err := store.Begin(context.Background(), BeginAttemptRequest{
		ExpectedRevision: finished.Revision, RequestID: "harness-begin", WorkUnit: "class-proof",
		EvidenceGoal: "prove reopen refusal", MaxAttempts: 2, MaxChangedLines: 20, Class: AttemptClassHarness,
	}); err != nil {
		t.Fatalf("independent not-yet-terminal class refused: %v", err)
	}
}

// TestRuntimeAttemptClassReplayRejectsForgedReopenOfPassedClass is the
// forged-ledger counterpart to
// TestRuntimeAttemptClassBeginRefusesReopeningAPassedClass: that test only
// proves the live Begin() guard refuses reopening a passed class. This one
// bypasses Begin() entirely and calls store.mutate directly to inject a
// syntactically valid "begin again" record for the same already-passed
// class the way a pre-R4-002 client or a tampered chain could have produced
// it, then requires the immediate post-commit replay (applyRuntimeBeginEvent)
// to reject it with the same ErrRuntimeObjectiveDone sentinel Begin uses.
// Scope is strictly this one regression; it takes no position on which
// class should globally complete the objective.
func TestRuntimeAttemptClassReplayRejectsForgedReopenOfPassedClass(t *testing.T) {
	repo := initRuntimeLedgerRepo(t)
	store := mustRuntimeStore(t, repo, "class-replay-reopen-refused")
	started, err := store.Begin(context.Background(), BeginAttemptRequest{
		RequestID: "environment-begin", WorkUnit: "class-proof", EvidenceGoal: "prove replay rejects a forged reopen",
		MaxAttempts: 2, MaxChangedLines: 20, Class: AttemptClassEnvironment,
	})
	if err != nil {
		t.Fatal(err)
	}
	finished, err := store.Finish(context.Background(), FinishAttemptRequest{
		ExpectedRevision: started.Revision, RequestID: "environment-finish", Outcome: AttemptPassed,
		EvidenceRevision: runtimeTestHash('a'), Diagnosis: "passed", HarnessDisposition: HarnessReused,
		CleanupEvidence: "cleanup complete", ProcessEvidence: "no descendants", Class: AttemptClassEnvironment,
	})
	if err != nil {
		t.Fatal(err)
	}

	forgedRequest := BeginAttemptRequest{
		ExpectedRevision: finished.Revision, RequestID: "environment-forged-reopen", WorkUnit: "class-proof",
		EvidenceGoal: "prove replay rejects a forged reopen", MaxAttempts: 2, MaxChangedLines: 20, Class: AttemptClassEnvironment,
	}
	digest := runtimeValueHash("gentle-ai.sdd-runtime-begin-request/v1", forgedRequest)
	_, err = store.mutate(context.Background(), finished.Revision, forgedRequest.RequestID, digest, func(replay runtimeReplay) (runtimeRecord, error) {
		// Reconstructs exactly the fields a legitimate continuing begin would
		// carry (same objective, same terminal candidate, next ordinal) —
		// the only thing "forged" here is that Begin's own guard, which would
		// normally refuse this, is never called.
		objective := replay.Status.Objective
		last := replay.Status.Attempts[len(replay.Status.Attempts)-1]
		return runtimeRecord{Operation: runtimeOperationBegin, Begin: &runtimeBeginEvent{
			ObjectiveID: objective.ID, WorkUnit: objective.WorkUnit, EvidenceGoal: objective.EvidenceGoal,
			Class: AttemptClassEnvironment, MaxAttempts: objective.MaxAttempts, MaxChangedLines: objective.MaxChangedLines,
			Ordinal: replay.Status.NextOrdinal, BeginCandidateIdentity: last.FinishCandidateIdentity,
			BeginCandidateTree: last.FinishCandidateTree, BeginWorktree: last.BeginWorktree, EffectiveWorktree: last.EffectiveWorktree,
		}}, nil
	})
	if !errors.Is(err, ErrRuntimeObjectiveDone) {
		t.Fatalf("forged reopen of a passed class replayed as = %v, want ErrRuntimeObjectiveDone", err)
	}

	forgedRevision, exists, err := readRuntimeHead(filepath.Join(store.Dir, "HEAD"))
	if err != nil || !exists {
		t.Fatalf("forged replay did not publish HEAD: revision=%q exists=%t err=%v", forgedRevision, exists, err)
	}
	forgedPath := filepath.Join(store.Dir, "records", strings.TrimPrefix(forgedRevision, "sha256:")+".json")
	if _, err := os.Stat(forgedPath); err != nil {
		t.Fatalf("forged replay record was not persisted in common-dir ledger: %v", err)
	}
	forgedRecord, err := store.loadRecord(forgedRevision)
	if err != nil || forgedRecord.Operation != runtimeOperationBegin || forgedRecord.RequestID != forgedRequest.RequestID {
		t.Fatalf("forged replay HEAD = %q record = %#v err=%v", forgedRevision, forgedRecord, err)
	}

	fresh := mustRuntimeStore(t, repo, "class-replay-reopen-refused")
	if fresh.commonDir != store.commonDir {
		t.Fatalf("fresh store common dir = %q, want %q", fresh.commonDir, store.commonDir)
	}
	if _, err := fresh.Status(); !errors.Is(err, ErrRuntimeObjectiveDone) {
		t.Fatalf("fresh store replayed forged reopen as = %v, want ErrRuntimeObjectiveDone", err)
	}
}

func classRequest(revision, id string, class AttemptClass) BeginAttemptRequest {
	return BeginAttemptRequest{ExpectedRevision: revision, RequestID: id + "-begin", WorkUnit: "class-proof", EvidenceGoal: "prove class accounting", MaxAttempts: 1, MaxChangedLines: 20, Class: class}
}

func finishRequest(revision, id string, class AttemptClass) FinishAttemptRequest {
	return FinishAttemptRequest{ExpectedRevision: revision, RequestID: id + "-finish", Outcome: AttemptFailed, EvidenceRevision: runtimeTestHash('a'), Diagnosis: "bounded class failure", HarnessDisposition: HarnessReused, CleanupEvidence: "cleanup complete", ProcessEvidence: "no descendants", Class: class}
}

func currentRuntimeRevision(t *testing.T, store RuntimeStore) string {
	t.Helper()
	status, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	return status.Revision
}
