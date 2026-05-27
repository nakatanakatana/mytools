package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type FileGenerator struct {
	tempDir string
	files   map[string]string // Name -> FilePath, empty string key for full
}

func NewFileGenerator(mapping map[string]string, relaysConfig map[string]string) (*FileGenerator, error) {
	tempDir, err := os.MkdirTemp("", "nip05-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}

	fg := &FileGenerator{
		tempDir: tempDir,
		files:   make(map[string]string),
	}

	parsedRelays := ParseRelays(relaysConfig)

	// 1. Generate full file
	fullResp := NIP05Response{
		Names:  mapping,
		Relays: parsedRelays,
	}
	fullPath, err := fg.writeJSON("", fullResp)
	if err != nil {
		fg.Cleanup()
		return nil, err
	}
	fg.files[""] = fullPath

	// 2. Generate individual user files
	for name, pubkey := range mapping {
		userResp := NIP05Response{
			Names: map[string]string{name: pubkey},
		}
		// Include relays for this pubkey if they exist
		if r, ok := parsedRelays[pubkey]; ok {
			userResp.Relays = map[string][]string{pubkey: r}
		}
		
		path, err := fg.writeJSON(name, userResp)
		if err != nil {
			fg.Cleanup()
			return nil, err
		}
		fg.files[name] = path
	}

	return fg, nil
}

func (fg *FileGenerator) writeJSON(name string, resp NIP05Response) (string, error) {
	var fileName string
	if name == "" {
		fileName = "full.json"
	} else {
		// Simple sanitization for file name
		fileName = fmt.Sprintf("user_%s.json", name)
	}

	filePath := filepath.Join(fg.tempDir, fileName)
	f, err := os.Create(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to create file %s: %w", fileName, err)
	}
	defer func() {
		_ = f.Close()
	}()

	if err := json.NewEncoder(f).Encode(resp); err != nil {
		return "", fmt.Errorf("failed to encode JSON to %s: %w", fileName, err)
	}

	return filePath, nil
}

func (fg *FileGenerator) GetFilePath(name string) string {
	return fg.files[name]
}

func (fg *FileGenerator) Cleanup() {
	if fg.tempDir != "" {
		_ = os.RemoveAll(fg.tempDir)
	}
}
