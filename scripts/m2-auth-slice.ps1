<#
.SYNOPSIS
    Boots auth and a shard, then walks the whole M2 admission path: register,
    log in, create a character, enter the zone at its chargen spawn, and prove
    that an unauthenticated connection is refused.

.DESCRIPTION
    This is the end-to-end evidence for ADR 0030 and ADR 0032. Everything it
    exercises is real: a real auth process holding the only credentials for the
    `auth` schema, a real shard redeeming a ticket over NATS, and the vendored
    golden pack supplying the character-creation option.

    Infrastructure is a precondition rather than something this script starts.
    PostgreSQL, Valkey and NATS all come from `infra/compose`; auth and the
    shard refuse to start without them, because sessions, tickets and play locks
    have no fallback (ADR 0030 §2) and character state has nowhere else to live
    (ADR 0031). Start them with:

        docker compose -f ..\infra\compose\docker-compose.yml up -d

.PARAMETER Address
    QUIC address the shard listens on.

.PARAMETER AuthAddress
    HTTP address the auth API listens on.
#>
param(
    [string]$Address = "127.0.0.1:4442",
    [string]$AuthAddress = "127.0.0.1:8493",
    [string]$ShardHealthAddress = "127.0.0.1:8491",
    [string]$AuthHealthAddress = "127.0.0.1:8492",
    [string]$PostgresDsn = "postgres://sarnaut:sarnaut_dev@127.0.0.1:5433/sarnaut?sslmode=disable",
    [string]$ValkeyAddress = "127.0.0.1:6379",
    [string]$NatsUrl = "nats://127.0.0.1:4222"
)

$ErrorActionPreference = "Stop"
$repositoryRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$contentPack = Join-Path $repositoryRoot "testdata\packs\demo"
$zoneId = "InstLeague1"

function Resolve-Go {
    $command = Get-Command go -ErrorAction SilentlyContinue
    if ($null -ne $command) { return $command.Source }
    $windowsGo = "C:\Program Files\Go\bin\go.exe"
    if ($IsWindows -and (Test-Path -LiteralPath $windowsGo -PathType Leaf)) { return $windowsGo }
    throw "Go was not found on PATH. Install the version from go.mod."
}

function Wait-Ready {
    param([string]$Uri, [System.Diagnostics.Process]$Process, [string]$Name, [string]$ErrorLog)

    for ($attempt = 0; $attempt -lt 80; $attempt++) {
        if ($Process.HasExited) {
            $detail = Get-Content -LiteralPath $ErrorLog -Raw -ErrorAction SilentlyContinue
            throw "$Name exited before becoming ready. $detail"
        }
        try {
            $response = Invoke-WebRequest -Uri $Uri -UseBasicParsing -TimeoutSec 1
            if ($response.StatusCode -eq 200) { return }
        }
        catch {
        }
        Start-Sleep -Milliseconds 250
    }
    $detail = Get-Content -LiteralPath $ErrorLog -Raw -ErrorAction SilentlyContinue
    throw "$Name did not become ready at $Uri within 20 seconds. $detail"
}

$goExecutable = Resolve-Go
$temporaryRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("sarnaut-m2-auth-" + [Guid]::NewGuid().ToString("N"))
$binaryExtension = if ($IsWindows) { ".exe" } else { "" }
$processes = @()

try {
    New-Item -ItemType Directory -Path $temporaryRoot -Force | Out-Null
    if (-not (Test-Path -LiteralPath (Join-Path $contentPack "manifest.json") -PathType Leaf)) {
        throw "The vendored fixture pack was not found at $contentPack."
    }

    Write-Output "== building =="
    foreach ($component in @("auth", "shard", "probe", "migrate")) {
        $output = Join-Path $temporaryRoot ($component + $binaryExtension)
        & $goExecutable build -o $output "./cmd/$component"
        if ($LASTEXITCODE -ne 0) { throw "Building cmd/$component failed with exit code $LASTEXITCODE." }
    }

    # The schema is applied by cmd/migrate, never by a service at start-up: two
    # services racing to migrate one database is not an operator's problem.
    $env:SARNAUT_POSTGRES_DSN = $PostgresDsn
    & (Join-Path $temporaryRoot ("migrate" + $binaryExtension)) up
    if ($LASTEXITCODE -ne 0) {
        throw "Migrations failed. Is infra/compose running? docker compose -f ..\infra\compose\docker-compose.yml up -d"
    }

    Write-Output "== starting auth =="
    $authEnvironment = @{
        SARNAUT_HEALTH_ADDRESS      = $AuthHealthAddress
        SARNAUT_AUTH_LISTEN_ADDRESS = $AuthAddress
        SARNAUT_CONTENT_PACK        = $contentPack
        SARNAUT_POSTGRES_DSN        = $PostgresDsn
        SARNAUT_VALKEY_ADDRESS      = $ValkeyAddress
        SARNAUT_NATS_URL            = $NatsUrl
        SARNAUT_OTEL_ENDPOINT       = ""
    }
    foreach ($name in $authEnvironment.Keys) {
        [System.Environment]::SetEnvironmentVariable($name, $authEnvironment[$name], "Process")
    }
    $authStdout = Join-Path $temporaryRoot "auth.stdout.log"
    $authStderr = Join-Path $temporaryRoot "auth.stderr.log"
    $authProcess = Start-Process -FilePath (Join-Path $temporaryRoot ("auth" + $binaryExtension)) `
        -WorkingDirectory $repositoryRoot -RedirectStandardOutput $authStdout -RedirectStandardError $authStderr -PassThru
    $processes += $authProcess
    Wait-Ready -Uri "http://$AuthHealthAddress/readyz" -Process $authProcess -Name "auth" -ErrorLog $authStderr

    Write-Output "== starting shard =="
    $shardEnvironment = @{
        SARNAUT_QUIC_LISTEN_ADDRESS = $Address
        SARNAUT_HEALTH_ADDRESS      = $ShardHealthAddress
        SARNAUT_CONTENT_PACK        = $contentPack
        SARNAUT_WORLD_ZONE_ID       = $zoneId
        SARNAUT_POSTGRES_DSN        = $PostgresDsn
        SARNAUT_VALKEY_ADDRESS      = $ValkeyAddress
        SARNAUT_NATS_URL            = $NatsUrl
        SARNAUT_OTEL_ENDPOINT       = ""
    }
    foreach ($name in $shardEnvironment.Keys) {
        [System.Environment]::SetEnvironmentVariable($name, $shardEnvironment[$name], "Process")
    }
    $shardStdout = Join-Path $temporaryRoot "shard.stdout.log"
    $shardStderr = Join-Path $temporaryRoot "shard.stderr.log"
    $shardProcess = Start-Process -FilePath (Join-Path $temporaryRoot ("shard" + $binaryExtension)) `
        -WorkingDirectory $repositoryRoot -RedirectStandardOutput $shardStdout -RedirectStandardError $shardStderr -PassThru
    $processes += $shardProcess
    Wait-Ready -Uri "http://$ShardHealthAddress/readyz" -Process $shardProcess -Name "shard" -ErrorLog $shardStderr

    # An account per run, so a rerun does not depend on what the last one left
    # behind. A character name is 3 to 16 ASCII letters (ADR 0032 §3), so the
    # run id is spelled in letters rather than hex.
    $runId = -join ((1..6) | ForEach-Object { [char](97 + (Get-Random -Maximum 26)) })
    $email = "slice-$runId@example.invalid"
    $password = "slice-password-$runId"
    $characterName = "Slice" + $runId

    Write-Output "== an unauthenticated connection is refused =="
    & (Join-Path $temporaryRoot ("probe" + $binaryExtension)) `
        -address $Address -zone $zoneId -expect-refusal -duration 10s
    if ($LASTEXITCODE -ne 0) {
        throw "The shard did not refuse an unauthenticated connection."
    }

    Write-Output "== register, log in, create a character, enter the zone =="
    $firstRun = & (Join-Path $temporaryRoot ("probe" + $binaryExtension)) `
        -address $Address -zone $zoneId -auth "http://$AuthAddress" `
        -email $email -password $password -character $characterName -duration 6s
    if ($LASTEXITCODE -ne 0) {
        throw "The authenticated probe failed with exit code $LASTEXITCODE."
    }
    $firstRun | ForEach-Object { Write-Output $_ }

    # The spawn the shard answered with has to be the chargen option's, which is
    # the whole point of ADR 0032: it is data, not a constant. Reading it back
    # from the service keeps this assertion honest when the data changes.
    $chargen = Invoke-RestMethod -Uri "http://$AuthAddress/v1/chargen/options" -TimeoutSec 5
    if ($chargen.options.Count -lt 1) { throw "The auth service offers no chargen options." }
    $option = $chargen.options[0]
    Write-Output ("chargen option {0} spawns at {1},{2},{3}" -f $option.id, $option.spawn_x, $option.spawn_y, $option.spawn_z)

    $firstSpawn = ($firstRun | Select-String -Pattern "spawn=([-0-9.e+]+),([-0-9.e+]+),([-0-9.e+]+)").Matches[0].Groups
    $firstX = [double]$firstSpawn[1].Value
    $firstY = [double]$firstSpawn[2].Value
    if ([math]::Abs($firstX - [double]$option.spawn_x) -gt 0.001 -or
        [math]::Abs($firstY - [double]$option.spawn_y) -gt 0.001) {
        throw "A fresh character spawned at $firstX,$firstY, not at the chargen spawn $($option.spawn_x),$($option.spawn_y)."
    }

    $shardLog = Get-Content -LiteralPath $shardStdout -Raw -ErrorAction SilentlyContinue
    if ($shardLog -notmatch "character entered zone") {
        throw "The shard log does not record a character entering the zone.`n$shardLog"
    }
    foreach ($secretValue in @($password, $email)) {
        if ($shardLog -match [regex]::Escape($secretValue)) {
            throw "The shard log carries a secret."
        }
    }
    $authLog = Get-Content -LiteralPath $authStdout -Raw -ErrorAction SilentlyContinue
    foreach ($secretValue in @($password, $email)) {
        if ($authLog -match [regex]::Escape($secretValue)) {
            throw "The auth log carries a secret (ADR 0030 §5)."
        }
    }
    if ($authLog -notmatch "sarnaut_tk_" -and $authLog -notmatch "ticket minted") {
        throw "The auth log does not record a ticket being minted.`n$authLog"
    }
    if ($authLog -match "sarnaut_tk_") {
        throw "The auth log carries a ticket value."
    }

    Write-Output "== reconnecting restores the saved position =="
    $secondRun = & (Join-Path $temporaryRoot ("probe" + $binaryExtension)) `
        -address $Address -zone $zoneId -auth "http://$AuthAddress" `
        -email $email -password $password -character $characterName -duration 6s
    if ($LASTEXITCODE -ne 0) {
        throw "The reconnect probe failed with exit code $LASTEXITCODE."
    }
    $secondRun | ForEach-Object { Write-Output $_ }

    # The probe walks east for its whole run, so the saved position is east of
    # the chargen spawn. A reconnect that landed back on the spawn would mean
    # the disconnect checkpoint never ran, or ran after the entity was evicted.
    $secondSpawn = ($secondRun | Select-String -Pattern "spawn=([-0-9.e+]+),([-0-9.e+]+),([-0-9.e+]+)").Matches[0].Groups
    $secondX = [double]$secondSpawn[1].Value
    if ($secondX -le $firstX) {
        throw "The reconnect spawned at x=$secondX, not east of the first spawn x=${firstX}: the character's position was not restored."
    }

    Write-Output "m2-auth-slice: OK"
}
finally {
    foreach ($process in $processes) {
        if ($null -ne $process -and -not $process.HasExited) {
            Stop-Process -Id $process.Id -ErrorAction SilentlyContinue
            Wait-Process -Id $process.Id -Timeout 5 -ErrorAction SilentlyContinue
        }
    }
    if (Test-Path -LiteralPath $temporaryRoot) {
        $resolved = [System.IO.Path]::GetFullPath($temporaryRoot)
        $safeParent = [System.IO.Path]::GetFullPath([System.IO.Path]::GetTempPath())
        if ($resolved.StartsWith($safeParent, [StringComparison]::OrdinalIgnoreCase) -and $resolved -ne $safeParent) {
            Remove-Item -LiteralPath $resolved -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}
