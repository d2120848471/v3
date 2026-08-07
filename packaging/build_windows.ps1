<#
.SYNOPSIS
    在 Windows 上把本项目打成免安装分发包。

.DESCRIPTION
    产出 dist\AliSlider\ 目录（含 AliSlider.exe）与同名 zip。目标机器不需要
    Python 或 OpenCV——两者都在包里。

    PyInstaller 不能跨平台交叉编译，所以这个脚本必须在 Windows 上跑。

.PARAMETER OneFile
    打成单个 exe。每次启动都要解压完整运行目录，冷启动更慢；除非确实需要
    单文件分发，否则别用。

.PARAMETER SkipZip
    只产出目录，不压缩。

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File packaging\build_windows.ps1
#>
[CmdletBinding()]
param(
    [switch]$OneFile,
    [switch]$SkipZip
)

$ErrorActionPreference = "Stop"

$PackagingDir = $PSScriptRoot
$ProjectRoot = Split-Path -Parent $PackagingDir
$BuildDir = Join-Path $ProjectRoot "build"
$DistDir = Join-Path $ProjectRoot "dist"
$VenvDir = Join-Path $BuildDir "venv"

function Write-Step([string]$Message) {
    Write-Host ""
    Write-Host "==> $Message" -ForegroundColor Cyan
}

function Write-Done([string]$Message) {
    Write-Host "    $Message" -ForegroundColor DarkGray
}


# --------------------------------------------------------------------------
# 调用外部程序的两个helper
#
# $ErrorActionPreference = "Stop" 之下，PowerShell 5.1 会把原生程序写到 stderr
# 的任何一行都升级成终止性错误——pip 随口一句 warning 就能让构建假失败。所以
# 调用外部程序期间必须临时降回 Continue，成败一律以退出码为准。
# --------------------------------------------------------------------------

function Invoke-Native {
    param(
        [Parameter(Mandatory = $true)][string]$Exe,
        [string[]]$Arguments = @(),
        [Parameter(Mandatory = $true)][string]$FailureMessage
    )
    $previous = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try {
        & $Exe @Arguments
        $code = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previous
    }
    if ($code -ne 0) { throw "$FailureMessage（退出码 $code）" }
}

function Get-NativeOutput {
    <# 取外部程序 stdout 的第一行；失败或没有输出时返回 $null。#>
    param(
        [Parameter(Mandatory = $true)][string]$Exe,
        [string[]]$Arguments = @()
    )
    $previous = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try {
        $output = & $Exe @Arguments 2>&1 | Select-Object -First 1
        $code = $LASTEXITCODE
    } catch {
        return $null
    } finally {
        $ErrorActionPreference = $previous
    }
    if ($code -ne 0 -or -not $output) { return $null }
    return "$output".Trim()
}


# --------------------------------------------------------------------------
# 1. 找一个 3.10 以上的 Python
# --------------------------------------------------------------------------

function Resolve-BuildPython {
    $pythonExe = $null
    $pythonPrefix = @()
    if ($env:pythonLocation) {
        $candidatePath = Join-Path $env:pythonLocation "python.exe"
        if (Test-Path -LiteralPath $candidatePath -PathType Leaf) {
            return @{
                Exe = [string]$candidatePath
                Prefix = @()
                Version = "3.12 (actions/setup-python)"
            }
        }
    }

    if (-not $pythonExe) {
        foreach ($name in @("py", "python", "python3")) {
            $command = Get-Command $name -CommandType Application -ErrorAction SilentlyContinue |
                Select-Object -First 1
            if (-not $command) { continue }
            $pythonExe = [string]$command.Path
            if ($name -eq "py") { $pythonPrefix = @("-3") }
            break
        }
    }
    if (-not $pythonExe) {
        throw "找不到 Python 3.10 或更高版本（已检查 pythonLocation、py、python 和 python3）。"
    }

    if ($pythonPrefix.Count -gt 0) {
        $version = Get-NativeOutput -Exe ([string]$pythonExe) `
            -Arguments @("-3", "--version")
    } else {
        $version = Get-NativeOutput -Exe ([string]$pythonExe) `
            -Arguments @("--version")
    }
    if (-not $version) {
        throw "找到 Python 但无法读取版本：$pythonExe"
    }

    $versionMatch = [regex]::Match([string]$version, "Python\s+(\d+)\.(\d+)")
    if (-not $versionMatch.Success) {
        throw "无法解析 Python 版本：$version"
    }
    $version = "$($versionMatch.Groups[1].Value).$($versionMatch.Groups[2].Value)"
    if ([int]$versionMatch.Groups[1].Value -ne 3 -or [int]$versionMatch.Groups[2].Value -lt 10) {
        throw "Python 版本过低，需要 3.10 或更高版本：$version"
    }
    return @{ Exe = [string]$pythonExe; Prefix = $pythonPrefix; Version = $version }
}

Write-Step "检查构建用 Python"
$python = Resolve-BuildPython
Write-Done "$($python.Exe) $($python.Prefix -join ' ')  ->  Python $($python.Version)"


# --------------------------------------------------------------------------
# 2. 干净的构建虚拟环境
# --------------------------------------------------------------------------

Write-Step "准备构建虚拟环境"
New-Item -ItemType Directory -Force -Path $BuildDir | Out-Null
if (Test-Path $VenvDir) { Remove-Item -Recurse -Force $VenvDir }

Invoke-Native -Exe $python.Exe `
    -Arguments (@($python.Prefix) + @("-m", "venv", $VenvDir)) `
    -FailureMessage "创建虚拟环境失败"

$VenvPython = Join-Path $VenvDir "Scripts\python.exe"
if (-not (Test-Path $VenvPython)) { throw "虚拟环境里没有 python.exe：$VenvPython" }
Write-Done $VenvDir

Write-Step "安装打包依赖"
Invoke-Native -Exe $VenvPython `
    -Arguments @("-m", "pip", "install", "--upgrade", "pip", "--quiet") `
    -FailureMessage "升级 pip 失败"

# headless 版 OpenCV 没有 GUI/视频依赖，本项目只用 core + imgproc，体积能省一大截。
Invoke-Native -Exe $VenvPython -Arguments @(
    "-m", "pip", "install", "--quiet",
    "requests>=2.31",
    "cryptography>=42",
    "opencv-python-headless>=4.8",
    "numpy>=1.26",
    "pyinstaller>=6.6"
) -FailureMessage "安装依赖失败"
Write-Done "requests / cryptography / opencv-headless / numpy / pyinstaller"


# --------------------------------------------------------------------------
# 3. 打包
# --------------------------------------------------------------------------

Write-Step "执行 PyInstaller"
if (Test-Path $DistDir) { Remove-Item -Recurse -Force $DistDir }

if ($OneFile) { $env:ALI_SLIDER_ONEFILE = "1" }
else { Remove-Item Env:ALI_SLIDER_ONEFILE -ErrorAction SilentlyContinue }

Invoke-Native -Exe $VenvPython -Arguments @(
    "-m", "PyInstaller",
    "--noconfirm", "--clean",
    "--distpath", $DistDir,
    "--workpath", (Join-Path $BuildDir "pyinstaller"),
    (Join-Path $PackagingDir "ali_slider.spec")
) -FailureMessage "PyInstaller 打包失败"


# --------------------------------------------------------------------------
# 4. 补齐说明文档并自检
# --------------------------------------------------------------------------

if ($OneFile) {
    $ExePath = Join-Path $DistDir "AliSlider.exe"
    $PayloadDir = $DistDir
} else {
    $ExePath = Join-Path $DistDir "AliSlider\AliSlider.exe"
    $PayloadDir = Join-Path $DistDir "AliSlider"
}
if (-not (Test-Path $ExePath)) { throw "没有产出可执行文件：$ExePath" }

$ReadmePath = Join-Path $PayloadDir "使用说明.txt"
Copy-Item -Force (Join-Path $PackagingDir "使用说明.txt") $ReadmePath

Write-Step "自检产物"
# doctor 会逐项确认轨迹资产与 Python 运行依赖是否都在包里。
Invoke-Native -Exe $ExePath -Arguments @("doctor") `
    -FailureMessage "产出的 exe 自检未通过，分发包不完整"
Write-Done "AliSlider.exe 自检通过"


# --------------------------------------------------------------------------
# 5. 压缩
# --------------------------------------------------------------------------

$sizeMb = [math]::Round(
    ((Get-ChildItem -Recurse $PayloadDir -File |
        Measure-Object -Property Length -Sum).Sum / 1MB), 1
)

if (-not $SkipZip) {
    Write-Step "压缩分发包"
    $ZipPath = Join-Path $DistDir "AliSlider-win64.zip"
    if (Test-Path $ZipPath) { Remove-Item -Force $ZipPath }
    # 单文件模式的产物直接躺在 dist 根下，压缩源必须逐个列出——把 dist 整个塞
    # 进它自己里面的 zip 是个自我包含的死循环。
    if ($OneFile) { $ZipSources = @($ExePath, $ReadmePath) }
    else { $ZipSources = @($PayloadDir) }
    Compress-Archive -Path $ZipSources -DestinationPath $ZipPath
    $zipMb = [math]::Round(((Get-Item $ZipPath).Length / 1MB), 1)
    Write-Done "$ZipPath（$zipMb MB）"
}

Write-Host ""
Write-Host "构建完成" -ForegroundColor Green
Write-Host "  可执行文件  $ExePath"
Write-Host "  解压后体积  $sizeMb MB"
Write-Host ""
Write-Host "把 dist 下的 zip 发给对方，解压后双击 AliSlider.exe 即可。" -ForegroundColor DarkGray
