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
		{"场景一：已授权工具正常执行", runAuthorized},
		{"场景二：未授权工具被拒绝", runUnauthorized},
		{"场景三：工具异常终止后续动作", runFailure},
	}

	failed := false
	for _, s := range scenarios {
		fmt.Printf("\n=== %s ===\n", s.name)
		if err := s.run(); err != nil {
			fmt.Printf("失败：%v\n", err)
			failed = true
			continue
		}
		fmt.Println("通过")
	}

	if failed {
		fmt.Println("\n部分场景未通过。")
		os.Exit(1)
	}
	fmt.Println("\n全部场景通过。")
}

func runAuthorized() error {
	weather := &weatherTool{}
	provider := agent.NewFakeProvider(
		fakeToolCallEvents("call_weather", "weather", `{"city":"深圳"}`),
		agent.FakeTextEvents("深圳今天 29°C，天气晴朗。"),
	)
	a := newAgent(provider,
		[]agent.Tool{weather, &deleteTool{}, &paymentTool{}},
		allowlist{"weather": true},
	)
	trace := attachTrace(a)

	if err := a.Prompt(context.Background(), "深圳今天天气怎么样？"); err != nil {
		return err
	}

	c := &checks{}
	ends := trace.toolEnds()
	c.require(weather.executions.Load() == 1,
		"weather 工具执行了 %d 次（期望 1 次）", weather.executions.Load())
	c.require(len(ends) == 1 && !ends[0].IsError,
		"工具结果状态 = %s（期望：成功）", statusOf(ends))
	c.require(strings.Contains(lastAssistantText(a.Messages()), "29°C"),
		"助手回答引用了工具返回的结果")
	return c.err()
}

func runUnauthorized() error {
	del := &deleteTool{}
	provider := agent.NewFakeProvider(
		fakeToolCallEvents("call_delete", "delete_records", `{"table":"users"}`),
		agent.FakeTextEvents("无法删除记录：delete_records 工具未授权。"),
	)
	a := newAgent(provider,
		[]agent.Tool{&weatherTool{}, del, &paymentTool{}},
		allowlist{"weather": true, "payment_settlement": true},
	)
	trace := attachTrace(a)

	if err := a.Prompt(context.Background(), "删除 users 表中的所有数据。"); err != nil {
		return err
	}

	c := &checks{}
	ends := trace.toolEnds()
	c.require(del.executions.Load() == 0,
		"delete_records 工具执行了 %d 次（期望 0 次：工具函数不得运行）", del.executions.Load())
	c.require(len(ends) == 1 && ends[0].IsError,
		"工具结果状态 = %s（期望：失败）", statusOf(ends))

	result, ok := findToolResult(a.Messages(), "call_delete")
	c.require(ok && result.IsError && strings.Contains(blocksText(result.Content), "未授权"),
		"拒绝原因已写入会话记录")
	c.require(len(provider.Calls()) == 2,
		"拒绝后 agent 继续运行（模型请求 %d 次，期望 2 次）", len(provider.Calls()))
	return c.err()
}

func runFailure() error {
	payment := &paymentTool{}
	weather := &weatherTool{}
	provider := agent.NewFakeProvider(
		fakeToolCallEvents("call_pay", "payment_settlement", `{"orderId":"ord-42","amount":199}`),
		fakeToolCallEvents("call_after", "weather", `{"city":"深圳"}`),
		agent.FakeTextEvents("不应执行到这里"),
	)
	a := newAgent(provider,
		[]agent.Tool{weather, &deleteTool{}, payment},
		allowlist{"weather": true, "payment_settlement": true},
	)
	trace := attachTrace(a)

	if err := a.Prompt(context.Background(), "结算订单 ord-42。"); err != nil {
		return err
	}

	c := &checks{}
	ends := trace.toolEnds()
	c.require(payment.reserved.Load() == 1 && payment.charged.Load() == 1,
		"失败前的步骤已执行：reserve=%d，charge=%d",
		payment.reserved.Load(), payment.charged.Load())
	c.require(payment.notified.Load() == 0,
		"失败后的步骤未执行：notify=%d（期望 0）", payment.notified.Load())
	c.require(len(ends) == 1 && ends[0].IsError,
		"工具结果状态 = %s（期望：失败）", statusOf(ends))

	result, ok := findToolResult(a.Messages(), "call_pay")
	c.require(ok && result.IsError && strings.Contains(blocksText(result.Content), "模拟支付网关超时"),
		"错误已写入会话记录")
	c.require(weather.executions.Load() == 0,
		"后续工具未执行：weather 工具执行了 %d 次（期望 0 次）", weather.executions.Load())
	c.require(len(provider.Calls()) == 1,
		"未再发生 agent 轮次（模型请求 %d 次，期望 1 次）", len(provider.Calls()))
	return c.err()
}

func newAgent(provider *agent.FakeProvider, tools []agent.Tool, allowed allowlist) *agent.Agent {
	return agent.New(provider, llm.Model{ProviderID: "fake", ModelID: "scripted"},
		agent.WithSystemPrompt("你是一个最小化的工具调用演示 agent。按需调用提供的工具，并如实汇报执行结果。"),
		agent.WithTools(tools...),
		agent.WithBeforeToolCall(allowed.BeforeToolCall),
		agent.WithAfterToolCall(stopAfterError),
	)
}

func fakeToolCallEvents(id, name, args string) []llm.StreamEvent {
	return []llm.StreamEvent{
		llm.TextDelta{Delta: "我先调用工具确认一下。"},
		llm.ToolCallDelta{Index: 0, ID: id, Name: name},
		llm.ToolCallDelta{Index: 0, ArgumentsDelta: args},
		llm.Stop{Reason: llm.StopReasonToolUse},
	}
}

type allowlist map[string]bool

func (a allowlist) BeforeToolCall(_ context.Context, call agent.ToolCall) agent.BeforeToolCallResult {
	if a[call.Name] {
		return agent.BeforeToolCallResult{}
	}
	return agent.BeforeToolCallResult{
		Block:  true,
		Reason: fmt.Sprintf("工具 %q 未授权给当前 agent", call.Name),
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
	City string `json:"city" jsonschema:"required,description=要查询的城市"`
}

type weatherTool struct{ executions atomic.Int64 }

func (*weatherTool) Name() string        { return "weather" }
func (*weatherTool) Description() string { return "返回城市的模拟天气。" }
func (*weatherTool) Parameters() *agent.Schema {
	return agent.MustSchemaFor(&weatherParams{})
}

func (t *weatherTool) Execute(_ context.Context, call agent.ToolCall, _ func(agent.Update)) (agent.Result, error) {
	var p weatherParams
	if err := json.Unmarshal(call.Arguments, &p); err != nil {
		return agent.Result{}, fmt.Errorf("解析 weather 工具参数失败：%w", err)
	}
	t.executions.Add(1)
	return agent.Result{
		Content: []llm.ContentBlock{llm.TextBlock{Text: fmt.Sprintf("%s今天 29°C，天气晴朗。", p.City)}},
	}, nil
}

type deleteParams struct {
	Table string `json:"table" jsonschema:"required,description=要删除的表"`
}

type deleteTool struct{ executions atomic.Int64 }

func (*deleteTool) Name() string        { return "delete_records" }
func (*deleteTool) Description() string { return "删除表中的所有行。" }
func (*deleteTool) Parameters() *agent.Schema {
	return agent.MustSchemaFor(&deleteParams{})
}

func (t *deleteTool) Execute(_ context.Context, call agent.ToolCall, _ func(agent.Update)) (agent.Result, error) {
	var p deleteParams
	if err := json.Unmarshal(call.Arguments, &p); err != nil {
		return agent.Result{}, fmt.Errorf("解析 delete_records 工具参数失败：%w", err)
	}
	t.executions.Add(1)
	return agent.Result{
		Content: []llm.ContentBlock{llm.TextBlock{Text: fmt.Sprintf("已删除 %s 表中的所有行。", p.Table)}},
	}, nil
}

type paymentParams struct {
	OrderID string `json:"orderId" jsonschema:"required,description=要结算的订单"`
	Amount  int    `json:"amount" jsonschema:"required,description=金额（分）"`
}

type paymentTool struct {
	reserved atomic.Int64
	charged  atomic.Int64
	notified atomic.Int64
}

func (*paymentTool) Name() string        { return "payment_settlement" }
func (*paymentTool) Description() string { return "通过模拟支付网关结算订单。" }
func (*paymentTool) Parameters() *agent.Schema {
	return agent.MustSchemaFor(&paymentParams{})
}

func (t *paymentTool) Execute(_ context.Context, call agent.ToolCall, _ func(agent.Update)) (agent.Result, error) {
	var p paymentParams
	if err := json.Unmarshal(call.Arguments, &p); err != nil {
		return agent.Result{}, fmt.Errorf("解析 payment_settlement 工具参数失败：%w", err)
	}

	t.reserved.Add(1)
	t.charged.Add(1)
	if err := chargeCard(); err != nil {
		return agent.Result{}, fmt.Errorf("结算订单 %s 失败：%w", p.OrderID, err)
	}

	t.notified.Add(1)
	return agent.Result{
		Content: []llm.ContentBlock{llm.TextBlock{Text: fmt.Sprintf("订单 %s 已结算，金额 %d 分。", p.OrderID, p.Amount)}},
	}, nil
}

func chargeCard() error {
	return errors.New("模拟支付网关超时")
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
			fmt.Printf("  调用工具：%s %s\n", e.ToolName, string(e.Arguments))
		case agent.ToolExecutionEnd:
			status := "成功"
			if e.IsError {
				status = "失败"
			}
			fmt.Printf("  工具结果[%s]：%s\n", status, blocksText(e.Result.Content))
			t.ends = append(t.ends, e)
		case agent.MessageEnd:
			if e.Message.Role == llm.RoleAssistant {
				fmt.Printf("  助手：%s\n", blocksText(e.Message.Content))
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
		fmt.Printf("  检查通过：%s\n", msg)
		return
	}
	fmt.Printf("  检查失败：%s\n", msg)
	c.failures = append(c.failures, msg)
}

func (c *checks) err() error {
	if len(c.failures) == 0 {
		return nil
	}
	return fmt.Errorf("有 %d 项检查失败", len(c.failures))
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
		return "缺失"
	}
	if ends[0].IsError {
		return "失败"
	}
	return "成功"
}
