package pack

import (
	"fmt"
	"math"
	"unicode"

	"google.golang.org/protobuf/proto"

	contentv1 "github.com/SarnautCore/server/gen/sarnaut/content/v1"
)

const tableMapLocators = "map-locators"

type mapLocatorKey struct {
	mapID    string
	scriptID string
}

// HasMapLocatorIndex reports whether the pack carries map-locators.sptbl.
// Older packs remain loadable and report false.
func (p *Pack) HasMapLocatorIndex() bool {
	return p != nil && p.mapLocators != nil
}

// MapLocator resolves one exact product map slug and authored script id.
func (p *Pack) MapLocator(mapID, scriptID string) (Vec3, bool) {
	if p == nil {
		return Vec3{}, false
	}
	position, ok := p.mapLocators[mapLocatorKey{mapID: mapID, scriptID: scriptID}]
	return position, ok
}

func mapLocatorRowKey(mapID, scriptID string) string {
	return mapID + "/" + scriptID
}

func readMapLocators(tables map[string]*table) (map[mapLocatorKey]Vec3, error) {
	loaded, ok := tables[tableMapLocators]
	if !ok {
		return nil, nil
	}
	if want := contentv1.RowType_ROW_TYPE_MAP_LOCATOR; contentv1.RowType(loaded.rowTypeID) != want {
		return nil, fmt.Errorf("%w: table %q holds %s rows, want %s",
			ErrMalformedTable, tableMapLocators, contentv1.RowType(loaded.rowTypeID), want)
	}

	locators := make(map[mapLocatorKey]Vec3, loaded.rowCount)
	for ordinal := uint32(0); ordinal < loaded.rowCount; ordinal++ {
		var row contentv1.MapLocator
		if err := proto.Unmarshal(loaded.row(ordinal), &row); err != nil {
			return nil, fmt.Errorf("%w: decode map locator row %d: %w", ErrMalformedTable, ordinal, err)
		}
		if err := validateLocatorComponent("map_id", row.GetMapId()); err != nil {
			return nil, fmt.Errorf("%w: map locator row %d %w", ErrMalformedTable, ordinal, err)
		}
		if err := validateLocatorComponent("script_id", row.GetScriptId()); err != nil {
			return nil, fmt.Errorf("%w: map locator row %d %w", ErrMalformedTable, ordinal, err)
		}
		keyText := mapLocatorRowKey(row.GetMapId(), row.GetScriptId())
		if !loaded.rowKeyMatches(ordinal, keyText) {
			return nil, fmt.Errorf("%w: map locator row %d fields form key %q, which does not match its key index entry",
				ErrMalformedTable, ordinal, keyText)
		}
		position := row.GetPosition()
		if position == nil || !finiteFloat32(position.GetX()) || !finiteFloat32(position.GetY()) || !finiteFloat32(position.GetZ()) {
			return nil, fmt.Errorf("%w: map locator %q has a missing or non-finite position", ErrMalformedTable, keyText)
		}
		key := mapLocatorKey{mapID: row.GetMapId(), scriptID: row.GetScriptId()}
		if _, exists := locators[key]; exists {
			return nil, fmt.Errorf("%w: table %q repeats locator key %q", ErrMalformedTable, tableMapLocators, keyText)
		}
		locators[key] = vec3(position)
	}
	return locators, nil
}

func validateLocatorComponent(name, value string) error {
	if value == "" {
		return fmt.Errorf("has empty %s", name)
	}
	for _, character := range value {
		if character == '/' || character == '\\' || unicode.IsControl(character) {
			return fmt.Errorf("has %s %q containing a slash, backslash, or control character", name, value)
		}
	}
	return nil
}

func finiteFloat32(value float32) bool {
	return !math.IsNaN(float64(value)) && !math.IsInf(float64(value), 0)
}
