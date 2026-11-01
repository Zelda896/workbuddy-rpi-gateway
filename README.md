# WorkBuddy Raspberry Pi Gateway

纯 Go、标准库实现的 WorkBuddy 兼容网关，提供：

- `GET /v1/models`
- `POST /v1/chat/completions`（OpenAI 兼容）
- `POST /v1/messages`（Anthropic Messages 兼容）
- `POST /v1/messages/count_tokens`（保守估算）
- SSE 流式转发、非流式聚合、401 自动刷新 token

## 配置

复制 `config.example.env`，再加载环境变量。凭据可以使用 WorkBuddy/CLIProxyAPI 形状的 JSON：

```json
{
  "auth": {
    "accessToken": "...",
    "refreshToken": "...",
    "expiresAt": 0,
    "domain": ""
  },
  "account": {
    "uid": "",
    "enterpriseId": "",
    "tenantId": ""
  }
}
```

不要把真实 token 提交到 Git。生产环境建议监听 `127.0.0.1`，通过 Nginx/Tailscale 暴露；如果直接监听局域网，设置 `WORKBUDDY_API_KEY`。

## 运行与交叉编译

```bash
set -a; . ./config.env; set +a
go run .

CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o workbuddy-gateway .
```

OpenAI 客户端使用 `http://127.0.0.1:8080/v1`，Anthropic 客户端使用 `http://127.0.0.1:8080`。
