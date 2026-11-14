package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
	"github.com/google/uuid"
)

const (
	fixtureInterviewPurpose = "interview"
	fixtureDraftPurpose     = "configuration_draft"
	// fixtureInvestigationPurpose scripts the WIS-276 case investigation (WIS-635). Its proposal
	// is written in the script; placeholders only copy values the run itself was given (the brief)
	// or returned by its own tool calls, so the Judge validates output it did not help produce.
	fixtureInvestigationPurpose = "investigation"
	// fixtureChatPurpose scripts a WIS-517 chat answer (WIS-631 browser pilot). Like the others it
	// may copy only what its own tool calls returned; the chat validator still checks every
	// citation and number against the captured, approved passages.
	fixtureChatPurpose = "chat"
	// fixtureOCRPurpose scripts a WIS-625 page transcription (WIS-643 browser pilot). It may call no
	// tool, as the real extraction may not; checkOCR still validates the transcription it returns.
	fixtureOCRPurpose = "ocr"
	// maxFixtureOffset bounds a "now" placeholder's offset in either direction.
	maxFixtureOffset      = 30 * 24 * 3600
	fixtureScriptVersion  = 1
	maxFixtureScriptSteps = 8
	maxFixtureToolCalls   = 4
)

// Script rejection reasons, wrapped with the purpose/step position so a bad script names its defect.
var (
	errFixtureScriptTooLarge = errors.New("script exceeds the contract size limit")
	errFixtureScriptSteps    = fmt.Errorf("purpose needs 1..%d steps", maxFixtureScriptSteps)
	errFixtureStepShape      = errors.New("step must contain exactly one of tool_calls or content")
	errFixtureToolCall       = errors.New("tool call must be an allowed tool for the purpose with a unique id")
)

type fixtureScript struct {
	SchemaVersion int                    `json:"schema_version"`
	Purposes      []fixtureScriptPurpose `json:"purposes"`
}

type fixtureScriptPurpose struct {
	Purpose string              `json:"purpose"`
	Steps   []fixtureScriptStep `json:"steps"`
}

type fixtureScriptStep struct {
	ToolCalls []fixtureScriptToolCall `json:"tool_calls,omitempty"`
	Content   json.RawMessage         `json:"content,omitempty"`
}

type fixtureScriptToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// LoadFixtureProvider reads a small, closed script once at worker startup. The provider only
// resolves placeholders from the ModelRequest conversation; it has no database access.
func LoadFixtureProvider(path string) (*FixtureProvider, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("SUPERVISOR_AGENT_FIXTURE_SCRIPT %q is not readable", path)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxContractBytes+1))
	if err != nil {
		return nil, fmt.Errorf("SUPERVISOR_AGENT_FIXTURE_SCRIPT %q is not readable", path)
	}
	if len(b) > MaxContractBytes {
		return nil, fmt.Errorf("SUPERVISOR_AGENT_FIXTURE_SCRIPT %q: %w", path, errFixtureScriptTooLarge)
	}
	p, err := NewFixtureProviderFromScript(b)
	if err != nil {
		return nil, fmt.Errorf("SUPERVISOR_AGENT_FIXTURE_SCRIPT %q: %w", path, err)
	}
	return p, nil
}

// NewFixtureProviderFromScript strictly decodes the fixture-only model response script.
func NewFixtureProviderFromScript(data []byte) (*FixtureProvider, error) {
	var script fixtureScript
	if strictjson.Decode(data, &script, MaxContractBytes) != nil || script.SchemaVersion != fixtureScriptVersion || len(script.Purposes) == 0 {
		return nil, errors.New("invalid fixture script: not a strict schema_version 1 script with purposes")
	}
	byPurpose := make(map[string][]fixtureScriptStep, len(script.Purposes))
	for i, purpose := range script.Purposes {
		if fixtureTools[purpose.Purpose] == nil {
			return nil, fmt.Errorf("invalid fixture script: purpose %d is unknown", i)
		}
		if _, exists := byPurpose[purpose.Purpose]; exists {
			return nil, fmt.Errorf("invalid fixture script: purpose %q is repeated", purpose.Purpose)
		}
		if len(purpose.Steps) == 0 || len(purpose.Steps) > maxFixtureScriptSteps {
			return nil, fmt.Errorf("invalid fixture script: purpose %q: %w", purpose.Purpose, errFixtureScriptSteps)
		}
		for j, step := range purpose.Steps {
			if err := validateFixtureScriptStep(purpose.Purpose, step); err != nil {
				return nil, fmt.Errorf("invalid fixture script: purpose %q step %d: %w", purpose.Purpose, j, err)
			}
		}
		byPurpose[purpose.Purpose] = purpose.Steps
	}
	return &FixtureProvider{script: byPurpose}, nil
}

// fixtureTools are the tools a scripted step may call (and the only tools a tool_result placeholder
// may read): evidence.get for interviews and drafts; an investigation may also read its
// measurements and contexts, and chat its snapshot claims, as the real specialists do.
var fixtureTools = map[string]map[string]bool{
	fixtureInterviewPurpose:     {ToolEvidenceGet: true},
	fixtureDraftPurpose:         {ToolEvidenceGet: true},
	fixtureInvestigationPurpose: {ToolEvidenceGet: true, ToolMeasurementsGet: true, ToolContextGet: true},
	fixtureChatPurpose:          {ToolEvidenceGet: true, ToolClaimsSearch: true},
	fixtureOCRPurpose:           {},
}

func validateFixtureScriptStep(purpose string, step fixtureScriptStep) error {
	if (len(step.ToolCalls) > 0) == (len(step.Content) > 0) {
		return errFixtureStepShape
	}
	if len(step.ToolCalls) > maxFixtureToolCalls {
		return errors.New("too many fixture tool calls")
	}
	if len(step.Content) > 0 {
		var content map[string]any
		if json.Unmarshal(step.Content, &content) != nil || content == nil {
			return errors.New("fixture content must be a JSON object")
		}
		return validateFixtureTemplate(purpose, step.Content)
	}
	seen := map[string]bool{}
	for _, call := range step.ToolCalls {
		if call.ID == "" || len(call.ID) > 128 || !fixtureTools[purpose][call.Name] || seen[call.ID] {
			return errFixtureToolCall
		}
		seen[call.ID] = true
		var arguments map[string]any
		if json.Unmarshal(call.Arguments, &arguments) != nil || arguments == nil {
			return errors.New("fixture tool arguments must be a JSON object")
		}
		if err := validateFixtureTemplate(purpose, call.Arguments); err != nil {
			return err
		}
	}
	return nil
}

func validateFixtureTemplate(purpose string, data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return errors.New("invalid fixture template")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("invalid fixture template")
	}
	// A tool_result placeholder may read only a tool its purpose may call.
	return walkFixtureTemplate(value, func(spec fixtureSpec) (any, error) {
		if spec.name == "tool_result" && !fixtureTools[purpose][spec.tool] {
			return nil, errors.New("invalid fixture placeholder")
		}
		return nil, nil
	})
}

// fixtureSpec is one validated placeholder.
type fixtureSpec struct {
	name   string
	count  int      // previous_evidence_span_ids
	path   []string // brief, tool_result: dotted path segments, each name or name[index]
	tool   string   // tool_result
	offset int64    // now: seconds
}

var fixturePathSegment = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,63}(\[[0-9]{1,3}\])?$`)

func fixturePath(v any) ([]string, error) {
	raw, ok := v.(string)
	if !ok || raw == "" || len(raw) > 256 {
		return nil, errors.New("invalid fixture path")
	}
	segments := strings.Split(raw, ".")
	for _, seg := range segments {
		if !fixturePathSegment.MatchString(seg) {
			return nil, errors.New("invalid fixture path")
		}
	}
	return segments, nil
}

func fixtureInt(v any, lo, hi int64) (int64, error) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, errors.New("invalid fixture placeholder")
	}
	parsed, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil || parsed < lo || parsed > hi {
		return 0, errors.New("invalid fixture placeholder")
	}
	return parsed, nil
}

func parseFixtureSpec(value map[string]any) (fixtureSpec, error) {
	name, ok := value["$fixture"].(string)
	if !ok {
		return fixtureSpec{}, errors.New("invalid fixture placeholder")
	}
	spec := fixtureSpec{name: name}
	keys := func(want ...string) bool {
		if len(value) != len(want)+1 {
			return false
		}
		for _, k := range want {
			if _, ok := value[k]; !ok {
				return false
			}
		}
		return true
	}
	var err error
	switch name {
	case "run_id", "first_required_evidence_ref", "first_fact_evidence_ref", "first_requirement_id", "first_requested_kind":
		if !keys() {
			return spec, errors.New("invalid fixture placeholder")
		}
	case "previous_evidence_span_ids":
		if !keys("count") {
			return spec, errors.New("invalid fixture placeholder")
		}
		var n int64
		n, err = fixtureInt(value["count"], 1, 20)
		spec.count = int(n)
	case "brief":
		if !keys("path") {
			return spec, errors.New("invalid fixture placeholder")
		}
		spec.path, err = fixturePath(value["path"])
	case "tool_result":
		if !keys("tool", "path") {
			return spec, errors.New("invalid fixture placeholder")
		}
		tool, _ := value["tool"].(string)
		if !anyFixtureTool(tool) {
			return spec, errors.New("invalid fixture placeholder")
		}
		spec.tool = tool
		spec.path, err = fixturePath(value["path"])
	case "now":
		if !keys("offset_seconds") {
			return spec, errors.New("invalid fixture placeholder")
		}
		spec.offset, err = fixtureInt(value["offset_seconds"], -maxFixtureOffset, maxFixtureOffset)
	default:
		return spec, errors.New("unknown fixture placeholder")
	}
	return spec, err
}

func anyFixtureTool(tool string) bool {
	for _, tools := range fixtureTools {
		if tools[tool] {
			return true
		}
	}
	return false
}

func walkFixtureTemplate(value any, resolve func(fixtureSpec) (any, error)) error {
	switch value := value.(type) {
	case map[string]any:
		if _, exists := value["$fixture"]; exists {
			spec, err := parseFixtureSpec(value)
			if err != nil {
				return err
			}
			if resolve != nil {
				_, err = resolve(spec)
			}
			return err
		}
		for _, child := range value {
			if err := walkFixtureTemplate(child, resolve); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := walkFixtureTemplate(child, resolve); err != nil {
				return err
			}
		}
	}
	return nil
}

// fixtureLookup copies the value at path from a decoded JSON document.
func fixtureLookup(doc any, path []string) (any, error) {
	cur := doc
	for _, seg := range path {
		key, index := seg, -1
		if i := strings.IndexByte(seg, '['); i >= 0 {
			key = seg[:i]
			n, _ := strconv.Atoi(seg[i+1 : len(seg)-1])
			index = n
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, errors.New("fixture path not in the conversation")
		}
		if cur, ok = m[key]; !ok {
			return nil, errors.New("fixture path not in the conversation")
		}
		if index >= 0 {
			list, ok := cur.([]any)
			if !ok || index >= len(list) {
				return nil, errors.New("fixture path not in the conversation")
			}
			cur = list[index]
		}
	}
	return cur, nil
}

type fixturePrompt struct {
	RunID                string   `json:"run_id"`
	RequiredEvidenceRefs []string `json:"required_evidence_refs"`
	Kinds                []string `json:"kinds"`
	Requirements         []struct {
		ID string `json:"id"`
	} `json:"requirements"`
}

func fixturePromptFromRequest(request ModelRequest) (fixturePrompt, error) {
	if request.PromptVersion == OCRPromptVersion {
		return fixtureOCRPrompt(request)
	}
	for _, message := range request.Messages {
		if message.Role != "user" {
			continue
		}
		var prompt fixturePrompt
		if json.Unmarshal([]byte(message.Content), &prompt) == nil && prompt.RunID != "" {
			return prompt, nil
		}
	}
	return fixturePrompt{}, errors.New("fixture prompt missing run id")
}

// fixtureOCRPrompt reads the run ID an OCR transcription must echo. The extraction's user message
// is an instruction that ends with the exact JSON object to return (extraction.go), not a JSON
// brief, so the object is taken from its first "{" to its last "}" and its run_id must be a UUID.
func fixtureOCRPrompt(request ModelRequest) (fixturePrompt, error) {
	for _, message := range request.Messages {
		if message.Role != "user" {
			continue
		}
		start, end := strings.Index(message.Content, "{"), strings.LastIndex(message.Content, "}")
		if start < 0 || end < start {
			continue
		}
		var prompt fixturePrompt
		if json.Unmarshal([]byte(message.Content[start:end+1]), &prompt) == nil && uuid.Validate(prompt.RunID) == nil {
			return fixturePrompt{RunID: prompt.RunID}, nil
		}
	}
	return fixturePrompt{}, errors.New("fixture OCR prompt missing run id")
}

func fixturePurpose(promptVersion string) string {
	switch promptVersion {
	case InterviewPromptVersion:
		return fixtureInterviewPurpose
	case DraftPromptVersion:
		return fixtureDraftPurpose
	case PromptVersion:
		return fixtureInvestigationPurpose
	case ChatPromptVersion:
		return fixtureChatPurpose
	case OCRPromptVersion:
		return fixtureOCRPurpose
	default:
		return ""
	}
}

func (step fixtureScriptStep) response(request ModelRequest, prompt fixturePrompt) (ModelResponse, error) {
	spans := previousEvidenceSpanIDs(request)
	brief, results := fixtureConversation(request)
	resolve := func(spec fixtureSpec) (any, error) {
		name, count := spec.name, spec.count
		switch name {
		case "brief":
			if brief != nil {
				return fixtureLookup(brief, spec.path)
			}
		case "tool_result":
			if result, ok := results[spec.tool]; ok {
				return fixtureLookup(result, spec.path)
			}
		case "now":
			return fixtureNow().Add(time.Duration(spec.offset) * time.Second).UTC().Format(time.RFC3339), nil
		case "run_id":
			return prompt.RunID, nil
		case "first_required_evidence_ref":
			if len(prompt.RequiredEvidenceRefs) > 0 {
				return prompt.RequiredEvidenceRefs[0], nil
			}
		case "first_fact_evidence_ref":
			// The evidence behind the brief's first approved fact (WIS-712): since the facts are
			// ranked by passage, the first required reference may belong to another fact.
			if brief != nil {
				span, err := fixtureLookup(brief, []string{"approved_facts[0]", "source_span_ids[0]"})
				if id, ok := span.(string); err == nil && ok {
					if ref, err := fixtureLookup(brief, []string{"evidence_by_span", id}); err == nil {
						return ref, nil
					}
				}
			}
		case "first_requirement_id":
			if len(prompt.Requirements) > 0 {
				return prompt.Requirements[0].ID, nil
			}
		case "first_requested_kind":
			if len(prompt.Kinds) > 0 {
				return prompt.Kinds[0], nil
			}
		case "previous_evidence_span_ids":
			if count > 0 && len(spans) >= count {
				return spans[:count], nil
			}
		}
		return nil, errors.New("fixture placeholder has no conversation value")
	}

	if len(step.ToolCalls) > 0 {
		calls := make([]ToolCall, 0, len(step.ToolCalls))
		for _, call := range step.ToolCalls {
			arguments, err := resolveFixtureJSON(call.Arguments, resolve)
			if err != nil {
				return ModelResponse{}, err
			}
			calls = append(calls, ToolCall{ID: call.ID, Name: call.Name, Arguments: arguments})
		}
		return ModelResponse{ToolCalls: calls, FinishReason: "tool_calls"}, nil
	}
	content, err := resolveFixtureJSON(step.Content, resolve)
	if err != nil {
		return ModelResponse{}, err
	}
	return ModelResponse{Content: string(content), FinishReason: "stop"}, nil
}

func resolveFixtureJSON(data []byte, resolve func(fixtureSpec) (any, error)) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, errors.New("invalid fixture template")
	}
	var replace func(any) (any, error)
	replace = func(value any) (any, error) {
		switch value := value.(type) {
		case map[string]any:
			if _, exists := value["$fixture"]; exists {
				var out any
				err := walkFixtureTemplate(value, func(spec fixtureSpec) (any, error) {
					resolved, resolveErr := resolve(spec)
					out = resolved
					return resolved, resolveErr
				})
				return out, err
			}
			for key, child := range value {
				resolved, err := replace(child)
				if err != nil {
					return nil, err
				}
				value[key] = resolved
			}
		case []any:
			for i, child := range value {
				resolved, err := replace(child)
				if err != nil {
					return nil, err
				}
				value[i] = resolved
			}
		}
		return value, nil
	}
	resolved, err := replace(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(resolved)
}

func previousEvidenceSpanIDs(request ModelRequest) []string {
	toolNames := map[string]string{}
	var spans []string
	for _, message := range request.Messages {
		if message.Role == "assistant" {
			for _, call := range message.ToolCalls {
				toolNames[call.ID] = call.Name
			}
			continue
		}
		if message.Role != "tool" || toolNames[message.ToolCallID] != ToolEvidenceGet {
			continue
		}
		var result struct {
			SpanIDs []string `json:"span_ids"`
		}
		if json.Unmarshal([]byte(message.Content), &result) == nil {
			spans = result.SpanIDs
		}
	}
	return spans
}

// fixtureNow is the fixture's clock for "now" placeholders, truncated to the second.
var fixtureNow = func() time.Time { return time.Now().UTC().Truncate(time.Second) }

// fixtureConversation decodes the brief (the first user message carrying a run_id) and the latest
// result of each tool called so far, by tool name. Numbers stay exact (json.Number).
func fixtureConversation(request ModelRequest) (map[string]any, map[string]any) {
	decode := func(s string) (any, bool) {
		d := json.NewDecoder(strings.NewReader(s))
		d.UseNumber()
		var v any
		return v, d.Decode(&v) == nil
	}
	var brief map[string]any
	names := map[string]string{}
	results := map[string]any{}
	for _, m := range request.Messages {
		switch m.Role {
		case "user":
			if brief == nil {
				if v, ok := decode(m.Content); ok {
					if b, ok := v.(map[string]any); ok && b["run_id"] != nil {
						brief = b
					}
				}
			}
		case "assistant":
			for _, c := range m.ToolCalls {
				names[c.ID] = c.Name
			}
		case "tool":
			if v, ok := decode(m.Content); ok && names[m.ToolCallID] != "" {
				results[names[m.ToolCallID]] = v
			}
		}
	}
	return brief, results
}
