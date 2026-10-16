//go:build windows

package cookies

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func platformDPAPIUnprotect(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, fmt.Errorf("empty DPAPI blob")
	}
	in := windows.DataBlob{
		Size: uint32(len(blob)),
		Data: &blob[0],
	}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, 0, &out); err != nil {
		return nil, err
	}
	defer func() {
		if out.Data != nil {
			_, _ = windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(out.Data))))
		}
	}()
	if out.Size == 0 || out.Data == nil {
		return nil, fmt.Errorf("DPAPI returned empty key")
	}
	raw := unsafe.Slice(out.Data, out.Size)
	key := make([]byte, len(raw))
	copy(key, raw)
	return key, nil
}
