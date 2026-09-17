# Reranker 连通性诊断（2026-09-16）

## 实测结论

当前开发环境中，`builtin-rerank` 的问题是网络路径与接口地址不匹配，客户端缺少总超时又放大了等待。使用环境代理及完整 endpoint 后，同一配置中的模型与凭证可以正常返回排序结果。小样本测试没有显示模型推理需要十几分钟。

测试通过项目 `ConfigFromModel → NewReranker → ZhipuReranker.Rerank` 调用。配置来源为本地 `config/builtin_models.yaml`，凭证变量与现有 SSRF 白名单来自 `.env`；没有读取数据库覆盖值，也没有修改配置或生产代码。

| 测试路径 | endpoint | 耗时 | 实际结果 |
| --- | --- | --- | --- |
| 项目原有 transport，不选择环境 HTTP 代理 | `/api/paas/v4` | 30.030s | TCP 9ms 建连，TLS 握手未完成，测试 deadline 中止 |
| 项目原有 transport，不选择环境 HTTP 代理 | `/api/paas/v4/rerank` | 30.029s | TCP 24ms 建连，TLS 握手未完成，测试 deadline 中止 |
| 测试内启用环境代理 `http://localhost:7897` | `/api/paas/v4` | 261ms | HTTP 404，地址错误 |
| 测试内启用环境代理，同进程复用连接 | `/api/paas/v4/rerank` | 190ms | 成功返回 2 条排序结果 |
| 测试内启用环境代理，另起进程建立新连接 | `/api/paas/v4/rerank` | 170ms | TLS 54ms 完成，首字节 169ms，成功返回 2 条排序结果 |

测试时间为北京时间 11:00–11:02。输入为两条合成短文本，无知识库内容。成功结果的 index 为 0、1，score 为 1.000000、0.975296；这里只证明调用与响应解析成功，不评估排序质量或大批量推理性能。

原有路径解析到 `28.0.1.70:443` 并成功建立 TCP 连接，随后停在 TLS 阶段，未记录请求写入或首字节。因此当前复现不是 DNS 不通、TCP 拒绝连接，也没有证据表明已进入模型推理。代理或虚拟 DNS 的具体实现未进一步核实；无需据此猜测上游服务算力不足。

## 代码证据

- `internal/models/rerank/transport.go` 使用 `NewSSRFSafeTransport`；`internal/utils/security.go` 中该 transport 没有 `Proxy` 函数。显式启用环境代理的另一构造器才设置 `http.ProxyFromEnvironment`。
- `internal/models/rerank/zhipu_reranker.go` 的默认 URL 是 `https://open.bigmodel.cn/api/paas/v4/rerank`，但配置了 BaseURL 后原样使用，不追加路径。当前 YAML 的 `builtin-rerank` 只有 `/api/paas/v4`。代理对照的 404 与此吻合。
- 智谱与通用 reranker 均使用 `newRerankHTTPClient(0)`，即没有 HTTP 客户端总超时。`internal/application/service/chat_pipeline/rerank.go` 的调用也没有设置重排专属 deadline；上层传入的 context 若没有较短 deadline，就可能长时间等待。
- 通用/OpenAI-compatible 适配器与智谱的 URL 语义不同：前者会追加 `/rerank`，不能把智谱的完整 endpoint 规则不加区分地应用于所有 provider。

## 日志边界

- 9 月 16 日请求 `k4ry0cW9ZyVM` 对应“测试”知识库和问题“泰安索道功能升级共有几大模块，只输出对应的模块名称即可”。主日志在 09:45:34 附近结束；调试日志记录 embedding 成功，耗时 455ms。没有该请求后续 reranker 或最终回复记录。不能把以上实时探测直接当作该请求完整 20 分钟时间线。
- 为查找同类故障，补充核查了相邻 9 月 15 日调试日志：`dWL2gOQ6fFfV.log` 的 rerank 等待 18m37.594s，`oJYUHecldA0S.log` 等待 15m36.825s，`qHTVqVlqiGGu.log` 等待 13m37.159s，均以 `connection reset by peer` 失败。该错误表示连接读取被重置，不能单凭它认定模型推理慢或确定哪一跳重置连接。
- `internal/types/builtin_models_config.go` 会保留非 YAML 管理的数据库覆盖值；实际业务 `GetRerankModel` 从数据库取配置。这里的测试复现 YAML 配置及当前工作区代码，不宣称已经核对运行中进程、DB 模型记录或原始大批量请求。

## 复现命令

在项目根目录运行。测试默认不会调用外部模型：

```bash
go test ./internal/models/rerank -count=1
go test ./internal/models/rerank -count=1 -race
```

生产 transport 对照：两个地址各最多等待 30 秒；当前环境预期两个子测试失败并给出阶段证据。

```bash
env WEKNORA_RERANK_LIVE=1 \
  WEKNORA_RERANK_ENV_FILE=/root/workspace/rag-platform/.env \
  WEKNORA_RERANK_TRANSPORT=production LLM_DEBUG_LOG=0 \
  go test ./internal/models/rerank \
  -run '^TestRerankerConnectivityLive$' -count=1 -v -timeout=90s
```

环境代理对照使用启动 shell 的 `http_proxy` / `https_proxy` / `NO_PROXY` 等变量。测试只替换测试进程的 transport，保留 SSRF 校验，不改变应用 transport。只验证完整地址的成功路径：

```bash
env WEKNORA_RERANK_LIVE=1 \
  WEKNORA_RERANK_ENV_FILE=/root/workspace/rag-platform/.env \
  WEKNORA_RERANK_TRANSPORT=environment_proxy LLM_DEBUG_LOG=0 \
  go test ./internal/models/rerank \
  -run '^TestRerankerConnectivityLive/complete_endpoint$' -count=1 -v -timeout=45s
```

将最后一条的 `-run` 改为 `'^TestRerankerConnectivityLive$'`、`-timeout` 改为 `90s`，会同时验证原地址的 404 与完整地址的成功；整体退出非零是原配置失败的真实诊断结果。

可选参数：`WEKNORA_RERANK_CONFIG` 指定 YAML 路径，`WEKNORA_RERANK_MODEL_ID` 选择其中的 rerank ID（默认 `builtin-rerank`）。支持 zhipu、generic、gpustack、openai；仅智谱自动增加完整 endpoint 对照项，其他 provider 使用其适配器现有规则。不会写回文件。`.env` 只提供安全策略及 YAML 的 `${NAME}` 插值后备值，已有进程环境优先；不会自动加载 `.env.local` 或启动其他应用功能。工厂校验耗时独立记录，30 秒 deadline 覆盖模型调用；`go test -timeout` 是整个测试进程的最终上限。

## 验证与后续建议

离线测试覆盖：智谱不补路径、完整智谱地址、通用 provider 追加路径，以及两个 provider 在等待响应头/响应体时的 deadline 退出。整个 rerank 包回归及 `-race` 检查均已通过，真实测试默认跳过。

建议后续修复分别处理三个位置：核对实际数据库配置并将智谱 reranker 设为完整 endpoint；让 reranker 按部署需求支持环境代理；增加有限的重排调用超时与失败降级策略。业务修复、服务重启及端到端知识库问答复测不在本次诊断测试的修改范围内。
