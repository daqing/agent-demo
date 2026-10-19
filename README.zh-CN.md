# agent-demo

一个基于 [motionloop](https://github.com/daqing/motionloop) 的最小 Agent 工具调用演示。
Agent 循环、工具调度、hook 和会话记录都来自 motionloop，只有 LLM 使用脚本化
provider，因此演示是确定性的，不需要 API key。

## 三个场景

1. **调用已授权工具并正常执行。** Agent 调用白名单中的 `weather`，工具执行一次并
   返回结果，assistant 基于该结果作答。
2. **调用未授权工具被拒绝。** Agent 调用不在白名单中的 `delete_records`。该调用在
   执行前被拦截，记录为错误结果，工具函数本体不会运行。
3. **工具执行异常并终止后续动作。** Agent 调用 `payment_settlement`，其扣款步骤
   失败。错误作为失败的工具结果返回并记录到会话记录中；结算工具的后续通知步骤
   以及脚本化的后续工具调用都不会执行。

## 运行

需要 Go 1.27+。

```bash
go run .
```

退出码 0 表示全部检查通过，1 表示至少有一项检查失败。

## 需求对应关系

| 需求 | 实现 |
|---|---|
| 已授权工具正常执行 | `weatherTool.Execute` 正常执行并返回模拟天气。 |
| 未授权工具拒绝执行 | `allowlist.BeforeToolCall` 返回 `Block: true`。motionloop 在参数校验之后、`Tool.Execute` 之前调用该 hook，因此 `deleteTool.Execute` 不会运行（执行 0 次）。 |
| 返回失败状态并记录错误 | `paymentTool.Execute` 返回 error，motionloop 将其转换为带 `IsError` 标记的 toolResult 写入会话记录。 |
| 失败后不继续执行后续动作 | 支付工具在扣款失败处立即返回（`notify = 0`）；`stopAfterError` 在 `AfterToolCall` hook 中设置 `Terminate`，使本轮 run 在下一次 LLM 请求前结束。脚本化的后续调用不会发生（weather 工具执行 0 次、模型请求 1 次）。 |

未授权调用的拒绝同样会生成错误 toolResult，但 run 会继续，以便 assistant 向用户
解释拒绝原因（模型请求 2 次）；被终止的只有这一次工具调用。

## 文件

- `main.go` —— 三个场景、模拟工具、授权策略和自检逻辑。
- `go.mod` —— motionloop 固定到本演示使用的 develop 提交
  （`v0.2.1-0.20260927080952-ed9cf00afce5`）。

## 预期输出

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
