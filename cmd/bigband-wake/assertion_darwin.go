//go:build darwin

package main

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <IOKit/pwr_mgt/IOPMLib.h>
#include <CoreFoundation/CoreFoundation.h>

// create_assertion wraps IOPMAssertionCreateWithName so we don't have to deal
// with CFString construction from cgo. The caller frees the returned id via
// release_assertion. Returns kIOReturnSuccess (0) on success.
static IOReturn create_assertion(const char *reason, IOPMAssertionID *out) {
    CFStringRef name = CFStringCreateWithCString(NULL, reason, kCFStringEncodingUTF8);
    if (name == NULL) {
        return kIOReturnNoMemory;
    }
    IOReturn r = IOPMAssertionCreateWithName(
        kIOPMAssertionTypePreventUserIdleSystemSleep,
        kIOPMAssertionLevelOn,
        name,
        out);
    CFRelease(name);
    return r;
}

static IOReturn release_assertion(IOPMAssertionID id) {
    return IOPMAssertionRelease(id);
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// createPMAssertion takes a PreventUserIdleSystemSleep assertion (equivalent
// to `caffeinate -i`) and returns its id. The id must be passed back to
// releasePMAssertion to drop the assertion — IOKit reference-counts these per
// process, so a leaked id keeps the system awake until bigband-wake exits.
func createPMAssertion(reason string) (uint32, error) {
	cReason := C.CString(reason)
	defer C.free(unsafe.Pointer(cReason))
	var id C.IOPMAssertionID
	ret := C.create_assertion(cReason, &id)
	if ret != 0 {
		return 0, fmt.Errorf("IOPMAssertionCreateWithName: IOReturn 0x%08x", uint32(ret))
	}
	return uint32(id), nil
}

// releasePMAssertion drops the assertion identified by id. Idempotent on the
// caller side — once released, the id is invalid; we never reuse it.
func releasePMAssertion(id uint32) error {
	ret := C.release_assertion(C.IOPMAssertionID(id))
	if ret != 0 {
		return fmt.Errorf("IOPMAssertionRelease: IOReturn 0x%08x", uint32(ret))
	}
	return nil
}
