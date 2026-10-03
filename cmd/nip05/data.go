package main

import (
	"encoding/json"
	"strings"
)

type NIP05Response struct {
	Names  map[string]string   `json:"names"`
	Relays map[string][]string `json:"relays,omitempty"`
}

func ParseRelays(relaysConfig map[string]string) map[string][]string {
	relays := make(map[string][]string, len(relaysConfig))
	for k, v := range relaysConfig {
		list := strings.Split(v, ",")
		for i := range list {
			list[i] = strings.TrimSpace(list[i])
		}
		relays[k] = list
	}
	return relays
}

type MemoryProvider struct {
	responses map[string][]byte
}

func NewMemoryProvider(mapping map[string]string, relaysConfig map[string]string) (*MemoryProvider, error) {
	parsedRelays := ParseRelays(relaysConfig)
	responses := make(map[string][]byte, len(mapping)+1)

	fullResp := NIP05Response{
		Names:  mapping,
		Relays: parsedRelays,
	}
	fullBytes, err := json.Marshal(fullResp)
	if err != nil {
		return nil, err
	}
	responses[""] = fullBytes

	for name, pubkey := range mapping {
		userResp := NIP05Response{
			Names: map[string]string{name: pubkey},
		}
		if r, ok := parsedRelays[pubkey]; ok {
			userResp.Relays = map[string][]string{pubkey: r}
		}
		userBytes, err := json.Marshal(userResp)
		if err != nil {
			return nil, err
		}
		responses[name] = userBytes
	}

	return &MemoryProvider{responses: responses}, nil
}

func (p *MemoryProvider) GetJSON(name string) []byte {
	return p.responses[name]
}

