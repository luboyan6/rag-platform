# GLM Rerank 后端 RAG 排障与修复记录

更新时间：2026-09-18

本文记录 GLM（智谱）Rerank 在后端 RAG 流程中调用失败的排查过程、根因判断、代码修复和验证结果。本文不包含任何 API Key；凭证必须通过环境变量或密钥管理系统注入。

## 1. 结论

本次故障不是单一的数据或模型问题，而是以下问题叠加：

| 问题 | 类型 | 影响 | 状态 |
| --- | --- | --- | --- |
| RAG 主流程没有执行 CHUNK_RERANK 阶段 | 后端代码 | 即使模型配置正确，也不会进入重排调用 | 已恢复 |
| Rerank HTTP transport 没有继承环境代理 | 代码与运行环境集成 | 直连在 TLS 阶段长时间等待，表现为测试卡住或超时 | 已修复 |
| Rerank 没有统一的调用兜底 deadline | 后端代码 | 网络异常时可能等待十几分钟，放大故障影响 | 已修复 |
| 智谱 base_url 使用了 /api/paas/v4，缺少 /rerank | 配置 | 通过代理访问时返回 HTTP 404 | 已修正配置 |
| 独立测试进程没有加载 GLM_API_KEY | 测试环境 | 工厂初始化时报 missing or unresolved API key | 已通过测试环境注入解决 |

因此，GLM API 本身、请求的基本 payload 以及候选文本数据不是本次主要根因。使用修复后的项目真实调用链可以成功返回排序结果。

## 2. 问题现象

用户提供的最小 Python 请求可以正常访问：

    POST https://open.bigmodel.cn/api/paas/v4/rerank
    {
      "model": "rerank",
      "query": "...",
      "documents": ["...", "..."],
      "top_n": 4
    }

而后端 RAG 流程表现为以下一种或多种情况：

- Rerank 阶段没有出现在流程事件或日志中；
- 请求长时间没有结果，最终以连接重置或超时结束；
- 使用代理后立即得到 404；
- 独立测试启动时提示 API Key 缺失。

这些现象分别对应流程是否进入重排、网络路径、endpoint 拼接规则和测试进程环境注入问题，不能只根据 Python 样例成功就断定后端实现完全正确。

## 3. 根因判断与依据

### 3.1 RAG 流程确实存在代码问题

internal/application/service/session_knowledge_qa.go 的知识问答 pipeline 原先没有加入 types.CHUNK_RERANK，所以检索完成后会直接进入后续阶段。现在流程为：

    CHUNK_SEARCH_PARALLEL
        -> CHUNK_RERANK
        -> WEB_FETCH（按需）
        -> CHUNK_MERGE
        -> FILTER_TOP_K

internal/application/service/chat_pipeline/rerank.go 已有重排插件。插件会：

1. 检查是否有检索结果和 Rerank model ID；
2. 从模型服务读取运行时模型配置；
3. 将候选段落和 query 发送给 Rerank provider；
4. 按返回的 index 和 score 重排候选结果。

因此“配置了模型但流程没有调用”是实际的代码问题，不是数据量或候选文本导致的。

### 3.2 Python 样例与 Go 后端的网络路径不同

修复前，internal/models/rerank/transport.go 使用的 SSRF 安全 transport 没有设置 Proxy，不会自动使用 HTTP_PROXY、HTTPS_PROXY 和 NO_PROXY。需要通过代理访问外部接口时，Go 后端会直连并停在 TLS 阶段；Python requests 则可能自动读取环境代理，因此出现“Python 成功、后端失败”的差异。

对照结果如下：

| 调用方式 | 地址 | 结果 |
| --- | --- | --- |
| 修复前 Go transport 直连 | /api/paas/v4/rerank | TCP 建连后 TLS 长时间未完成 |
| 环境代理 + 根路径 | /api/paas/v4 | HTTP 404，说明已到达服务但地址不完整 |
| 环境代理 + 完整路径 | /api/paas/v4/rerank | 成功返回排序结果 |

这组结果把问题边界定位在“客户端网络路径与 endpoint 配置”，而不是模型推理或候选文本内容。

### 3.3 智谱 endpoint 必须使用完整路径

智谱 Rerank 配置应使用：

    base_url: https://open.bigmodel.cn/api/paas/v4/rerank
    provider: zhipu
    api_key: ${GLM_API_KEY}

不能把只适用于部分 OpenAI-compatible provider 的根地址规则直接套到智谱 provider。当前智谱适配器使用配置中的 URL，不会为自定义 base_url 自动补 /rerank。

此外，应用实际调用 GetRerankModel 时优先读取数据库中的运行时模型记录。YAML 通常只在应用启动时同步，已经存在的同 ID 数据库记录可能不会被 YAML 自动覆盖。因此必须同时核对 YAML 和运行时数据库记录。

### 3.4 缺少 deadline 会放大网络故障

修复前，Rerank HTTP client 没有统一的总超时，RAG 调用方也没有专用 deadline。连接被代理、TLS 或上游服务卡住时，请求可能等待十几分钟。

现在 NewReranker 为没有上游 deadline 的调用增加默认 30 秒 deadline，可通过正整数环境变量调整：

    WEKNORA_RERANK_TIMEOUT_SECONDS=30

如果调用方已经设置了更短或更长的 deadline，Rerank wrapper 保留调用方 deadline，不覆盖上层请求生命周期。

### 3.5 不是数据、依赖或模型推理耗时的直接证据

- 使用两条合成候选文本即可复现“直连 TLS 等待”和“代理完整地址成功”的差异；不依赖知识库数据内容。
- 修复后的真实调用链 ConfigFromModel -> NewReranker -> ZhipuReranker.Rerank 成功返回两条排序结果，响应解析正常。
- 离线单元测试、竞态检查和 go vet 均通过，没有发现依赖版本导致的编译或运行时错误。
- 相邻历史日志中的 connection reset by peer 只能说明连接被重置，不能单凭该错误推断为模型推理慢。

## 4. 修复方案

### 4.1 恢复 RAG 的 Rerank 阶段

在 session_knowledge_qa.go 中将 types.CHUNK_RERANK 放回知识问答 pipeline，并保留现有 PluginRerank 作为该事件的处理器。

### 4.2 让安全 transport 继承环境代理

将 Rerank transport 替换为带环境代理能力的 SSRF 安全 transport。修复保留了：

- SSRF 地址校验；
- 安全 DNS 与拨号保护；
- 重定向校验。

变化仅是允许 transport 按标准环境变量选择代理，不是关闭 SSRF 防护或信任任意重定向。

部署时应确保后端进程实际继承代理变量；只在交互式 shell 中设置而没有传给服务进程，仍会复现直连问题。

### 4.3 增加可配置的 Rerank 兜底超时

新增 WEKNORA_RERANK_TIMEOUT_SECONDS：

- 未设置或不是正整数时使用 30 秒；
- 仅在调用方没有 deadline 时补充；
- 不覆盖调用方已经设置的 deadline；
- 超时错误会返回到 RAG pipeline，并保留原有检索结果作为 fallback。

fallback 只保证请求不会无限等待，不代表应该忽略告警。生产环境仍需根据日志中的 api_error_fallback 排查代理、endpoint、凭证和上游状态。

### 4.4 校正模型配置并核对运行时记录

配置检查清单：

1. provider 为 zhipu；
2. base_url 精确包含 /api/paas/v4/rerank；
3. api_key 使用 ${GLM_API_KEY} 等变量引用，不写入仓库；
4. 后端进程能够读取 GLM_API_KEY；
5. 数据库中实际被 GetRerankModel 读取的模型记录与 YAML 一致；
6. RerankModelID 已配置到该模型，且候选检索结果非空。

## 5. 验证结果

### 5.1 自动化验证

以下检查已在当前工作区执行并通过：

    go test ./internal/models/rerank -count=1
    go test -race ./internal/models/rerank -count=1
    go test ./internal/application/service/chat_pipeline -count=1
    go vet ./internal/models/rerank ./internal/application/service/chat_pipeline ./internal/application/service

其中竞态检查通过，Rerank 包没有发现数据竞争。

### 5.2 真实 GLM 调用链验证

使用测试环境加载本地 .env（不在命令行或文档中传递明文 Key），并启用生产 transport 运行配置模型的 live probe：

    GOCACHE=/tmp/rag-platform-go-cache \
    WEKNORA_RERANK_LIVE=1 \
    WEKNORA_RERANK_ENV_FILE=/path/to/.env \
    WEKNORA_RERANK_TRANSPORT=production \
    LLM_DEBUG_LOG=0 \
    go test ./internal/models/rerank \
      -run '^TestRerankerConnectivityLive/configured$' \
      -count=1 -v -timeout=45s

实测结果：

- 选择了环境代理；
- endpoint 为完整的 https://open.bigmodel.cn/api/paas/v4/rerank；
- TLS、请求写入和首字节阶段均完成；
- 返回两个排序结果，score 解析成功；
- 测试退出码为 0。

该 live probe 会访问外部供应商，可能产生调用费用；只应在明确需要时执行。

### 5.3 尚未替代部署验证的事项

以上验证证明代码和当前测试配置的真实 provider 调用已经修复，但不能替代目标环境的部署检查。上线前仍需：

- 核对目标数据库中的 Rerank 模型记录；
- 确认服务进程继承代理和 GLM_API_KEY；
- 重启后端使模型配置与代码生效；
- 用一个真实知识库请求确认日志出现 CHUNK_RERANK，并观察其耗时和 api_error_fallback 指标。

## 6. 安全注意事项

- 不要把 API Key 写入 Markdown、YAML、日志、命令行参数或 Git 历史；使用 ${GLM_API_KEY} 等环境变量引用。
- 用户提供的测试片段包含真实凭证形态的 token；如果该 token 曾在真实环境使用，应立即在智谱控制台轮换并撤销旧 Key。
- live probe 输出只保留 endpoint、阶段耗时和结果数量，不输出 Authorization header 或完整环境变量。

## 7. 相关文档

- [Reranker 连通性诊断](./reranker-connectivity.md)：包含原始网络对照数据、历史日志边界和更详细的 transport 诊断。
