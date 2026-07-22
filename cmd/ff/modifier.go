package main

import (
	"github.com/mmcdole/gofeed"
)

type (
	ModifierFunc        = func(i *gofeed.Item) *gofeed.Item
	ModifierFuncCreator = func(param string) ModifierFunc
	ModifierFuncMap     = map[string]ModifierFuncCreator
)

func CreateModifierMap() ModifierFuncMap {
	return map[string]ModifierFuncCreator{
		"rm.description": RemoveDescription,
		"rm.content":     RemoveContent,
	}
}

func CreateModifier(key string, value string, modifiers map[string]ModifierFuncCreator) ModifierFunc {
	f, ok := modifiers[key]
	if !ok {
		return nil
	}

	return f(value)
}

func modifierApply(i *gofeed.Item, mf ...ModifierFunc) *gofeed.Item {
	mi := i
	for _, f := range mf {
		mi = f(mi)
	}

	return mi
}
