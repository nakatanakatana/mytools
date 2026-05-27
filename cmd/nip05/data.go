package main

import (
	"strings"
)

type NIP05Response struct {
	Names  map[string]string   `json:"names"`
	Relays map[string][]string `json:"relays,omitempty"`
}

func ParseRelays(relaysConfig map[string]string) map[string][]string {
	relays := make(map[string][]string)
	for k, v := range relaysConfig {
		// Split comma-separated relays
		list := strings.Split(v, ",")
		for i := range list {
			list[i] = strings.TrimSpace(list[i])
		}
		relays[k] = list
	}
	return relays
}
