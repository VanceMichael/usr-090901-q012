# 可疑取现协同止付时限服务

银行或反诈中心提交风险登记、联系回执和处置决定后，服务为每笔可疑大额取现案件
维护一张按紧迫度排序的工作单：`next_action`、`due_at`、负责机构、阻塞材料与
逾期原因一目了然。处置期限和角色范围均以本地夹具为准，开发环境不连接银行核心
或警务系统。

## 快速开始

```sh
# 基础资料校验（夹具完整性）
docker compose run --rm --no-deps scaffold-check

# 完整验收：从空数据库出发，依次验证七项行为（见下文）
scripts/acceptance.sh

# 日常开发：启动 PostgreSQL 与服务
docker compose up -d postgres app
```

验收脚本 `scripts/acceptance.sh` 从空数据库出发，依次验证：

1. **重复回执**：同一 `receipt_id` 重复送达返回 `duplicate`，不产生第二项工作；
2. **工作时间边界**：跨午夜与节假日时限按夹具日历顺延（含 `HOLIDAY_ROLLOVER`）；
3. **材料阻塞**：必需材料未齐时复核被阻塞，阻塞期间处置决定被拒绝；
4. **越权决定**：权限不足的决定返回 `422 ROLE_NOT_ALLOWED`，工作单不变；
5. **期限重算**：材料补齐触发期限重算，重算依据可在单案解释中查阅；
6. **原子失败**：注入故障时原始回执、工作单、重算依据三者一起回滚；
7. **重启后队列顺序**：重启应用容器后，工作单队列顺序与积压汇总保持不变。

## 接口

所有时间均为 RFC3339；读取接口接受可选 `?now=` 参数（验收/演练用固定时钟）。
敏感字段按 `X-Actor-Role` 请求头裁剪（见下文）。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/receipts` | 提交回执（契约见 `contracts/request.schema.json`） |
| GET | `/worklist?org=&limit=&now=` | 即将到期队列，按紧迫度排序 |
| GET | `/cases/{id}` | 单案详情（含当前工作单与回执历史） |
| GET | `/cases/{id}/deadline-explanation` | 单案时限解释：每次重算的起算点、营业小时、分段与跳过日 |
| GET | `/backlog?now=` | 机构积压汇总（开放数、逾期数、最近截止） |
| POST | `/cases/{id}/simulate-material` | 模拟补齐某份材料后的工作单变化（不落库） |
| GET | `/healthz` | 健康检查 |

### 回执类型与状态机

`risk_registered`（风险登记）→ `customer_contacted`（联系回执）→ 复核材料 →
`decision_release` / `decision_escalate`（解除/升级止付，结案）。任何回执都可
携带 `material_codes` 补充材料；`material_submitted` 专门用于补材料。材料在
联系客户时仍未齐的，案件停留在复核阶段并列出 `blocking_materials`；补齐最后
一份材料即进入处置决定阶段并以其回执发生时刻重算期限。

### 响应状态

- `200 applied`：已受理；`200 duplicate`：重复送达（幂等，返回已存结果）；
- `422 rejected_role`：角色无权提交该类型（`ROLE_NOT_ALLOWED`），回执留痕但工作单不变；
- `409 rejected_state`：状态冲突（如 `MATERIAL_BLOCKED`、`CASE_NOT_FOUND`、`CASE_CLOSED`）；
- `400 invalid_request`：不符合请求契约（字段缺失、多余字段、非法枚举等）。

## 时限规则（夹具）

`fixtures/rules.json` 定义：工作日周一至周五 09:00–17:00（`Asia/Shanghai`），
节假日顺延；时限按营业小时累计，跨午夜自动滚入下一工作日。

| 阶段 | 下一动作 | 负责机构 | 营业小时（高/中/低） |
| --- | --- | --- | --- |
| 联系客户 | `contact_customer` | `branch_outlet` | 2 / 4 / 8 |
| 复核材料 | `review_materials` | `bank_backoffice` | 4 / 8 / 16 |
| 处置决定 | `decide_release_or_escalate` | `anti_fraud_center` | 8 / 24 / 48 |

动作权限：`decision_release` 仅 `anti_fraud_officer`；`decision_escalate` 允许
`bank_reviewer` 与 `anti_fraud_officer`；详见夹具 `permissions`。

## 持久化与原子性

PostgreSQL 三张表在同一事务写入，失败整体回滚：

- `receipts`：原始回执（`receipt_id` 主键即幂等键，含受理/拒绝结果）；
- `worklist`：当前工作单（每案件至多一条开放条目）；
- `recalc_log`：每次期限重算依据（起算点、营业小时、分段、跳过日、是否节假日顺延）。

`cases` 表保存案件当前状态与脱敏交易快照（来自 `fixtures/transactions.json`）。

## 敏感字段裁剪

| 角色 | 金额 | 联系方式 |
| --- | --- | --- |
| `teller` | 分档标签（如「100万-500万」） | 隐藏 |
| `bank_reviewer` | 全量 | 打码（保留前4后2） |
| `anti_fraud_officer` | 全量 | 全量 |
| 其他/未提供 | 隐藏 | 隐藏 |

## 故障注入（仅供验收）

设置 `APP_FAULT_INJECTION=1` 后，`POST /receipts` 携带
`X-Fault-Inject: rollback` 会在三张表全部写入之后、提交之前强制回滚，
用于验证原子性。生产部署请勿开启。

## 本地开发

```sh
# 单元测试（时限计算、状态机、脱敏）
go test ./...

# 目录结构
#   cmd/server       服务入口        cmd/acceptance  验收程序
#   internal/domain  纯函数领域层    internal/store  PostgreSQL 持久化
#   internal/httpapi HTTP 接口       fixtures/       规则与脱敏交易夹具
```

依赖已全部 vendor（`vendor/`），构建无需访问网络。
