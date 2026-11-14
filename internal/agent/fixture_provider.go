package agent

import (
	"context"
	"sync"
)

// FixtureStep is one scripted turn: a response, or a failure returned as-is.
type FixtureStep struct {
	Response  ModelResponse
	Err       error
	OnRequest func(ModelRequest)
}

// FixtureProvider is the deterministic-fixture adapter: it replays a script in order and
// makes no network call. It is visibly labelled, records provider_actual
// "deterministic-fixture" on every response, reports Actual() == false so it can never
// satisfy an actual-model gate (rule 09), and config refuses it in production.
type FixtureProvider struct {
	mu     sync.Mutex
	steps  []FixtureStep
	script map[string][]fixtureScriptStep
	calls  int
}

func NewFixtureProvider(steps ...FixtureStep) *FixtureProvider {
	return &FixtureProvider{steps: steps}
}

func (f *FixtureProvider) Name() string { return ProviderFixture }
func (f *FixtureProvider) Actual() bool { return false }

// Calls is the number of Complete calls that reached the script.
func (f *FixtureProvider) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *FixtureProvider) Complete(ctx context.Context, r ModelRequest) (ModelResponse, error) {
	if err := r.Validate(); err != nil {
		return ModelResponse{}, &ProviderError{Code: CodeSchemaInvalid}
	}
	if ctx.Err() != nil {
		return ModelResponse{}, &ProviderError{Code: CodeProviderCanceled}
	}
	var resp ModelResponse
	if f.script == nil {
		f.mu.Lock()
		if f.calls >= len(f.steps) {
			f.mu.Unlock()
			return ModelResponse{}, &ProviderError{Code: CodeFixtureExhausted}
		}
		step := f.steps[f.calls]
		f.calls++
		f.mu.Unlock()
		if step.OnRequest != nil {
			step.OnRequest(r)
		}
		if step.Err != nil {
			return ModelResponse{}, step.Err
		}
		resp = step.Response
	} else {
		prompt, err := fixturePromptFromRequest(r)
		if err != nil {
			return ModelResponse{}, &ProviderError{Code: CodeSchemaInvalid}
		}
		// The position is the number of model responses already in this conversation, so an
		// activity retry that rebuilds the conversation from its first request replays step 1.
		position := 0
		for _, m := range r.Messages {
			if m.Role == "assistant" {
				position++
			}
		}
		steps := f.script[fixturePurpose(r.PromptVersion)]
		if position >= len(steps) {
			return ModelResponse{}, &ProviderError{Code: CodeFixtureExhausted}
		}
		step := steps[position]
		f.mu.Lock()
		f.calls++
		f.mu.Unlock()
		resp, err = step.response(r, prompt)
		if err != nil {
			return ModelResponse{}, &ProviderError{Code: CodeSchemaInvalid}
		}
	}
	resp.SchemaVersion, resp.ProviderActual = ModelSchemaVersion, ProviderFixture
	if err := checkToolCalls(r, resp.ToolCalls); err != nil {
		return ModelResponse{}, err
	}
	return resp, nil
}
