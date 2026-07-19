package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"fiatjaf.com/nostr"
	relaystore "github.com/nakatanakatana/mytools/cmd/nostr-relay/store"
)

const (
	managementMediaType = "application/nostr+json+rpc"
	managementBodyLimit = 64 << 10
)

var supportedManagementMethods = []string{
	"allowpubkey", "unallowpubkey", "listallowedpubkeys",
}

type managementRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type managementResponse struct {
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

type allowedPubKey struct {
	PubKey string `json:"pubkey"`
	Reason string `json:"reason"`
}

func NewManagementHandler(next http.Handler, store relaystore.Store, auth NIP98Validator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentTypes := r.Header.Values("Content-Type")
		if r.Method != http.MethodPost || len(contentTypes) != 1 || contentTypes[0] != managementMediaType {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Content-Type", managementMediaType)
		body, err := io.ReadAll(io.LimitReader(r.Body, managementBodyLimit+1))
		if err != nil {
			writeManagementResponse(w, http.StatusBadRequest, managementResponse{Error: "failed to read request body"})
			return
		}
		if len(body) > managementBodyLimit {
			writeManagementResponse(w, http.StatusRequestEntityTooLarge, managementResponse{Error: "request body too large"})
			return
		}
		if err := auth.Validate(r, body); err != nil {
			writeManagementResponse(w, http.StatusUnauthorized, managementResponse{Error: "unauthorized"})
			return
		}

		var request managementRequest
		decoder := json.NewDecoder(bytes.NewReader(body))
		if err := decoder.Decode(&request); err != nil || request.Method == "" || request.Params == nil {
			writeManagementResponse(w, http.StatusOK, managementResponse{Error: "invalid request"})
			return
		}
		if err := ensureJSONEOF(decoder); err != nil {
			writeManagementResponse(w, http.StatusOK, managementResponse{Error: "invalid request"})
			return
		}

		result, err := dispatchManagement(r, store, request)
		if err != nil {
			writeManagementResponse(w, http.StatusOK, managementResponse{Error: err.Error()})
			return
		}
		writeManagementResponse(w, http.StatusOK, managementResponse{Result: result})
	})
}

func dispatchManagement(r *http.Request, store relaystore.Store, request managementRequest) (any, error) {
	switch request.Method {
	case "supportedmethods":
		if err := decodeManagementParams(request.Params); err != nil {
			return nil, err
		}
		return supportedManagementMethods, nil
	case "allowpubkey":
		pubkey, reason, err := decodePubKeyReason(request.Params)
		if err != nil {
			return nil, err
		}
		if err := store.AllowPublisher(r.Context(), relaystore.Publisher{PubKey: pubkey, Reason: reason, CreatedAt: time.Now()}); err != nil {
			return nil, errors.New("management operation failed")
		}
		return true, nil
	case "unallowpubkey":
		pubkey, _, err := decodePubKeyReason(request.Params)
		if err != nil {
			return nil, err
		}
		if err := store.UnallowPublisher(r.Context(), pubkey); err != nil {
			return nil, errors.New("management operation failed")
		}
		return true, nil
	case "listallowedpubkeys":
		if err := decodeManagementParams(request.Params); err != nil {
			return nil, err
		}
		publishers, err := store.ListPublishers(r.Context())
		if err != nil {
			return nil, errors.New("management operation failed")
		}
		result := make([]allowedPubKey, len(publishers))
		for i, publisher := range publishers {
			result[i] = allowedPubKey{PubKey: publisher.PubKey.Hex(), Reason: publisher.Reason}
		}
		return result, nil
	default:
		return nil, errors.New("method not supported")
	}
}

func decodeManagementParams(raw json.RawMessage) error {
	var params []json.RawMessage
	if err := json.Unmarshal(raw, &params); err != nil || params == nil || len(params) != 0 {
		return errors.New("invalid params")
	}
	return nil
}

func decodePubKeyReason(raw json.RawMessage) (nostr.PubKey, string, error) {
	var params []json.RawMessage
	if err := json.Unmarshal(raw, &params); err != nil || len(params) < 1 || len(params) > 2 {
		return nostr.PubKey{}, "", errors.New("invalid params")
	}
	var encoded, reason string
	if err := json.Unmarshal(params[0], &encoded); err != nil {
		return nostr.PubKey{}, "", errors.New("invalid params")
	}
	if len(params) == 2 {
		if err := json.Unmarshal(params[1], &reason); err != nil {
			return nostr.PubKey{}, "", errors.New("invalid params")
		}
	}
	pubkey, err := nostr.PubKeyFromHex(encoded)
	if err != nil {
		return nostr.PubKey{}, "", errors.New("invalid params")
	}
	return pubkey, reason, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	return errors.New("trailing JSON data")
}

func writeManagementResponse(w http.ResponseWriter, status int, response managementResponse) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}
