package combat

import "time"

// spawnStream is the zone's spawn random stream: the one place a combat draw
// comes from, so a seed pins every mob level and every respawn delay.
//
// It is splitmix64, written out rather than taken from math/rand, because the
// determinism claim is "the same seed gives the same fixture forever". A
// standard library generator is free to change its distribution algorithm
// between releases; six lines of arithmetic are not. Drawing with a modulo is
// biased, and deliberately so: the alternative is rejection sampling, whose
// consumption of the stream depends on the values drawn, which is exactly the
// property that makes a stream position stop being stable when content
// changes.
//
// It has no lock. Every draw happens inside the zone lock.
type spawnStream struct {
	state uint64
}

func newSpawnStream(seed uint64) *spawnStream { return &spawnStream{state: seed} }

func (stream *spawnStream) next() uint64 {
	stream.state += 0x9e3779b97f4a7c15
	value := stream.state
	value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9
	value = (value ^ (value >> 27)) * 0x94d049bb133111eb
	return value ^ (value >> 31)
}

// uniform draws from the inclusive range [low, high].
func (stream *spawnStream) uniform(low, high uint64) uint64 {
	if high <= low {
		// The draw still happens. Rule 5.1.5 performs a degenerate level draw
		// on purpose, so that widening a mob's level range later does not
		// shift every subsequent draw in the stream.
		_ = stream.next()
		return low
	}
	return low + stream.next()%(high-low+1)
}

// level draws a mob's level, rule 5.1.5.
func (stream *spawnStream) level(low, high uint32) uint32 {
	if low < 1 {
		low = 1
	}
	if high < low {
		high = low
	}
	return uint32(stream.uniform(uint64(low), uint64(high)))
}

// respawnDelay draws from the placement's window, rule 5.9.6.
func (stream *spawnStream) respawnDelay(low, high time.Duration) time.Duration {
	if low <= 0 {
		low = defaultRespawnMin
	}
	if high < low {
		high = low
	}
	return time.Duration(stream.uniform(uint64(low), uint64(high)))
}
