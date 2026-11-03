//go:build windows

package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

type dpapiProtector struct{}
type dpapiEnvelope struct {
	Version    int    `json:"version"`
	WrappedKey []byte `json:"wrappedKey"`
	Data       []byte `json:"data"`
}

func systemProtector() (protector, error) { return dpapiProtector{}, nil }
func (dpapiProtector) Scheme() string     { return "dpapi-aes-gcm-v1" }

func (dpapiProtector) Seal(identity string, plain []byte) ([]byte, error) {
	key := make([]byte, 32)
	defer clear(key)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	wrapped, err := dpapiKey(key, identity, false)
	if err != nil {
		return nil, err
	}
	data, err := sealAES(key, identity, plain)
	if err != nil {
		return nil, err
	}
	return json.Marshal(dpapiEnvelope{Version: 1, WrappedKey: wrapped, Data: data})
}

func (dpapiProtector) Open(identity string, encrypted []byte) ([]byte, error) {
	if len(encrypted) > maxEnvelopeBytes {
		return nil, ErrStoreCorrupt
	}
	var record dpapiEnvelope
	if json.Unmarshal(encrypted, &record) != nil || record.Version != 1 ||
		len(record.WrappedKey) == 0 || len(record.WrappedKey) > 64<<10 ||
		len(record.Data) == 0 || len(record.Data) > maxCredentialBytes+64 {
		return nil, ErrStoreCorrupt
	}
	key, err := dpapiKey(record.WrappedKey, identity, true)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	if len(key) != 32 {
		return nil, ErrStoreCorrupt
	}
	return openAES(key, identity, record.Data)
}

// DPAPI protects only the random record key, in the current user's scope.
// UI_FORBIDDEN prevents an unexpected credential prompt. LOCAL_MACHINE is
// deliberately absent: another account must not decrypt these credentials.
func dpapiKey(input []byte, identity string, decrypt bool) ([]byte, error) {
	entropy := sha256.Sum256([]byte("agentcfg.oauth.dpapi.v1\x00" + identity))
	in := windows.DataBlob{Size: uint32(len(input)), Data: &input[0]}
	extra := windows.DataBlob{Size: uint32(len(entropy)), Data: &entropy[0]}
	var out windows.DataBlob
	const uiForbidden uint32 = 1
	var err error
	if decrypt {
		err = windows.CryptUnprotectData(&in, nil, &extra, 0, nil, uiForbidden, &out)
	} else {
		name, _ := windows.UTF16PtrFromString("agentcfg OAuth credential key")
		err = windows.CryptProtectData(&in, name, &extra, 0, nil, uiForbidden, &out)
	}
	runtime.KeepAlive(input)
	runtime.KeepAlive(entropy)
	if err != nil {
		return nil, fmt.Errorf("Windows DPAPI credential-key operation: %w", err)
	}
	if out.Data == nil {
		return nil, fmt.Errorf("Windows DPAPI returned no credential-key data")
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(out.Data))))
	if out.Size == 0 || out.Size > 64<<10 {
		return nil, fmt.Errorf("Windows DPAPI returned invalid credential-key size")
	}
	native := unsafe.Slice(out.Data, int(out.Size))
	defer clear(native)
	return append([]byte(nil), native...), nil
}

func privatePath(path string, directory bool) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("resolve Windows credential owner: %w", err)
	}
	flags := ""
	if directory {
		flags = "OICI"
	}
	sddl := "D:P(A;" + flags + ";FA;;;" + user.User.Sid.String() + ")(A;" + flags + ";FA;;;SY)"
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	runtime.KeepAlive(descriptor)
	if err != nil {
		return fmt.Errorf("restrict Windows credential permissions: %w", err)
	}
	return nil
}

// Confidentiality is enforced by per-user DPAPI (or the explicit external AES
// key). Newly created paths additionally receive the protected DACL above.
func checkPrivatePath(_ string, _ os.FileInfo) error { return nil }
