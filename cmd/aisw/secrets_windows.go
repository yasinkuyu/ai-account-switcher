package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Windows Credential Manager through advapi32 (part of every Windows install).

var (
	advapi32       = syscall.NewLazyDLL("advapi32.dll")
	procCredReadW  = advapi32.NewProc("CredReadW")
	procCredWriteW = advapi32.NewProc("CredWriteW")
	procCredDelete = advapi32.NewProc("CredDeleteW")
	procCredFree   = advapi32.NewProc("CredFree")
)

const (
	credTypeGeneric         = 1
	credPersistLocalMachine = 2
	errorNotFound           = 1168
)

type credential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        syscall.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

func credTarget(service, account string) string { return service + ":" + account }

func secretGet(service, account string) (string, string, bool, error) {
	target, err := syscall.UTF16PtrFromString(credTarget(service, account))
	if err != nil {
		return "", "", false, err
	}
	var pcred *credential
	r, _, e := procCredReadW.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&pcred)))
	if r == 0 {
		if errno, ok := e.(syscall.Errno); ok && errno == errorNotFound {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("credential read %s: %v", service, e)
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(pcred)))
	blob := make([]byte, pcred.CredentialBlobSize)
	if pcred.CredentialBlobSize > 0 {
		copy(blob, unsafe.Slice(pcred.CredentialBlob, pcred.CredentialBlobSize))
	}
	return string(blob), account, true, nil
}

func secretPut(service, account, value string) error {
	target, err := syscall.UTF16PtrFromString(credTarget(service, account))
	if err != nil {
		return err
	}
	user, _ := syscall.UTF16PtrFromString(account)
	data := []byte(value)
	c := credential{Type: credTypeGeneric, TargetName: target, UserName: user,
		Persist: credPersistLocalMachine, CredentialBlobSize: uint32(len(data))}
	if len(data) > 0 {
		c.CredentialBlob = &data[0]
	}
	r, _, e := procCredWriteW.Call(uintptr(unsafe.Pointer(&c)), 0)
	if r == 0 {
		return fmt.Errorf("credential write %s: %v", service, e)
	}
	return nil
}

func secretDel(service, account string) error {
	target, err := syscall.UTF16PtrFromString(credTarget(service, account))
	if err != nil {
		return err
	}
	procCredDelete.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0)
	return nil
}

func secretStoreName() string { return "Windows Credential Manager" }

// osKeyring* access entries other tools keep in the OS keyring. On this
// platform that is the same store aisw uses for its own profiles.
func osKeyringGet(service, account string) (string, bool, error) {
	v, _, ok, err := secretGet(service, account)
	return v, ok, err
}

func osKeyringPut(service, account, value string) error {
	return secretPutVerified(service, account, value)
}

func osKeyringDel(service, account string) error { return secretDel(service, account) }
