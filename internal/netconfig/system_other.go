//go:build !windows

package netconfig

func systemProxy() (string, error) { return "", nil }
