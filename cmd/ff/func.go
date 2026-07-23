package main

import (
	"context"
	"net/url"

	"github.com/mmcdole/gofeed"
)

func ParseQueries(queries url.Values,
	filtersMap FilterFuncMap,
	modifiersMap ModifierFuncMap) ([]FilterFunc,
	[]ModifierFunc,
) {
	var filters []FilterFunc

	for key, values := range queries {
		for _, v := range values {
			f := CreateFilter(key, v, filtersMap)
			if f != nil {
				filters = append(filters, f)
			}
		}
	}

	var modifiers []ModifierFunc

	for key, values := range queries {
		for _, v := range values {
			m := CreateModifier(key, v, modifiersMap)
			if m != nil {
				modifiers = append(modifiers, m)
			}
		}
	}

	return filters, modifiers
}

func Apply(ctx context.Context, f *gofeed.Feed, ff []FilterFunc, mf []ModifierFunc) (*gofeed.Feed, error) {
	items := make([]*gofeed.Item, len(f.Items))
	count := 0

	for _, i := range f.Items {
		if filterApply(ctx, i, ff...) {
			mi := modifierApply(i, mf...)
			items[count] = mi
			count++
		}
	}

	f.Items = items[:count]

	return f, nil
}
