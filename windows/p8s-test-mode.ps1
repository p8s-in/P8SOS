# Switch Windows to P8S TEST MODE (VMware/VirtualBox can run VMs inside VMs; Docker Desktop/WSL2 do NOT work).
# Right-click -> "Run with PowerShell". It asks for Administrator.
if (-not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Start-Process powershell "-ExecutionPolicy Bypass -File `"$PSCommandPath`"" -Verb RunAs; exit
}
Write-Host "Switching to P8S TEST MODE..." -ForegroundColor Cyan
bcdedit /set hypervisorlaunchtype off | Out-Null
bcdedit /set vsmlaunchtype off | Out-Null
reg add "HKLM\SYSTEM\CurrentControlSet\Control\DeviceGuard" /v EnableVirtualizationBasedSecurity /t REG_DWORD /d 0 /f | Out-Null
reg add "HKLM\SYSTEM\CurrentControlSet\Control\DeviceGuard\Scenarios\HypervisorEnforcedCodeIntegrity" /v Enabled /t REG_DWORD /d 0 /f | Out-Null
Write-Host "Done. Nested virtualization will work after a restart." -ForegroundColor Green
if ((Read-Host "Restart now? (y/n)") -eq "y") { Restart-Computer }
