package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Watchdog budgets carried over from the original server. They bound idle
// upstream reads before/after content and let the plugin absorb the usage
// frame the upstream sometimes places right after [DONE].
const (
	firstEventTimeout   = 30 * time.Second
	bodyIdleTimeout     = 120 * time.Second
	trailingUsageWindow = time.Second
	// Frames buffered before the first content-carrying frame. Normal traffic
	// needs 1-2 (role + a possible empty delta); the cap keeps memory O(1)
	// when the upstream misbehaves. (MAX_PENDING_FRAMES)
	maxPendingFrames = 8
	// Upper bound for upstream error-body diagnostics (readError BODY_MAX).
	errorBodyMaxBytes = 64 * 1024
)

func jsonMarshal(value any) ([]byte, error) { return json.Marshal(value) }

// ----------------------------------------------------------------------------
// request-scoped response context
// ----------------------------------------------------------------------------

// streamCtx plays the role of the ctx object in the original server.
type streamCtx struct {
	requestID       string
	model           string
	thinkingEnabled bool
	expectedChoices int
	toolsDisabled   bool
	// sseFrame mirrors workbuddy's clientNeedsSSEFrame: the host's native
	// chat-completions writer wraps every chunk with "data: ...\n\n" itself,
	// so payloads for /v1/chat/completions and /v1/completions entry points
	// must be bare JSON; every other entry point arrives through a
	// cross-format response translator that only consumes payloads already
	// framed as "data: " lines.
	sseFrame      bool
	aliases       map[string]string // alias → original
	reservedNames []string
	clientNames   []string
}

func streamCtxFrom(request *upstreamRequest) *streamCtx {
	expected := request.ChoiceCount
	if expected <= 0 {
		expected = 1
	}
	return &streamCtx{
		requestID:       request.RequestID,
		model:           request.Model,
		thinkingEnabled: request.ThinkingEnabled,
		expectedChoices: expected,
		toolsDisabled:   request.ToolsDisabled,
		aliases:         request.Aliases,
		reservedNames:   append([]string(nil), request.ReservedToolNames...),
		clientNames:     append([]string(nil), request.ClientToolNames...),
	}
}

// clientNeedsSSEFrame mirrors workbuddy: only the native OpenAI entry points
// get their SSE framing added by the host writer; all other paths (Claude,
// Gemini, Codex/responses translators) consume "data: " framed payloads.
func clientNeedsSSEFrame(metadata map[string]any) bool {
	path, _ := metadata["request_path"].(string)
	switch strings.ToLower(strings.TrimSpace(path)) {
	case "/v1/chat/completions", "/v1/completions":
		return false
	default:
		return true
	}
}

// candidates mirrors the JS candidate list: every alias name plus every
// reserved name, used for prefix-aware name restoration.
func (c *streamCtx) candidates() []string {
	out := make([]string, 0, len(c.clientNames)+len(c.reservedNames))
	out = append(out, c.clientNames...)
	out = append(out, c.reservedNames...)
	return out
}

// ----------------------------------------------------------------------------
// incremental UTF-8 decoding (TextDecoder {stream:true} port)
// ----------------------------------------------------------------------------

type textDecoder struct {
	pending []byte
}

func (d *textDecoder) decode(chunk []byte, flush bool) string {
	d.pending = append(d.pending, chunk...)
	if flush {
		text := string(d.pending)
		d.pending = nil
		return text
	}
	for cut := 1; cut <= 3 && cut <= len(d.pending); cut++ {
		split := len(d.pending) - cut
		if split < 0 {
			break
		}
		if utf8.RuneStart(d.pending[split]) && utf8.Valid(d.pending[:split]) {
			text := string(d.pending[:split])
			d.pending = append([]byte(nil), d.pending[split:]...)
			return text
		}
	}
	// Fall through: complete text (or tolerate an invalid sequence mid-body).
	text := string(d.pending)
	d.pending = nil
	return text
}

// ----------------------------------------------------------------------------
// safeParse + upstream error extraction
// ----------------------------------------------------------------------------

// safeParseAny is the JS safeParse with UseNumber so integers round-trip.
func safeParseAny(text string) any {
	var out any
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if err := decoder.Decode(&out); err != nil || out == nil {
		return nil
	}
	// Mirror typeof parsed !== "object" || Array.isArray: only objects count.
	if _, ok := out.(map[string]any); !ok {
		return nil
	}
	return out
}

// ----------------------------------------------------------------------------
// readEvents port
// ----------------------------------------------------------------------------

// eventParser is the direct port of readEvents. Iterate with run(); frames
// arrive through the callback, [DONE] 通过 nil frame 报告, and the parser
// owns the trailing usage window and per-choice name restoration state.
type eventParser struct {
	body           io.ReadCloser
	ctx            *streamCtx
	decoder        textDecoder
	buffer         string
	sawAny         bool
	formatDecided  bool
	sawDone        bool
	trailingUntil  int64
	choices        map[int]bool
	finished       map[int]bool
	names          map[int]*toolCallBook
	data           []string
	eventType      string
	dataBytes      int
	eventOversized bool
}

func newEventParser(body io.ReadCloser, ctx *streamCtx) *eventParser {
	return &eventParser{
		body:     body,
		ctx:      ctx,
		choices:  map[int]bool{},
		finished: map[int]bool{},
		names:    map[int]*toolCallBook{},
	}
}

type frameAction int

const (
	frameEmit frameAction = iota
	frameIgnore
	frameDoneMark
)

// run drives the upstream body to completion, mirroring the JS while(true)
// loop: bounded reads, line assembly, trailing budget, EOF flush.
func (p *eventParser) run(onFrame func(frame map[string]any) error) error {
	for {
		budget := p.budget()
		if p.trailingUntil > 0 && budget <= 0 {
			return nil // trailing budget exhausted = normal end of stream
		}
		chunk, eof, readErr := readUpstreamChunk(p.body, budget)
		if readErr != nil {
			if p.trailingUntil > 0 {
				return nil // no more trailing data = normal finish
			}
			return asZenStream(readErr, "Failed to read upstream stream")
		}
		text := p.decoder.decode(chunk, eof)
		if !eof {
			p.sawAny = true
		}
		p.buffer += text
		if len(p.buffer) > maxEventBytes {
			return &zenStreamError{message: "Upstream SSE line exceeds size limit"}
		}
		if !p.formatDecided {
			lead := strings.TrimLeft(p.buffer, " \t\r\n\v\f")
			if lead != "" {
				p.formatDecided = true
				// The free tier can answer HTTP 200 with a raw JSON error body;
				// do not wait out the whole idle window before recognizing it.
				if strings.HasPrefix(lead, "{") {
					message := readErrorText(p.body, p.ctx, 200, p.buffer, &p.decoder)
					return &zenStreamError{message: message}
				}
			}
		}
		lines := strings.Split(p.buffer, "\n")
		p.buffer = lines[len(lines)-1]
		for _, line := range lines[:len(lines)-1] {
			eventPayload, eventKind := p.readLine(line)
			if p.eventOversized {
				return &zenStreamError{message: "Upstream SSE event exceeds size limit"}
			}
			if eventPayload == "" {
				continue
			}
			if err := p.emit(eventPayload, eventKind, onFrame); err != nil {
				return err
			}
		}
		if eof {
			eventPayload, eventKind := p.takeEvent()
			if eventPayload != "" {
				if err := p.emit(eventPayload, eventKind, onFrame); err != nil {
					return err
				}
			}
			return nil
		}
	}
}

// budget is the JS budget selection: the idle window bounds network reads only.
func (p *eventParser) budget() time.Duration {
	idle := firstEventTimeout
	if p.sawAny {
		idle = bodyIdleTimeout
	}
	if p.trailingUntil > 0 {
		remaining := time.Until(time.UnixMilli(p.trailingUntil))
		if remaining < idle {
			return remaining
		}
	}
	return idle
}

// readLine is the JS readLine: field/value assembly; a blank line ends the
// event. Returns the completed payload and its event type.
func (p *eventParser) readLine(line string) (string, string) {
	line = strings.TrimSuffix(line, "\r")
	if line == "" {
		return p.takeEvent()
	}
	if strings.HasPrefix(line, ":") {
		return "", ""
	}
	colon := strings.IndexByte(line, ':')
	field, value := line, ""
	if colon != -1 {
		field, value = line[:colon], line[colon+1:]
	}
	if strings.HasPrefix(value, " ") {
		value = value[1:]
	}
	switch field {
	case "data":
		if len(value) > maxEventBytes-p.dataBytes {
			p.eventOversized = true
			return "", ""
		}
		p.dataBytes += len(value) + 1
		p.data = append(p.data, value)
	case "event":
		p.eventType = value
	}
	return "", ""
}

func (p *eventParser) takeEvent() (string, string) {
	payload := strings.Join(p.data, "\n")
	kind := p.eventType
	p.data = nil
	p.dataBytes = 0
	p.eventType = ""
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return "", ""
	}
	return payload, kind
}

// emit is the JS emit(event): parse then forward, [DONE] 通过 nil frame 报告.
func (p *eventParser) emit(payload, kind string, onFrame func(frame map[string]any) error) error {
	action, frame, err := p.parse(payload, kind)
	if err != nil {
		return err
	}
	switch action {
	case frameIgnore:
		return nil
	case frameDoneMark:
		return onFrame(nil)
	default:
		return onFrame(frame)
	}
}

// parse is the JS parse(event); the trailing state is re-evaluated per event
// because one network chunk can carry [DONE] plus the frames after it.
func (p *eventParser) parse(payload, kind string) (frameAction, map[string]any, error) {
	trailing := p.trailingUntil > 0
	if payload == "[DONE]" {
		// A completion signal proves the answer is whole; check deferred names.
		if !trailing {
			for _, book := range p.names {
				if err := book.checkToolNamesComplete(); err != nil {
					return frameIgnore, nil, err
				}
			}
		}
		p.sawDone = true
		p.beginTrailing(trailingUsageWindow)
		return frameDoneMark, nil, nil
	}
	root, ok := safeParseAny(payload).(map[string]any)
	if !ok {
		// The trailing tail cannot invalidate an already complete answer.
		if trailing {
			return frameIgnore, nil, nil
		}
		return frameIgnore, nil, &zenStreamError{message: "Invalid SSE payload from upstream"}
	}
	if message, isFailure := upstreamErrorOf(root, kind == "error"); isFailure {
		if trailing {
			// Ignoring is not [DONE]: never fabricate a completion signal.
			return frameIgnore, nil, nil
		}
		logUpstreamBody(p.ctx.requestID, p.ctx.model, 200, payload, message)
		return frameIgnore, nil, &zenStreamError{message: message}
	}
	if trailing {
		if usage := root["usage"]; UsagePresent(usage) {
			pruned := copyObject(root)
			pruned["choices"] = []any{}
			return frameEmit, pruned, nil
		}
		return frameIgnore, nil, nil
	}
	choices, _ := root["choices"].([]any)
	for _, raw := range choices {
		choice := asObject(raw)
		if choice == nil {
			continue
		}
		// The upstream no longer accepts tool_choice:none; disabled semantics
		// are enforced here, before anything is streamed or aggregated.
		if p.ctx.toolsDisabled && choiceHasToolAction(choice) {
			return frameIgnore, nil, &zenStreamError{message: "Upstream returned a tool call when tools are disabled"}
		}
		index := choiceIndex(choice)
		// Late-arriving calls for settled choices are ignored like any other
		// out-of-band frame; they must not re-trigger mapping or rejection.
		if !p.finished[index] {
			if err := p.restoreChoiceToolNames(choice, index); err != nil {
				return frameIgnore, nil, err
			}
		}
		p.choices[index] = true
		if reason, isStr := choice["finish_reason"].(string); isStr && reason != "" {
			p.finished[index] = true
		}
	}
	if len(p.choices) >= p.ctx.expectedChoices && p.allFinished() {
		// usage / [DONE] can still arrive late: begin the trailer only after
		// every choice finished, so one early choice cannot truncate the rest.
		p.beginTrailing(bodyIdleTimeout)
	}
	return frameEmit, root, nil
}

func (p *eventParser) allFinished() bool {
	for index := range p.choices {
		if !p.finished[index] {
			return false
		}
	}
	return true
}

// beginTrailing mirrors beginTrailer: earlier deadlines stay in place so
// repeat completion signals never extend the total budget.
func (p *eventParser) beginTrailing(window time.Duration) {
	deadline := time.Now().Add(window).UnixMilli()
	if p.trailingUntil == 0 || deadline < p.trailingUntil {
		p.trailingUntil = deadline
	}
}

// choiceHasToolAction mirrors the toolsDisabled enforcement predicate.
func choiceHasToolAction(choice map[string]any) bool {
	delta := asObject(choice["delta"])
	if delta != nil {
		if calls, ok := delta["tool_calls"].([]any); ok && len(calls) > 0 {
			return true
		}
		if raw, has := delta["function_call"]; has && raw != nil && asObject(raw) != nil {
			return true
		}
	}
	reason, _ := choice["finish_reason"].(string)
	return reason == "tool_calls" || reason == "function_call"
}

// restoreChoiceToolNames is the parse-layer port of restoreChoiceToolNames:
// tool names arrive split across frames, so mapping is deferred until a name
// stops matching a longer candidate; per-choice/per-id accumulation; reserved
// tool calls fail the stream.
func (p *eventParser) restoreChoiceToolNames(choice map[string]any, index int) error {
	if len(p.ctx.aliases) == 0 && len(p.ctx.reservedNames) == 0 {
		return nil
	}
	delta := asObject(choice["delta"])
	if delta == nil {
		return nil
	}
	calls, hasCalls := delta["tool_calls"].([]any)
	hasCalls = hasCalls && len(calls) > 0
	fnCallRaw, hasFnRaw := delta["function_call"]
	hasFunctionCall := hasFnRaw && fnCallRaw != nil && asObject(fnCallRaw) != nil
	if !hasCalls && !hasFunctionCall {
		return nil
	}
	book := p.names[index]
	if book == nil {
		book = newToolCallBook()
		p.names[index] = book
	}
	if hasCalls {
		restored := make([]any, 0, len(calls))
		for _, raw := range calls {
			call := asObject(raw)
			if call == nil {
				restored = append(restored, raw)
				continue
			}
			target := toolCallTarget(book, call)
			if id := callID(call); id != "" {
				target.id = id
				book.callsById[id] = target
			}
			if fn := asObject(call["function"]); fn != nil {
				next := copyObject(call)
				restoredFn, err := p.restoreFunctionName(copyObject(fn), target)
				if err != nil {
					return err
				}
				next["function"] = restoredFn
				restored = append(restored, next)
				continue
			}
			restored = append(restored, call)
		}
		delta["tool_calls"] = restored
	}
	if hasFunctionCall {
		fn := asObject(delta["function_call"])
		target := book.ensureLegacy()
		restoredFn, err := p.restoreFunctionName(copyObject(fn), target)
		if err != nil {
			return err
		}
		delta["function_call"] = restoredFn
	}
	if reason, isStr := choice["finish_reason"].(string); isStr && reason != "" {
		if book := p.names[index]; book != nil {
			if err := book.checkToolNamesComplete(); err != nil {
				return err
			}
		}
	}
	return nil
}

// restoreFunctionName is the faithful port of restoreFunctionName. Fragment
// accumulation waits until the name stops matching a longer candidate and
// arguments have appeared; alias decisions resolve per call identity.
func (p *eventParser) restoreFunctionName(fn map[string]any, target *callIdentity) (map[string]any, error) {
	fragment, _ := fn["name"].(string)
	hasName := len(fragment) > 0
	if !hasName && !target.pendingName {
		return fn, nil
	}
	if hasName && target.upstreamName != fragment {
		target.upstreamName = target.upstreamName + fragment
	}
	name := target.upstreamName
	next := copyObject(fn)
	candidates := p.ctx.candidates()
	// A full tool name can also be a prefix of another name (oc2cpa_read vs.
	// the oc2cpa_read_1 alias generated for a colliding client read), so the
	// decision is deferred until arguments exist.
	if !argsTruthy(fn) {
		for _, candidate := range candidates {
			if candidate != name && strings.HasPrefix(candidate, name) {
				delete(next, "name")
				target.pendingName = true
				return next, nil
			}
		}
	}
	if p.isReserved(name) {
		// Fail the whole stream like the original: never hand the reserved
		// gate tool back to the client as if it were the client's own tool.
		return next, &zenStreamError{message: "Upstream called reserved tool: " + name}
	}
	restored, found := p.ctx.aliases[name]
	if found {
		if target.nameRestored {
			delete(next, "name")
		} else {
			next["name"] = restored
		}
		target.nameRestored = true
		target.pendingName = false
		return next, nil
	}
	known := false
	for _, client := range p.ctx.clientNames {
		if client == name {
			known = true
			break
		}
	}
	if !known && stringsHasAnyPrefix(candidates, name) {
		delete(next, "name")
		target.pendingName = true
		return next, nil
	}
	if target.pendingName {
		next["name"] = name
	}
	target.pendingName = false
	return next, nil
}

func stringsHasAnyPrefix(candidates []string, name string) bool {
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate, name) {
			return true
		}
	}
	return false
}

func (p *eventParser) isReserved(name string) bool {
	for _, reserved := range p.ctx.reservedNames {
		if name == reserved {
			return true
		}
	}
	return false
}

// argsTruthy ports `!fn.arguments` (undefined/null/""/0 excluded).
func argsTruthy(fn map[string]any) bool {
	value, has := fn["arguments"]
	if !has || value == nil {
		return false
	}
	if text, isStr := value.(string); isStr {
		return text != ""
	}
	if number, isNum := value.(json.Number); isNum {
		return number.String() != "0"
	}
	return true
}

// ----------------------------------------------------------------------------
// upstream error-body reader (readError port)
// ----------------------------------------------------------------------------

// readErrorText is the port of readError: bounded diagnostic read (64 KiB
// within a one-second budget), then message extraction fallbacks. The reader
// is cancelled on exit so the connection never lingers.
func readErrorText(reader io.ReadCloser, ctx *streamCtx, status int, initialText string, decoder *textDecoder) string {
	text := initialText
	if len(text) > errorBodyMaxBytes {
		text = string([]byte(text)[:errorBodyMaxBytes])
	}
	total := len(text)
	initialMessage, hasInitial := upstreamErrorOf(safeParseAnyToAny(text), false)
	if !hasInitial {
		deadline := time.Now().Add(time.Second)
		for !hasInitial && total < errorBodyMaxBytes {
			budget := time.Until(deadline)
			if budget <= 0 {
				break
			}
			chunk, eof, readErr := readUpstreamChunk(reader, budget)
			if readErr != nil || eof {
				break
			}
			limit := errorBodyMaxBytes - total
			if len(chunk) > limit {
				chunk = chunk[:limit]
			}
			text += decoder.decode(chunk, false)
			total += len(chunk)
		}
	}
	text += decoder.decode(nil, true)
	_ = reader.Close() // reader.cancel() parity; caller closes as well, safe
	message := ""
	if hasInitial {
		message = initialMessage
	} else if fallback, found := upstreamErrorOf(safeParseAnyToAny(text), true); found {
		message = fallback
	} else {
		message = fmt.Sprintf("Upstream returned HTTP %d", status)
	}
	logUpstreamBody(ctx.requestID, ctx.model, status, text, message)
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}

// safeParseAnyToAny is safeParseAny without the object-only restriction, so
// error bodies shaped as non-objects still parse (JS safeParse keeps them).
func safeParseAnyToAny(text string) any {
	var out any
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if err := decoder.Decode(&out); err != nil {
		return nil
	}
	return out
}

// readUpstreamChunk is the raceRead port: a deadline-bounded read so upstream
// silence is detected instead of hanging the executor forever.
func readUpstreamChunk(body io.Reader, budget time.Duration) ([]byte, bool, error) {
	if budget <= 0 {
		return nil, false, &zenStreamError{message: "Upstream stream idle timeout"}
	}
	type readResult struct {
		n   int
		err error
	}
	buf := make([]byte, 64*1024)
	done := make(chan readResult, 1)
	go func() {
		n, err := body.Read(buf)
		done <- readResult{n: n, err: err}
	}()
	select {
	case result := <-done:
		switch result.err {
		case nil:
			return buf[:result.n], false, nil
		case io.EOF:
			// EOF may surface with or without a final payload chunk.
			if result.n > 0 {
				return buf[:result.n], false, nil
			}
			return nil, true, nil
		default:
			// A payload chunk plus an error: surface the payload first; the
			// next read reports the error.
			if result.n > 0 {
				return buf[:result.n], false, nil
			}
			return nil, false, result.err
		}
	case <-time.After(budget):
		return nil, false, &zenStreamError{message: "Upstream stream idle timeout"}
	}
}

func asZenStream(err error, fallback string) error {
	if zenErr, ok := err.(*zenStreamError); ok {
		return zenErr
	}
	message := fallback
	if err != nil && err.Error() != "" {
		message = err.Error()
	}
	return &zenStreamError{message: message}
}

func choiceIndex(choice map[string]any) int {
	index, ok := asInt(choice["index"])
	if !ok || index < 0 {
		return 0
	}
	return index
}

// ----------------------------------------------------------------------------
// logging (logUpstreamBody port; the host captures plugin stderr)
// ----------------------------------------------------------------------------

func logUpstreamBody(requestID, model string, status int, text, zenMessage string) {
	// Upstream errors may echo prompts, credentials, or proxy URLs.
	entry := fmt.Sprintf(`[ZEN ERROR] {"requestId":%q,"model":%q,"status":%d}`,
		requestID, model, status)
	fsDebugLog(entry)
}

// UsagePresent mirrors JS truthiness for a usage field: only real objects
// count (null/false/"" are treated as absent).
func UsagePresent(value any) bool {
	usage, ok := value.(map[string]any)
	return ok && len(usage) > 0
}

// ----------------------------------------------------------------------------
// normalize ports
// ----------------------------------------------------------------------------

// normalizeDelta: drop upstream-only fields, unify reasoning, sanitize calls.
func normalizeDelta(delta map[string]any, thinking bool) map[string]any {
	if delta == nil {
		return map[string]any{}
	}
	out := copyObject(delta)
	if raw, has := out["tool_calls"]; has && raw != nil {
		if calls, ok := raw.([]any); ok {
			kept := make([]any, 0, len(calls))
			for _, call := range calls {
				if asObject(call) != nil {
					kept = append(kept, call)
				}
			}
			out["tool_calls"] = kept
		} else {
			delete(out, "tool_calls")
		}
	}
	// Upstream private extensions: strict clients reject unknown schema fields.
	delete(out, "name")
	delete(out, "cost")

	// Thinking normalization: reasoning first; reasoning_details mirror the
	// same content (verified against the upstream) and are dropped.
	reasoning := ""
	if value, isStr := delta["reasoning"].(string); isStr && value != "" {
		reasoning = value
	} else if value, isStr := delta["reasoning_content"].(string); isStr {
		reasoning = value
	}
	delete(out, "reasoning")
	delete(out, "reasoning_content")
	delete(out, "reasoning_details")
	if thinking && reasoning != "" {
		out["reasoning_content"] = reasoning
	}
	return out
}

// normalizeChunk: drop cost, force the client model name, prune usage:null.
func normalizeChunk(chunk map[string]any, model string, thinking bool) map[string]any {
	if chunk == nil {
		return nil
	}
	out := copyObject(chunk)
	delete(out, "cost")
	if model != "" {
		out["model"] = model
	}
	if usage, has := out["usage"]; has && usage == nil {
		delete(out, "usage")
	}
	if choices, ok := out["choices"].([]any); ok {
		normalized := make([]any, 0, len(choices))
		for _, raw := range choices {
			choice := asObject(raw)
			if choice == nil {
				continue
			}
			next := copyObject(choice)
			if delta := asObject(next["delta"]); delta != nil {
				next["delta"] = normalizeDelta(delta, thinking)
			} else if next["delta"] != nil {
				// A non-object delta normalizes to {}.
				next["delta"] = map[string]any{}
			}
			normalized = append(normalized, next)
		}
		out["choices"] = normalized
	} else {
		out["choices"] = []any{}
	}
	return out
}

// hasContent mirrors hasContent: any delta key other than role counts.
func hasContent(chunk map[string]any) bool {
	choices, ok := chunk["choices"].([]any)
	if !ok {
		return false
	}
	for _, raw := range choices {
		choice := asObject(raw)
		if choice == nil {
			continue
		}
		delta := asObject(choice["delta"])
		if delta == nil {
			continue
		}
		for key := range delta {
			if key != "role" {
				return true
			}
		}
	}
	return false
}

// normalizeFinish: the upstream labels about 40% of tool-call answers
// finish_reason:"stop" (observed on big-pickle); opencode's official client
// normalizes those to tool_calls so downstream executes the tools.
func normalizeFinish(reason string, toolCallCount int) string {
	if reason == "stop" && toolCallCount > 0 {
		return "tool_calls"
	}
	return reason
}

// ----------------------------------------------------------------------------
// merge helpers
// ----------------------------------------------------------------------------

// mergeValue: string increments concatenate, objects merge recursively
// (the upstream splits function_call name/arguments across frames; plain
// assignment would drop the earlier fragment).
func mergeValue(existing, incoming any) any {
	if existing == nil {
		return incoming
	}
	switch left := existing.(type) {
	case string:
		if right, ok := incoming.(string); ok {
			return left + right
		}
	case map[string]any:
		if right, ok := incoming.(map[string]any); ok {
			merged := copyObject(left)
			for key, value := range right {
				if value == nil {
					continue
				}
				merged[key] = mergeValue(left[key], value)
			}
			return merged
		}
	}
	return incoming
}

// mergePassthrough: non-stream aggregation must carry every delta field the
// stream path would pass through, otherwise the two modes diverge.
func mergePassthrough(extra map[string]any, delta map[string]any) {
	for key, value := range delta {
		switch key {
		case "role", "content", "reasoning_content", "tool_calls":
			continue
		}
		if value == nil {
			continue
		}
		extra[key] = mergeValue(extra[key], value)
	}
}

// ----------------------------------------------------------------------------
// tool-call identity book (JS newCallState port)
// ----------------------------------------------------------------------------

// toolCallBook mirrors JS newCallState: one per choice, shared by the parse
// layer (name restoration), the streaming writer (index assignment) and the
// non-stream aggregator (argument accumulation).
type toolCallBook struct {
	calls         map[string]*callIdentity // slot key: decimal index or "auto:N"
	callsById     map[string]*callIdentity
	unindexed     map[int]*callIdentity // keyed by creation order
	current       *callIdentity
	nextOrder     int
	autoKeys      int
	legacyTool    *callIdentity
	role          string
	content       strings.Builder
	reasoning     strings.Builder
	finish        string
	extra         map[string]any
	streamIndexes map[int]bool // streaming writer only
	nextIndex     int          // streaming writer only
}

func newToolCallBook() *toolCallBook {
	return &toolCallBook{
		calls:         map[string]*callIdentity{},
		callsById:     map[string]*callIdentity{},
		unindexed:     map[int]*callIdentity{},
		streamIndexes: map[int]bool{},
		role:          "assistant",
		extra:         map[string]any{},
	}
}

// callIdentity ports the JS tool-call identity objects.
type callIdentity struct {
	id        string
	name      string
	arguments string
	order     int
	slot      string // "" until bound to a numeric slot, or "auto:N"
	// streaming index assignment (stream writer only)
	streamIndexAssigned bool
	streamIndex         int
	// name restoration state (parse layer only)
	upstreamName string
	pendingName  bool
	nameRestored bool
}

// toolCallTarget finds (or creates) the identity a tool_call fragment merges
// into. Parallel upstream calls often omit index (12/36 frames observed), so
// index alone cannot drive the mapping:
//   - with index: archive by index; an index-less first frame binds via id or
//     implicit creation order and migrates to the numeric slot;
//   - index-less but id already seen: merge by id, other calls may interleave;
//   - index-less with fresh id / function name: new slot;
//   - index-less with only arguments (continuation): merge into the current
//     accumulating call.
func toolCallTarget(book *toolCallBook, call map[string]any) *callIdentity {
	identified := book.callsById[callID(call)]
	if rawIndex, hasIndex := callIndexInt(call); hasIndex {
		key := strconv.Itoa(rawIndex)
		target := book.calls[key]
		if target == nil && identified != nil && identified.slot != "" {
			// JS: any string slot (numeric binding or auto:N) qualifies.
			target = identified
		}
		if target == nil {
			if candidate := book.unindexed[rawIndex]; candidate != nil {
				id := callID(call)
				if id == "" || candidate.id == "" {
					target = candidate
				}
			}
		}
		if target == nil {
			target = newToolCall(book)
		}
		if target.slot != key {
			bindCallIndex(book, target, rawIndex)
		}
		book.current = target
		return target
	}
	if identified != nil {
		book.current = identified
		return identified
	}
	id := callID(call)
	name := ""
	if fn := asObject(call["function"]); fn != nil {
		name, _ = fn["name"].(string)
	}
	startsNew := id != "" || name != ""
	if !startsNew && book.current != nil {
		return book.current
	}
	target := newToolCall(book)
	target.slot = fmt.Sprintf("auto:%d", book.autoKeys)
	book.autoKeys++
	book.calls[target.slot] = target
	book.unindexed[target.order] = target
	book.current = target
	return target
}

func newToolCall(book *toolCallBook) *callIdentity {
	target := &callIdentity{order: book.nextOrder}
	book.nextOrder++
	return target
}

// bindCallIndex migrates an auto-slotted call onto a numeric slot when the
// upstream starts labeling indexes later in the stream.
func bindCallIndex(book *toolCallBook, target *callIdentity, index int) {
	if strings.HasPrefix(target.slot, "auto:") {
		delete(book.calls, target.slot)
		delete(book.unindexed, target.order)
	}
	target.slot = strconv.Itoa(index)
	book.calls[target.slot] = target
}

func callID(call map[string]any) string {
	id, _ := call["id"].(string)
	return id
}

func callIndexInt(call map[string]any) (int, bool) {
	return asInt(call["index"])
}

// ensureLegacy ports `state.legacyTool ??= {}`.
func (book *toolCallBook) ensureLegacy() *callIdentity {
	if book.legacyTool == nil {
		book.legacyTool = &callIdentity{}
	}
	return book.legacyTool
}

// checkToolNamesComplete mirrors assertToolNamesComplete.
func (book *toolCallBook) checkToolNamesComplete() error {
	targets := make([]*callIdentity, 0, len(book.calls)+1)
	for _, entry := range book.calls {
		targets = append(targets, entry)
	}
	targets = append(targets, book.legacyTool)
	for _, target := range targets {
		if target != nil && target.pendingName {
			return &zenStreamError{message: "Incomplete upstream tool name"}
		}
	}
	return nil
}

// orderedToolCalls: standard indexed streams restore order by index (same as
// the official client); mixed index-less streams keep arrival order.
func orderedToolCalls(book *toolCallBook) []*callIdentity {
	type slotEntry struct {
		key     string
		numeric int
		target  *callIdentity
	}
	entries := make([]slotEntry, 0, len(book.calls))
	allNumeric := true
	for key, target := range book.calls {
		numeric, err := strconv.Atoi(key)
		if err != nil {
			allNumeric = false
		}
		entries = append(entries, slotEntry{key: key, numeric: numeric, target: target})
	}
	if allNumeric {
		sort.Slice(entries, func(i, j int) bool { return entries[i].numeric < entries[j].numeric })
	} else {
		sort.Slice(entries, func(i, j int) bool { return entries[i].target.order < entries[j].target.order })
	}
	out := make([]*callIdentity, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.target)
	}
	return out
}

// assertCompleted is the shared completion gate for both response modes:
//   - no choice at all (usage only) → upstream fault;
//   - choices present without a completion signal and some choice missing its
//     finish_reason → truncated;
//   - otherwise ([DONE] or every choice finished) → complete.
func assertCompleted(sawDone bool, choices, finished map[int]bool) error {
	if len(choices) == 0 {
		return &zenStreamError{message: "Empty response from upstream"}
	}
	allFinished := true
	for index := range choices {
		if !finished[index] {
			allFinished = false
			break
		}
	}
	if !sawDone && !allFinished {
		return &zenStreamError{message: "Incomplete response from upstream"}
	}
	return nil
}

// ----------------------------------------------------------------------------
// non-stream aggregation (respondJson port)
// ----------------------------------------------------------------------------

type jsonAggregator struct {
	ctx     *streamCtx
	books   map[int]*toolCallBook
	id      string
	created any
	usage   map[string]any
	sawDone bool
}

func aggregateUpstreamJSON(body io.ReadCloser, ctx *streamCtx) (map[string]any, error) {
	aggregator := &jsonAggregator{ctx: ctx, books: map[int]*toolCallBook{}}
	parser := newEventParser(body, ctx)
	err := parser.run(aggregator.consume)
	if err != nil {
		return nil, err
	}
	_ = parser // [DONE] already flipped aggregator.sawDone through the nil frame
	return aggregator.finish()
}

func (a *jsonAggregator) consume(frame map[string]any) error {
	if frame == nil {
		a.sawDone = true
		return nil
	}
	normalized := normalizeChunk(frame, a.ctx.model, a.ctx.thinkingEnabled)
	if normalized == nil {
		return nil
	}
	if id, isStr := normalized["id"].(string); isStr && id != "" && a.id == "" {
		a.id = id
	}
	if a.created == nil {
		if created, has := normalized["created"]; has {
			a.created = created
		}
	}
	if usage := normalized["usage"]; UsagePresent(usage) {
		a.usage = usage.(map[string]any)
	}
	choices, _ := normalized["choices"].([]any)
	for _, raw := range choices {
		choice := asObject(raw)
		if choice == nil {
			continue
		}
		index := choiceIndex(choice)
		// Choices settled earlier (or everything after [DONE]) drop late
		// bodies just like the original.
		book := a.books[index]
		if a.sawDone || (book != nil && book.finish != "") {
			continue
		}
		if book == nil {
			book = newToolCallBook()
			a.books[index] = book
		}
		delta := asObject(choice["delta"])
		if delta == nil {
			delta = map[string]any{}
		}
		if role, isStr := delta["role"].(string); isStr && role != "" {
			book.role = role
		}
		if content, isStr := delta["content"].(string); isStr {
			book.content.WriteString(content)
		}
		if reasoning, isStr := delta["reasoning_content"].(string); isStr {
			book.reasoning.WriteString(reasoning)
		}
		// The stream path passes unknown fields through; the aggregated
		// response must too, otherwise the same upstream call behaves
		// differently per mode.
		mergePassthrough(book.extra, delta)
		calls, _ := delta["tool_calls"].([]any)
		for _, callRaw := range calls {
			call := asObject(callRaw)
			if call == nil {
				continue
			}
			target := toolCallTarget(book, call)
			if target == nil {
				continue
			}
			if id := callID(call); id != "" {
				target.id = id
				book.callsById[id] = target
			}
			if fn := asObject(call["function"]); fn != nil {
				if name, isStr := fn["name"].(string); isStr && name != "" {
					target.name = name
				}
				if args, isStr := fn["arguments"].(string); isStr {
					target.arguments += args
				}
			}
		}
		if reason, isStr := choice["finish_reason"].(string); isStr && reason != "" {
			book.finish = reason
		}
	}
	return nil
}

func (a *jsonAggregator) finish() (map[string]any, error) {
	// JS: choiceIndexes = keys of the aggregation map; finished = those with
	// a finish_reason. Choice-level, not global, so one early choice cannot
	// swallow the other choices' later finish frames.
	choiceSet := make(map[int]bool, len(a.books))
	finishedSet := make(map[int]bool, len(a.books))
	for index, book := range a.books {
		choiceSet[index] = true
		if book.finish != "" {
			finishedSet[index] = true
		}
	}
	if err := assertCompleted(a.sawDone, choiceSet, finishedSet); err != nil {
		return nil, err
	}
	indices := make([]int, 0, len(a.books))
	for index := range a.books {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	choices := make([]any, 0, len(indices))
	for _, index := range indices {
		book := a.books[index]
		message := map[string]any{
			"role": book.role,
		}
		if content := book.content.String(); content != "" {
			message["content"] = content
		} else {
			message["content"] = nil
		}
		if reasoning := book.reasoning.String(); reasoning != "" {
			message["reasoning_content"] = reasoning
		}
		if len(book.calls) > 0 {
			toolCalls := make([]any, 0, len(book.calls))
			for _, call := range orderedToolCalls(book) {
				toolCalls = append(toolCalls, map[string]any{
					"id":   call.id,
					"type": "function",
					"function": map[string]any{
						"name":      call.name,
						"arguments": call.arguments,
					},
				})
			}
			message["tool_calls"] = toolCalls
		}
		for key, value := range book.extra {
			message[key] = value
		}
		count := len(book.calls)
		finish := book.finish
		if finish == "" {
			finish = "stop"
		}
		choices = append(choices, map[string]any{
			"index":         index,
			"message":       message,
			"finish_reason": normalizeFinish(finish, count),
		})
	}
	created := a.created
	if created == nil {
		created = json.Number(strconv.FormatInt(time.Now().Unix(), 10))
	}
	result := map[string]any{
		"id":      firstNonEmpty(a.id, a.ctx.requestID),
		"object":  "chat.completion",
		"created": created,
		"model":   a.ctx.model,
		"choices": choices,
	}
	if len(a.usage) > 0 {
		result["usage"] = a.usage
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// helpers used across the response layer
// ---------------------------------------------------------------------------

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// ----------------------------------------------------------------------------
// streaming writer (respondStream port)
// ----------------------------------------------------------------------------

// streamEncoder is the write-side port of respondStream's per-frame pipeline:
// finished choice filtering, streaming index normalization, finish rewriting
// and the pending pre-content frame buffer. Encoded output is one complete
// JSON object (or "data: "-framed object) per chunk, matching the host's
// stream forwarder contract (see streamCtx.sseFrame).
type streamEncoder struct {
	ctx      *streamCtx
	started  bool
	sawDone  bool
	saw      map[int]bool
	fin      map[int]bool
	toolIdx  map[int]bool
	books    map[int]*toolCallBook
	pending  [][]byte
	finished bool
}

func newStreamEncoder(ctx *streamCtx) *streamEncoder {
	return &streamEncoder{
		ctx:     ctx,
		saw:     map[int]bool{},
		fin:     map[int]bool{},
		toolIdx: map[int]bool{},
		books:   map[int]*toolCallBook{},
	}
}

// encode turns one parsed frame into zero or more SSE frames. A nil result
// means "buffered or nothing worth sending yet".
func (s *streamEncoder) encode(frame map[string]any) ([]byte, error) {
	if frame == nil {
		s.sawDone = true
		return nil, nil
	}
	normalized := normalizeChunk(frame, s.ctx.model, s.ctx.thinkingEnabled)
	if normalized == nil {
		return nil, nil
	}
	choices, _ := normalized["choices"].([]any)
	// Settled choices (and everything after [DONE]) no longer accept bodies:
	// late upstream out-of-band content must not leak into finished responses.
	usable := make([]any, 0, len(choices))
	for _, raw := range choices {
		choice := asObject(raw)
		if choice == nil {
			continue
		}
		index := choiceIndex(choice)
		if s.sawDone || s.fin[index] {
			continue
		}
		usable = append(usable, choice)
	}
	hasChoices := len(usable) > 0
	hasUsage := UsagePresent(normalized["usage"])
	if !hasChoices && !hasUsage {
		return nil, nil
	}
	for _, raw := range usable {
		choice := raw.(map[string]any)
		index := choiceIndex(choice)
		s.saw[index] = true
		// Tool-call frames always precede the finish frame, so the rewrite
		// decision below is informed per choice; n>1 choices stay independent.
		if delta := asObject(choice["delta"]); delta != nil {
			if calls, ok := delta["tool_calls"].([]any); ok && len(calls) > 0 {
				s.toolIdx[index] = true
				book := s.books[index]
				if book == nil {
					book = newToolCallBook()
					s.books[index] = book
				}
				rewritten, err := normalizeStreamToolCalls(calls, book)
				if err != nil {
					return nil, err
				}
				delta["tool_calls"] = rewritten
			}
		}
		if choice["finish_reason"] != nil {
			if reason, isStr := choice["finish_reason"].(string); isStr && reason != "" {
				s.fin[index] = true
			}
		}
	}
	// finish_reason rewrite must run after the per-choice loop so this
	// chunk's own tool-call deltas count for their own choice.
	for _, raw := range usable {
		choice := raw.(map[string]any)
		reason, isStr := choice["finish_reason"].(string)
		if !isStr || reason == "" {
			continue
		}
		count := 0
		if s.toolIdx[choiceIndex(choice)] {
			count = 1
		}
		choice["finish_reason"] = normalizeFinish(reason, count)
	}
	out := copyObject(normalized)
	out["choices"] = usable
	raw, err := jsonMarshal(out)
	if err != nil {
		return nil, err
	}
	encoded := s.frame(raw)
	// Buffer a few content-less frames before the first body: if the upstream
	// errors right afterwards the caller can still recover a JSON error.
	if !hasContent(out) && !s.started && len(s.pending) < maxPendingFrames {
		s.pending = append(s.pending, encoded)
		return nil, nil
	}
	if len(s.pending) > 0 {
		flushed := bytes.Join(s.pending, nil)
		flushed = append(flushed, encoded...)
		s.pending = nil
		s.started = true
		return flushed, nil
	}
	s.started = true
	return encoded, nil
}

// frame wraps one JSON object for the wire: bare for the native chat-completions
// writer (which adds "data: ...\n\n" itself) or "data: " framed for the
// cross-format translators.
func (s *streamEncoder) frame(raw []byte) []byte {
	if s.ctx.sseFrame {
		return append([]byte("data: "), raw...)
	}
	return raw
}

// finish is called once the parser completes: it asserts completion the same
// way both modes do. It emits no [DONE]: the host appends its own stream
// terminator (emitTranslatedExecutorStreamTail for chat-completions, or the
// cross-format translator's terminator otherwise).
func (s *streamEncoder) finish() ([]byte, error) {
	choiceSet := s.saw
	finishedSet := s.fin
	if err := assertCompleted(s.sawDone, choiceSet, finishedSet); err != nil {
		return nil, err
	}
	out := bytes.Join(s.pending, nil)
	s.pending = nil
	return out, nil
}

// normalizeStreamToolCalls ports the JS function: each fragment's downstream
// index is assigned once and never changed afterwards; late-arriving real
// indexes only locate the same target. Standard indexed streams keep their
// value; colliding indexes get free slots so the client never merges two
// calls into one.
func normalizeStreamToolCalls(calls []any, book *toolCallBook) ([]any, error) {
	out := make([]any, 0, len(calls))
	for _, raw := range calls {
		call := asObject(raw)
		if call == nil {
			out = append(out, raw)
			continue
		}
		target := toolCallTarget(book, call)
		if target == nil {
			continue
		}
		if id := callID(call); id != "" {
			target.id = id
			book.callsById[id] = target
		}
		if !target.streamIndexAssigned {
			index, has := callIndexInt(call)
			canKeep := has && index >= 0 && !book.streamIndexes[index]
			if canKeep {
				target.streamIndex = index
			} else {
				target.streamIndex = book.nextIndex
			}
			book.streamIndexes[target.streamIndex] = true
			for book.streamIndexes[book.nextIndex] {
				book.nextIndex++
			}
			target.streamIndexAssigned = true
		}
		rewrite := copyObject(call)
		rewrite["index"] = target.streamIndex
		out = append(out, rewrite)
	}
	return out, nil
}
