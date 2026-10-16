//go:build !windows

package cookies

import "fmt"

func platformDPAPIUnprotect(_ []byte) ([]byte, error) {
	return nil, fmt.Errorf("DPAPI is only supported on Windows")
}
