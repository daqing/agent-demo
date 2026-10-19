# agent-demo

A minimal agent tool-calling demo built on
[motionloop](https://github.com/daqing/motionloop). The agent loop, tool
dispatch, hooks, and transcript all come from motionloop; only the LLM is a
scripted provider, so the demo is deterministic and needs no API key.

## Scenarios

1. **Authorized tool executes.** The agent calls `weather`, which is on the
   allowlist. The tool runs once, returns its result, and the assistant
   answers using that result.
2. **Unauthorized tool is refused.** The agent calls `delete_records`, which
   is not on the allowlist. The call is blocked before execution, recorded
   as an error result, and the tool body never runs.
3. **Tool failure stops execution.** The agent calls `payment_settlement`,
   whose charge step fails. The error is returned as a failed tool result
   and recorded in the transcript; the settlement's notify step and the
   agent's scripted follow-up tool call are never executed.

## Run

Requires Go 1.27+.

```bash
go run .
```

Exit code 0 means every check passed; 1 means at least one check failed.

## How the requirements are met

| Requirement | Implementation |
|---|---|
| Authorized tool executes | `weatherTool.Execute` runs and returns a mock forecast. |
| Unauthorized tool is refused | `allowlist.BeforeToolCall` returns `Block: true`. Motionloop invokes this hook after argument validation and before `Tool.Execute`, so `deleteTool.Execute` never runs (0 executions reported). |
| Failure status is returned and recorded | `paymentTool.Execute` returns an error; motionloop converts it into an `IsError` tool result in the transcript. |
| No follow-up actions after failure | The payment tool returns immediately at the failed charge step (`notify = 0`), and `stopAfterError` sets `Terminate` in the `AfterToolCall` hook, ending the run before another LLM request. The scripted follow-up call is never reached (`weather` runs 0 times, 1 provider request). |

The unauthorized refusal is also an error tool result, but the run continues
so the assistant can explain the denial (2 provider requests); only the
blocked tool call itself is stopped.

## Files

- `main.go` — the three scenarios, mock tools, authorization policy, and
  self-checks.
- `go.mod` — pins motionloop to the develop commit used for this demo
  (`v0.2.1-0.20260927080952-ed9cf00afce5`).

## Expected output

```text
=== 场景一：已授权工具正常执行 ===
  助手：我先调用工具确认一下。
  调用工具：weather {"city":"深圳"}
  工具结果[成功]：深圳今天 29°C，天气晴朗。
  助手：深圳今天 29°C，天气晴朗。
  检查通过：weather 工具执行了 1 次（期望 1 次）
  检查通过：工具结果状态 = 成功（期望：成功）
  检查通过：助手回答引用了工具返回的结果
通过

=== 场景二：未授权工具被拒绝 ===
  助手：我先调用工具确认一下。
  调用工具：delete_records {"table":"users"}
  工具结果[失败]：工具 "delete_records" 未授权给当前 agent
  助手：无法删除记录：delete_records 工具未授权。
  检查通过：delete_records 工具执行了 0 次（期望 0 次：工具函数不得运行）
  检查通过：工具结果状态 = 失败（期望：失败）
  检查通过：拒绝原因已写入会话记录
  检查通过：拒绝后 agent 继续运行（模型请求 2 次，期望 2 次）
通过

=== 场景三：工具异常终止后续动作 ===
  助手：我先调用工具确认一下。
  调用工具：payment_settlement {"orderId":"ord-42","amount":199}
  工具结果[失败]：结算订单 ord-42 失败：模拟支付网关超时
  检查通过：失败前的步骤已执行：reserve=1，charge=1
  检查通过：失败后的步骤未执行：notify=0（期望 0）
  检查通过：工具结果状态 = 失败（期望：失败）
  检查通过：错误已写入会话记录
  检查通过：后续工具未执行：weather 工具执行了 0 次（期望 0 次）
  检查通过：未再发生 agent 轮次（模型请求 1 次，期望 1 次）
通过

全部场景通过。
```
