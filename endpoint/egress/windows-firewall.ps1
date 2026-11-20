# AgentGate egress guard for a managed agent process (Windows).
# Run as Administrator. Creates:
#   1) allow outbound from the agent process to the AgentGate gateway only
#   2) block all other outbound traffic from that process
param(
  [Parameter(Mandatory=$true)][string]$AgentPath,
  [Parameter(Mandatory=$true)][string]$GatewayHost,
  [int]$GatewayPort = 8787
)

$allowName = "AgentGate-Allow-Gateway"
$blockName = "AgentGate-Block-Other-Egress"

Remove-NetFirewallRule -DisplayName $allowName -ErrorAction SilentlyContinue
Remove-NetFirewallRule -DisplayName $blockName -ErrorAction SilentlyContinue

New-NetFirewallRule -DisplayName $allowName -Direction Outbound -Action Allow `
  -Program $AgentPath -RemoteAddress $GatewayHost -RemotePort $GatewayPort -Protocol TCP | Out-Null

New-NetFirewallRule -DisplayName $blockName -Direction Outbound -Action Block `
  -Program $AgentPath | Out-Null

Write-Host "Egress guard applied for $AgentPath -> ${GatewayHost}:${GatewayPort} only."
