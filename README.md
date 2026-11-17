# IoT Alert Agent

> 设备告警 → LLM 分诊 → 工单摘要/处置建议 → 人工确认 → 评测回归。
> 一个用 **Go + Gin + MCP** 实现的、可完整演示的 Agent 闭环项目。

![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go)
![MCP](https://img.shields.io/badge/MCP-stdio-5EEAD4)
![Data](https://img.shields.io/badge/data-100%25%20synthetic-FBBF24)
![License](https://img.shields.io/badge/license-MIT-8A9BB0)

---

## 为什么做这个项目

我在物联网平台侧做过设备接入、告警规则和现场处置流程。迁移到 Agent 平台方向时，我想证明的不是"会调 LLM API"，而是三件更难的事：

1. **把存量平台能力变成 Agent 可调用的工具**——不是写一个聊天框，而是设计 schema、错误语义和幂等边界，让任意 Agent 客户端可以通过 **MCP** 查询告警、读取设备档案、生成分诊建议、确认告警。
2. **约束 LLM 的输出，而不是相信它**——固定 JSON 契约、严格校验、失败不写缓存、无凭据时明确回退到规则引擎，绝不把规则输出伪装成模型输出。
3. **量化 LLM 的输出质量**——14 个带人工标注要点的真实告警场景，`go test` 直接跑评测，输出要点命中率、定级准确率和失败明细，形成可回归的质量门禁。

一句话：**这是从"物联网平台工程师"走向"Agent 平台工程师"的能力证明，而不是一个 Demo 壳子。**

## 它做了什么

- 内置 **30 条全合成告警 / 12 台合成设备**，覆盖 5 类真实故障：设备离线、低电量、传感器数值异常、OTA 失败、通信抖动。
- 单页 Server-Side Rendering 值班台：告警列表、筛选、详情、设备档案、AI 处置建议、人工确认、处置时间线。
- **MCP server（stdio）** 暴露 4 个工具：`list_alerts` / `get_device` / `summarize_alert` / `ack_alert`，同一份领域逻辑同时服务 Web 和 Agent。
- **评测集 + Go test runner**：要点命中评分、定级一致性、逐用例失败明细、通过阈值；LLM 不可达时可用 mock 模式跑通全链路。
- 任意 OpenAI 兼容端点；base_url / api_key / model / token 预算全部来自环境变量，**无硬编码密钥**。

## 架构

```mermaid
flowchart LR
  subgraph Consumers[消费方]
    Browser[浏览器 / 值班台]
    Agent[MCP 客户端<br/>Claude Desktop · IDE · 自研 Agent]
  end

  subgraph Go[Go 进程]
    Web[Gin SSR + JSON API]
    MCP[MCP stdio server]
    SVC[service 用例层<br/>缓存 · 超时 · 幂等]
    STORE[(内存告警库<br/>30 告警 / 12 设备)]
    ANA[analyzer<br/>prompt · 解析 · 校验]
  end

  SYN[内嵌合成数据 JSON] --> STORE
  Browser --> Web
  Agent -->|4 个 MCP tools| MCP
  Web --> SVC
  MCP --> SVC
  SVC --> STORE
  SVC --> ANA
  ANA -->|chat/completions| LLM[OpenAI 兼容端点]
  ANA -.->|无 key / 显式开启| MOCK[规则回退分析器]
  EVAL[evalset runner<br/>go test] -->|14 cases| ANA
```

关键点：Web、MCP、评测三端共用 `internal/agent` 的 prompt 契约与输出校验，模型换了、入口换了，行为仍然一致。

## 快速开始

要求：Go 1.25.5+（`mcp-go` v1.5.0 的最低版本）。

```bash
git clone https://github.com/devyunmu/iot-alert-agent.git
cd iot-alert-agent
go mod download

export LLM_BASE_URL="http://127.0.0.1:3000/v1"   # OpenAI 兼容端点
export LLM_API_KEY="sk-..."                      # 只从环境变量读取
export LLM_MODEL="glm-5.3-flash"                 # 换成你的模型
export LLM_MAX_TOKENS=4000                       # 推理型模型建议 4000+
export LLM_TIMEOUT=180s

go run ./cmd/server
# http://127.0.0.1:8080
```

**零凭据演示**：不设置 `LLM_API_KEY` 时自动使用确定性规则分析器，页面顶部会显示 `MOCK · mock://rules-v1`，AI 卡片标注"规则回退"，不会把规则结果冒充模型输出。

常用命令：

```bash
make run          # 启动 Web 值班台
make mcp          # 以 stdio 启动 MCP server
make test         # 全部单测
make vet          # go vet
make eval         # 实时 LLM 评测（需 key）
make eval-mock    # 离线评测（无需 key）
```

## MCP 接入

项目核心叙事是 **"物联网平台 API 按 MCP tool schema 暴露给 Agent"**。

```bash
go build -o bin/iot-mcp ./cmd/mcp
./bin/iot-mcp        # stdout 为协议通道，日志全部走 stderr
```

Claude Desktop / 任意 MCP 客户端配置：

```json
{
  "mcpServers": {
    "iot-alert-agent": {
      "command": "/absolute/path/to/bin/iot-mcp",
      "env": {
        "LLM_BASE_URL": "http://127.0.0.1:3000/v1",
        "LLM_API_KEY": "sk-...",
        "LLM_MODEL": "glm-5.3-flash",
        "LLM_MAX_TOKENS": "4000"
      }
    }
  }
}
```

调试可用官方 Inspector：

```bash
npx @modelcontextprotocol/inspector ./bin/iot-mcp
```

### 工具清单

| Tool | 参数 | 行为 | 返回 |
|---|---|---|---|
| `list_alerts` | `status?` `severity?` `type?` `limit?` | 按严重度 + 时间排序的告警队列 | `{count, alerts[]}` |
| `get_device` | `device_id` | 设备档案 + 最近 5 条关联告警 | `{device, recent_alerts[]}` |
| `summarize_alert` | `alert_id` `force?` | 生成/复用分诊建议（摘要、定级、原因、步骤） | `{analysis, alert, device}` |
| `ack_alert` | `alert_id` `operator?` | 幂等确认；已关闭告警返回工具错误 | `{alert, changed, message}` |

设计细节：

- 参数使用 `mcp.Enum` 约束，非法值返回 `isError` 的工具结果而不是协议错误，Agent 能自行纠错。
- `summarize_alert` 默认读缓存，`force=true` 才重新调用模型，避免 Agent 循环烧 token。
- 同一告警重复 `ack_alert` 返回 `changed=false`，不覆盖首次确认人，保留审计语义。
- 所有工具返回 `structuredContent` + 等价文本，兼容只读文本的客户端。

## HTTP API

| Method | Path | 说明 |
|---|---|---|
| `GET` | `/` | 告警列表（`q` / `type` / `severity` / `status`） |
| `GET` | `/alerts/:id` | 告警详情 + AI 建议 |
| `POST` | `/alerts/:id/analyze` | 生成 AI 建议（表单，`force=1` 重算） |
| `POST` | `/alerts/:id/ack` | 确认告警（表单） |
| `GET` | `/api/alerts` | 告警列表 JSON（支持 `limit`） |
| `GET` | `/api/alerts/:id` | 详情 JSON（含缓存分析） |
| `POST` | `/api/alerts/:id/analyze` | `{"force":true}` |
| `POST` | `/api/alerts/:id/ack` | `{"operator":"..."}` |
| `GET` | `/api/stats` | 看板计数 + 当前分析器 |
| `GET` | `/healthz` | 存活探针 |

```bash
curl -s "http://127.0.0.1:8080/api/alerts?status=open&limit=3" | jq

curl -s -X POST http://127.0.0.1:8080/api/alerts/ALT-0021/analyze \
  -H 'Content-Type: application/json' -d '{}' | jq

curl -s -X POST http://127.0.0.1:8080/api/alerts/ALT-0006/ack \
  -H 'Content-Type: application/json' -d '{"operator":"oncall-li"}'
```

## 评测：把"感觉还行"变成分数

`evalset/evals.json` 有 **14 个用例 / 60 个关键要点**，覆盖五类告警。每个用例指定：

- 期望严重度（人工标注）；
- 3–5 个关键要点，每个要点配置多个可接受表述（如 `9.6` / `高于上限` / `超温`），避免只认一种措辞。

评分：

```
用例得分 = 0.8 × 要点命中率 + 0.2 × 定级是否一致
总分     = 所有用例得分的平均值
通过条件 = 平均分 ≥ 0.75 且 无分析错误
```

运行：

```bash
# 离线（默认）：验证评测链路与规则回退，不产生 API 费用
go test ./evalset -v

# 实时：需要 LLM_API_KEY
EVAL_MODE=live EVAL_WORKERS=3 LLM_MAX_TOKENS=5000 LLM_TIMEOUT=240s \
  go test ./evalset -v

# 指定模型 / 只跑前 N 条
EVAL_MODE=live LLM_MODEL=gpt-4o-mini EVAL_LIMIT=5 go test ./evalset -v

# 不带 EVAL_MODE 时默认 mock，`go test ./...` 不会意外产生模型费用。
```

### 真实评测结果

以下为 2026-11-17 的实际运行结果（`glm-5.3-flash`，14 条全量，并发 3）。LLM 输出存在随机性，不同模型/不同批次分数会有波动，因此评测结论应看趋势与失败类型，而不是单次小数点：

```text
=== IoT Alert Agent · 评测报告 ===
模式: live     分析器: glm-5.3-flash                用例: 14
评分: 要点命中 80% + 严重度一致 20%，通过阈值 0.75
------------------------------------------------------------------------------
CASE    ALERT      LEVEL  SEV  KEYPOINTS SCORE  LATENCY 
EV-01   ALT-0001   crit   ✓    5/5       1.00   18791ms 
EV-02   ALT-0002   crit   ✓    4/4       1.00   28409ms 
EV-03   ALT-0005   crit   ✓    5/5       1.00   16249ms 
EV-04   ALT-0007   warn   ✗    4/4       0.80   45684ms 
EV-05   ALT-0006   warn   ✗    3/4       0.60   36923ms 
EV-06   ALT-0009   crit   ✓    4/4       1.00   14790ms 
EV-07   ALT-0012   crit   ✓    4/5       0.84   24168ms 
EV-08   ALT-0013   crit   ✓    4/4       1.00   20257ms 
EV-09   ALT-0018   warn   ✗    4/4       0.80   39315ms 
EV-10   ALT-0021   crit   ✓    4/4       1.00   23935ms 
EV-11   ALT-0025   crit   ✓    4/4       1.00   36848ms 
EV-12   ALT-0027   crit   ✓    4/4       1.00   32180ms 
EV-13   ALT-0020   crit   ✓    4/5       0.84   30335ms 
EV-14   ALT-0030   warn   ✗    3/4       0.60   17080ms 
------------------------------------------------------------------------------
要点命中率 0.933 (56/60) · 定级准确率 0.714 · 平均得分 0.891 · 错误 0
未命中明细:
  EV-04 ALT-0007: 定级 critical != 期望 warning
  EV-05 ALT-0006: 定级 critical != 期望 warning
  EV-05 ALT-0006: 漏检 kp2 (关注重传/丢包)
  EV-07 ALT-0012: 漏检 kp5 (通风/断电/排烟)
  EV-09 ALT-0018: 定级 critical != 期望 warning
  EV-13 ALT-0020: 漏检 kp5 (备件/工单)
  EV-14 ALT-0030: 定级 critical != 期望 warning
  EV-14 ALT-0030: 漏检 kp1 (引用离线 9 分钟)
结论: PASS（阈值 0.75，错误数 0）

总耗时 131.6s（并发 3）

```

**结果解读（这部分比分数本身更重要）：**

- **要点命中 93.3%**：事实引用（温度、电流、版本号、错误码）和领域动作（制冷机组、断路器、消防疏散）基本稳定，说明"设备档案 + 告警事件"的上下文组织是有效的。
- **定级准确率 71.4%**：4 个分歧全部是**模型偏保守、把 warning 升成 critical**（OTA 校验和失败、网关通信抖动、污水 PH 超标、单设备离线 9 分钟）。对值班场景，过度定级会消耗人工注意力；下一步应把定级 rubric 拆成可判定的条件（业务中断时长 / 影响设备数 / 安全后果），并加入边界样例做少样本校准。
- **四处要点漏检**：网关抖动未提"重传/丢包"、烟雾告警未给"通风/断电"、消防电池告警未提"备件/工单"、单设备离线未引用"9 分钟"。说明演练型 runbook 还需要在 prompt 中显式要求"同时给出止损动作与资源动作，并引用关键数值"。
- **延迟 15–46s**：推理型模型在复杂用例上思考较长；并发 3 时全量约 2 分钟。作为异步分诊任务可接受，但不适合同步阻塞值班页面，生产上应改为任务队列 + 轮询/推送。
- **零分析错误**：面对推理模型偶发的"预算耗尽、content 为空"，客户端会自动以 2 倍预算重试一次；该机制让本次 14 条全部产出有效 JSON。
- **mock 模式 1.000 不代表模型能力**：规则回退的文案与评测要点同源，它只用于验证"评测链路能跑通"，报告中会明确标注 `mock://rules-v1`。

## 项目结构

```text
.
├── cmd/
│   ├── server/          # Web + JSON API 入口
│   └── mcp/             # MCP stdio 入口
├── data/
│   ├── alerts.json      # 30 条合成告警
│   ├── devices.json     # 12 台合成设备
│   └── data.go          # go:embed，二进制自包含
├── evalset/
│   ├── evals.json       # 14 用例 / 60 要点
│   └── eval_test.go     # go test 评测 runner
└── internal/
    ├── agent/           # prompt 契约、JSON 解析校验、规则回退
    ├── app/             # 依赖装配
    ├── config/          # 纯环境变量配置
    ├── domain/          # Alert / Device / Analysis
    ├── eval/            # 评分、聚合、报告渲染
    ├── llm/             # 轻量 OpenAI 兼容客户端（net/http）
    ├── mcpserver/       # 4 个 MCP 工具
    ├── service/         # 用例层：缓存、超时、幂等
    ├── store/           # 并发安全内存仓储
    └── web/             # Gin 路由、内嵌模板与静态 CSS
```

## 设计取舍

- **Go + Gin，而不是 Python/LangChain**：目标是展示 Agent 能力可以长在平台工程里——单二进制部署、强类型、标准库 HTTP 客户端、无框架黑盒。整个项目只有两个直接依赖：`gin` 与 `mark3labs/mcp-go`。
- **内存仓储**：演示要一条命令启动且结果可复现；`store` 接口边界清晰，替换 SQLite/Postgres 不需要改上层。代价是进程重启后确认状态与分析缓存丢失（见 TODO）。
- **空响应重试**：推理模型偶尔把预算耗在 reasoning 上，客户端检测到空 content 时自动以 2 倍预算重试一次（上限 16k），仍失败才向上抛错，避免整条分诊因一次坏采样而失败。
- **严格输出契约**：模型必须返回 `summary / severity / severity_reason / likely_causes / actions / confidence`；缺字段、级别非法、动作为空一律报错，不猜测、不落缓存。
- **MCP 错误语义**：找不到设备/告警、非法枚举、确认已关闭告警，均返回 `isError` 工具结果，让 Agent 能读到原因并自我纠正。
- **规则回退可见**：无 key 或显式 `LLM_MOCK=1` 时使用规则分析器，UI、API、评测报告都带 `mock` 标记。
- **合成数据**：设备号、站点、固件版本、错误码全部虚构，仓库不含任何真实客户、设备或公司信息。

## 质量与验证

```bash
go vet ./...                 # 通过
go test ./... -count=1       # 全部通过（store / agent / llm / service / web / mcp / eval）
go test -race ./...          # 通过（mock 模式）
```

覆盖的关键路径：

- 数据集完整性：30 告警、12 设备、5 类告警、设备引用完整；
- 告警排序/筛选/搜索、确认幂等、已关闭冲突、空操作人兜底；
- 模型输出：正常、带 Markdown 围栏、前后缀噪声、缺字段、非法级别、置信度截断；
- LLM 客户端：鉴权头、非 2xx、空 choices、空 content、超时；
- Web：列表渲染、筛选、详情、分析缓存、确认、404、API JSON；
- MCP：`tools/list` 工具面、四个工具的调用结果、非法参数、重复确认；
- 评测：要点匹配（忽略大小写与空白）、评分权重、报告 PASS/FAIL。

## 局限与 TODO

- [ ] 持久化：告警确认状态与分析缓存目前只在内存，重启即重置；接 SQLite/Postgres 并保留审计表。
- [ ] 鉴权与多租户：当前是本地演示服务，无登录/RBAC；公开部署前必须加。
- [ ] 同步分析 → 异步任务：推理模型 30s+，应改为队列 + 前端轮询/SSE，避免阻塞请求。
- [ ] MCP 传输：目前 stdio；补充 Streamable HTTP/SSE 以支持远程 Agent 与多客户端。
- [ ] Prompt 版本化：把 prompt/rubric 纳入评测基线，CI 中对分数回退设门禁。
- [ ] 评测增强：加入少样本定级示例、按告警类型分组的分数、成本（token）统计。
- [ ] 人工反馈闭环：把值班人员对 AI 建议的采纳/修改记录写回评测集。

## 数据与隐私

`data/*.json` 为全合成演示数据（30 条告警 / 12 台设备），设备号、站点、固件版本、错误码均为虚构，不含任何真实设备号、客户名或公司信息。仓库不包含任何 API key、token 或内网地址；运行时凭据只从环境变量读取。

## License

MIT