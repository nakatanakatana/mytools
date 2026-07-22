package main

import (
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	HTTPReadTimeout  = 30 * time.Second
	HTTPWriteTimeout = 30 * time.Second
)

var latestOnlyFlag bool

func parseQueries(queries url.Values,
	filtersMap FilterFuncMap,
	modifiersMap ModifierFuncMap) ([]FilterFunc,
	[]ModifierFunc,
) {
	var filters []FilterFunc
	if latestOnlyFlag {
		filters = append(filters, CreateFilter("published_at.latest", "", filtersMap))
		filters = append(filters, CreateFilter("updated_at.latest", "", filtersMap))
	}

	f, m := ParseQueries(queries, filtersMap, modifiersMap)
	filters = append(filters, f...)

	return filters, m
}

func main() {
	muteAuthors := strings.Split(os.Getenv("MUTE_AUTHORS"), ",")
	muteURLs := strings.Split(os.Getenv("MUTE_URLS"), ",")

	latestOnly := os.Getenv("LATEST_ONLY")
	if latestOnly != "" {
		latestOnlyFlag = true
	}

	filtersMap := CreateFiltersMap(muteAuthors, muteURLs)
	modifiersMap := CreateModifierMap()

	handler := createHandler(filtersMap, modifiersMap)

	cacheMiddleware, err := NewCacheMiddleware(handler)
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", cacheMiddleware)

	server := http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  HTTPReadTimeout,
		WriteTimeout: HTTPWriteTimeout,
	}

	log.Fatal(server.ListenAndServe())
}
