Ali Slider Go Windows AMD64 便携版
==================================

本程序是运行在本机的控制台 HTTP 服务，不是桌面图形应用，但内置浏览器 API 测试页。
解压后可直接运行，无需安装 Go、Python、Node.js、OpenCV、VC++ Runtime 或第三方 DLL。
系统要求：Windows 10 / Windows Server 2016 或更高版本，AMD64/x64 处理器。
测试页需要现代 Edge、Chrome 或 Firefox，不支持 Internet Explorer。没有现代浏览器时，EXE 和 PowerShell/API 仍可使用。

安全边界
--------

1. 仅限自有系统或获得明确授权的测试环境。
2. 默认只监听 127.0.0.1:8000，不要改成 0.0.0.0 后直接暴露到公网或共享网络。
3. 服务没有应用内鉴权，也不会按本机在途请求数主动返回 429；只能在受控机器和网络中运行。
4. 默认启动会预热 32 个设备会话并访问外部 Device RPC。真实求解还需要访问 Captcha RPC 和图片 CDN。
5. 失败或低置信样本可能写入 var\artifacts。请解压到当前用户的私有可写目录，不要放在 Web root、公共共享盘、多人共享目录或公开同步目录。
6. Windows 文件权限继承解压目录的 NTFS ACL。不要把 var\artifacts、日志、token、certifyId、代理密码或完整响应发送给无关人员。
7. 旧 GET query 会立即执行真实求解，不是 health 或只读页面。禁止在 URL 放入 AaduaneId 或含账号密码的 proxy。

首次运行
--------

1. 必须先完整解压 ZIP。不要直接在压缩软件预览窗口中运行。
2. 双击 start.bat。
3. 等待控制台出现：event=listen status=ready。
4. API 测试页：http://127.0.0.1:8000/
5. 健康检查：http://127.0.0.1:8000/health
6. API 地址：http://127.0.0.1:8000/api/slider
7. OpenAPI：http://127.0.0.1:8000/openapi.json
8. 停止服务：在服务窗口按 Ctrl+C，然后等待窗口退出。

默认预热失败时，程序会记录 warning 并继续冷启动服务；网络较慢时，ready 最多可能等待约 25 秒。

浏览器测试
----------

1. 打开 http://127.0.0.1:8000/。
2. 确认页面显示“服务已就绪”。
3. 参数可全部留空；点击“发送一次求解”后检查 HTTP、业务状态、ok、VerifyCode 和 VerifyResult。
4. 页面打开只检查 health，不自动求解；执行期间防双击，也不会自动重试。
5. RPC key、代理、securityToken 和 certifyId 默认遮罩。不要把完整请求/响应截图、复制到公开日志、工单或聊天。
6. 页面不保存历史、Cookie 或浏览器存储；刷新或点击“清空”即清除当前结果。
7. 页面固定同源 POST JSON 调用，不使用旧 GET。其他网站的跨源请求不会成功：需要预检的浏览器 fetch 可能先被浏览器拦截；若明确跨源的 POST 或旧 GET 实际到达服务，则返回 403 ApiOriginError 且不进入 Solver。地址栏直接打开和无浏览器头的旧 API 客户端仍可用；这不是鉴权。不要放宽 CORS 绕过保护。

PowerShell 调用示例
-------------------

$body = @{
  SceneId = "1ug4aptr"
  prefix  = "fsgtmi"
} | ConvertTo-Json

Invoke-RestMethod `
  -Method Post `
  -Uri "http://127.0.0.1:8000/api/slider" `
  -ContentType "application/json" `
  -Body $body

这会访问真实上游。HTTP 200 仍需同时检查 ok、VerifyCode 和 VerifyResult；不要把完整响应写入公开日志。

旧 GET query 兼容
------------------

新调用请继续使用上面的 POST JSON。仅当旧客户端尚未迁移时，可使用：

Invoke-RestMethod `
  -Method Get `
  -Uri "http://127.0.0.1:8000/api/slider?SceneId=1ug4aptr"

该 GET 会立即创建真实挑战，不要用作书签、链接、预取、健康检查或监控地址。URL 可能进入浏览器历史和代理/网关 access log；禁止在 URL 放入 AaduaneId 或含 userinfo 的 proxy。

GET raw query 上限 64 KiB；支持 SceneId/sceneId、prefix/Prefix、AaduaneId/aaduaneId 和 proxy/Proxy。同名参数取最后值；标准字段只要出现就压过别名，标准字段为空时使用默认值，不使用别名。GET 只读 query，POST 只读 JSON body，两者不合并。不支持 URL-encoded 或 multipart form body。

POST 和旧 GET 的应用响应只有 HTTP 200/400/403/500，不会因本地在途请求数返回 429。

可选参数
--------

默认不需要配置。需要临时换端口时，在当前目录打开 cmd.exe：

start.bat --port=8001

此时测试页、健康检查、API 和 OpenAPI 地址中的端口也应改为 8001。页面使用同源路径，不需要其他配置。启动窗口开头标注的是默认 8000；最终以 event=listen 日志中的实际监听地址为准。

完整参数可运行：

ali-slider-go.exe --help

常见问题
--------

1. address already in use
   端口 8000 已被占用。关闭旧实例，或使用 start.bat --port=8001。

2. 健康检查暂时失败
   等待 status=ready。默认预热会先访问外部 Device RPC；网络慢时启动会延迟。

3. 测试页打不开
   确认控制台已出现 status=ready，并使用启动日志中的实际端口。若改为 --port=8001，页面地址也是 http://127.0.0.1:8001/。

4. 页面已打开，但 solve 失败
   页面打开和 /health 只证明本地 HTTP 服务可响应，不检查上游。根据 HTTP 状态、errorType 和 traceId 排查网络或协议问题。

5. Windows Defender / SmartScreen 提示未知发布者
   当前自动构建未做商业 Authenticode 代码签名。请只从可信仓库取得文件，并核对 SHA256SUMS.txt；不要关闭系统防护或绕过组织安全策略。

6. “此应用无法在你的电脑上运行”
   确认系统至少为 Windows 10 / Windows Server 2016，并使用 64 位 Intel/AMD 架构。本包不支持 32 位 Windows；ARM64 也不是本包的原生目标。

7. artifact_purge warning 或无法保存失败样本
   检查解压目录是否可写，以及当前用户是否具有该目录的 NTFS ACL 权限。不要以管理员身份作为常规解决办法。

完整性校验
----------

在 PowerShell 中运行：

Get-FileHash .\ali-slider-go.exe -Algorithm SHA256

结果应与 SHA256SUMS.txt 中 ali-slider-go.exe 对应值一致。SHA-256 只证明文件未变化，不等于发布者代码签名。

包内文件
--------

ali-slider-go.exe     Windows AMD64 控制台 HTTP 服务
start.bat             双击启动入口
README-Windows.txt    本说明
BUILD-INFO.txt        构建 commit、Go 版本和目标平台
SHA256SUMS.txt        包内文件完整性校验值
