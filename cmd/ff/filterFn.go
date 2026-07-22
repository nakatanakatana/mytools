package main

import (
	"context"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
)

// Equal.
func equal(param string, attr string) bool {
	return attr == param
}

func TitleEqual(param string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return equal(param, i.Title)
	}
}

func DescriptionEqual(param string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return equal(param, i.Description)
	}
}

func LinkEqual(param string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return equal(param, i.Link)
	}
}

func AuthorEqual(param string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return i.Author != nil && equal(param, i.Author.Name)
	}
}

// NotEqual.
func notEqual(param string, attr string) bool {
	return attr != param
}

func TitleNotEqual(param string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return notEqual(param, i.Title)
	}
}

func DescriptionNotEqual(param string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return notEqual(param, i.Description)
	}
}

func LinkNotEqual(param string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return notEqual(param, i.Link)
	}
}

func AuthorNotEqual(param string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return (i.Author == nil) || notEqual(param, i.Author.Name)
	}
}

// Contains.
func contains(param string, attr string) bool {
	return strings.Contains(attr, param)
}

func TitleContains(param string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return contains(param, i.Title)
	}
}

func DescriptionContains(param string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return contains(param, i.Description)
	}
}

func LinkContains(param string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return contains(param, i.Link)
	}
}

func AuthorContains(param string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return i.Author != nil && contains(param, i.Author.Name)
	}
}

// NotContains.
func notContains(param string, attr string) bool {
	return !strings.Contains(attr, param)
}

func TitleNotContains(param string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return notContains(param, i.Title)
	}
}

func DescriptionNotContains(param string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return notContains(param, i.Description)
	}
}

func LinkNotContains(param string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return notContains(param, i.Link)
	}
}

func AuthorNotContains(param string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return (i.Author == nil) || notContains(param, i.Author.Name)
	}
}

func From(param string, attr *time.Time) bool {
	parsedParam, err := time.Parse(time.RFC3339, param)
	// if parsed error, ignore this params
	if err != nil {
		return true
	}

	if attr == nil {
		return true
	}

	return parsedParam.Before(*attr)
}

func UpdateAtFrom(param string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return From(param, i.UpdatedParsed)
	}
}

func PublishedAtFrom(param string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return From(param, i.PublishedParsed)
	}
}

func Latest(_ string, attr *time.Time) bool {
	return From(time.Now().AddDate(0, 0, -7).Format(time.RFC3339), attr)
}

func UpdateAtLatest(_ string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return Latest("", i.UpdatedParsed)
	}
}

func PublishedAtLatest(_ string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return Latest("", i.PublishedParsed)
	}
}

func DateLatest(_ string) FilterFunc {
	return func(_ context.Context, i *gofeed.Item) bool {
		return Latest("", i.UpdatedParsed) || Latest("", i.PublishedParsed)
	}
}

func NilFilter(_ string) FilterFunc {
	return func(_ context.Context, _ *gofeed.Item) bool {
		return true
	}
}

// mute.
func Mute(params []string, attr string) bool {
	for _, p := range params {
		if strings.Contains(attr, p) {
			return false
		}
	}

	return true
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
