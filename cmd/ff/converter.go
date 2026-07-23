package main

import (
	"github.com/gorilla/feeds"
	"github.com/mmcdole/gofeed"
)

func convertItem(i *gofeed.Item) *feeds.Item {
	if i == nil {
		return nil
	}

	item := &feeds.Item{
		Title:       i.Title,
		Link:        &feeds.Link{Href: i.Link},
		Description: i.Description,
		Id:          i.GUID,
		Content:     i.Content,
	}

	if i.Author != nil {
		item.Author = &feeds.Author{Name: i.Author.Name, Email: i.Author.Email}
	}

	if i.UpdatedParsed != nil {
		item.Updated = *i.UpdatedParsed
	}

	if i.PublishedParsed != nil {
		item.Created = *i.PublishedParsed
	}

	return item
}

func Convert(f *gofeed.Feed) *feeds.Feed {
	if f == nil {
		return &feeds.Feed{}
	}

	feed := &feeds.Feed{
		Title:       f.Title,
		Link:        &feeds.Link{Href: f.Link},
		Description: f.Description,
		Copyright:   f.Copyright,
	}

	if f.Author != nil {
		feed.Author = &feeds.Author{Name: f.Author.Name, Email: f.Author.Email}
	}

	if f.UpdatedParsed != nil {
		feed.Updated = *f.UpdatedParsed
	}

	if f.PublishedParsed != nil {
		feed.Created = *f.PublishedParsed
	}

	if f.Image != nil {
		feed.Image = &feeds.Image{Url: f.Image.URL, Title: f.Image.Title}
	}

	items := make([]*feeds.Item, 0, len(f.Items))

	for _, i := range f.Items {
		if item := convertItem(i); item != nil {
			items = append(items, item)
		}
	}

	feed.Items = items

	return feed
}
