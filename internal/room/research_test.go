package room

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timhavens/mohuddle/internal/agent"
	"github.com/timhavens/mohuddle/internal/chat"
	appsettings "github.com/timhavens/mohuddle/internal/settings"
	"github.com/timhavens/mohuddle/internal/store"
)

func newResearchTestOrchestrator(t *testing.T, worker *fakeAgent, researcher *fakeResearcher) *Orchestrator {
	t.Helper()
	stateRoot := t.TempDir()
	roomStore, err := store.New(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	roomState, err := roomStore.Create(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	roomState.Members = map[chat.Participant]bool{chat.Codex: true}
	roomState.StreamMode = chat.StreamHistory
	orchestrator, err := New(roomState, nil, roomStore, worker)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { orchestrator.Close() })
	orchestrator.ConfigureResearch(researcher)
	preferences, err := appsettings.Open(filepath.Join(stateRoot, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := preferences.SetWebSearchEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.Configure(preferences, nil); err != nil {
		t.Fatal(err)
	}
	return orchestrator
}

func TestHostResearchRoundLimitPublishesFinalResponse(t *testing.T) {
	const sourceURL = "https://example.com/documentation"
	const summary = "The research limit was reached. Supported findings: " + sourceURL + ". The remaining question is unverified."
	const retryText = "I will try another research round."
	researchRequests := []agent.ResearchRequest{{Type: "open", URL: sourceURL}}
	for _, tc := range []struct {
		name             string
		rounds           int
		requestsPerRound int
		cutoff           bool
		retrievalError   string
		final            agent.TurnResult
		fallback         bool
	}{
		{name: "continues past three rounds", rounds: 4, requestsPerRound: 1, final: agent.TurnResult{Text: "Fourth round answered the question.", Done: true}},
		{name: "finishes on tenth round", rounds: 10, requestsPerRound: 1, final: agent.TurnResult{Text: "Tenth round answered the question.", Done: true}},
		{name: "summarizes at limit and caps batch size", rounds: 10, requestsPerRound: 5, cutoff: true, final: agent.TurnResult{Text: summary}},
		{name: "repeated request becomes explicit fallback", rounds: 10, requestsPerRound: 1, cutoff: true, final: agent.TurnResult{Text: retryText, Research: researchRequests}, fallback: true},
		{name: "empty summary becomes explicit fallback", rounds: 10, requestsPerRound: 1, cutoff: true, final: agent.TurnResult{Text: " \n\t"}, fallback: true},
		{name: "retrieval failures count toward limit", rounds: 10, requestsPerRound: 1, cutoff: true, retrievalError: "429 rate limited", final: agent.TurnResult{Text: "The research limit was reached. Retrieval failed, so the question remains unanswered."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make([]agent.ResearchRequest, tc.requestsPerRound)
			for i := range requests {
				requests[i] = researchRequests[0]
			}
			finalCall := tc.rounds + 1
			if tc.cutoff {
				finalCall++
			}
			worker := &fakeAgent{participant: chat.Codex}
			worker.run = func(_ context.Context, call int, request agent.TurnRequest, emit func(agent.Event)) (agent.TurnResult, error) {
				if call > finalCall {
					return agent.TurnResult{}, fmt.Errorf("unexpected extra agent call %d", call)
				}
				if call > 1 {
					if tc.cutoff && call == finalCall {
						for _, fragment := range []string{"HOST WEB RESEARCH SUMMARY REQUIRED", "10 research rounds", "source URLs", "unanswered questions", "Do not request more research"} {
							if !strings.Contains(request.Prompt, fragment) {
								return agent.TurnResult{}, fmt.Errorf("summary prompt missing %q: %s", fragment, request.Prompt)
							}
						}
					} else if !strings.Contains(request.Prompt, "HOST-PROVIDED WEB RESEARCH RESULTS") || !strings.Contains(request.Prompt, sourceURL) || (tc.retrievalError != "" && !strings.Contains(request.Prompt, tc.retrievalError)) {
						return agent.TurnResult{}, fmt.Errorf("research results missing: %s", request.Prompt)
					}
					if len(request.Attachments) != 0 {
						return agent.TurnResult{}, errors.New("continuation replayed attachments")
					}
				}
				if call == finalCall {
					final := tc.final
					final.SessionID = "research-session"
					emit(agent.Event{Type: agent.EventDelta, Text: final.Text})
					return final, nil
				}
				emit(agent.Event{Type: agent.EventDelta, Text: retryText})
				return agent.TurnResult{Text: retryText, SessionID: "research-session", Research: requests}, nil
			}
			researcher := &fakeResearcher{results: []agent.ResearchResult{{Type: "open", URL: sourceURL, Content: "Official documentation.", Error: tc.retrievalError}}}
			orchestrator := newResearchTestOrchestrator(t, worker, researcher)
			if err := orchestrator.Post("@codex research the question"); err != nil {
				t.Fatal(err)
			}
			warnings, batches, resets := 0, 0, 0
			waitForRound(t, orchestrator.Events(), func(event Event) {
				if event.Type == EventError {
					t.Errorf("orchestrator error: %v", event.Err)
				}
				if event.Type == EventWarning && strings.Contains(event.Text, "web research limit") {
					warnings++
				}
				if event.AgentEvent != nil {
					if event.AgentEvent.Type == agent.EventReset {
						resets++
					}
					if event.AgentEvent.Type == agent.EventTool && strings.Contains(event.AgentEvent.Text, "host web research batch") {
						batches++
					}
				}
			})
			if researcher.callCount() != tc.rounds*min(tc.requestsPerRound, 4) || batches != tc.rounds || worker.callCount() != finalCall {
				t.Fatalf("broker requests=%d batches=%d agent calls=%d; expected rounds=%d agent calls=%d", researcher.callCount(), batches, worker.callCount(), tc.rounds, finalCall)
			}
			wantWarnings, wantResets := 0, tc.rounds
			if tc.cutoff {
				wantWarnings++
				wantResets++
			}
			if tc.fallback {
				wantResets++
			}
			if warnings != wantWarnings || resets != wantResets {
				t.Fatalf("warnings=%d resets=%d; want warnings=%d resets=%d", warnings, resets, wantWarnings, wantResets)
			}
			roomCopy, messages := orchestrator.Snapshot()
			if len(roomCopy.TurnHistory) != 1 || roomCopy.TurnHistory[0].State != chat.TurnRecordFinal || roomCopy.Sessions[chat.Codex].ID != "research-session" {
				t.Fatalf("final turn/session missing: history=%+v sessions=%+v", roomCopy.TurnHistory, roomCopy.Sessions)
			}
			finalSequence := roomCopy.TurnHistory[0].FinalSequence
			finalSeen := false
			for _, message := range messages {
				if strings.Contains(message.Text, retryText) {
					t.Fatalf("unfinished retry leaked into transcript: %+v", message)
				}
				if message.Sequence != finalSequence {
					continue
				}
				finalSeen = true
				if tc.fallback {
					if !strings.Contains(message.Text, "web research limit of 10 rounds") || !strings.Contains(message.Text, "response is incomplete") {
						t.Fatalf("missing explicit limit explanation: %s", message.Text)
					}
				} else if message.Text != tc.final.Text {
					t.Fatalf("final text=%q want %q", message.Text, tc.final.Text)
				}
			}
			if !finalSeen {
				t.Fatal("final response was not published")
			}
		})
	}
}

func TestHostResearchLimitPreservesErrorsAndCancellation(t *testing.T) {
	providerErr := errors.New("summary provider failed")
	for _, tc := range []struct {
		name        string
		cancelFirst bool
		cancelFinal bool
		runErr      error
		wantErr     error
	}{
		{name: "already canceled", cancelFirst: true, wantErr: context.Canceled},
		{name: "canceled during summary", cancelFinal: true, wantErr: context.Canceled},
		{name: "summary provider error", runErr: providerErr, wantErr: providerErr},
		{name: "summary deadline exceeded", runErr: context.DeadlineExceeded, wantErr: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pending := agent.TurnResult{Text: "I will retry.", Research: []agent.ResearchRequest{{Type: "search", Query: "documentation"}}}
			worker := &fakeAgent{participant: chat.Codex}
			worker.run = func(runCtx context.Context, call int, _ agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
				if runCtx != ctx {
					t.Fatal("research continuation replaced the turn context")
				}
				if call <= 10 {
					return pending, nil
				}
				if call != 11 {
					t.Fatalf("unexpected agent call %d", call)
				}
				if tc.cancelFinal {
					cancel()
				}
				return agent.TurnResult{Text: "late summary", Done: true}, tc.runErr
			}
			researcher := &fakeResearcher{}
			orchestrator := newResearchTestOrchestrator(t, worker, researcher)
			if tc.cancelFirst {
				cancel()
			}
			result, _, err := orchestrator.completeResearch(ctx, chat.Codex, worker, agent.TurnRequest{}, pending, func(agent.Event) {})
			if !errors.Is(err, tc.wantErr) || result.Text != "" || result.Done {
				t.Fatalf("result=%+v error=%v; want error=%v without a completed answer", result, err, tc.wantErr)
			}
			wantRequests, wantCalls := 10, 11
			if tc.cancelFirst {
				wantRequests, wantCalls = 0, 0
			}
			if researcher.callCount() != wantRequests || worker.callCount() != wantCalls {
				t.Fatalf("broker requests=%d agent calls=%d; want requests=%d calls=%d", researcher.callCount(), worker.callCount(), wantRequests, wantCalls)
			}
		})
	}
}

func TestHostResearchCooldownUsesAlternativesOrSummarizes(t *testing.T) {
	retryAt := time.Now().Add(time.Hour).UTC()
	limited := agent.ResearchResult{Type: "search", Error: "HTTP 429", ErrorCode: agent.ResearchRateLimited, StatusCode: 429, Host: "search.example.com", RetryAt: &retryAt}
	cooldown := limited
	cooldown.ErrorCode = agent.ResearchCooldown
	exhausted := limited
	exhausted.RetryExhausted = true
	for _, tc := range []struct {
		name    string
		results []agent.ResearchResult
		summary bool
	}{
		{"first rate limit permits alternate sources", []agent.ResearchResult{limited}, false},
		{"repeated cooldown ends research", []agent.ResearchResult{cooldown}, true},
		{"exhausted retries end research", []agent.ResearchResult{exhausted}, true},
		{"mixed batch keeps useful results", []agent.ResearchResult{cooldown, {Type: "open", URL: "https://docs.example.com", Content: "useful evidence"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker := &fakeAgent{participant: chat.Codex}
			worker.run = func(_ context.Context, call int, request agent.TurnRequest, _ func(agent.Event)) (agent.TurnResult, error) {
				if call == 1 {
					return agent.TurnResult{Research: []agent.ResearchRequest{{Type: "search", Query: "documentation"}}}, nil
				}
				if call != 2 {
					return agent.TurnResult{}, fmt.Errorf("unnecessary continuation %d", call)
				}
				for _, fragment := range []string{"retry_at", "search.example.com", "Use other available sources", "Do not sleep", "Failed retrieval is not evidence"} {
					if !strings.Contains(request.Prompt, fragment) {
						return agent.TurnResult{}, fmt.Errorf("rate limit guidance missing %q", fragment)
					}
				}
				if strings.Contains(request.Prompt, "HOST WEB RESEARCH SUMMARY REQUIRED") != tc.summary {
					return agent.TurnResult{}, fmt.Errorf("summary=%v prompt=%s", tc.summary, request.Prompt)
				}
				return agent.TurnResult{Text: "Some sources are rate-limited; the unanswered question remains unverified.", Done: true}, nil
			}
			researcher := &fakeResearcher{results: tc.results}
			orchestrator := newResearchTestOrchestrator(t, worker, researcher)
			if err := orchestrator.Post("@codex research the documentation"); err != nil {
				t.Fatal(err)
			}
			waitForRound(t, orchestrator.Events(), func(event Event) {
				if event.Type == EventError {
					t.Errorf("orchestrator error: %v", event.Err)
				}
			})
			if researcher.callCount() != 1 || worker.callCount() != 2 {
				t.Fatalf("broker calls=%d agent calls=%d", researcher.callCount(), worker.callCount())
			}
			roomCopy, _ := orchestrator.Snapshot()
			if len(roomCopy.TurnHistory) != 1 || roomCopy.TurnHistory[0].State != chat.TurnRecordFinal || roomCopy.TurnHistory[0].FinalSequence == 0 {
				t.Fatalf("no final answer: %+v", roomCopy.TurnHistory)
			}
		})
	}
}
