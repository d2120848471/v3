Ali Slider Go Windows AMD64 便携版
==================================

本程序是运行在本机的 HTTP 服务，不是图形界面。
解压后可直接运行，无需安装 Go、Python、Node.js、OpenCV、VC++ Runtime 或第三方 DLL。
系统要求：Windows 10 / Windows Server 2016 或更高版本，AMD64/x64 处理器。

安全边界
--------

1. 仅限自有系统或获得明确授权的测试环境。
2. 默认只监听 127.0.0.1:8000，不要改成 0.0.0.0 后直接暴露到公网或共享网络。
3. 服务没有应用内鉴权，也不会按本机在途请求数主动返回 429；只能在受控机器和网络中运行。
4. 默认启动会预热 32 个设备会话并访问外部 Device RPC。真实求解还需要访问 Captcha RPC 和图片 CDN。
5. 失败或低置信样本可能写入 var\artifacts。请解压到当前用户的私有可写目录，不要放在 Web root、公共共享盘、多人共享目录或公开同步目录。
6. Windows 文件权限继承解压目录的 NTFS ACL。不要把 var\artifacts、日志、token、certifyId、代理密码或完整响应发送给无关人员。

首次运行
--------

1. 必须先完整解压 ZIP。不要直接在压缩软件预览窗口中运行。
2. 双击 start.bat。
3. 等待控制台出现：event=listen status=ready。
4. 健康检查：http://127.0.0.1:8000/health
5. API 地址：http://127.0.0.1:8000/api/slider
6. OpenAPI：http://127.0.0.1:8000/openapi.json
7. 停止服务：在服务窗口按 Ctrl+C，然后等待窗口退出。

默认预热失败时，程序会记录 warning 并继续冷启动服务；网络较慢时，ready 最多可能等待约 25 秒。

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

可选参数
--------

默认不需要配置。需要临时换端口时，在当前目录打开 cmd.exe：

start.bat --port=8001

此时健康检查和 API 地址中的端口也应改为 8001。启动窗口开头标注的是默认 8000；最终以 event=listen 日志中的实际监听地址为准。

完整参数可运行：

ali-slider-go.exe --help

常见问题
--------

1. address already in use
   端口 8000 已被占用。关闭旧实例，或使用 start.bat --port=8001。

2. 健康检查暂时失败
   等待 status=ready。默认预热会先访问外部 Device RPC；网络慢时启动会延迟。

3. health ready，但 solve 失败
   /health 只证明本地 HTTP 服务可响应，不检查上游。根据 HTTP 状态、errorType 和 traceId 排查网络或协议问题。

4. Windows Defender / SmartScreen 提示未知发布者
   当前自动构建未做商业 Authenticode 代码签名。请只从可信仓库取得文件，并核对 SHA256SUMS.txt；不要关闭系统防护或绕过组织安全策略。

5. “此应用无法在你的电脑上运行”
   确认系统至少为 Windows 10 / Windows Server 2016，并使用 64 位 Intel/AMD 架构。本包不支持 32 位 Windows；ARM64 也不是本包的原生目标。

6. artifact_purge warning 或无法保存失败样本
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
