# Shows which mode Windows is in right now.
$vbs = (Get-CimInstance -ClassName Win32_DeviceGuard -Namespace root\Microsoft\Windows\DeviceGuard).VirtualizationBasedSecurityStatus
$hv  = (Get-CimInstance Win32_ComputerSystem).HypervisorPresent
if (-not $hv -and $vbs -eq 0) { Write-Host "P8S TEST MODE: VMware/VirtualBox nested VMs work. Docker Desktop is off." -ForegroundColor Green }
else { Write-Host "DOCKER MODE: Docker/WSL2 work. Nested VMs for P8S testing will NOT work. (hypervisor=$hv, VBS=$vbs)" -ForegroundColor Yellow }
Read-Host "Press Enter to close"
