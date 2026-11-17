//go:build windows

package netconfig

import (
	"encoding/binary"
	"errors"
	"syscall"
)

func systemProxy() (string, error) {
	path, _ := syscall.UTF16PtrFromString(`Software\Microsoft\Windows\CurrentVersion\Internet Settings`)
	var key syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, path, 0, syscall.KEY_QUERY_VALUE, &key); err != nil {
		return "", nil
	}
	defer syscall.RegCloseKey(key)
	enabled, err := registryValue(key, "ProxyEnable")
	if err != nil || len(enabled) < 4 || binary.LittleEndian.Uint32(enabled) != 1 {
		return "", nil
	}
	data, err := registryValue(key, "ProxyServer")
	if err != nil {
		return "", errors.New("the Windows proxy is enabled but its address could not be read")
	}
	characters := make([]uint16, len(data)/2)
	for index := range characters {
		characters[index] = binary.LittleEndian.Uint16(data[index*2:])
	}
	return parseSystemProxy(syscall.UTF16ToString(characters)), nil
}

func registryValue(key syscall.Handle, name string) ([]byte, error) {
	encoded, _ := syscall.UTF16PtrFromString(name)
	var size uint32
	if err := syscall.RegQueryValueEx(key, encoded, nil, nil, nil, &size); err != nil {
		return nil, err
	}
	if size > 65536 {
		return nil, errors.New("Windows proxy setting is too large")
	}
	data := make([]byte, size)
	if len(data) == 0 {
		return data, nil
	}
	if err := syscall.RegQueryValueEx(key, encoded, nil, nil, &data[0], &size); err != nil {
		return nil, err
	}
	return data[:size], nil
}
