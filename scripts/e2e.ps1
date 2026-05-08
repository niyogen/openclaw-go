#Requires -Version 5.1
<#
.SYNOPSIS
  openclaw-go end-to-end smoke test for Windows (PowerShell).

.DESCRIPTION
  Builds the binary, starts the gateway on a free port, runs every major
  HTTP surface area, then stops the gateway.

.PARAMETER Token
  Optional bearer token for gateway auth.

.PARAMETER Port
  Port to bind the gateway on (default: auto-select).

.PARAMETER SkipBuild
  Skip the build step (use existing dist/openclaw).

.EXAMPLE
  .\scripts\e2e.ps1
  .\scripts\e2e.ps1 -Token "secret" -Port 18790
#>
param(
    [string]$Token  = "",
    [int]   $Port   = 0,
    [switch]$SkipBuild
)

$ErrorActionPreference = "Stop"
$Root   = Split-Path (Split-Path $PSScriptRoot -Parent) -Parent | Join-Path -ChildPath (Split-Path $PSScriptRoot -Leaf | ForEach-Object { ".." }) | Resolve-Path
$Root   = (Resolve-Path "$PSScriptRoot\..").Path
$Binary = Join-Path $Root "dist\openclaw.exe"

$PASS = 0; $FAIL = 0

function Pass($label) { Write-Host "  PASS  $label" -ForegroundColor Green; $global:PASS++ }
function Fail($label, $info) { Write-Host "  FAIL  $label - $info" -ForegroundColor Red; $global:FAIL++ }
function CheckStatus($label, $got, $want) {
    if ($got -eq $want) { Pass $label } else { Fail $label "got $got want $want" }
}

function Invoke-Get($path) {
    $h = @{}; if ($Token) { $h["Authorization"] = "Bearer $Token" }
    try { (Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$Port$path" -Headers $h -EA Stop).StatusCode }
    catch { $_.Exception.Response.StatusCode.Value__ }
}

function Invoke-Post($path, $body) {
    $h = @{ "Content-Type" = "application/json" }
    if ($Token) { $h["Authorization"] = "Bearer $Token" }
    try { (Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$Port$path" -Method Post -Headers $h -Body $body -EA Stop).StatusCode }
    catch { $_.Exception.Response.StatusCode.Value__ }
}

function Invoke-PostBody($path, $body) {
    $h = @{ "Content-Type" = "application/json" }
    if ($Token) { $h["Authorization"] = "Bearer $Token" }
    try { (Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$Port$path" -Method Post -Headers $h -Body $body -EA SilentlyContinue).Content }
    catch { "" }
}

function Invoke-Del($path) {
    $h = @{}; if ($Token) { $h["Authorization"] = "Bearer $Token" }
    try { (Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$Port$path" -Method Delete -Headers $h -EA Stop).StatusCode }
    catch { $_.Exception.Response.StatusCode.Value__ }
}

function Invoke-RPC($method, $params = "{}") {
    $body = '{"jsonrpc":"2.0","id":1,"method":"' + $method + '","params":' + $params + '}'
    $result = Invoke-PostBody "/rpc" $body
    if ($result -match '"result"') { Pass "rpc $method" } else { Fail "rpc $method" $result }
}

# ── Build ─────────────────────────────────────────────────────────────────────

if (-not $SkipBuild) {
    Write-Host "Building binary..." -ForegroundColor Cyan
    Push-Location $Root
    go build -o $Binary .\cmd\openclaw
    Pop-Location
}
if (-not (Test-Path $Binary)) { Write-Error "Binary not found at $Binary"; exit 1 }

# ── Pick free port ─────────────────────────────────────────────────────────────

if ($Port -eq 0) {
    $l = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
    $l.Start(); $Port = $l.LocalEndpoint.Port; $l.Stop()
}

# ── Start gateway ─────────────────────────────────────────────────────────────
# Write config directly into the user's real home dir under a unique e2e subfolder.
$RealHome   = [System.Environment]::GetFolderPath("UserProfile")
$E2ESubdir  = ".openclaw-go"   # use the standard path so the binary finds it
$CfgDir     = Join-Path $RealHome $E2ESubdir
$OrigCfg    = Join-Path $CfgDir "openclaw.json"
$BackupCfg  = Join-Path $CfgDir "openclaw.json.e2e-backup"
$TmpDir     = Join-Path ([System.IO.Path]::GetTempPath()) ("openclaw_e2e_" + [System.IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Force -Path $CfgDir | Out-Null
New-Item -ItemType Directory -Force -Path $TmpDir | Out-Null

# Back up existing config.
if (Test-Path $OrigCfg) { Copy-Item $OrigCfg $BackupCfg -Force }

# Write e2e config without BOM (PS 5 Set-Content adds BOM; use UTF8Encoding($false) to avoid it).
$cfg = @{ gateway = @{ host = "127.0.0.1"; port = $Port; authToken = $Token }; agent = @{ provider = "echo" } }
$cfgJson = $cfg | ConvertTo-Json -Depth 5
$utf8NoBom = New-Object System.Text.UTF8Encoding $false
[System.IO.File]::WriteAllText($OrigCfg, $cfgJson, $utf8NoBom)

$gw = Start-Process -FilePath $Binary -ArgumentList "gateway","run" -PassThru -WindowStyle Hidden
Start-Sleep -Milliseconds 2000

# Wait up to 10s for gateway.
$ready = $false
for ($i = 0; $i -lt 20; $i++) {
    try {
        $r = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$Port/health" -EA Stop
        if ($r.StatusCode -eq 200) { $ready = $true; break }
    } catch {}
    Start-Sleep -Milliseconds 500
}
if (-not $ready) { Stop-Process -Id $gw.Id -EA SilentlyContinue; Write-Error "Gateway did not start on port $Port"; exit 1 }

Write-Host ""
Write-Host "=== openclaw-go E2E smoke test  (port $Port) ===" -ForegroundColor Cyan

try {

# ── 1. Health & readiness ─────────────────────────────────────────────────────
Write-Host "`n1. Health & readiness" -ForegroundColor Cyan
foreach ($path in @("/health","/healthz","/ready","/readyz","/v1/health","/v1/healthz")) {
    CheckStatus $path (Invoke-Get $path) 200
}

# ── 2. Auth ───────────────────────────────────────────────────────────────────
Write-Host "`n2. Auth" -ForegroundColor Cyan
if ($Token) {
    $unauth = try { (Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$Port/sessions" -EA Stop).StatusCode } catch { $_.Exception.Response.StatusCode.Value__ }
    CheckStatus "/sessions no-token → 401" $unauth 401
    CheckStatus "/sessions with token → 200" (Invoke-Get "/sessions") 200
} else {
    Pass "auth skipped (no token configured)"
}

# ── 3. Sessions ────────────────────────────────────────────────────────────────
Write-Host "`n3. Sessions" -ForegroundColor Cyan
CheckStatus "POST /message"                (Invoke-Post "/message" '{"sessionId":"ps-s","message":"hello","channel":"cli"}') 200
CheckStatus "GET /sessions"               (Invoke-Get "/sessions") 200
CheckStatus "GET /sessions/ps-s"          (Invoke-Get "/sessions/ps-s") 200
CheckStatus "GET /sessions/ps-s/history"  (Invoke-Get "/sessions/ps-s/history") 200
CheckStatus "POST patch"                  (Invoke-Post "/sessions/ps-s/patch" '[{"index":0,"content":"patched"}]') 200
CheckStatus "POST kill"                   (Invoke-Post "/sessions/ps-s/kill" '{}') 200
CheckStatus "DELETE /sessions/ps-s"       (Invoke-Del "/sessions/ps-s") 200
CheckStatus "GET deleted → 404"           (Invoke-Get "/sessions/ps-s") 404

# ── 4. RPC ────────────────────────────────────────────────────────────────────
Write-Host "`n4. RPC methods" -ForegroundColor Cyan
Invoke-RPC "health"
Invoke-RPC "gateway.status"
Invoke-RPC "sessions.list"
Invoke-RPC "plugins.list"
Invoke-RPC "models.list"
Invoke-RPC "models.capability" '{"provider":"openai"}'
Invoke-RPC "tools.list"
Invoke-RPC "tools.invoke" '{"name":"echo","arguments":{"text":"hi"}}'
Invoke-RPC "tools.invoke" '{"name":"time.now","arguments":{}}'
Invoke-RPC "logs.list"
Invoke-RPC "cron.list"
Invoke-RPC "hooks.list"
Invoke-RPC "secrets.list"
Invoke-RPC "approvals.list"
Invoke-RPC "agent.run" '{"sessionId":"rpc-ps","message":"ping"}'
Invoke-RPC "message.send" '{"sessionId":"rpc-msg","message":"hi","channel":"cli"}'

# ── 5. Tools ──────────────────────────────────────────────────────────────────
Write-Host "`n5. Tools REST" -ForegroundColor Cyan
CheckStatus "GET /tools"                   (Invoke-Get "/tools") 200
CheckStatus "POST /tools/invoke echo"      (Invoke-Post "/tools/invoke" '{"name":"echo","arguments":{"text":"hi"}}') 200
CheckStatus "POST /tools/invoke time.now"  (Invoke-Post "/tools/invoke" '{"name":"time.now","arguments":{}}') 200
CheckStatus "POST /tools/invoke unknown"   (Invoke-Post "/tools/invoke" '{"name":"no.tool","arguments":{}}') 400

# ── 6. OpenAI compat ──────────────────────────────────────────────────────────
Write-Host "`n6. OpenAI-compat" -ForegroundColor Cyan
CheckStatus "GET /v1/models"              (Invoke-Get "/v1/models") 200
CheckStatus "POST /v1/chat/completions"   (Invoke-Post "/v1/chat/completions" '{"model":"echo","messages":[{"role":"user","content":"hi"}]}') 200

# ── 7. Cron ───────────────────────────────────────────────────────────────────
Write-Host "`n7. Cron" -ForegroundColor Cyan
CheckStatus "POST /cron"       (Invoke-Post "/cron" '{"id":"ps-job","name":"j","schedule":"@every 1h","command":"echo","enabled":true}') 200
CheckStatus "GET /cron"        (Invoke-Get "/cron") 200
CheckStatus "DELETE /cron/ps-job" (Invoke-Del "/cron/ps-job") 200

# ── 8. Hooks ──────────────────────────────────────────────────────────────────
Write-Host "`n8. Hooks" -ForegroundColor Cyan
CheckStatus "POST /hooks"      (Invoke-Post "/hooks" '{"id":"ps-hook","name":"h","event":"message.received","type":"log","enabled":true}') 200
CheckStatus "GET /hooks"       (Invoke-Get "/hooks") 200
CheckStatus "DELETE /hooks/ps-hook" (Invoke-Del "/hooks/ps-hook") 200

# ── 9. Secrets ────────────────────────────────────────────────────────────────
Write-Host "`n9. Secrets" -ForegroundColor Cyan
CheckStatus "POST /secrets"    (Invoke-Post "/secrets" '{"name":"PS_KEY","value":"val"}') 200
CheckStatus "GET /secrets"     (Invoke-Get "/secrets") 200
CheckStatus "DELETE /secrets/PS_KEY" (Invoke-Del "/secrets/PS_KEY") 200

# ── 10. Channel webhooks ──────────────────────────────────────────────────────
# Note: channel webhooks (/webhooks/telegram etc.) are only mounted when the
# corresponding channel is enabled in config.  The Go E2E suite tests them
# in-process (see e2e/e2e_test.go TestE2E_ChannelWebhooks).
# Here we just confirm the route 404s gracefully when channels are disabled.
Write-Host "`n10. Channel webhooks (disabled in default config - covered by Go E2E)" -ForegroundColor Cyan
$tgCode = try { (Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$Port/webhooks/telegram" -Method Post -Headers @{"Content-Type"="application/json"} -Body '{}' -EA Stop).StatusCode } catch { $_.Exception.Response.StatusCode.Value__ }
if ($tgCode -eq 404 -or $tgCode -eq 200) { Pass "channel route responds (404=disabled, 200=enabled)" } else { Fail "channel route" "unexpected $tgCode" }

} finally {
    # Stop gateway.
    Stop-Process -Id $gw.Id -ErrorAction SilentlyContinue
    # Restore original config.
    if (Test-Path $BackupCfg) {
        Copy-Item $BackupCfg $OrigCfg -Force
        Remove-Item $BackupCfg -Force -ErrorAction SilentlyContinue
    } else {
        Remove-Item $OrigCfg -Force -ErrorAction SilentlyContinue
    }
    Remove-Item -Recurse -Force $TmpDir -ErrorAction SilentlyContinue
}

# ── summary ───────────────────────────────────────────────────────────────────
Write-Host ""
Write-Host "================================" -ForegroundColor Cyan
$passColor = if ($FAIL -eq 0) { "Green" } else { "White" }
$failColor = if ($FAIL -eq 0) { "Green" } else { "Red" }
Write-Host "  PASS: $PASS" -ForegroundColor $passColor -NoNewline
Write-Host "   FAIL: $FAIL" -ForegroundColor $failColor
Write-Host "================================" -ForegroundColor Cyan
if ($FAIL -gt 0) { exit 1 }
