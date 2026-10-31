package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
)

// Platform secret stores implement these (secrets_darwin.go, secrets_windows.go,
// secrets_other.go):
//
//	secretGet(service, account) (value, foundAccount string, ok bool, err error)
//	secretPut(service, account, value string) error
//	secretDel(service, account) error
//
// An empty account in secretGet matches any account for that service.

// ownService names aisw's own entries in the OS credential store.
// AISW_SERVICE overrides it (used by the tests to stay away from real data).
var ownService = func() string {
	if s := os.Getenv("AISW_SERVICE"); s != "" {
		return s
	}
	return "aisw"
}()

const chunkSize = 1200 // base64 chars per item; fits every platform's limit

func secretPutVerified(service, account, value string) error {
	if err := secretPut(service, account, value); err != nil {
		return err
	}
	got, _, ok, err := secretGet(service, account)
	if err != nil || !ok || got != value {
		return fmt.Errorf("could not verify secret %s/%s after writing", service, account)
	}
	return nil
}

func chunkKey(key string, i int) string { return fmt.Sprintf("%s#%d", key, i) }

// blobPut stores a snapshot in the OS secret store, split into chunks.
func blobPut(key string, snap Snapshot) (int, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return 0, err
	}
	b64 := base64.StdEncoding.EncodeToString(raw)
	n := 0
	for i := 0; i < len(b64); i += chunkSize {
		end := i + chunkSize
		if end > len(b64) {
			end = len(b64)
		}
		if err := secretPutVerified(ownService, chunkKey(key, n), b64[i:end]); err != nil {
			return 0, err
		}
		n++
	}
	// drop leftover chunks of an older, larger snapshot
	for i := n; ; i++ {
		_, _, ok, _ := secretGet(ownService, chunkKey(key, i))
		if !ok {
			break
		}
		_ = secretDel(ownService, chunkKey(key, i))
	}
	return n, nil
}

func blobGet(key string, n int) (Snapshot, error) {
	b64 := ""
	for i := 0; i < n; i++ {
		v, _, ok, err := secretGet(ownService, chunkKey(key, i))
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("profile data missing from secret store (%s)", chunkKey(key, i))
		}
		b64 += v
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	var snap Snapshot
	return snap, json.Unmarshal(raw, &snap)
}

func blobDel(key string, n int) {
	for i := 0; i < n+5; i++ {
		_ = secretDel(ownService, chunkKey(key, i))
	}
}
