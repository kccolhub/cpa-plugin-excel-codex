# CPA Excel Codex 插件

这是一个适配 CLIProxyAPI（CPA）原生插件 ABI 的动态库插件。它把参考项目
[`excel-codex-bridge`](https://github.com/Kaixxrua/excel-codex-bridge) 的 Excel 模型别名和
Responses API 接入 CPA：CPA 收到 `gpt-*-excel` 请求后，插件将请求转发到你配置的
Excel bridge 或 `excel-sub2api` 端点，并支持非流式和 SSE 流式响应。

插件不会读取 CPA 容器所在机器上的 Excel WebView 缓存，也不会自己实现登录。服务器部署时，
请先按参考项目的 SUB2API 文档运行 bridge sidecar，并把 Excel/Codex 会话显式推送到你信任的
sidecar；插件只持有 sidecar API key。这样本机 Excel 登录态不会被插件隐式扫描或落盘。

## CPA 配置

先把自定义商店加入 CPA（官方商店不会自动包含这个仓库）：

```yaml
plugins:
  enabled: true
  store-sources:
    - https://raw.githubusercontent.com/kccolhub/cpa-plugin-excel-codex/main/registry.json
  configs:
    excel-codex:
      enabled: true
      priority: 20
      base_url: http://excel-sub2api:8000
      api_key_env: EXCEL_CODEX_API_KEY
```

`base_url` 可以是：

- `http://excel-sub2api:8000`（推荐，插件会请求 `/v1/responses`）；
- `https://host/v1`；
- `https://bps.openai.com/basispoints/api`（直接访问上游时使用，会请求 `/responses`）。

API key 优先读配置中的 `api_key`，为空时读取 `api_key_env` 指定的环境变量。推荐通过
Kubernetes Secret 注入环境变量，不要把 key 提交到配置仓库。`account_id` 可选，会同时发送
`chatgpt-account-id` 和 `x-openai-account-id`。

安装后插件会提供以下模型：

```text
gpt-5.6-luna-excel       gpt-5.6-luna-1m-excel
gpt-5.6-terra-excel      gpt-5.6-terra-1m-excel
gpt-5.6-sol-excel        gpt-5.6-sol-1m-excel
gpt-6-sol-excel          gpt-6-sol-1m-excel
gpt-6-luna-excel         gpt-6-luna-1m-excel
gpt-6-astra-excel        gpt-6-astra-1m-excel
```

## 管理 API 安装

启用 `plugins.enabled` 后，CPA 管理页面可从上面的 registry 发现 `Excel Codex` 并一键安装。
也可以直接调用管理 API（把地址和 key 换成你的 CPA）：

```bash
curl -X POST \
  -H 'X-Management-Key: <CPA_MANAGEMENT_KEY>' \
  'http://127.0.0.1:8317/v0/management/plugin-store/excel-codex/install'
```

安装器会从 GitHub Release 下载当前平台的动态库，校验 `checksums.txt`，并自动启用插件。CPA
需要能写入 `plugins.dir`；Kubernetes 部署请把该目录挂载到持久化卷，否则 Pod 重建后需要重新
安装。安装后在插件配置中填写 sidecar 地址和 key，再重新加载配置。

## 构建

本地构建当前平台：

```bash
cd go
go test ./...
CGO_ENABLED=1 go build -buildmode=c-shared -o excel-codex.dylib .
```

GitHub Actions 会在 PR 上测试，在 `v*` tag 上为 Linux amd64/arm64、macOS amd64/arm64、
Windows amd64/arm64 生成 CPA 兼容资产：

```text
excel-codex_<version>_<goos>_<goarch>.zip
checksums.txt
```

每个 zip 根目录只有对应的 `excel-codex.so`、`excel-codex.dylib` 或 `excel-codex.dll`，
符合 CPA plugin-store 的 GitHub release 安装规则。

## 许可与风险

本插件采用 MIT 许可。Excel bridge 的服务条款、账号权限和会话转发风险由部署者负责；请只把
会话推送到你控制或明确信任的 sidecar，并遵守相关服务条款。
