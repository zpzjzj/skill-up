# Custom Engine 设计

> **实现状态**：本文档描述 Custom Engine 的完整设计（`local` 与 `http` 两种 transport）。
> 当前阶段（Phase 1）已实现 `transport: local`；`transport: http` 已完成设计与配置 schema，
> 但实现将在后续 PR 中落地，配置选用 `http` 时 `skill-eval` 会返回明确的“尚未实现”错误。

本文定义 `skill-eval` 的 Custom Engine 配置接口和返回结果规约。Custom Engine 用于接入未内置支持的 Agent 执行器，例如本地 CLI、脚本、内部调度任务，或远程 HTTP Agent 服务。

## 目标

- 支持两类调用方式：`local` 本地任务调用和 `http` 远程服务调用。
- 支持在配置中引用环境变量，用于命令路径、URL、Header、Token、模型参数等。
- 对 runner / evaluator / judge 暴露统一的 `SessionResult`，不让下游关心 Engine 的具体调用方式。
- 保持 runtime 边界清晰：本地任务必须通过 `runtime.Exec` 在当前 runtime workspace 内执行。

## 非目标

- 不兼容旧设计中的 `engine.entry` 单字段配置。
- `engine.name` 命中内置 Agent 时，不读取 `engine.custom`。
- `skill-eval` 对所有 Agent 都不提供任何隐式文件同步逻辑。无论是内置 Agent、Custom Agent，还是 Custom Engine 的 `local` / `http` transport，只有返回结果中显式声明的产物会被下载或写入本地报告目录。

## 配置入口

`engine.name` 是用户自定义 Agent 名称；`engine.model` 是可选配置，仅当 custom agent 需要通过模板变量引用模型信息时才需填写。

当 `engine.name` 命中内置 Agent（例如 `claude_code`、`codex`、`qodercli`）时，`skill-eval` 使用内置实现。当 `engine.name` 没有命中内置 Agent 时，必须提供 `engine.custom`，`skill-eval` 按 Custom Engine 配置创建 agent。

```yaml
engine:
  name: my-agent
  custom:
    transport: local
    kwargs:
      profile: strict
      max_files: "20"
```

如果 `engine.name` 没有命中内置 Agent 且未提供 `engine.custom`，应报配置错误，例如 `unsupported agent "my-agent": missing engine.custom`。

## 接入者最小契约

接入 Custom Engine 时，你需要做三件事：

1. 在 `eval.yaml` 中选择 `transport: local` 或 `transport: http`。
2. 让你的 agent 接收标准 `SessionInput`。
3. 让你的 agent 返回标准 `SessionResult`。

`local` 和 `http` 的差异只在“怎么传输”：

- local agent 从输入文件读取 `SessionInput`，把 `SessionResult` 写到 output file 或 stdout。
- HTTP agent 从 JSON body 或 multipart `payload` 读取 `SessionInput`，把 `SessionResult` 作为 HTTP response body 返回。

### 最小 local 配置

```yaml
engine:
  name: review-cli
  custom:
    transport: local
    response_format: session_result
    env:
      OPENAI_API_KEY: ${api_key}
    local:
      command: ${REVIEW_AGENT_BIN}
      args:
        - run
        - --input
        - ${input_file}
        - --output
        - ${output_file}
      input_file: ${input_file}
      output_file: ${output_file}
```

local agent 需要读取 `${input_file}` 中的 `SessionInput`，并把 `SessionResult` JSON 写到 `${output_file}`。

### 最小 HTTP 配置

```yaml
engine:
  name: review-service
  custom:
    transport: http
    response_format: session_result
    http:
      url: ${CUSTOM_AGENT_ENDPOINT}/v1/run
      method: POST
      headers:
        Authorization: Bearer ${api_key}
      request_body: ${session_input}
```

HTTP agent 需要接收 `SessionInput` JSON，并返回 `SessionResult` JSON。若配置了 `custom.http.files`，请求会变成 multipart，`SessionInput` 放在 `payload` 字段中。

### 对接检查清单

接入方实现完成前，应逐项确认：

- 能读取 `SessionInput.messages`，并把它当作完整会话历史处理。
- 能读取 `SessionInput.kwargs`，所有值按字符串处理。
- 需要 workspace 文件时，只依赖 `custom.http.files` 或 local runtime workspace 中的显式路径。
- 成功和失败都返回可解析的 `SessionResult`。
- 至少返回 `exit_code` 和 `final_message`。
- 需要归档的文件都写进 `SessionResult.artifacts`，不依赖 `skill-eval` 自动扫描。
- 不要求在 `eval.yaml` 中写 secret；API Key 通过 `${api_key}` 引用解析后的凭据。
- 不依赖跨 case、跨 variant 或跨 iteration 的隐式会话状态。

最小 `SessionResult`：

```json
{
  "exit_code": 0,
  "final_message": "done"
}
```

## 完整配置 Schema

```yaml
engine:
  name: string
  model:
    provider: string
    name: string
    base_url: string
    params:
      string: string
  custom:
    transport: local | http
    timeout_seconds: int
    response_format: session_result | text
    env:
      string: string
    kwargs:
      string: string
    local:
      command: string
      args: [string]
      cwd: string
      input_file: string
      output_file: string
    http:
      url: string
      method: POST
      headers:
        string: string
      files:
        - path: string
          required: bool
      request_body:
        string: any
```

### 字段说明

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `engine.name` | 是 | Agent 名称；命中内置 Agent 时使用内置实现，否则读取 `engine.custom` |
| `custom.transport` | 是 | 调用方式，支持 `local` 或 `http` |
| `custom.timeout_seconds` | 否 | Engine 调用超时；未设置时使用 case timeout |
| `custom.response_format` | 否 | 返回结果解析方式，默认 `session_result`；推荐保持默认值 |
| `custom.env` | 否 | 自定义环境变量；local 会注入进程环境，HTTP 不会自动发送 |
| `custom.kwargs` | 否 | 传给 custom engine 的自定义参数，类型为 `dict[str]string` |
| `custom.local.command` | local 必填 | runtime 内可执行命令 |
| `custom.local.args` | 否 | 命令参数数组 |
| `custom.local.cwd` | 否 | 命令工作目录；默认 `${workspace}` |
| `custom.local.input_file` | 否 | runtime 内的输入文件路径；默认 `${input_file}` |
| `custom.local.output_file` | 否 | runtime 内的返回结果 JSON 文件路径 |
| `custom.http.url` | http 必填 | HTTP 调用 URL |
| `custom.http.method` | 否 | 首版仅支持 `POST` |
| `custom.http.headers` | 否 | HTTP Header |
| `custom.http.files` | 否 | 声明随 HTTP 请求上传的 workspace 文件集合 |
| `custom.http.request_body` | 否 | HTTP JSON body 模板 |

## Transport 一致性原则

`local` 和 `http` 是同一个 Custom Engine 契约的两种承载方式。两者应尽量共享同一组输入、输出和安全语义，只在 transport 必需的地方分化：

| 维度 | 统一语义 | local 承载 | http 承载 |
| --- | --- | --- | --- |
| 输入 | `SessionInput` | 写入 `custom.local.input_file` | JSON body；有文件时为 multipart `payload` |
| 多轮会话 | `messages` 是完整会话历史 | 从 input file 读取 | 从 request body / payload 读取 |
| 自定义参数 | `custom.kwargs` | 出现在 input file，可模板引用 | 出现在 request body / payload，可模板引用 |
| 凭据 | `${api_key}` 显式引用，不自动注入 | 通过 `custom.env` 注入 | 通过 `custom.http.headers` 注入 |
| Workspace 输入 | 显式声明才传递 | agent 直接在 runtime workspace 内执行 | `custom.http.files` 显式上传 |
| 返回 | `SessionResult` | stdout 或 `output_file` | HTTP response body |
| 返回解析 | `custom.response_format` | 同左 | 同左 |
| 产物归档 | `SessionResult.artifacts` 显式声明 | 同左 | 同左 |

不要为某个 transport 引入另一套消息、kwargs、凭据或返回结果模型。新增能力应优先落在统一契约上；只有承载方式确实不同，才放进 `custom.local` 或 `custom.http`。

`session_result` 是主路径。`text` 只适合临时脚本或极简集成：`skill-eval` 会把返回文本当作 `final_message` 构造最小结果，但无法获得完整 transcript、token、产物等结构化信息。

## API Key

Custom Engine 不在 `eval.yaml` 中配置 secret 值。`api_key` 来自 `skill-eval` 现有凭据解析链路，例如 CLI `--api-key`、provider 环境变量、或 `~/.skill-eval/credentials.yaml`。Custom Engine 只通过模板变量 `${api_key}` 引用解析后的 API Key。

`${api_key}` 的使用方式由 Custom Engine 配置决定：

- local transport 可以通过 `custom.env` 显式注入，例如 `OPENAI_API_KEY: ${api_key}`
- HTTP transport 可以通过 header 显式引用，例如 `Authorization: Bearer ${api_key}`
- 如果 custom agent 不需要 API Key，可以不引用 `${api_key}`

`api_key` 不应自动注入到所有 custom agent 的环境变量或 HTTP header。自动注入会让不同 provider、不同 agent 的鉴权语义变得不透明，也容易把不该传递的凭据传给下游。

日志、debug 输出、错误信息和报告中都必须 mask `api_key` 的真实值。

`custom.env` 对不同 transport 的含义不同：local transport 会把它注入到进程环境；HTTP transport 不会把 `custom.env` 自动发送给服务端。HTTP 需要凭据或自定义 header 时，应在 `custom.http.headers` 或 `custom.http.request_body` 中显式引用 `${api_key}` 或 `${VAR}`。

## 环境变量引用

Custom Engine 配置中的字符串字段支持环境变量引用：

```yaml
custom:
  env:
    OPENAI_API_KEY: ${OPENAI_API_KEY}
    AGENT_ENDPOINT: ${AGENT_ENDPOINT:-https://agent.example.com}
  http:
    headers:
      Authorization: Bearer ${CUSTOM_AGENT_TOKEN?CUSTOM_AGENT_TOKEN is required}
```

支持形式：

| 形式 | 语义 |
| --- | --- |
| `${VAR}` | `VAR` 必须存在且非空，否则配置解析失败 |
| `${VAR:-default}` | `VAR` 不存在或为空时使用 `default` |
| `${VAR?message}` | `VAR` 不存在或为空时用 `message` 报错 |

变量替换只作用于 `engine.custom` 内的字符串字段，以及 `engine.model.base_url` / `engine.model.params` 中的字符串值。不得对 case prompt、judge criteria 或整个 YAML 做全局替换，避免误替换用户输入内容。

日志输出必须隐藏敏感值。字段名匹配 `KEY`、`TOKEN`、`SECRET`、`PASSWORD`、`AUTHORIZATION`，或 URL query 中疑似 token 的值，都应 mask。

## 自定义参数 kwargs

`custom.kwargs` 用于传递 agent 自定义参数，类型固定为 `dict[str]string`：

```yaml
engine:
  name: review-cli
  custom:
    transport: local
    kwargs:
      profile: strict
      max_files: "20"
      report_format: markdown
```

`kwargs` 和 `env` 的职责不同：

| 字段 | 用途 | 是否适合敏感值 |
| --- | --- | --- |
| `custom.env` | 凭据、Token、运行时环境变量 | 是，但日志必须 mask |
| `custom.kwargs` | agent 行为参数、开关、业务配置 | 否 |

`kwargs` 中的值支持环境变量引用：

```yaml
custom:
  kwargs:
    profile: ${CUSTOM_AGENT_PROFILE:-default}
```

`kwargs` 解析后会进入 local 输入文件和 HTTP request body，也可以通过模板变量引用。所有 kwargs 值都按字符串处理；如果 agent 需要数字或布尔值，应由 agent 自己解析。

## Agent 产物归档边界

`skill-eval` 的 Agent 产物归档由 `SessionResult` 返回结果驱动，不由 workspace 扫描、agent 类型或 transport 类型驱动。`skill-eval` 不会因为某个 agent 修改了 workspace、远端目录或本机临时目录，就自动同步这些文件。

所有需要进入报告目录的文件，都必须在 `SessionResult.artifacts` 中显式声明：

- 内置 Agent 和 Custom Agent 都遵循同一规则
- 在 runtime 内执行的 Agent 可以声明 runtime workspace 内的 `path`
- HTTP 或其他远端 Agent 可以声明可下载的 `url`
- 任意 Agent 都可以声明小文件的 `content` 或 `content_base64`

未声明的文件不会被 `skill-eval` 探测、下载或写入报告目录。

HTTP agent 如果需要读取本地 workspace 文件，必须通过 `custom.http.files` 显式声明请求输入文件。这属于请求输入，不属于 workspace 同步；`skill-eval` 只上传声明的文件集合，不会扫描整个 workspace。

## 内置模板变量

Custom Engine 配置还支持以下由 `skill-eval` 提供的模板变量：

| 变量 | 说明 |
| --- | --- |
| `${workspace}` | 当前 runtime workspace 绝对路径 |
| `${prompt}` | 当前 case 的单轮 prompt；多轮 case 时为空 |
| `${messages_json}` | 当前 case 的消息数组 JSON 字符串 |
| `${messages}` | 当前 case 的消息数组；仅在 JSON body、payload 或输入文件模板中作为结构化值使用 |
| `${session_input}` | 标准 SessionInput 结构；仅在 JSON body、payload 或输入文件模板中作为结构化值使用 |
| `${session_input_json}` | 标准 SessionInput JSON 字符串 |
| `${input_file}` | 建议的 runtime 输入文件路径，默认 `inputs/messages.json` |
| `${model}` | `provider/name` 形式的模型引用；未配置 `engine.model` 时为空字符串 |
| `${model_provider}` | `engine.model.provider`；未配置时为空字符串 |
| `${model_name}` | `engine.model.name`；未配置时为空字符串 |
| `${api_key}` | 由现有凭据解析链路得到的 API Key；敏感值，日志必须 mask |
| `${output_file}` | 建议的 runtime 输出文件路径，默认 `outputs/session-result.json` |
| `${case_id}` | 当前 case ID |
| `${variant}` | `with_skill` 或 `without_skill` |
| `${max_turns}` | 当前 case 的最大交互轮数 |
| `${timeout_seconds}` | 当前 Engine 调用超时时间 |
| `${kwargs}` | `custom.kwargs` 的结构化对象；仅在 JSON body 或输入文件模板中作为结构化值使用 |
| `${kwargs_json}` | `custom.kwargs` 的 JSON 字符串 |
| `${kwargs.<key>}` | 引用单个 kwarg 值，例如 `${kwargs.profile}` |

模板变量和环境变量使用同一语法空间。若同名，内置模板变量优先。

## 多轮会话输入契约

Custom Engine 必须支持统一消息数组作为标准输入形态。`skill-eval` 会把 case input 规范化为 `messages`：

```json
[
  { "role": "user", "content": "先阅读当前目录。" },
  { "role": "assistant", "content": "已阅读。" },
  { "role": "user", "content": "现在基于刚才的信息生成报告。" }
]
```

单轮 case 等价于只包含一条 `user` 消息的数组。`prompt` 只是为兼容简单 CLI 暴露的便捷变量；Custom Engine 的主契约应以 `messages` 为准。

### SessionInput 格式

`skill-eval` 会为每次 Agent 调用构造统一的 `SessionInput`。local transport 推荐把它写入 `${input_file}`，HTTP transport 默认把它作为 JSON request body；存在文件上传时，作为 multipart 中的 `payload` 字段。

```json
{
  "case_id": "multi-turn-report",
  "variant": "with_skill",
  "workspace": "/tmp/skill-eval/workspace",
  "model": "openai/gpt-4.1",
  "kwargs": {
    "profile": "strict",
    "max_files": "20"
  },
  "messages": [
    { "role": "user", "content": "先阅读当前目录。" },
    { "role": "assistant", "content": "已阅读。" },
    { "role": "user", "content": "现在基于刚才的信息生成报告。" }
  ],
  "max_turns": 12,
  "timeout_seconds": 300
}
```

`messages[*].role` 支持 `system`、`user`、`assistant`、`tool`。

`content` 首版定义为字符串。后续如果需要多模态或结构化 content，应扩展为 `content_blocks`，而不是改变 `content` 的含义。

### 会话状态边界

每个 case variant 是一次独立会话。Custom Engine 不应依赖跨 case、跨 variant 或跨 iteration 的隐式远端会话状态。

如果 Engine 自身支持 session resume，也只能在一次 `Run` 内部使用。返回结果中可以包含完整 transcript，但不要求暴露远端 session ID；如果暴露，应放在 `artifacts.logs` 或后续新增的 metadata 字段中。

### 多轮执行语义

Custom Engine 收到多条 `messages` 时，应把它们视为同一会话历史，并基于最后一条用户消息继续执行。它不需要逐条回放并在每条用户消息后都调用一次模型；是否压缩上下文、是否真实 replay，由 Engine 自己决定。

返回的 `transcript` 应至少包含输入消息和最终 assistant 回复。若 Engine 在执行过程中产生工具调用或中间 assistant 消息，应按发生顺序追加到 transcript。

## Local Transport

`local` transport 通过 `runtime.Exec` 执行命令。命令运行在当前 runtime 中，因此能访问 runtime workspace、已安装的 Skill、fixtures、MCP 配置和环境变量。

示例：

```yaml
engine:
  name: review-cli
  model:
    provider: openai
    name: gpt-4.1
  custom:
    transport: local
    timeout_seconds: 300
    response_format: session_result
    env:
      OPENAI_API_KEY: ${api_key}
    kwargs:
      profile: strict
      max_files: "20"
    local:
      command: ${REVIEW_AGENT_BIN}
      args:
        - run
        - --input
        - ${input_file}
        - --workspace
        - ${workspace}
        - --model
        - ${model}
        - --profile
        - ${kwargs.profile}
        - --output
        - ${output_file}
      cwd: ${workspace}
      input_file: ${input_file}
      output_file: ${output_file}
```

调用规则：

1. `skill-eval` 在 runtime 内写入 `custom.local.input_file` 指定的路径，内容为上文定义的输入文件 JSON。
2. `skill-eval` 渲染 `command`、`args`、`cwd`、`env`。
3. `skill-eval` 使用 shell-safe quoting 组装命令，或直接通过 runtime 支持的 argv 接口执行。
4. 命令必须在 `timeout_seconds` 内退出。
5. 若配置 `output_file`，优先从该文件读取返回结果。
6. 若未配置 `output_file`，从 stdout 读取返回结果。
7. `custom.response_format: text` 时，将 stdout 作为 `final_message` 构造最小 `SessionResult`。

本地任务的文件修改应发生在 `${workspace}` 下。若这些文件需要进入报告目录，agent 必须在返回结果的 `artifacts` 中显式声明。

## HTTP Transport

> Phase 1 暂未实现；本节为完整设计。

`http` transport 用于远程 Agent 服务或本机 HTTP Agent 服务。它接收标准 `SessionInput`，执行后通过 `SessionResult` 返回文本、transcript 和产物声明。`skill-eval` 只会下载或写入返回结果中显式声明的产物。

示例：

```yaml
engine:
  name: remote-review-agent
  model:
    provider: openai
    name: gpt-4.1
  custom:
    transport: http
    timeout_seconds: 300
    response_format: session_result
    kwargs:
      profile: strict
      max_files: "20"
    http:
      url: ${CUSTOM_AGENT_ENDPOINT}/v1/run
      method: POST
      headers:
        Authorization: Bearer ${api_key}
        Content-Type: application/json
      files:
        - path: diff.patch
          required: true
        - path: "src/**/*.go"
          required: false
        - path: "**/*"
          required: false
      request_body: ${session_input}
```

调用规则：

1. `skill-eval` 渲染 URL、headers 和 request body 中的字符串值。
2. 未配置 `custom.http.request_body` 时，HTTP request body 默认为 `${session_input}`。
3. `request_body` 中字段值若完整等于 `${session_input}`、`${messages}` 或 `${kwargs}`，应作为 JSON 结构注入，而不是作为字符串注入。
4. 若配置 `custom.http.files`，`skill-eval` 从 runtime workspace 展开声明的文件集合，并以 multipart form-data 逐文件上传。
5. 无文件上传时，请求 body 使用 JSON 编码。
6. 存在文件上传时，使用 multipart form-data；JSON body 作为 multipart 中的 `payload` 字段。
7. 非 2xx HTTP 状态视为 Engine 执行错误。
8. `custom.response_format: session_result` 时，响应 body 必须是 `SessionResult` JSON。
9. `custom.response_format: text` 时，响应 body 作为 `final_message`。

### HTTP 多轮会话

HTTP transport 的多轮会话语义与 local transport 相同：同一次请求中的 `payload.messages` 是完整会话历史，agent 应基于最后一条用户消息继续执行。HTTP transport 不依赖服务端保存跨请求 session 状态。

有文件上传时，multipart 结构为：

- `payload`: `SessionInput` JSON
- `files`: 一个或多个文件 part，`filename` 是 workspace 相对路径

HTTP transport 和其他 transport 一样，产物归档由返回结果驱动：未在 `artifacts.files` 或兼容字段中声明的文件，不会被 `skill-eval` 探测、同步或下载。

### HTTP 输入文件

`custom.http.files` 用于把 runtime workspace 中的文件集合作为 HTTP 请求输入传给 agent：

```yaml
custom:
  http:
    files:
      - path: diff.patch
        required: true
      - path: fixtures/context.json
        required: false
      - path: "src/**/*.go"
        required: false
      - path: "**/*"
        required: false
```

字段说明：

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `path` | 是 | runtime workspace 内的相对文件路径或 glob pattern |
| `required` | 否 | 默认 `true`；为 `false` 时文件不存在则跳过 |

约束：

- `path` 必须是 runtime workspace 内的相对路径，不能是绝对路径，不能包含 `..`
- `path` 可以是精确文件路径，也可以是 glob pattern
- 包含 glob 元字符（如 `*`、`?`、`[`、`]`、`**`）时按 glob 展开；否则按精确文件路径处理
- glob 仅在 runtime workspace 内展开；不跟随结果逃逸到 workspace 外
- glob 只上传文件；目录本身不会作为单独条目上传
- `**/*` 表示选择 workspace 下所有匹配文件
- 每个匹配文件作为独立 multipart file part 上传，并保留其相对 workspace 的路径
- `required: true` 时，精确文件不存在或 glob 匹配为空应视为配置/输入错误
- `required: false` 时，精确文件不存在或 glob 匹配为空则跳过该条
- 文件按原内容上传
- 未被 `custom.http.files[].path` 显式选中的 workspace 文件不会上传
- 上传文件只作为 HTTP 请求输入，不会改变产物归档规则

multipart file part 建议使用固定字段名 `files`，并在每个 part 的 `filename` 中携带 workspace 相对路径，例如 `src/main.go`。

## 返回结果规约

Custom Engine 的标准返回结果是 `SessionResult` JSON：

```json
{
  "engine": "custom",
  "model": "openai/gpt-4.1",
  "exit_code": 0,
  "duration_ms": 45200,
  "turns": 3,
  "input_tokens": 1200,
  "output_tokens": 450,
  "final_message": "Review completed. Found one issue.",
  "stderr": "",
  "transcript": [
    { "role": "user", "content": "Review the current diff." },
    { "role": "assistant", "content": "Found one issue in config parsing." }
  ],
  "artifacts": {
    "workspace_diff": "diff --git a/report.md b/report.md ...",
    "generated_files": ["outputs/report.md"],
    "logs": "agent log text"
  }
}
```

### 必填字段

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `exit_code` | integer | Engine 进程或远程任务退出码；成功为 `0` |
| `final_message` | string | Agent 最终输出文本；允许为空，但不建议 |

### 可选字段

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `engine` | string | 返回方标识；未设置时由 `skill-eval` 填为 `engine.name` |
| `model` | string | 模型引用；未设置时由 `skill-eval` 根据配置填充 |
| `duration_ms` | integer | Engine 侧耗时；未设置时由 `skill-eval` 用调用耗时填充 |
| `turns` | integer | Agent 交互轮数 |
| `input_tokens` | integer | 输入 token 数 |
| `output_tokens` | integer | 输出 token 数 |
| `stderr` | string | 错误输出或诊断信息 |
| `transcript` | array | 统一 transcript |
| `artifacts` | object | 产物、日志和 workspace diff |

### Transcript 规约

`transcript` 使用统一消息结构，`role` 支持 `system`、`user`、`assistant`、`tool`。如果 custom engine 无法提供完整 transcript，至少应返回 `final_message`。`skill-eval` 会用输入消息和 `final_message` 构造最小 transcript。

### Artifacts 规约

```json
{
  "workspace_diff": "diff --git ...",
  "generated_files": ["outputs/report.md"],
  "files": [
    { "name": "report.md", "path": "outputs/report.md", "content_type": "text/markdown" },
    { "name": "remote-report.html", "url": "http://127.0.0.1:8080/artifacts/report.html", "content_type": "text/html" },
    { "name": "summary.json", "content": "{\"status\":\"pass\"}", "content_type": "application/json" }
  ],
  "logs": "agent logs"
}
```

`generated_files` 是兼容现有报告结构的轻量字段，适合 local transport 返回 runtime workspace 内已存在的文件路径。相对路径以 runtime workspace 为根；local transport 返回绝对路径时必须位于 runtime workspace 或 runtime 可下载产物目录内。

`artifacts.files` 是 Custom Engine 推荐使用的结构化产物字段：

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `name` | 是 | 产物文件名，用于报告目录归档 |
| `path` | 条件 | runtime workspace 内文件路径，local transport 常用 |
| `url` | 条件 | 可下载 URL，HTTP transport 常用 |
| `content` | 条件 | 小文本产物的内联内容 |
| `content_base64` | 条件 | 二进制产物的 base64 内容 |
| `content_type` | 否 | MIME type |

`path`、`url`、`content`、`content_base64` 必须至少提供一个。非 local transport 不应通过 `generated_files` 或 `files.path` 指向任意 host 文件路径。

## 错误处理

Custom Engine 调用失败分为三类：

| 类型 | 条件 | 处理 |
| --- | --- | --- |
| 配置错误 | 缺少必填字段、环境变量未解析、非法 transport | run 前失败 |
| 调用错误 | local 命令无法启动、HTTP 非 2xx、超时 | case 结果为 `ERROR` |
| 结果错误 | 返回 JSON 无法解析、缺少 `exit_code`、字段类型错误 | case 结果为 `ERROR` |

若 Engine 返回合法 `SessionResult` 且 `exit_code != 0`，runner 应保留该 `SessionResult`，并将 case 标记为执行错误。`stderr` 和 `final_message` 应进入报告，便于排查。

## 安全约束

- 不在日志中打印未 mask 的 token、API key、Authorization header。
- 非 local transport 不应指定任意 host 文件路径作为 generated file；这类产物应使用 `artifacts.files[].url` 或内联内容。
- local transport 的命令在 runtime 中执行，不直接使用 host `os/exec`。
- 环境变量解析失败时应在配置阶段报错，避免运行到一半才发现凭据缺失。

## 实现说明（维护者）

- `internal/config/schema.go` 定义 `CustomEngineConfig`，挂到 `EngineConfig.Custom`。
- `internal/config/customengine.go` 实现 env 引用解析（`${VAR}` / `${VAR:-default}` / `${VAR?message}`），只作用于 `engine.custom` 配置树及 `engine.model` 字符串值；内置模板变量名保留到运行期解析。
- `internal/config/validator.go` 先判断 `engine.name` 是否命中内置 Agent；未命中时要求存在 `engine.custom`，并校验 transport 和必填字段。
- `internal/agent/custom.go` 实现 `CustomAgent`。
- `internal/agent/factory.go` 优先匹配内置 Agent；未命中且存在 `engine.custom` 时创建 `CustomAgent`；未命中且缺少 custom 配置时报 `unsupported agent "<name>": missing engine.custom`。
- local transport 复用 `runtime.Exec`，http transport 使用 host 侧 HTTP client。
- 单测覆盖 env 引用、敏感信息 mask、local stdout JSON、local output file JSON、HTTP JSON、HTTP 非 2xx。
