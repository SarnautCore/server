package loot

import (
	"encoding/binary"
	"encoding/hex"
	"math/rand/v2"

	"github.com/google/uuid"
	"github.com/zeebo/blake3"
)

// Constants of mechanics/loot.md section 3 that concern the stream.
const (
	// seedBytes is SEED_BYTES: the first sixteen bytes of the digest become the
	// two PCG64 words.
	seedBytes = 16
	// MaxDrawsPerRoll is MAX_DRAWS_PER_ROLL, a runaway guard rather than a
	// design limit. The curated depth-3 fixture spends eight draws.
	MaxDrawsPerRoll = 4096
)

// Stream is a named, ordered source of float64 values in [0, 1).
//
// The evaluator takes this interface and not a seed (rule 5.2.7). Production
// passes a PCG64-backed stream; a test passes a scripted one, and nothing in
// rules 5.3 to 5.5 can tell which it has.
type Stream interface {
	// NextFloat64 returns the next value, in [0, 1).
	NextFloat64() float64
	// Draws is how many values have been taken. It only ever increases, and it
	// is part of the contract: a roll's draw count is what catches an evaluator
	// that silently changed its draw budget.
	Draws() int
}

// Seed is the input tuple of rule 5.2.2.
//
// It is a struct rather than five arguments because the field order is part of
// the hash: swapping two arguments at one call site would reshuffle every drop
// in the game and compile cleanly.
type Seed struct {
	// WorldSeed is per-shard-instance configuration, not per-process-start
	// (rule 5.2.4). A restart must not change the seed a given corpse produces.
	WorldSeed string
	ZoneID    string
	// SpawnSlotID is the authored placement the victim came from.
	SpawnSlotID       string
	DeathServerTick   uint64
	KillerCharacterID uuid.UUID
}

// Digest is SEED_HASH over the length-prefixed concatenation of the tuple.
//
// Length prefixing is not decoration. Without it ("ab", "c") and ("a", "bc")
// hash to the same seed, and two different corpses would roll the same drop
// whenever their ids happened to line up that way.
func (seed Seed) Digest() [32]byte {
	hasher := blake3.New()
	writeLengthPrefixed(hasher, []byte(seed.WorldSeed))
	writeLengthPrefixed(hasher, []byte(seed.ZoneID))
	writeLengthPrefixed(hasher, []byte(seed.SpawnSlotID))
	var tick [8]byte
	binary.LittleEndian.PutUint64(tick[:], seed.DeathServerTick)
	writeLengthPrefixed(hasher, tick[:])
	killer := seed.KillerCharacterID
	writeLengthPrefixed(hasher, killer[:])

	var digest [32]byte
	copy(digest[:], hasher.Sum(nil))
	return digest
}

// Hex is the full digest as lowercase hex. It is what rule 5.2.5 logs before
// the first draw, and the only thing anybody needs to replay a roll offline.
func (seed Seed) Hex() string {
	digest := seed.Digest()
	return hex.EncodeToString(digest[:])
}

func writeLengthPrefixed(hasher *blake3.Hasher, payload []byte) {
	var length [4]byte
	binary.LittleEndian.PutUint32(length[:], uint32(len(payload)))
	_, _ = hasher.Write(length[:])
	_, _ = hasher.Write(payload)
}

// pcgStream is the production stream: PCG64 seeded from the first SEED_BYTES of
// the digest, read as two little-endian words (rule 5.2.3).
//
// Float64 is derived here rather than taken from math/rand/v2.Rand, and that is
// deliberate. PCG64's Uint64 is a published algorithm that cannot move under
// us; the standard library's float conversion is an implementation detail it is
// free to change, and the golden value in the seed-pinned test would then fail
// for a reason that has nothing to do with this package.
type pcgStream struct {
	source *rand.PCG
	draws  int
}

// NewStream builds the production stream for one corpse.
func NewStream(seed Seed) Stream {
	digest := seed.Digest()
	low := binary.LittleEndian.Uint64(digest[:8])
	high := binary.LittleEndian.Uint64(digest[8:seedBytes])
	return &pcgStream{source: rand.NewPCG(low, high)}
}

func (stream *pcgStream) NextFloat64() float64 {
	stream.draws++
	// 53 significant bits, the most a float64 holds, taken from the high end of
	// the word because PCG's low bits are the ones its output permutation works
	// hardest on. The result is in [0, 1): the numerator is at most 2^53 - 1.
	return float64(stream.source.Uint64()>>11) / (1 << 53)
}

func (stream *pcgStream) Draws() int { return stream.draws }

// ScriptedStream replays a fixed list of values and then repeats its last one.
//
// It exists so the worked example of mechanics/loot.md section 6.1 is a unit
// test rather than a description. Running off the end is not an error: the
// draw-count assertion is what catches an evaluator taking more draws than the
// trace says, and a panic here would only tell the same story less clearly.
type ScriptedStream struct {
	values []float64
	draws  int
}

// NewScriptedStream returns a stream over `values`.
func NewScriptedStream(values ...float64) *ScriptedStream {
	return &ScriptedStream{values: values}
}

func (stream *ScriptedStream) NextFloat64() float64 {
	if len(stream.values) == 0 {
		stream.draws++
		return 0
	}
	index := min(stream.draws, len(stream.values)-1)
	stream.draws++
	return stream.values[index]
}

func (stream *ScriptedStream) Draws() int { return stream.draws }
