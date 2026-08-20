# Vendored content packs

## demo

The golden fixture pack of [ADR 0029](https://github.com/SarnautCore/docs/blob/main/adr/0029-runtime-pack-format.md).
It is compiled from `data-schemas/demo`, a hand-authored dataset of invented
content, so it contains no MY.GAMES-derived data and may live in a public
repository. It is the only pack that may.

Every server test that needs content loads this pack, as does the SAR-20 client
smoke, so the Rust writer and the Go reader are exercised against the same
bytes.

Rebuild it from the `tools` repository after changing the pack format or the
demo dataset:

```powershell
cargo run -p sarnaut-pack -- build --fixture `
  --src ..\data-schemas\demo --out ..\server\testdata\packs\demo
```

## demo-extended

The same dataset with `data-schemas/demo/overlays/m2-combat-extended` layered
on: a second ability and a second mob, and nothing else.

It exists to hold one claim honest. `mechanics/combat.md` says every combat
rule is content — the ability's range, damage and cooldown; the mob's level,
health multiplier, aggro radius, leash radius and respawn window — and the way
to prove that is to add some and change no Go. `internal/combat` runs its whole
kill loop against this pack as well as against `demo`, and a different number
comes out of every one of those fields.

```powershell
cargo run -p sarnaut-pack -- build --fixture `
  --src ..\data-schemas\demo `
  --overlay ..\data-schemas\demo\overlays\m2-combat-extended `
  --out ..\server\testdata\packs\demo-extended
```

`internal/pack` pins the resulting `pack_id`, so a rebuild that changes the
bytes fails the test suite until the pin is updated deliberately.

`--keep-extra` output must never be committed here: `scripts/check-fixture-pack.ps1`
fails the build if any committed manifest records `keep_extra: true`.
