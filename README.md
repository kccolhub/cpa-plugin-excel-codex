# CPA Excel Codex 插件

这是一个适配 CLIProxyAPI（CPA）原生插件 ABI 的动态库插件。它参考
[`excel-codex-bridge`](https://github.com/Kaixxrua/excel-codex-bridge)，把 Excel 模型别名和
Responses API 接入 CPA：CPA 收到 `gpt-*-excel` 请求后，插件将请求转发到官方 BPS 上游或
`excel-sub2api`，并支持非流式和 SSE 流式响应。

插件支持两种凭证来源：

- `auth_mode: home` 通过 CPA 的 `host.auth.list` / `host.auth.get` 回调读取 Home 已保存的
  Codex 凭证，因此不需要再次登录。Home token 只允许发往固定的
  `https://bps.openai.com/basispoints/api`。
- `auth_mode: sidecar` 使用 `excel-sub2api` 自己的 API key。sidecar 仍然适合 CPA 与已登录
  电脑分离的部署，但 ChatGPT token 和 sidecar API key 是两种不同的凭证。

`auth_mode: auto` 会在官方 BPS 地址优先复用 Home 凭证，在其他地址使用 sidecar API key。
Home 保存的凭证由 CPA 自己负责刷新，插件每次请求读取最新文件，不会写回或复制一份登录态。

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
      auth_mode: sidecar
      api_key_env: EXCEL_CODEX_API_KEY
```

如果 CPA Home 节点已经有 Codex 登录凭证，可以直接使用 Home 模式，不需要部署
`excel-sub2api`：

```yaml
plugins:
  enabled: true
  configs:
    excel-codex:
      enabled: true
      priority: 20
      auth_mode: home
      base_url: https://bps.openai.com/basispoints/api
      auth_provider: codex
      # Home 有多个 Codex 账号时，可填 host.auth.list 返回的 auth_index
      # auth_index: codex-xxx.json
```

`base_url` 可以是：

- `http://excel-sub2api:8000`（sidecar，插件会请求 `/v1/responses`）；
- `https://host/v1`（sidecar 或其他兼容 bridge，只能配合 `auth_mode: sidecar`）；
- `https://bps.openai.com/basispoints/api`（Home 模式固定使用，会请求 `/responses`）。

sidecar API key 优先读配置中的 `api_key`，为空时读取 `api_key_env` 指定的环境变量。推荐
通过 Kubernetes Secret 注入环境变量，不要把 key 提交到配置仓库。Home 模式的
`account_id` 只作为可选覆盖值；一般让插件从 Home auth JSON 或 JWT 中自动读取。

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
安装。安装后在插件配置中选择 Home 或 sidecar 模式，再重新加载配置。

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
