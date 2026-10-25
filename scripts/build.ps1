<#
    Hand-built frontend-free build (scripts\build.ps1) - the only build entry of this project.

    WHY THIS FILE IS ASCII-ONLY: Windows PowerShell 5.1 decodes a .ps1 as ANSI (GBK here) when
    there is no UTF-8 BOM, so Chinese comments eat the quotes and braces next to them and the
    parser reports "unexpected token" at lines that look perfectly fine. ASCII decodes the same
    under UTF-8 and GBK, which makes it correct without depending on a BOM I cannot verify from
    here. Chinese prose belongs in README.md and the design doc instead.

    WHY THERE IS NO wails3 CALL: this machine's application control refuses freshly compiled
    Go binaries, and `wails build` dies exactly at "run the wails.exe helper I just built"
    (measured in PMDir). So the skeleton, the embedded assets and the JS bindings are all
    hand-written and committed; building is one `go build`. That is also why there is no
    wails.json and why the frontend is never copied to dist: a dist directory is an entry
    point where you can edit the wrong file and still see a green build.

    Usage (from anywhere):
        powershell -ExecutionPolicy Bypass -File scripts\build.ps1
        powershell -ExecutionPolicy Bypass -File scripts\build.ps1 -Clean
        powershell -ExecutionPolicy Bypass -File scripts\build.ps1 -Version 0.2.0

    -Version stamps main.appVersion with the release tag. Without it the exe carries whatever
    literal is in main.go, so a CI build of tag v0.2.0 would announce itself as 0.1.0 in the
    About panel - the artifact and its release would disagree and only a build flag can keep
    them the same.

    -Clean wipes build/ (the exe and download2_log.txt with it) and the Go build cache before
    rebuilding, so no stale artifact can make a rebuild look green. build/ holds artifacts
    only - the icon's sources live in tools\genicon. It never touches vendor/.

    Output: build\bin\download2.exe, then its size and SHA-256 - a fresh hash needs the user to
    approve it once, and without printing the hash we cannot even say which file was blocked.
#>

param(
    [switch]$Clean,
    [string]$Version = ""
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$exe = Join-Path $root "build\bin\download2.exe"

# -mod=vendor is the premise of an offline build: only packages that take part in the build
# land in vendor/, and after that the network is never touched again.
# If vendor/ is missing we stop - we do NOT silently fall back to -mod=mod, because that turns
# "builds here, does not build elsewhere" into a mine nobody knows about. The fix is one command.
if (-not (Test-Path (Join-Path $root "vendor\modules.txt"))) {
    Write-Host "No vendor directory yet. Run this once: go mod vendor (only this step needs the module cache)."
    exit 1
}

# -Clean deletes all of build\, which is safe because build\ holds nothing but artifacts
# (build\bin: the exe and download2_log.txt). It was not safe while the icon's .rc and .ico
# lived in there as well - the wipe took those inputs with it and the next build then stopped
# dead at the icon check below. They live in tools\genicon now, out of reach of this line.
# Deleting the whole directory also discards download2_log.txt - that is the point of
# "thorough", but it means a log you still wanted to read has to be copied out first.
# go clean -cache is the other half: without it a rebuild can be satisfied from cached objects
# and prove nothing about the current sources.
if ($Clean) {
    $buildDir = Join-Path $root "build"
    if (Test-Path -LiteralPath $buildDir) {
        Remove-Item -LiteralPath $buildDir -Recurse -Force
        Write-Host "Removed $buildDir"
    }
    $ErrorActionPreference = "Continue"
    & go clean -cache
    $crc = $LASTEXITCODE
    $ErrorActionPreference = "Stop"
    if ($crc -ne 0) {
        Write-Host "go clean -cache failed (exit code $crc)."
        exit $crc
    }
    Write-Host "Cleared the Go build cache."
}

# A missing WebView2 Runtime does not fail the build - it gives the user one page of runtime
# error at launch. So read the registry here and say the conclusion up front.
# HKLM covers the machine-wide install, HKCU the per-user one.
function Get-WebView2Version {
    $paths = @(
        "HKLM:\SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}",
        "HKLM:\SOFTWARE\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}",
        "HKCU:\Software\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}"
    )
    foreach ($p in $paths) {
        if (Test-Path $p) {
            $v = (Get-ItemProperty -Path $p -ErrorAction SilentlyContinue).pv
            if ($v -and $v -ne "0.0.0.0") { return $v }
        }
    }
    return $null
}

$wb = Get-WebView2Version
if ($wb) {
    Write-Host "WebView2 Runtime: $wb"
} else {
    Write-Host "WARNING: no WebView2 Runtime found. The exe will build, but the window will most likely stay white."
}

# -tags production is not optional: without it the dev build (application_dev.go) waits for
# FRONTEND_DEVSERVER_URL and the UI never loads. -H windowsgui drops the console window, at the
# price of losing stdout entirely - hence the file logger in main.go (download2_log.txt).
$ldflags = "-w -s -H windowsgui"
if ($Version -ne "") {
    # Checked before it reaches the linker line: a stray space or quote in there fails as
    # "bad flag syntax", which says nothing about the version you actually typed.
    if ($Version -notmatch '^[0-9A-Za-z][0-9A-Za-z._+-]*$') {
        Write-Host "-Version '$Version' is unusable: start with a letter or digit, then letters, digits, . _ + - only."
        exit 1
    }
    $ldflags = "$ldflags -X main.appVersion=$Version"
    Write-Host "Stamping appVersion = $Version"
}

$goArgs = @(
    "build",
    "-tags", "production",
    "-mod", "vendor",
    "-ldflags", $ldflags,
    "-o", $exe,
    "."
)

# The exe's icon. go build picks up any *_windows_amd64.syso in the package directory and
# hands it to the linker, which gets the icon in without the wails3 CLI (this machine's
# application control kills that helper, so it never runs here anyway).
#
# The .syso is committed, and CI is the reason: a GitHub windows runner has no windres, and
# the alternative - a build that quietly skips the icon there - is exactly the "CI ships
# something other than the local build" the release workflow refuses to allow. So the shipped
# default is the committed object and windres only ever refreshes it, on the machine that
# draws the icon.
#
# Refresh runs when the .syso is missing or older than its inputs, rather than every build,
# because a forced rebuild would stamp the .syso newer than its own inputs and then look stale
# again on the next build, forever. The comparison is loose after a fresh checkout (git hands
# every file roughly the same mtime), which costs at most one redundant windres run.
#
# The .ico is generated, not edited by hand: tools\genicon\gen_icon.py is its only source.
# Resource ID 3 in the .rc is not a free choice - see the comment in that file.
$syso = Join-Path $root "icon_windows_amd64.syso"
$rc = Join-Path $root "tools\genicon\icon.rc"
$ico = Join-Path $root "tools\genicon\icon.ico"
if (-not (Test-Path -LiteralPath $rc) -or -not (Test-Path -LiteralPath $ico)) {
    Write-Host "Missing an icon source - the exe would carry no icon:"
    if (-not (Test-Path -LiteralPath $rc)) { Write-Host "    $rc (hand-written: restore it from git)" }
    if (-not (Test-Path -LiteralPath $ico)) {
        Write-Host "    $ico (generated: F:\miniconda3\envs\mis\python.exe tools\genicon\gen_icon.py)"
    }
    exit 1
}
$needSyso = -not (Test-Path -LiteralPath $syso)
if (-not $needSyso) {
    $sysoTime = (Get-Item -LiteralPath $syso).LastWriteTime
    $needSyso = ((Get-Item -LiteralPath $rc).LastWriteTime -gt $sysoTime) -or ((Get-Item -LiteralPath $ico).LastWriteTime -gt $sysoTime)
}
if ($needSyso) {
    # WinLibs is a user-level winget package and its bin is not on PATH, so look it up by
    # prefix: the source id at the end of the directory name changes with the source.
    $windres = $null
    if ($env:DOWNLOAD2_MINGW_BIN) { $windres = Join-Path $env:DOWNLOAD2_MINGW_BIN "windres.exe" }
    if (-not $windres -or -not (Test-Path -LiteralPath $windres)) {
        $hit = Get-ChildItem -Directory -Path (Join-Path $env:LOCALAPPDATA "Microsoft\WinGet\Packages") -Filter "BrechtSanders.WinLibs.POSIX.UCRT*" -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($hit) { $windres = Join-Path $hit.FullName "mingw64\bin\windres.exe" }
    }
    if (-not $windres -or -not (Test-Path -LiteralPath $windres)) {
        # No windres is the normal state on CI, and it is not a failure: the committed .syso
        # already carries the icon and go build links it as-is. Only a build with neither can
        # produce an exe that has an icon.
        if (-not (Test-Path -LiteralPath $syso)) {
            Write-Host "No windres and no committed icon_windows_amd64.syso - the exe would carry no icon."
            Write-Host "    set DOWNLOAD2_MINGW_BIN to the dir holding windres.exe, or install BrechtSanders.WinLibs.POSIX.UCRT."
            exit 1
        }
        Write-Host "WARNING: no windres here - building with the committed icon_windows_amd64.syso."
        Write-Host "    if the icon changed, refresh that file on a machine that has windres and commit it."
    } else {
        Write-Host "windres: tools\genicon\icon.rc -> icon_windows_amd64.syso"
        & $windres --input=$rc --output=$syso --output-format=coff
        if ($LASTEXITCODE -ne 0) {
            Write-Host "windres failed (exit code $LASTEXITCODE)."
            exit $LASTEXITCODE
        }
    }
}

New-Item -ItemType Directory -Force -Path (Split-Path $exe) | Out-Null

Write-Host ("go " + ($goArgs -join " "))

# Relax for the native call: under "Stop", PS 5.1 promotes anything go writes to stderr into a
# terminating error, so a NativeCommandError buries the actual go diagnostics - the hardest kind
# of failure to read. The exit code below is the real verdict.
$ErrorActionPreference = "Continue"
& go @goArgs
$rc = $LASTEXITCODE
$ErrorActionPreference = "Stop"

if ($rc -ne 0) {
    Write-Host "go build failed (exit code $rc)."
    exit $rc
}

$info = Get-Item $exe
$hash = (Get-FileHash -Algorithm SHA256 -Path $exe).Hash
Write-Host ("Artifact: {0}  {1:N0} bytes  built {2:yyyy-MM-dd HH:mm:ss}" -f $info.FullName, $info.Length, $info.LastWriteTime)
Write-Host ("SHA-256: $hash")
Write-Host "Approve this new hash once before double-clicking. Log: download2_log.txt next to the exe."
