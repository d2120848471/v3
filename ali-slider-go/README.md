# Ali Slider Go

`ali-slider-go` 提供可并发复用的 Go SDK 和 HTTP 服务。Go 负责单轮编排、网络、图像识别和轨迹；动态 Device SDK 与 PE 通过同进程 V8 执行。Init 返回的 `CaptchaType` 决定 `PUZZLE`、`TRACELESS` 或 `SLIDING` 分支，每轮最多发送一次 Verify。

生产运行需要 Go 主程序与同平台 V8 wrapper。Go 使用 `CGO_ENABLED=0` 和 `purego`，服务端无需 Python、Node.js 或浏览器。仅用于自有系统或已获授权的测试环境；服务没有应用内鉴权，默认监听 `127.0.0.1:8000`。

## 启动与 HTTP 接入

- [启动指南](docs/guides/getting-started.md)：Linux/macOS 源码、Windows 便携包和 Docker。
- [HTTP API](docs/reference/http-api.md)：V3 验证码调用 `POST /api/slider`，Baxia 调用 `POST /api/bxua`，WAF/ESA 页面验证调用 `POST /api/waf`；包含代理、完整字段、错误和 OpenAPI 合同。
- [配置参考](docs/reference/configuration.md)：命令行参数、环境变量、默认值与资源边界。

启动成功后打开 `http://127.0.0.1:8000/` 使用内嵌测试页，或访问 `/health` 检查进程就绪。启动和 health 不访问真实上游；手工提交后才执行对应操作。V3 接口的 HTTP `200` 仍需读取 `ok`、`VerifyCode` 和 `VerifyResult` 判断业务结果。

WAF/ESA 使用独立的 `POST /api/waf`，JSON 只需必填 `pageUrl` 和可选 `proxy`（兼容 `Proxy`）。服务器读取保护页，自动提取本轮挑战并执行 `verifyType1.0` / `InitCaptchaV2 → SLIDING` 验证，无需调用方填写场景、用户、token、地区或语言。字段名区分大小写，未知字段忽略；`proxy` 与 `Proxy` 同时出现时，`proxy` 优先，包括空值。

将以下地址替换为当前出现上述 WAF/ESA 挑战的授权页面后调用；需要代理时增加 `proxy`，本轮全部网络请求与后续业务请求须保持相同出口：

```bash
curl --fail-with-body --request POST \
  --header 'Content-Type: application/json' \
  --data '{"pageUrl":"https://example.com/pc/index.html?orgId=your-org"}' \
  http://127.0.0.1:8000/api/waf
```

页面 URL 仅允许无凭据、无 fragment、使用默认或 443 端口的公网域名 HTTPS 地址。成功响应包含 `u_atoken`、`u_asig`、`verifyCode` 和匹配设备画像的 `uaHeaders`；后续携带签名请求须使用返回的 `uaHeaders`。签名成功不表示已进入原页面或完成登录，服务不自动回访页面或提交后续业务。完整字段、示例与错误见 [HTTP API](docs/reference/http-api.md#waf--esapost-apiwaf)。

`bmy.albatrip.cn` 已完成一次自动抓页模式的真实生产 HTTP 验收：调用方仅提交 `pageUrl` 和 `proxy`，返回 HTTP `200`、`ok=true`、`verifyCode=T001`，耗时 6,316 ms；携带返回签名与 `uaHeaders` 回访取得 HTTP `200`，命中“快速购票”标题，未再出现挑战。验收使用无 Node 的 PATH、`CGO_ENABLED=0` 和实际 V8 库。此前自动抓页模式的 4 次 Go + V8 Verify 也均返回 `T001`；这些结果仅覆盖此站点，尚未做跨站实测。WAF 路径使用新增的 V8 原生浏览器绑定，需按本版源码重建同平台 V8 wrapper；旧库不包含该能力。离线合同测试与真实站点验收分别执行。

自动抓页模式的真实 HTTP 验收只运行以下测试；`F001` 等拒绝会使测试失败，不把请求发出或 HTTP 200 当作通过：

```bash
CGO_ENABLED=0 \
ALI_SLIDER_V8_TEST_LIBRARY=/absolute/path/to/platform-v8-library \
ALI_SLIDER_WAF_ONLINE_URL='https://your-authorized-page.example/' \
go test -tags online -run '^TestOnlineWAFHTTP$' -count=1 -v ./internal/bootstrap
```

可选设置 `ALI_SLIDER_WAF_ONLINE_BODY_MARKER` 为预期业务页正文片段，测试才会额外携带签名和返回的 `uaHeaders` 回访一次并检查该片段；不设置时只验收验证结果。直接调用生产执行器的自动抓页验收仍可使用 `-run '^TestOnlineWAF$'`。

## Go SDK 接入

公共包路径保持为 `github.com/d2120848471/v3/ali-slider-go/pkg/slider`。在宿主中复用一个 `Client`，停止接收工作后关闭。以下示例会访问真实上游，场景与 wrapper 路径应替换为自己的授权配置：

```go
package main

import (
	"context"
	"log"

	"github.com/d2120848471/v3/ali-slider-go/pkg/slider"
)

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	options := slider.DefaultClientOptions()
	options.V8RuntimeLibrary = "/opt/ali-slider/libali_slider_v8_runtime.so"
	client, err := slider.NewClient(options)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.CheckRuntime(); err != nil {
		return err
	}
	result, err := client.Solve(ctx, slider.Request{
		SceneID: "1ug4aptr",
		Prefix:  "fsgtmi",
	})
	if err != nil {
		return err
	}
	log.Printf("ok=%t verifyCode=%s", result.OK, result.VerifyCode)
	return nil
}
```

`NewClient` 不发外部请求，`CheckRuntime` 只检查本地 wrapper/ABI/V8/ICU。`err == nil` 表示取得完成态结果，业务可能未通过；技术失败可用 `errors.As` 读取 `*slider.Error` 的 `Kind` 与 `Stage`。从 `DefaultClientOptions()` 开始覆盖配置，可保留置信度 `0` 等有意义的零值。

每轮独占 Device/V8、token、挑战、轨迹和 Verify 状态，结束时关闭会话。`Close` 拒绝新工作并等待活动调用返回，不主动取消 Solve；取消由调用方 context 或总超时负责。`Prime` 保留为兼容空操作。HTTP 宿主在启动时和每小时清理失败样本；SDK 宿主负责定期调用 `PurgeArtifacts`。

方法与类型以 [公共包源码](pkg/slider) 和 `go doc ./pkg/slider` 为准；源码导航、错误合同和兼容边界见 [SDK 参考](docs/reference/go-sdk.md)。

## Baxia / Fireye 的 bx-ua

对外 HTTP 调用使用 `POST /api/bxua`，JSON 传 `pageUrl`、`requestUrl` 和可选 `proxy`（兼容 `Proxy`）。代理格式与 `/api/slider` 一致；服务使用现有 `--v8-library` 配置，无需另起进程。打开首页选择“Baxia bx-ua”即可手工调用；完整 curl 示例、返回字段及错误见 [Baxia HTTP 文档](docs/reference/http-api.md#baxiapost-apibxua)，机器可读合同位于 `/openapi.json`。

`pkg/baxia` 和独立命令 `cmd/bxua` 默认由 Go 获取当前 Fireye SDK，再在同进程 V8 中生成 `bx-ua`，与 V3 的 `Solve` 分开。运行时不需要浏览器或 Node。Go 下载器只访问允许的公开脚本地址；SDK 发现、采样和生成本身都在禁网 V8 中执行，不发送登录、遥测或验证码请求。

调用链是 `baxiaCommon.getUA(url) → postFYModule.getFYToken(url) → options.reqUrl = url → __fyModule.getFYToken(options)`。新会话先取得[当前 AWSC](https://g.alicdn.com/??/AWSC/AWSC/awsc.js)，在页面 URL 和设备画像对应的环境中执行原 loader，捕获它实际选择的 Fireye 地址，再下载该脚本。当前站点的配置包含稳定 `1.231.69` 和灰度 `1.234.37`；不能简单挑最高版本，也不把某个数字写死为生产校验规则。

每份源码在初始化时恢复 `RuntimeProfile`：实际协议版本、token 前缀、输出外层编码字母表和 SDK SHA-256。字母表从真实 getter 的编码调用中取证并绑定完整输出，Go 再按恢复的字母表严格解码和重编码核对样本。`234` 的字母表位于运行时字符串池，不能靠固定变量名或源码偏移提取。核心算法由本次下载的原 SDK 执行，内部实现随脚本一起更新；这些 profile 字段不代表已将 SDK 内部加密算法重写成 Go。

每次创建新会话都会向 CDN 重新确认 AWSC 和选中的 SDK。复用 `baxia.Client` 时可以通过 ETag/Last-Modified 条件请求复用未变化的源码；同 URL 内容发生变化也会重新采样，不按协议版本号复用旧 profile，下载失败不会静默使用旧版。已建立的 Session 固定自己的源码与 profile，连续 `Token` 调用保留原会话状态；新版本只作用于后续新会话。

```bash
go run ./cmd/bxua \
  -v8-library /opt/ali-slider/libali_slider_v8_runtime.so \
  -page-url 'https://www.galaxyticketing.com/en/#/login/account?loginBefore=%252FuserCenter%252FaccountList' \
  -request-url 'https://rest-sig.imaitix.com/api/user/userLogin?_bx-v=2.5.37'
```

V8 动态库必须与执行命令的操作系统和 CPU 一致；macOS 使用 `.dylib`，Windows 使用 `.dll`。输出 JSON 包含完整 `bx-ua`、版本、实际下载地址及摘要、恢复的 `profile` 和同一画像对应的 `uaHeaders`。`-proxy` 只影响公开脚本下载，`-timeout` 控制整次操作。省略 `-profile` 时沿用项目的移动 Chromium 画像；用 `-profile profile.json` 可以传入 `baxia.Profile` 的 JSON。画像必须与宿主实际发送的 User-Agent 等字段一致。

Go 宿主先创建 `client, err := baxia.NewClient(baxia.ClientOptions{})`，再调用 `client.NewSession(ctx, baxia.Config{V8LibraryPath: libraryPath, PageURL: pageURL, Profile: profile})`。Session 使用完毕调用 `Close()`；Client 的 `Close()` 取消正在构造的会话并释放下载连接，已交付的 Session 仍单独关闭。单次调用也可以用 `baxia.NewSession`。`baxia.GenerateProfile()` 创建默认画像，`Session.Profile()` 返回经校验的编码合同。

需要离线复现时，显式传 `Config.SDKSource` 或命令的 `-sdk /path/to/fireye.js`，就会跳过网络发现与下载，但仍执行相同的 profile 恢复和校验。原始 SDK 不纳入源码仓库。新脚本如果更改了导出接口、必需环境或编码路径，会明确返回 `ErrUnsupportedSDK` 或运行时错误，不靠删除版本限制或回退旧字母表伪装兼容。自动更新不保证任意未来算法变化均无需适配；本地 profile 检查也不等同于服务端认可。

生成结果不等于服务端风控放行。浏览器的 Cookie、交互历史和环境会影响结果，同一 URL 的 token 也不固定；这个入口只生成 `bx-ua`，不包含独立的 `bx_et`、`bx-umidtoken` 或图片验证结果。当前目标原始登录请求虽返回 HTTP 200，响应带有 `Bxpunish: 1`，随后进入图片验证。

## 开发与维护

```bash
make fmt-check
make vet
make test
```

`make test` 包括架构依赖检查；`make bench` 只运行离线视觉和 Mock HTTP 基准。完整命令、真实 V8 ABI、覆盖率和平台门禁见 [开发指南](docs/guides/development.md)。Dockerfile 位于 [build/docker](build/docker/Dockerfile)，构建 context 仍是本 module 根目录；Windows 包素材位于 [build/packaging/windows](build/packaging/windows)。

应用编排、共享生命周期和生产组装分别由 `internal/application/solve`、`internal/application/service` 与 `internal/bootstrap` 承担。HTTP 与 SDK 共同使用应用 service；公共 `pkg/slider` 保留 DTO、错误及兼容映射。目录和依赖方向见 [架构](docs/design/architecture.md)，迁移映射见 [迁移指南](docs/guides/migration.md)。内部路径不是稳定接入 API。

`MaxConcurrency` 只控制出站每 host 连接和 PE 空闲引擎保留量；HTTP 在途请求、Device 会话与活跃 PE 调用没有本地并发上限。完整 Client/HTTP 延迟和长时间资源稳定性需要针对实际部署测量，已有离线数字的范围见 [性能说明](docs/design/performance.md)。全部文档见 [索引](docs/README.md)，用户可见变化见 [CHANGELOG](CHANGELOG.md)。
