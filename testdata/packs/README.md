# Vendored content packs

## demo

The golden fixture pack of [ADR 0029](https://github.com/SarnautCore/docs/blob/main/adr/0029-runtime-pack-format.md).
It is compiled from `data-schemas/demo`, a hand-authored dataset of invented
content, so it contains no MY.GAMES-derived data and may live in a public
repository. It is the only pack that may.

Every server test that needs content loads this pack, as does the SAR-20 client
smoke, so the Rust writer and the Go reader are exercised against the same
bytes.

It carries fourteen tables: the six every pack writes, plus `chargen`,
`items`, `loot-tables`, `quests`, `routes`, `locale`, `mob-kinds` and
`level-curve`, which appear only when the source tree authors documents of that
kind.

It carries six quests, and the set is chosen so that the quest state machine can
be driven entirely from content: one with no `objectives` key at all
(`mechanics/quests.md` rule 5.6), one gated on another finishing, one gated on a
level a fresh character does not have, one that counts held items rather than
kills, and two that count kills. The NPC that offers them stands in the player's
own faction with an aggro radius of zero, so nothing a test does can kill the
holder of its own turn-in.

It carries two loot trees and four items. The two trees are deliberately
different shapes — `loot.fixture.m2-nested` is the curated depth-3 tree of
`mechanics/loot.md` section 6.1, `loot.fixture.m2-flat` is the flat `and` root
that section 8 counts as the shape of 4,225 of the 4,234 reference tables — so
that "adding a loot table is content" is a claim `internal/loot` can be tested
against rather than one it asserts about itself. The flat tree also carries a
chance of `0.00618751`, which is not representable in binary32 and is what pins
`LootNode.chances` to a double.

Rebuild it from the `tools` repository after changing the pack format or the
demo dataset:

```powershell
cargo run -p sarnaut-pack -- build --fixture `
  --src ..\data-schemas\demo --out ..\server\testdata\packs\demo
```

## demo-extended

The same dataset with `data-schemas/demo/overlays/m2-combat-extended` layered
on: a second ability and a second mob, and nothing else. `--overlay` names a
**layer id** out of `data-schemas/demo/overlays/layers.yaml`, not a directory
path; that file is the sole authority on which layers exist and in what order
they apply (ADR 0029). The layer is marked `apply_by_default: false`, which is
what keeps `demo` above free of it.

It exists to hold one claim honest. `mechanics/combat.md` says every combat
rule is content — the ability's range, damage and cooldown; the mob's level,
health multiplier, aggro radius, leash radius and respawn window — and the way
to prove that is to add some and change no Go. `internal/combat` runs its whole
kill loop against this pack as well as against `demo`, and a different number
comes out of every one of those fields.

```powershell
cargo run -p sarnaut-pack -- build --fixture `
  --src ..\data-schemas\demo `
  --overlay m2-combat-extended `
  --out ..\server\testdata\packs\demo-extended
```

`internal/pack` pins the resulting `pack_id`, so a rebuild that changes the
bytes fails the test suite until the pin is updated deliberately.

`sarnaut-pack build` also writes a `build-report.json` beside the manifest,
carrying the curation notes, the layer list and the reference counts. It is a
private-path artifact, it is not an input to `pack_id`, and it is deliberately
not committed here.

## demo-quest-unsupported

The same dataset with `data-schemas/demo/overlays/m2-quest-unsupported` layered
on: one quest whose only objective is `quest-count-special`, and nothing else.

It is the one vendored pack the shard is required to **refuse**. `pack.Load`
accepts it — the bytes are well formed and the objective kind is a value the
row type defines — and `quests.CatalogFromPack` then fails, naming the quest.
That split is the point: `mechanics/quests.md` rule 5.5.6 puts the refusal at
content-load rather than at completion time, because the alternative is offering
a player a quest that can never be finished.

```powershell
cargo run -p sarnaut-pack -- build --fixture `
  --src ..\data-schemas\demo `
  --overlay m2-quest-unsupported `
  --out ..\server\testdata\packs\demo-quest-unsupported
```

## Rules for every pack here

`--keep-extra` output must never be committed here: `scripts/check-fixture-pack.ps1`
fails the build if any committed manifest records `keep_extra: true`.

`build-report.json` is written beside every manifest and is git-ignored: it is a
private-path artifact and it is not an input to `pack_id`.
