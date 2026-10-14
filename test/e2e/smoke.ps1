# Smoke CBL-3.6: compose-стенд mock-gateway поднимается, API провайдера и вебхуки работают.
# Запуск: powershell -NoProfile -File test/e2e/smoke.ps1
$ErrorActionPreference = "Stop"

$RepoRoot = Resolve-Path (Join-Path $PSScriptRoot "..\..")
$ComposeFile = Join-Path $RepoRoot "deploy\docker-compose.yml"
$BaseURL = "http://localhost:8120"

Push-Location $RepoRoot
try {
    docker compose -f $ComposeFile --profile app up -d --build
    if ($LASTEXITCODE -ne 0) { throw "docker compose up failed" }

    $ready = $false
    foreach ($attempt in 1..30) {
        try {
            $response = Invoke-WebRequest -UseBasicParsing -TimeoutSec 2 "$BaseURL/healthz"
            if ($response.StatusCode -eq 200) { $ready = $true; break }
        } catch {
            Start-Sleep -Seconds 2
        }
    }
    if (-not $ready) { throw "mock-gateway healthz is not ready" }

    $paymentID = [guid]::NewGuid().ToString()
    $expiresAt = (Get-Date).ToUniversalTime().AddMinutes(15).ToString("yyyy-MM-ddTHH:mm:ssZ")
    $invoiceBody = @{
        payment_id = $paymentID
        amount     = @{ minor_units = 100000; asset = "USDT"; decimals = 2 }
        network    = "trc20"
        expires_at = $expiresAt
    } | ConvertTo-Json -Depth 4
    $invoice = Invoke-RestMethod -Method Post -Uri "$BaseURL/api/v1/invoices" `
        -ContentType "application/json" -Headers @{ "Idempotency-Key" = "smoke-1" } -Body $invoiceBody
    if (-not $invoice.gateway_id) { throw "invoice has no gateway_id" }

    $replayed = Invoke-RestMethod -Method Post -Uri "$BaseURL/api/v1/invoices" `
        -ContentType "application/json" -Headers @{ "Idempotency-Key" = "smoke-1" } -Body $invoiceBody
    if ($replayed.gateway_id -ne $invoice.gateway_id) { throw "idempotency replay failed" }

    $fetched = Invoke-RestMethod -Method Get -Uri "$BaseURL/api/v1/invoices/$($invoice.gateway_id)"
    if ($fetched.status -ne "pending") { throw "unexpected invoice status $($fetched.status)" }

    $simulateBody = @{ gateway_id = $invoice.gateway_id; status = "confirmed"; duplicate = 2 } | ConvertTo-Json
    $simulated = Invoke-RestMethod -Method Post -Uri "$BaseURL/api/v1/simulate" `
        -ContentType "application/json" -Body $simulateBody
    if (-not $simulated.event_id) { throw "simulate returned no event_id" }
    if ($simulated.deliveries.Count -ne 2) { throw "expected two deliveries" }
    # В smoke цель вебхука (Webhook Ingress) может быть недоступна: доставка
    # обязана быть зафиксирована либо успехом, либо сетевой ошибкой (http_status 0).
    foreach ($delivery in $simulated.deliveries) {
        if ($delivery.http_status -ne 0 -and ($delivery.http_status -lt 200 -or $delivery.http_status -ge 300)) {
            throw "unexpected delivery status $($delivery.http_status): $($delivery.error)"
        }
    }

    $log = Invoke-RestMethod -Method Get -Uri "$BaseURL/api/v1/webhooks"
    if ($log.webhooks.Count -lt 2) { throw "webhook log is empty" }

    Write-Host "OK: invoice $($invoice.gateway_id) replayed, simulate $($simulated.event_id), deliveries $($log.webhooks.Count)"
} finally {
    docker compose -f $ComposeFile --profile app down -v | Out-Null
    Pop-Location
}
