//go:build windows

package whp

import (
	"fmt"
	"unsafe"
)

// QueryGpaRangeDirtyBitmap returns the dirty-page bitmap for the given
// guest physical address range. The range must have been mapped with
// MapGpaTrack — otherwise WHP rejects the call with
// WHV_E_INVALID_PARAMETER (0xc0350005). The partition itself must also
// have dirty-page tracking enabled at SetupPartition time (the property
// is set implicitly by the MapGpaTrack flag on at least one range).
//
// Layout: one bit per 4 KiB guest page, little-endian inside each uint64
// word, words concatenated in ascending GPA order. For a 256 MiB range
// (65 536 pages) the bitmap is 1 024 uint64 words = 8 192 bytes.
//
// Reference: WinHvPlatform.h (Windows 11 24H2 SDK, 10.0.26100):
//
//	HRESULT WINAPI WHvQueryGpaRangeDirtyBitmap(
//	    WHV_PARTITION_HANDLE Partition,
//	    WHV_GUEST_PHYSICAL_ADDRESS GuestAddress,
//	    UINT64 RangeSizeInBytes,
//	    UINT64* Bitmap,                  // _Out_writes
//	    UINT32 BitmapSizeInBytes);
//
// The HRESULT is wrapped as whp.HResult on a non-S_OK return.
//
// sizeBytes must be a non-zero multiple of the 4 KiB page size; the
// caller is responsible for choosing a range that matches a prior
// MapGpaRange call exactly (partial ranges are tolerated but the
// dirty state for pages outside the original map is undefined).
func QueryGpaRangeDirtyBitmap(h PartitionHandle, gpa uint64, sizeBytes uint64) ([]uint64, error) {
	if err := loadDLL(); err != nil {
		return nil, err
	}
	const pageSize = 4096
	if sizeBytes == 0 {
		return nil, fmt.Errorf("whp.QueryGpaRangeDirtyBitmap: sizeBytes must be > 0")
	}
	if sizeBytes%pageSize != 0 {
		return nil, fmt.Errorf("whp.QueryGpaRangeDirtyBitmap: sizeBytes=%d not a multiple of 4 KiB", sizeBytes)
	}
	pages := sizeBytes / pageSize
	words := (pages + 63) / 64
	buf := make([]uint64, words)
	// BitmapSizeInBytes is the 5th argument and a UINT32 — the SDK header
	// is explicit. We pass words*8 because that is the byte length of
	// the uint64 array we allocated; WHP will write at most
	// ceil(pages/8) bytes into it.
	hr, _, _ := procQueryDirtyBitmap.Call(
		uintptr(h),
		uintptr(gpa),
		uintptr(sizeBytes),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(uint32(words*8)),
	)
	if HResult(hr) != sOK {
		return nil, HResult(hr)
	}
	return buf, nil
}
