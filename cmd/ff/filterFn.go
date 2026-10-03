package main

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
)

func equal(param, attr string) bool       { return attr == param }
func notEqual(param, attr string) bool    { return attr != param }
func contains(param, attr string) bool    { return strings.Contains(attr, param) }
func notContains(param, attr string) bool { return !strings.Contains(attr, param) }

func stringFilter(extract func(*gofeed.Item) string, predicate func(param, attr string) bool) FilterFuncCreator {
	return func(param string) FilterFunc {
		return func(_ context.Context, i *gofeed.Item) bool {
			return predicate(param, extract(i))
		}
	}
}

func authorFilter(predicate func(param, attr string) bool, nilResult bool) FilterFuncCreator {
	return func(param string) FilterFunc {
		return func(_ context.Context, i *gofeed.Item) bool {
			if i.Author == nil {
				return nilResult
			}

			return predicate(param, i.Author.Name)
		}
	}
}

var (
	TitleEqual       = stringFilter(func(i *gofeed.Item) string { return i.Title }, equal)
	DescriptionEqual = stringFilter(func(i *gofeed.Item) string { return i.Description }, equal)
	LinkEqual        = stringFilter(func(i *gofeed.Item) string { return i.Link }, equal)
	AuthorEqual      = authorFilter(equal, false)

	TitleNotEqual       = stringFilter(func(i *gofeed.Item) string { return i.Title }, notEqual)
	DescriptionNotEqual = stringFilter(func(i *gofeed.Item) string { return i.Description }, notEqual)
	LinkNotEqual        = stringFilter(func(i *gofeed.Item) string { return i.Link }, notEqual)
	AuthorNotEqual      = authorFilter(notEqual, true)

	TitleContains       = stringFilter(func(i *gofeed.Item) string { return i.Title }, contains)
	DescriptionContains = stringFilter(func(i *gofeed.Item) string { return i.Description }, contains)
	LinkContains        = stringFilter(func(i *gofeed.Item) string { return i.Link }, contains)
	AuthorContains      = authorFilter(contains, false)

	TitleNotContains       = stringFilter(func(i *gofeed.Item) string { return i.Title }, notContains)
	DescriptionNotContains = stringFilter(func(i *gofeed.Item) string { return i.Description }, notContains)
	LinkNotContains        = stringFilter(func(i *gofeed.Item) string { return i.Link }, notContains)
	AuthorNotContains      = authorFilter(notContains, true)
)

func timeFromFilter(extract func(*gofeed.Item) *time.Time) FilterFuncCreator {
	return func(param string) FilterFunc {
		parsed, err := time.Parse(time.RFC3339, param)
		if err != nil {
			return NilFilter(param)
		}

		return func(_ context.Context, i *gofeed.Item) bool {
			t := extract(i)

			return t == nil || parsed.Before(*t)
		}
	}
}

var (
	UpdateAtFrom    = timeFromFilter(func(i *gofeed.Item) *time.Time { return i.UpdatedParsed })
	PublishedAtFrom = timeFromFilter(func(i *gofeed.Item) *time.Time { return i.PublishedParsed })
)

func timeLatestFilter(extract func(*gofeed.Item) *time.Time) FilterFuncCreator {
	return func(_ string) FilterFunc {
		cutoff := time.Now().AddDate(0, 0, -7)

		return func(_ context.Context, i *gofeed.Item) bool {
			t := extract(i)

			return t == nil || cutoff.Before(*t)
		}
	}
}

var (
	UpdateAtLatest    = timeLatestFilter(func(i *gofeed.Item) *time.Time { return i.UpdatedParsed })
	PublishedAtLatest = timeLatestFilter(func(i *gofeed.Item) *time.Time { return i.PublishedParsed })
)

func DateLatest(_ string) FilterFunc {
	cutoff := time.Now().AddDate(0, 0, -7)

	return func(_ context.Context, i *gofeed.Item) bool {
		u := i.UpdatedParsed
		p := i.PublishedParsed

		if u == nil && p == nil {
			return true
		}

		return (u != nil && cutoff.Before(*u)) || (p != nil && cutoff.Before(*p))
	}
}

func NilFilter(_ string) FilterFunc {
	return func(_ context.Context, _ *gofeed.Item) bool {
		return true
	}
}

func Mute(params []string, attr string) bool {
	return !slices.ContainsFunc(params, func(p string) bool {
		return strings.Contains(attr, p)
	})
}

func CreateAuthorMute(targets []string) FilterFuncCreator {
	return func(_ string) FilterFunc {
		return func(_ context.Context, i *gofeed.Item) bool {
			return ((i.Author == nil) || Mute(targets, i.Author.Name)) &&
				((i.Author == nil) || Mute(targets, i.Author.Email)) &&
				Mute(targets, i.Link) &&
				Mute(targets, i.Title) &&
				Mute(targets, i.Description)
		}
	}
}

func CreateLinkMute(targets []string) FilterFuncCreator {
	return func(_ string) FilterFunc {
		return func(_ context.Context, i *gofeed.Item) bool {
			return Mute(targets, i.Link)
		}
	}
}
