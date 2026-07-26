# go2api

`go2api` 是一个聚合多个 OpenCode Go 订阅 key 的本地代理服务,把多个低成本 key 合并成单一的兼容 OpenAI / Anthropic 的 API 端点,并附带请求级缓存、多种 key 调度策略和故障转移。

## 特性

- ✅ 同时兼容 **OpenAI Chat Completions** 和 **Anthropic Messages** 两种 API 格式
- ✅ 多种 key 调度策略:轮询 (`round_robin`)、加权轮询 (`weighted`)、额度感知 (`quota_aware`)
- ✅ 故障转移:遇到 429 / 5xx 自动切换下一个 key
- ✅ 请求级缓存:相同请求的第二次响应直接命中 SQLite,降低额度消耗
- ✅ SSE 流式透传 (`stream=true`)
- ✅ 简易管理 API:动态增删 key、查看额度、清空缓存

## 快速开始

```bash
# 1. 复制示例配置并填入你的 key
cp configs/config.example.yaml configs/config.yaml

# 2. 启动(默认监听 :8080,数据写入 data/go2api.db)
go run ./cmd/server -config configs/config.yaml

# 3. 调用
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer change-me-please" \
  -H "Content-Type: application/json" \
  -d '{"model":"kimi-k3","messages":[{"role":"user","content":"hello"}]}'
```

## API

| 方法   | 路径                          | 说明                                    |
| ------ | ----------------------------- | --------------------------------------- |
| POST   | `/v1/chat/completions`        | OpenAI 兼容转发(Grok/GLM/Kimi/DeepSeek…)|
| POST   | `/v1/messages`                | Anthropic 兼容转发(MiniMax/Qwen)         |
| GET    | `/v1/models`                  | 模型目录                                 |
| GET    | `/admin/keys`                 | 列出所有 key 及当前额度                  |
| POST   | `/admin/keys`                 | 动态新增 key                            |
| PATCH  | `/admin/keys/:id`             | 启用 / 停用 key                         |
| DELETE | `/admin/keys/:id`             | 删除 key                                |
| GET    | `/admin/stats`                | 过去 24h 调用 / 命中 / 延迟              |
| POST   | `/admin/cache/flush`          | 清空缓存                                |
| GET    | `/healthz`                    | 健康检查                                |

## 模型 ID

代理收到客户端请求后会直接用 `model` 字段转发(已自动剥离 `opencode-go/` 前缀),所以你只需要填简写即可:

| 简写                    | 端点                  | SDK 包                     |
| ----------------------- | --------------------- | -------------------------- |
| `grok-4.5`, `glm-5.2` … | `/chat/completions`   | `@ai-sdk/openai-compatible`|
| `minimax-m3`, `qwen3.7-max` … | `/messages`     | `@ai-sdk/anthropic`        |

## 配置示例

```yaml
keypool:
  strategy: weighted          # round_robin | weighted | quota_aware
  failover:
    enabled: true
    max_retries: 2
    cooldown_seconds: 60
    retry_on: [429, 500, 502, 503, 504]
  keys:
    - label: "personal-1"
      api_key: "sk-go-xxxxxx"
      weight: 1
    - label: "personal-2"
      api_key: "sk-go-yyyyyy"
      weight: 2
```

## 构建

```bash
go build -o bin/go2api ./cmd/server
./bin/go2api -config configs/config.yaml
```

## 数据存储

所有数据保存在 SQLite(`data/go2api.db`),包含:

- `api_keys` —— key 池与状态(weight、disabled、cooldown)
- `cache` —— 请求缓存(带 TTL)
- `quota_state` —— 各 key 的实时额度快照
- `request_logs` —— 调用日志(用于统计)

缓存失效策略:默认 1 小时;后台 goroutine 每 5 分钟清理过期行。流式请求不缓存。

## License

MIT