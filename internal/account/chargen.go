package account

import (
	"errors"
	"fmt"
	"sort"

	"github.com/SarnautCore/server/internal/pack"
)

// Chargen failures a caller distinguishes.
var (
	// ErrUnknownOption reports a chargen_option_id no pack row carries.
	ErrUnknownOption = errors.New("account: unknown chargen option")
	// ErrOptionDisabled reports an option that exists and is not playable.
	// It is a separate answer from [ErrUnknownOption] because an option that
	// ships disabled is a promise, not a typo.
	ErrOptionDisabled = errors.New("account: chargen option is not enabled")
	// ErrNoChargenOptions reports a pack with no chargen table. Auth refuses to
	// start rather than serving an empty creation form.
	ErrNoChargenOptions = errors.New(
		"account: the content pack carries no chargen options: rebuild it from a source " +
			"tree that has chargen documents (ADR 0032). There is no built-in option list",
	)
)

// Catalogue is the set of character-creation options, read from the compiled
// pack and from nowhere else.
//
// This is the whole point of ADR 0032: the race and class a player may pick,
// the spawn they get and the gear they start with are pack rows, so the second
// playable option is a data change rather than a server rebuild. Nothing in
// this package may add a field the pack does not carry.
type Catalogue struct {
	byID    map[string]pack.ChargenOption
	ordered []pack.ChargenOption
}

// NewCatalogue indexes the options a loaded pack carries. It returns
// [ErrNoChargenOptions] when the pack has no chargen table, and refuses a pack
// whose rows do not carry the fields a character cannot be created without.
func NewCatalogue(options []pack.ChargenOption) (*Catalogue, error) {
	if len(options) == 0 {
		return nil, ErrNoChargenOptions
	}
	catalogue := &Catalogue{byID: make(map[string]pack.ChargenOption, len(options))}
	enabled := 0
	for _, option := range options {
		if option.ID == "" {
			return nil, fmt.Errorf("account: a chargen row carries no id")
		}
		if option.Race == "" || option.Class == "" || option.Faction == "" {
			return nil, fmt.Errorf("account: chargen option %q has no race, class or faction", option.ID)
		}
		if option.StartingLevel == 0 {
			return nil, fmt.Errorf("account: chargen option %q has no starting level", option.ID)
		}
		if option.SpawnZoneID == "" {
			return nil, fmt.Errorf("account: chargen option %q names no spawn zone", option.ID)
		}
		if _, duplicate := catalogue.byID[option.ID]; duplicate {
			return nil, fmt.Errorf("account: chargen option %q appears twice", option.ID)
		}
		catalogue.byID[option.ID] = option
		catalogue.ordered = append(catalogue.ordered, option)
		if option.Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		return nil, fmt.Errorf("%w: every option in the pack is disabled", ErrNoChargenOptions)
	}
	sort.Slice(catalogue.ordered, func(left, right int) bool {
		return catalogue.ordered[left].ID < catalogue.ordered[right].ID
	})
	return catalogue, nil
}

// Playable returns the options a client may offer, in canonical id order. The
// client renders this list and never derives it.
func (catalogue *Catalogue) Playable() []pack.ChargenOption {
	result := make([]pack.ChargenOption, 0, len(catalogue.ordered))
	for _, option := range catalogue.ordered {
		if option.Enabled {
			result = append(result, option)
		}
	}
	return result
}

// Select resolves an option a character is being created with. A disabled
// option is refused: shipping it in the pack is how the next one is queued, not
// how it is offered.
func (catalogue *Catalogue) Select(optionID string) (pack.ChargenOption, error) {
	option, ok := catalogue.byID[optionID]
	if !ok {
		return pack.ChargenOption{}, fmt.Errorf("%w: %q", ErrUnknownOption, optionID)
	}
	if !option.Enabled {
		return pack.ChargenOption{}, fmt.Errorf("%w: %q", ErrOptionDisabled, optionID)
	}
	return option, nil
}

// Lookup returns an option whether or not it is enabled, for a shard
// materializing a character that was created while the option was playable.
func (catalogue *Catalogue) Lookup(optionID string) (pack.ChargenOption, bool) {
	option, ok := catalogue.byID[optionID]
	return option, ok
}
