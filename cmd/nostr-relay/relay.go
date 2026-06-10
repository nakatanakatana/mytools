package main

import (
	"fiatjaf.com/nostr/eventstore/slicestore"
	"fiatjaf.com/nostr/khatru"
)

const softwareURL = "https://github.com/nakatanakatana/mytools"

func NewRelay(cfg Config) (*khatru.Relay, func(), error) {
	store := &slicestore.SliceStore{}
	if err := store.Init(); err != nil {
		return nil, nil, err
	}

	relay := khatru.NewRelay()
	relay.Info.Name = cfg.Name
	relay.Info.Description = cfg.Description
	relay.Info.Software = softwareURL
	relay.Info.Version = "dev"
	relay.Info.AddSupportedNIPs([]int{1, 11})
	relay.UseEventstore(store, cfg.MaxQueryLimit)

	return relay, store.Close, nil
}
