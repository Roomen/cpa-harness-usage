//go:build darwin && cgo

package quota

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <stdlib.h>
#include <string.h>
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>

#define HU_MAX_SECRET 262144

// hu_read_generic_password copies the first generic password for service
// into a malloc'd buffer. The caller wipes and frees it.
static OSStatus hu_read_generic_password(const char *service, void **out, size_t *out_len) {
	*out = NULL;
	*out_len = 0;
	CFStringRef svc = CFStringCreateWithCString(NULL, service, kCFStringEncodingUTF8);
	if (svc == NULL) {
		return errSecParam;
	}
	const void *keys[] = {kSecClass, kSecAttrService, kSecReturnData, kSecMatchLimit};
	const void *vals[] = {kSecClassGenericPassword, svc, kCFBooleanTrue, kSecMatchLimitOne};
	CFDictionaryRef query = CFDictionaryCreate(NULL, keys, vals, 4,
		&kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFRelease(svc);
	if (query == NULL) {
		return errSecParam;
	}
	CFTypeRef result = NULL;
	OSStatus status = SecItemCopyMatching(query, &result);
	CFRelease(query);
	if (status != errSecSuccess) {
		if (result != NULL) {
			CFRelease(result);
		}
		return status;
	}
	if (result == NULL || CFGetTypeID(result) != CFDataGetTypeID()) {
		if (result != NULL) {
			CFRelease(result);
		}
		return errSecDecode;
	}
	CFIndex n = CFDataGetLength((CFDataRef)result);
	if (n <= 0 || n > HU_MAX_SECRET) {
		CFRelease(result);
		return errSecDecode;
	}
	void *buf = malloc((size_t)n);
	if (buf == NULL) {
		CFRelease(result);
		return errSecAllocate;
	}
	memcpy(buf, CFDataGetBytePtr((CFDataRef)result), (size_t)n);
	CFRelease(result);
	*out = buf;
	*out_len = (size_t)n;
	return errSecSuccess;
}

static void hu_wipe_free(void *ptr, size_t len) {
	if (ptr != NULL) {
		memset(ptr, 0, len);
		free(ptr);
	}
}
*/
import "C"

import "unsafe"

const (
	errSecItemNotFound = -25300
	errSecDecode       = -26275
)

// systemKeychain reads the login keychain through the Security framework, so
// macOS shows its own authorization prompt the first time CPA asks.
var systemKeychain keychainReader = func(service string) ([]byte, error) {
	cService := C.CString(service)
	defer C.free(unsafe.Pointer(cService))
	var ptr unsafe.Pointer
	var size C.size_t
	status := C.hu_read_generic_password(cService, &ptr, &size)
	switch int(status) {
	case 0:
		defer C.hu_wipe_free(ptr, size)
		return C.GoBytes(ptr, C.int(size)), nil
	case errSecItemNotFound, errSecDecode:
		return nil, errNotConfigured
	default:
		// User canceled, authorization failed, interaction not allowed (no
		// GUI session), or a locked keychain.
		return nil, errKeychainDenied
	}
}
