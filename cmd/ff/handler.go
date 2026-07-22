package main

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/mmcdole/gofeed"
)

var (
	ErrMustSetURL        = errors.New("must set URL")
	ErrCannotSetMultiURL = errors.New("cannot set multiple URL")
)

func parseAndValidateURL(r *http.Request) (string, error) {
	queries := r.URL.Query()

	upstream, ok := queries["url"]
	if !ok {
		return "", ErrMustSetURL
	}

	if len(upstream) != 1 {
		return "", ErrCannotSetMultiURL
	}

	return upstream[0], nil
}

func createHandler(filtersMap FilterFuncMap, modifiersMap ModifierFuncMap) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)

			return
		}

		u, err := parseAndValidateURL(r)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, err)

			return
		}

		fp := gofeed.NewParser()

		originFeed, err := fp.ParseURL(u)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintln(w, fmt.Errorf("ParseURL Error: %w", err))

			return
		}

		filters, modifiers := parseQueries(r.URL.Query(), filtersMap, modifiersMap)

		filteredFeed, err := Apply(r.Context(), originFeed, filters, modifiers)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintln(w, err)

			return
		}

		c := Convert(filteredFeed)

		rss, err := c.ToRss()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintln(w, err)

			return
		}

		w.WriteHeader(http.StatusOK)

		if r.Method == http.MethodHead {
			return
		}

		fmt.Fprintln(w, rss) // #nosec G705
	}
}
