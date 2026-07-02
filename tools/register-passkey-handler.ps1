# Registers the wapasskey:// URL scheme -> the local onboarding helper, so the
# manager's "Link via passkey" button launches it with one click (no terminal,
# no extension). Per-user (HKCU), so it needs NO admin rights. Run once.
#
#   powershell -ExecutionPolicy Bypass -File register-passkey-handler.ps1
#
# Uninstall:  Remove-Item HKCU:\Software\Classes\wapasskey -Recurse

param(
  [string]$Script = (Join-Path $PSScriptRoot "onboard-passkey.mjs")
)

$node = (Get-Command node -ErrorAction SilentlyContinue).Source
if (-not $node) { Write-Error "node not found on PATH. Install Node.js first."; exit 1 }
if (-not (Test-Path $Script)) { Write-Error "Helper not found: $Script"; exit 1 }

$cmd = "`"$node`" `"$Script`" `"%1`""
$base = "HKCU:\Software\Classes\wapasskey"

New-Item -Path $base -Force | Out-Null
Set-ItemProperty -Path $base -Name "(default)" -Value "URL:WhatsApp Passkey Helper"
Set-ItemProperty -Path $base -Name "URL Protocol" -Value ""
New-Item -Path "$base\shell\open\command" -Force | Out-Null
Set-ItemProperty -Path "$base\shell\open\command" -Name "(default)" -Value $cmd

Write-Host "Registered wapasskey:// ->" $cmd
Write-Host "You can now click 'Link via passkey' in the manager."
