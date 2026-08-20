package inventory

import "github.com/SarnautCore/server/internal/pack"

// packLimits reads stack limits out of a loaded content pack.
//
// It is three lines because the pack already answers the question lazily: the
// item table is looked up through its key index and never materialized, so a
// bag insertion costs one binary search per distinct item rather than a map of
// every item in the game.
type packLimits struct {
	content *pack.Pack
}

// LimitsFromPack adapts a loaded pack to [Limits].
func LimitsFromPack(content *pack.Pack) Limits { return packLimits{content: content} }

func (limits packLimits) StackLimit(itemID string) (int32, bool) {
	item, ok := limits.content.Item(itemID)
	if !ok {
		return 0, false
	}
	return item.Stack(), true
}
