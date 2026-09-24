# CLIProxyAPI-ZCode

[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 插件。把 ZCode 客户端
（智谱 BigModel / Z.ai 的 Coding Plan）额度接入 CPA，注册为 `zcode/*` 模型。

插件按 ZCode 桌面客户端构造上游请求：同样的伪装头、同样的 UA、同样的
`HTTP-Referer`，然后把 Anthropic Messages 流量原样转发到上游 Anthropic 兼容端点。

## 安装

### 从插件市场

在 CPA 管理中心的插件市场里搜索 `ZCode`，一键安装。

### 手动安装

1. 下载对应平台的 release 包（`zcode_<version>_<os>_<arch>.zip`），解压出动态库；
2. 放到 `<CPA>/plugins/<goos>/<goarch>/zcode.so`（macOS 是 `zcode.dylib`）；
3. 在 `config.yaml` 里加配置：

   ```yaml
   plugins:
     enabled: true
     configs:
       zcode:
         enabled: true
         priority: 1
         base_url: https://open.bigmodel.cn/api/anthropic
         models:
           - zcode/glm-5.3
           - zcode/glm-5.3-flash
   ```

4. 把凭据放进 `auth-dir`，文件名以 `zcode` 开头：

   ```json
   {"type":"zcode","api_key":"<apiKeyId>.<secretKey>","label":"BigModel Coding Plan"}
   ```

5. 重启 CPA。管理页在 `/v0/resource/plugins/zcode/console`。

## 配置项

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `base_url` | `https://api.z.ai/api/anthropic` | 上游 Anthropic 兼容基址。BigModel 用 `https://open.bigmodel.cn/api/anthropic` |
| `models` | `zcode/glm-5.3`、`zcode/glm-5.3-flash` | 注册给客户端调用的模型 ID。前缀 `zcode/` 避免和原生 provider 撞名 |
| `model_map` | 空 | 模型 ID 到上游模型名的覆盖映射 |
| `request_timeout` | `600` | 上游请求超时秒数 |
| `client_version` | `3.12.3` | 伪装 ZCode 客户端版本，影响 UA 和 `X-ZCode-App-Version` |
| `client_platform` | 按编译目标 | `X-Platform`，如 `darwin-arm64` |
| `client_os_category` | 按编译目标 | `X-Os-Category`，如 `macos` |
| `client_os_version` | 空 | `X-Os-Version`，留空则不发送该头 |
| `device_mid` | 空 | `X-Device-Mid`，留空则每次请求随机生成 |

## 管理接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/v0/management/zcode/status` | 配置、额度消耗计数、账号列表 |
| GET | `/v0/management/zcode/models` | 注册的模型 |
| POST | `/v0/management/zcode/probe` | 用一次 `count_tokens` 验证某个账号 key 是否可用 |
| GET | `/v0/resource/plugins/zcode/console` | 管理页（管理中心已记住密码时自动读取密钥） |

计数在进程内累计，重启归零。

## 从源码构建

需要 CGO 和对应平台的 C 工具链（`c-shared` 不能纯交叉编译）。

```bash
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  go build -mod=vendor -buildmode=c-shared -o zcode.so .
```

macOS 把输出名换成 `zcode.dylib`。推一个 `v*` tag 会触发 `.github/workflows/release.yml`
自动构建并发布 release。

## 上游请求特征

插件发出的请求头对齐 ZCode 客户端的 `buildZCodeSourceHeaders` 加 Anthropic 传输层：

```text
User-Agent: ZCode/<client_version>
HTTP-Referer: <base_url 的 origin>/
X-Title: Z Code@electron
X-ZCode-App-Version / X-Platform / X-Os-Category / X-Release-Channel
X-Client-Language / X-Client-Timezone
X-Api-Key / Authorization / Anthropic-Version / X-Request-Id
```

ZCode 客户端在自己的推理路径上**不**发送 `X-Zcode-Agent`、`X-Zcode-Session-Type`、
`X-Zcode-Trace-Id`，插件同样不发，避免多出客户端不会有的指纹。

## 说明

ZCode 客户端里有一套 `ClientRequestSigningV4` 请求签名（`X-Client-Sig` / `X-Client-Pow`
等），由服务端 `/api/v1/agent/configs` 的 `codingPlanSignature.enable` 开关控制。
截至 2026-09，该开关未对模型端点启用，签名头也不参与校验，因此本插件不实现签名。
如果上游将来强制签名，请求会返回 `VERIFY_SIGNATURE_INVALID` 或 `VERIFY_APIKEY_EXPIRED`。

## License

MIT
