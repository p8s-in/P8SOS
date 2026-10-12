# Switch Windows to DOCKER MODE (Docker Desktop / WSL2 work; nested VMs for P8S testing do NOT).
# Right-click -> "Run with PowerShell". It asks for Administrator.
if (-not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Start-Process powershell "-ExecutionPolicy Bypass -File `"$PSCommandPath`"" -Verb RunAs; exit
}
Write-Host "Switching to DOCKER MODE..." -ForegroundColor Cyan
bcdedit /set hypervisorlaunchtype auto | Out-Null
Enable-WindowsOptionalFeature -Online -NoRestart -FeatureName VirtualMachinePlatform -All -WarningAction SilentlyContinue | Out-Null
Enable-WindowsOptionalFeature -Online -NoRestart -FeatureName HypervisorPlatform -All -WarningAction SilentlyContinue -ErrorAction SilentlyContinue | Out-Null
Write-Host "Done. Docker Desktop and WSL2 will work after a restart." -ForegroundColor Green
if ((Read-Host "Restart now? (y/n)") -eq "y") { Restart-Computer }
