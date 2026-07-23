package main

import (
	"context"

	"github.com/mmcdole/gofeed"
)

type (
	FilterFunc        = func(ctx context.Context, i *gofeed.Item) bool
	FilterFuncCreator = func(param string) FilterFunc
	FilterFuncMap     = map[string]FilterFuncCreator
)

func CreateFiltersMap(muteAuthors, muteURLs []string) FilterFuncMap {
	return map[string]FilterFuncCreator{
		"title.equal":              TitleEqual,
		"description.equal":        DescriptionEqual,
		"link.equal":               LinkEqual,
		"author.equal":             AuthorEqual,
		"title.not_equal":          TitleNotEqual,
		"description.not_equal":    DescriptionNotEqual,
		"link.not_equal":           LinkNotEqual,
		"author.not_equal":         AuthorNotEqual,
		"title.contains":           TitleContains,
		"description.contains":     DescriptionContains,
		"link.contains":            LinkContains,
		"author.contains":          AuthorContains,
		"title.not_contains":       TitleNotContains,
		"description.not_contains": DescriptionNotContains,
		"link.not_contains":        LinkNotContains,
		"author.not_contains":      AuthorNotContains,
		"updated_at.from":          UpdateAtFrom,
		"published_at.from":        PublishedAtFrom,
		"updated_at.latest":        UpdateAtLatest,
		"published_at.latest":      PublishedAtLatest,
		"latest":                   DateLatest,
		"mute_authors":             CreateAuthorMute(muteAuthors),
		"mute_urls":                CreateLinkMute(muteURLs),
	}
}

func CreateFilter(key string, value string, filters map[string]FilterFuncCreator) FilterFunc {
	f, ok := filters[key]
	if !ok {
		return nil
	}

	return f(value)
}

func filterApply(ctx context.Context, i *gofeed.Item, ff ...FilterFunc) bool {
	for _, f := range ff {
		if !f(ctx, i) {
			return false
		}
	}

	return true
}
