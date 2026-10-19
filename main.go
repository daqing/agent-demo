package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/daqing/motionloop/agent"
	"github.com/daqing/motionloop/llm"
)

func main() {
	scenarios := []struct {
		name string
		run  func() error
	}{
		{"Scenario 1: authorized tool executes", runAuthorized},
		{"Scenario 2: unauthorized tool is refused", runUnauthorized},
		{"Scenario 3: tool failure stops execution", runFailure},
	}

	failed := false
	for _, s := range scenarios {
		fmt.Printf("\n=== %s ===\n", s.name)
		if err := s.run(); err != nil {
			fmt.Printf("FAIL: %v\n", err)
			failed = true
			continue
		}
		fmt.Println("PASS")
	}

	if failed {
		fmt.Println("\nSome scenarios failed.")
		os.Exit(1)
	}
	fmt.Println("\nAll scenarios passed.")
}

func runAuthorized() error {
	weather := &weatherTool{}
	provider := agent.NewFakeProvider(
		agent.FakeToolCallEvents("call_weather", "weather", `{"city":"Shenzhen"}`),
		agent.FakeTextEvents("Shenzhen is 29°C and clear."),
	)
	a := newAgent(provider,
		[]agent.Tool{weather, &deleteTool{}, &paymentTool{}},
		allowlist{"weather": true},
	)
	trace := attachTrace(a)

	if err := a.Prompt(context.Background(), "What is the weather in Shenzhen?"); err != nil {
		return err
	}

	c := &checks{}
	ends := trace.toolEnds()
	c.require(weather.executions.Load() == 1,
		"weather executions = %d (want 1)", weather.executions.Load())
	c.require(len(ends) == 1 && !ends[0].IsError,
		"tool result status = %s (want ok)", statusOf(ends))
	c.require(strings.Contains(lastAssistantText(a.Messages()), "29°C"),
		"assistant answer uses the tool result")
	return c.err()
}

func runUnauthorized() error {
	del := &deleteTool{}
	provider := agent.NewFakeProvider(
		agent.FakeToolCallEvents("call_delete", "delete_records", `{"table":"users"}`),
		agent.FakeTextEvents("I cannot delete records because delete_records is not authorized."),
	)
	a := newAgent(provider,
		[]agent.Tool{&weatherTool{}, del, &paymentTool{}},
		allowlist{"weather": true, "payment_settlement": true},
	)
	trace := attachTrace(a)

	if err := a.Prompt(context.Background(), "Delete every row in the users table."); err != nil {
		return err
	}

	c := &checks{}
	ends := trace.toolEnds()
	c.require(del.executions.Load() == 0,
		"delete_records executions = %d (want 0: tool body must not run)", del.executions.Load())
	c.require(len(ends) == 1 && ends[0].IsError,
		"tool result status = %s (want error)", statusOf(ends))

	result, ok := findToolResult(a.Messages(), "call_delete")
	c.require(ok && result.IsError && strings.Contains(blocksText(result.Content), "not authorized"),
		"refusal recorded in the transcript")
	c.require(len(provider.Calls()) == 2,
		"agent continued after the refusal (provider requests = %d, want 2)", len(provider.Calls()))
	return c.err()
}

func runFailure() error {
	payment := &paymentTool{}
	weather := &weatherTool{}
	provider := agent.NewFakeProvider(
		agent.FakeToolCallEvents("call_pay", "payment_settlement", `{"orderId":"ord-42","amount":199}`),
		agent.FakeToolCallEvents("call_after", "weather", `{"city":"Shenzhen"}`),
		agent.FakeTextEvents("should not be reached"),
	)
	a := newAgent(provider,
		[]agent.Tool{weather, &deleteTool{}, payment},
		allowlist{"weather": true, "payment_settlement": true},
	)
	trace := attachTrace(a)

	if err := a.Prompt(context.Background(), "Settle order ord-42."); err != nil {
		return err
	}

	c := &checks{}
	ends := trace.toolEnds()
	c.require(payment.reserved.Load() == 1 && payment.charged.Load() == 1,
		"steps before the failure ran: reserve=%d charge=%d",
		payment.reserved.Load(), payment.charged.Load())
	c.require(payment.notified.Load() == 0,
		"step after the failure did not run: notify=%d (want 0)", payment.notified.Load())
	c.require(len(ends) == 1 && ends[0].IsError,
		"tool result status = %s (want error)", statusOf(ends))

	result, ok := findToolResult(a.Messages(), "call_pay")
	c.require(ok && result.IsError && strings.Contains(blocksText(result.Content), "mock payment gateway timeout"),
		"error recorded in the transcript")
	c.require(weather.executions.Load() == 0,
		"follow-up tool did not execute: weather executions = %d (want 0)", weather.executions.Load())
	c.require(len(provider.Calls()) == 1,
		"no further agent turn: provider requests = %d (want 1)", len(provider.Calls()))
	return c.err()
}

func newAgent(provider *agent.FakeProvider, tools []agent.Tool, allowed allowlist) *agent.Agent {
	return agent.New(provider, llm.Model{ProviderID: "fake", ModelID: "scripted"},
		agent.WithSystemPrompt("You are a minimal tool-calling demo agent. Use the provided tools and report their results."),
		agent.WithTools(tools...),
		agent.WithBeforeToolCall(allowed.BeforeToolCall),
		agent.WithAfterToolCall(stopAfterError),
	)
}

type allowlist map[string]bool

func (a allowlist) BeforeToolCall(_ context.Context, call agent.ToolCall) agent.BeforeToolCallResult {
	if a[call.Name] {
		return agent.BeforeToolCallResult{}
	}
	return agent.BeforeToolCallResult{
		Block:  true,
		Reason: fmt.Sprintf("tool %q is not authorized for this agent", call.Name),
	}
}

func stopAfterError(_ context.Context, _ agent.ToolCall, _ agent.Result, isError bool) agent.AfterToolCallResult {
	if !isError {
		return agent.AfterToolCallResult{}
	}
	terminate := true
	return agent.AfterToolCallResult{Terminate: &terminate}
}

type weatherParams struct {
	City string `json:"city" jsonschema:"required,description=City to look up"`
}

type weatherTool struct{ executions atomic.Int64 }

func (*weatherTool) Name() string        { return "weather" }
func (*weatherTool) Description() string { return "Return the mock weather for a city." }
func (*weatherTool) Parameters() *agent.Schema {
	return agent.MustSchemaFor(&weatherParams{})
}

func (t *weatherTool) Execute(_ context.Context, call agent.ToolCall, _ func(agent.Update)) (agent.Result, error) {
	var p weatherParams
	if err := json.Unmarshal(call.Arguments, &p); err != nil {
		return agent.Result{}, fmt.Errorf("parse weather arguments: %w", err)
	}
	t.executions.Add(1)
	return agent.Result{
		Content: []llm.ContentBlock{llm.TextBlock{Text: fmt.Sprintf("%s: 29°C, clear", p.City)}},
	}, nil
}

type deleteParams struct {
	Table string `json:"table" jsonschema:"required,description=Table to delete"`
}

type deleteTool struct{ executions atomic.Int64 }

func (*deleteTool) Name() string        { return "delete_records" }
func (*deleteTool) Description() string { return "Delete every row from a table." }
func (*deleteTool) Parameters() *agent.Schema {
	return agent.MustSchemaFor(&deleteParams{})
}

func (t *deleteTool) Execute(_ context.Context, call agent.ToolCall, _ func(agent.Update)) (agent.Result, error) {
	var p deleteParams
	if err := json.Unmarshal(call.Arguments, &p); err != nil {
		return agent.Result{}, fmt.Errorf("parse delete arguments: %w", err)
	}
	t.executions.Add(1)
	return agent.Result{
		Content: []llm.ContentBlock{llm.TextBlock{Text: fmt.Sprintf("deleted all rows from %s", p.Table)}},
	}, nil
}

type paymentParams struct {
	OrderID string `json:"orderId" jsonschema:"required,description=Order being settled"`
	Amount  int    `json:"amount" jsonschema:"required,description=Amount in cents"`
}

type paymentTool struct {
	reserved atomic.Int64
	charged  atomic.Int64
	notified atomic.Int64
}

func (*paymentTool) Name() string        { return "payment_settlement" }
func (*paymentTool) Description() string { return "Settle one order through the mock payment gateway." }
func (*paymentTool) Parameters() *agent.Schema {
	return agent.MustSchemaFor(&paymentParams{})
}

func (t *paymentTool) Execute(_ context.Context, call agent.ToolCall, _ func(agent.Update)) (agent.Result, error) {
	var p paymentParams
	if err := json.Unmarshal(call.Arguments, &p); err != nil {
		return agent.Result{}, fmt.Errorf("parse payment arguments: %w", err)
	}

	t.reserved.Add(1)
	t.charged.Add(1)
	if err := chargeCard(); err != nil {
		return agent.Result{}, fmt.Errorf("settle order %s: %w", p.OrderID, err)
	}

	t.notified.Add(1)
	return agent.Result{
		Content: []llm.ContentBlock{llm.TextBlock{Text: fmt.Sprintf("settled order %s for %d cents", p.OrderID, p.Amount)}},
	}, nil
}

func chargeCard() error {
	return errors.New("mock payment gateway timeout")
}

type traceLog struct {
	mu   sync.Mutex
	ends []agent.ToolExecutionEnd
}

func attachTrace(a *agent.Agent) *traceLog {
	t := &traceLog{}
	a.Subscribe(func(ev agent.Event) {
		t.mu.Lock()
		defer t.mu.Unlock()
		switch e := ev.(type) {
		case agent.ToolExecutionStart:
			fmt.Printf("  tool call: %s %s\n", e.ToolName, string(e.Arguments))
		case agent.ToolExecutionEnd:
			status := "ok"
			if e.IsError {
				status = "error"
			}
			fmt.Printf("  tool result [%s]: %s\n", status, blocksText(e.Result.Content))
			t.ends = append(t.ends, e)
		case agent.MessageEnd:
			if e.Message.Role == llm.RoleAssistant {
				fmt.Printf("  assistant: %s\n", blocksText(e.Message.Content))
			}
		}
	})
	return t
}

func (t *traceLog) toolEnds() []agent.ToolExecutionEnd {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]agent.ToolExecutionEnd(nil), t.ends...)
}

type checks struct {
	failures []string
}

func (c *checks) require(ok bool, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if ok {
		fmt.Printf("  check ok: %s\n", msg)
		return
	}
	fmt.Printf("  check FAILED: %s\n", msg)
	c.failures = append(c.failures, msg)
}

func (c *checks) err() error {
	if len(c.failures) == 0 {
		return nil
	}
	return fmt.Errorf("%d check(s) failed", len(c.failures))
}

func findToolResult(msgs []llm.Message, callID string) (llm.Message, bool) {
	for _, m := range msgs {
		if m.Role == llm.RoleToolResult && m.ToolCallID == callID {
			return m, true
		}
	}
	return llm.Message{}, false
}

func lastAssistantText(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleAssistant {
			return blocksText(msgs[i].Content)
		}
	}
	return ""
}

func blocksText(blocks []llm.ContentBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		if tb, ok := b.(llm.TextBlock); ok {
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}

func statusOf(ends []agent.ToolExecutionEnd) string {
	if len(ends) == 0 {
		return "missing"
	}
	if ends[0].IsError {
		return "error"
	}
	return "ok"
}
