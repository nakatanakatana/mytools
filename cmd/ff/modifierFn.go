package main

import (
	"github.com/mmcdole/gofeed"
)

// Remove.
func RemoveDescription(_ string) ModifierFunc {
	return func(i *gofeed.Item) *gofeed.Item {
		mi := *i
		mi.Description = ""

		return &mi
	}
}

func RemoveContent(_ string) ModifierFunc {
	return func(i *gofeed.Item) *gofeed.Item {
		mi := *i
		mi.Content = ""

		return &mi
	}
}
