# M3 tutorial exit driver

This command audits the private InstLeague1 pack and owns the final server-side
tutorial gate. It never writes pack content.

Staged mode is useful while server lanes are still landing:

```powershell
$env:SARNAUT_M3_INTEGRATION_PACK = 'E:\private\instleague1-pack'
go run ./scripts/m3-tutorial-driver -mode staged
```

It exits zero when available checks pass and prints every missing runtime proof
as `PENDING`. A reported failure still exits nonzero.

Strict mode must launch a production-composition scenario after `--`:

```powershell
go run ./scripts/m3-tutorial-driver -mode strict -- `
  go test ./internal/acceptance -run TestProductionTutorialJourney -count=1
```

The driver gives the child process these variables:

- `SARNAUT_M3_INTEGRATION_PACK`
- `SARNAUT_M3_ACCEPTANCE_PACK_ID`
- `SARNAUT_M3_ACCEPTANCE_REPORT`
- `SARNAUT_M3_ACCEPTANCE_RUN_ID`

The scenario must write the report before it exits. Strict mode rejects an
existing `-report`; it requires a new run and checks the one-time run ID. The
report schema is `sarnaut.m3-tutorial-acceptance/v1`, composition must be
`production`, and the pack ID must match the pack the driver opened.

Staged report inspection accepts `-report path.json`. That path may contain
`pending` proofs. It is for integration work and cannot close the strict gate.

The report has this shape:

```json
{
  "schema": "sarnaut.m3-tutorial-acceptance/v1",
  "run_id": "value supplied by the driver",
  "pack_id": "compiled pack identity",
  "build_id": "server build identity",
  "composition": "production",
  "started_at": "2026-08-22T00:00:00Z",
  "finished_at": "2026-08-22T00:02:00Z",
  "proofs": [
    {
      "id": "skipped-impacts",
      "outcome": "pass",
      "observed": 0,
      "detail": "runtime evaluator census was empty"
    }
  ]
}
```

The scenario must report every proof below:

- `production-composition`, exactly one fresh production composition
- `real-warrior-chargen`, exactly one real League Warrior creation
- `warrior-authored-action`, at least one server-resolved authored action
- `tutorial-quest-progress`, exactly 21 scripted quest activations with progress
- `tutorial-kills`, `tutorial-item-events`, and `tutorial-equip-events`, at least one each
- `player-death`, `player-respawn`, `experience-awards`, and `level-changes`, at least one each
- `terrain-cues`, `device-cues`, and `path-cues`, at least one each
- `restart-safe-deferred-impacts`, at least one impact completed after a server restart
- `skipped-impacts`, `skipped-handlers`, and `fallbacks`, exactly zero each
- `clean-shutdown`, exactly one completed shutdown

Strict is the default mode, so an unqualified CI invocation fails closed.
