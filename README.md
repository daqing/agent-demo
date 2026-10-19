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
| Unauthorized tool is refused | `allowlist.BeforeToolCall` returns `Block: true`. Motionloop invokes this hook after argument validation and before `Tool.Execute`, so `deleteTool.Execute` never runs (`executions = 0`). |
| Failure status is returned and recorded | `paymentTool.Execute` returns an error; motionloop converts it into an `IsError` tool result in the transcript. |
| No follow-up actions after failure | The payment tool returns immediately at the failed charge step (`notify = 0`), and `stopAfterError` sets `Terminate` in the `AfterToolCall` hook, ending the run before another LLM request. The scripted follow-up call is never reached (`weather executions = 0`, `provider requests = 1`). |

The unauthorized refusal is also an error tool result, but the run continues
so the assistant can explain the denial (`provider requests = 2`); only the
blocked tool call itself is stopped.

## Files

- `main.go` — the three scenarios, mock tools, authorization policy, and
  self-checks.
- `go.mod` — pins motionloop to the develop commit used for this demo
  (`v0.2.1-0.20260927080952-ed9cf00afce5`).

## Expected output

```text
=== Scenario 1: authorized tool executes ===
  assistant: let me check
  tool call: weather {"city":"Shenzhen"}
  tool result [ok]: Shenzhen: 29°C, clear
  assistant: Shenzhen is 29°C and clear.
  check ok: weather executions = 1 (want 1)
  check ok: tool result status = ok (want ok)
  check ok: assistant answer uses the tool result
PASS

=== Scenario 2: unauthorized tool is refused ===
  assistant: let me check
  tool call: delete_records {"table":"users"}
  tool result [error]: tool "delete_records" is not authorized for this agent
  assistant: I cannot delete records because delete_records is not authorized.
  check ok: delete_records executions = 0 (want 0: tool body must not run)
  check ok: tool result status = error (want error)
  check ok: refusal recorded in the transcript
  check ok: agent continued after the refusal (provider requests = 2, want 2)
PASS

=== Scenario 3: tool failure stops execution ===
  assistant: let me check
  tool call: payment_settlement {"orderId":"ord-42","amount":199}
  tool result [error]: settle order ord-42: mock payment gateway timeout
  check ok: steps before the failure ran: reserve=1 charge=1
  check ok: step after the failure did not run: notify=0 (want 0)
  check ok: tool result status = error (want error)
  check ok: error recorded in the transcript
  check ok: follow-up tool did not execute: weather executions = 0 (want 0)
  check ok: no further agent turn: provider requests = 1 (want 1)
PASS

All scenarios passed.
```
