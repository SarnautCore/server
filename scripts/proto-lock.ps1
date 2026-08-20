<#
.SYNOPSIS
    Writes or verifies proto/PROTO_LOCK.sha256 over the canonical .proto set.

.DESCRIPTION
    The lock is plain sha256sum output over every .proto of the wire contract
    `sarnaut/v1`, relative to the proto root, sorted bytewise by path, with LF
    line endings (ADR 0027). The client repository commits a byte-identical
    copy, so a hand-edited client proto is detectable offline and inside a
    release artifact, where a server checkout is not available.

    The lock deliberately covers `sarnaut/v1` only. `sarnaut/content/v1`
    describes compiled pack rows, carries fields no client ever sees, and is not
    bound to the wire evolution rules of ADR 0027 (ADR 0029). Locking it would
    oblige the client to mirror a server-only schema.

    Digests are taken over the file with CRLF normalised to LF, so a checkout
    that ignores .gitattributes still produces the digest Linux CI computes.

.PARAMETER Check
    Verify only. Writes nothing and exits non-zero when the committed lock does
    not match the tree.
#>
param(
    [switch]$Check
)

$ErrorActionPreference = "Stop"

$repositoryRoot = Split-Path -Parent $PSScriptRoot
$protoRoot = Join-Path $repositoryRoot "proto"
$wireRoot = Join-Path $protoRoot "sarnaut\v1"
$lockPath = Join-Path $protoRoot "PROTO_LOCK.sha256"

function Get-ProtoLockContent {
    param([string]$Root, [string]$WireRoot)

    $files = Get-ChildItem -LiteralPath $WireRoot -Recurse -File -Filter "*.proto" |
        ForEach-Object {
            [System.IO.Path]::GetRelativePath($Root, $_.FullName).Replace("\", "/")
        } |
        Sort-Object -CaseSensitive

    if ($files.Count -eq 0) {
        throw "No .proto files were found under $WireRoot."
    }

    $sha256 = [System.Security.Cryptography.SHA256]::Create()
    try {
        $builder = [System.Text.StringBuilder]::new()
        foreach ($relativePath in $files) {
            $bytes = [System.IO.File]::ReadAllBytes((Join-Path $Root $relativePath))
            $text = [System.Text.Encoding]::UTF8.GetString($bytes).Replace("`r`n", "`n")
            $digest = $sha256.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($text))
            $hex = [System.BitConverter]::ToString($digest).Replace("-", "").ToLowerInvariant()
            [void]$builder.Append("$hex  $relativePath`n")
        }
        return $builder.ToString()
    }
    finally {
        $sha256.Dispose()
    }
}

$expected = Get-ProtoLockContent -Root $protoRoot -WireRoot $wireRoot

if ($Check) {
    if (-not (Test-Path -LiteralPath $lockPath -PathType Leaf)) {
        throw "proto/PROTO_LOCK.sha256 is missing. Run scripts/proto-lock.ps1."
    }
    $actual = [System.IO.File]::ReadAllText($lockPath).Replace("`r`n", "`n")
    if ($actual -ne $expected) {
        Write-Host "Committed proto/PROTO_LOCK.sha256:" -ForegroundColor Red
        Write-Host $actual
        Write-Host "Recomputed from proto/sarnaut/v1:" -ForegroundColor Red
        Write-Host $expected
        throw "proto/PROTO_LOCK.sha256 is stale. Run 'make generate' and commit the result."
    }
    Write-Host "proto/PROTO_LOCK.sha256 matches the proto tree."
    exit 0
}

[System.IO.File]::WriteAllText($lockPath, $expected, [System.Text.UTF8Encoding]::new($false))
Write-Host "Wrote proto/PROTO_LOCK.sha256."
