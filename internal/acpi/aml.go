package acpi

import (
	"encoding/binary"
	"fmt"
	"strings"
)

type amlBuilder struct {
	buf []byte
}

func (b *amlBuilder) bytes() []byte {
	return append([]byte(nil), b.buf...)
}

func appendPkgLength(dst []byte, payloadLen int, includeSelf bool) []byte {
	lengthLen := 1
	switch {
	case payloadLen < (1<<6)-1:
		lengthLen = 1
	case payloadLen < (1<<12)-2:
		lengthLen = 2
	case payloadLen < (1<<20)-3:
		lengthLen = 3
	default:
		lengthLen = 4
	}
	length := payloadLen
	if includeSelf {
		length += lengthLen
	}
	switch lengthLen {
	case 1:
		return append(dst, byte(length))
	case 2:
		return append(dst, byte((1<<6)|(length&0x0f)), byte((length>>4)&0xff))
	case 3:
		return append(dst, byte((2<<6)|(length&0x0f)), byte((length>>4)&0xff), byte((length>>12)&0xff))
	default:
		return append(dst, byte((3<<6)|(length&0x0f)), byte((length>>4)&0xff), byte((length>>12)&0xff), byte((length>>20)&0xff))
	}
}

func encodePath(name string) ([]byte, error) {
	root := strings.HasPrefix(name, "\\")
	if root {
		name = strings.TrimPrefix(name, "\\")
	}
	if name == "" {
		return nil, fmt.Errorf("aml path is empty")
	}
	parts := strings.Split(name, ".")
	for _, part := range parts {
		if len(part) != 4 {
			return nil, fmt.Errorf("aml path part %q must be 4 chars", part)
		}
	}
	var out []byte
	if root {
		out = append(out, '\\')
	}
	switch len(parts) {
	case 1:
	case 2:
		out = append(out, 0x2E)
	default:
		out = append(out, 0x2F, byte(len(parts)))
	}
	for _, part := range parts {
		out = append(out, []byte(part)...)
	}
	return out, nil
}

func appendName(dst []byte, path string, value []byte) ([]byte, error) {
	pathBytes, err := encodePath(path)
	if err != nil {
		return nil, err
	}
	dst = append(dst, 0x08)
	dst = append(dst, pathBytes...)
	dst = append(dst, value...)
	return dst, nil
}

func encodeEISA(name string) ([]byte, error) {
	if len(name) != 7 {
		return nil, fmt.Errorf("invalid EISA name %q", name)
	}
	data := []byte(name)
	value := ((uint32(data[0]-0x40) << 26) |
		(uint32(data[1]-0x40) << 21) |
		(uint32(data[2]-0x40) << 16) |
		(mustHex(name[3]) << 12) |
		(mustHex(name[4]) << 8) |
		(mustHex(name[5]) << 4) |
		mustHex(name[6]))
	value = (value&0xFF)<<24 | (value&0xFF00)<<8 | (value&0xFF0000)>>8 | (value>>24)
	out := []byte{0x0C, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(out[1:], value)
	return out, nil
}

func mustHex(ch byte) uint32 {
	switch {
	case ch >= '0' && ch <= '9':
		return uint32(ch - '0')
	case ch >= 'a' && ch <= 'f':
		return uint32(ch-'a') + 10
	case ch >= 'A' && ch <= 'F':
		return uint32(ch-'A') + 10
	default:
		return 0
	}
}

func encodeString(v string) []byte {
	out := make([]byte, 0, len(v)+2)
	out = append(out, 0x0D)
	out = append(out, []byte(v)...)
	out = append(out, 0)
	return out
}

func encodeByte(v byte) []byte {
	return []byte{0x0A, v}
}

func encodeWord(v uint16) []byte {
	out := []byte{0x0B, 0, 0}
	binary.LittleEndian.PutUint16(out[1:], v)
	return out
}

func encodeDWord(v uint32) []byte {
	out := []byte{0x0C, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(out[1:], v)
	return out
}

func buildResourceTemplate(descriptors ...[]byte) []byte {
	var payload []byte
	for _, desc := range descriptors {
		payload = append(payload, desc...)
	}
	payload = append(payload, 0x79, 0x00)
	lenObj := encodeInteger(uint64(len(payload)))
	tmp := append(lenObj, payload...)
	out := []byte{0x11}
	out = appendPkgLength(out, len(tmp), true)
	out = append(out, tmp...)
	return out
}

func encodeInteger(v uint64) []byte {
	switch v {
	case 0:
		return []byte{0x00}
	case 1:
		return []byte{0x01}
	case 0xFFFFFFFFFFFFFFFF:
		return []byte{0xFF}
	}
	if v <= 0xFF {
		return encodeByte(byte(v))
	}
	if v <= 0xFFFF {
		return encodeWord(uint16(v))
	}
	if v <= 0xFFFFFFFF {
		return encodeDWord(uint32(v))
	}
	out := []byte{0x0E, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint64(out[1:], v)
	return out
}

func resMemory32Fixed(readWrite bool, base uint32, length uint32) []byte {
	out := make([]byte, 12)
	out[0] = 0x86
	binary.LittleEndian.PutUint16(out[1:3], 9)
	if readWrite {
		out[3] = 1
	}
	binary.LittleEndian.PutUint32(out[4:8], base)
	binary.LittleEndian.PutUint32(out[8:12], length)
	return out
}

func resIO(min uint16, max uint16, alignment uint8, length uint8) []byte {
	out := make([]byte, 8)
	out[0] = 0x47
	out[1] = 1
	binary.LittleEndian.PutUint16(out[2:4], min)
	binary.LittleEndian.PutUint16(out[4:6], max)
	out[6] = alignment
	out[7] = length
	return out
}

func resInterrupt(consumer, edgeTriggered, activeLow, shared bool, number uint32) []byte {
	out := make([]byte, 9)
	out[0] = 0x89
	binary.LittleEndian.PutUint16(out[1:3], 6)
	flags := byte(0)
	if shared {
		flags |= 1 << 3
	}
	if activeLow {
		flags |= 1 << 2
	}
	if edgeTriggered {
		flags |= 1 << 1
	}
	if consumer {
		flags |= 1
	}
	out[3] = flags
	out[4] = 1
	binary.LittleEndian.PutUint32(out[5:9], number)
	return out
}

func appendDevice(dst []byte, path string, children ...[]byte) ([]byte, error) {
	pathBytes, err := encodePath(path)
	if err != nil {
		return nil, err
	}
	var payload []byte
	payload = append(payload, pathBytes...)
	for _, child := range children {
		payload = append(payload, child...)
	}
	dst = append(dst, 0x5B, 0x82)
	dst = appendPkgLength(dst, len(payload), true)
	dst = append(dst, payload...)
	return dst, nil
}

func appendScope(dst []byte, path string, children ...[]byte) ([]byte, error) {
	pathBytes, err := encodePath(path)
	if err != nil {
		return nil, err
	}
	var payload []byte
	payload = append(payload, pathBytes...)
	for _, child := range children {
		payload = append(payload, child...)
	}
	dst = append(dst, 0x10)
	dst = appendPkgLength(dst, len(payload), true)
	dst = append(dst, payload...)
	return dst, nil
}

func appendMethod(dst []byte, path string, args byte, serialized bool, children ...[]byte) ([]byte, error) {
	pathBytes, err := encodePath(path)
	if err != nil {
		return nil, err
	}
	var payload []byte
	payload = append(payload, pathBytes...)
	flags := args & 0x7
	if serialized {
		flags |= 1 << 3
	}
	payload = append(payload, flags)
	for _, child := range children {
		payload = append(payload, child...)
	}
	dst = append(dst, 0x14)
	dst = appendPkgLength(dst, len(payload), true)
	dst = append(dst, payload...)
	return dst, nil
}

func encodeReturn(value []byte) []byte {
	return append([]byte{0xA4}, value...)
}

func buildDSDT(mmio []MMIODevice) ([]byte, error) {
	com1HID, err := encodeEISA("PNP0501")
	if err != nil {
		return nil, err
	}
	ps2HID, err := encodeEISA("PNP0303")
	if err != nil {
		return nil, err
	}
	var children [][]byte
	{
		var devChildren [][]byte
		var name []byte
		name, err = appendName(nil, "_HID", com1HID)
		if err != nil {
			return nil, err
		}
		devChildren = append(devChildren, name)
		name, err = appendName(nil, "_UID", encodeByte(0))
		if err != nil {
			return nil, err
		}
		devChildren = append(devChildren, name)
		name, err = appendName(nil, "_DDN", encodeString("COM1"))
		if err != nil {
			return nil, err
		}
		devChildren = append(devChildren, name)
		name, err = appendName(nil, "_CRS", buildResourceTemplate(
			resInterrupt(true, true, false, false, 4),
			resIO(0x3F8, 0x3F8, 0, 8),
		))
		if err != nil {
			return nil, err
		}
		devChildren = append(devChildren, name)
		dev, err := appendDevice(nil, "COM1", devChildren...)
		if err != nil {
			return nil, err
		}
		children = append(children, dev)
	}
	{
		var devChildren [][]byte
		var name []byte
		name, err = appendName(nil, "_HID", ps2HID)
		if err != nil {
			return nil, err
		}
		devChildren = append(devChildren, name)
		method, err := appendMethod(nil, "_STA", 0, false, encodeReturn(encodeByte(0x0F)))
		if err != nil {
			return nil, err
		}
		devChildren = append(devChildren, method)
		name, err = appendName(nil, "_CRS", buildResourceTemplate(
			resIO(0x0060, 0x0060, 0, 1),
			resIO(0x0064, 0x0064, 0, 1),
			resInterrupt(true, true, false, false, 1),
		))
		if err != nil {
			return nil, err
		}
		devChildren = append(devChildren, name)
		dev, err := appendDevice(nil, "PS2_", devChildren...)
		if err != nil {
			return nil, err
		}
		children = append(children, dev)
	}

	for i, dev := range mmio {
		nameSeg := fmt.Sprintf("V%03d", i)
		var devChildren [][]byte
		hid, err := appendName(nil, "_HID", encodeString("LNRO0005"))
		if err != nil {
			return nil, err
		}
		devChildren = append(devChildren, hid)
		uid, err := appendName(nil, "_UID", encodeInteger(uint64(i)))
		if err != nil {
			return nil, err
		}
		devChildren = append(devChildren, uid)
		cca, err := appendName(nil, "_CCA", []byte{0x01})
		if err != nil {
			return nil, err
		}
		devChildren = append(devChildren, cca)
		crs, err := appendName(nil, "_CRS", buildResourceTemplate(
			resMemory32Fixed(true, uint32(dev.Addr), uint32(dev.Len)),
			resInterrupt(true, true, false, false, dev.GSI),
		))
		if err != nil {
			return nil, err
		}
		devChildren = append(devChildren, crs)
		amlDev, err := appendDevice(nil, nameSeg, devChildren...)
		if err != nil {
			return nil, err
		}
		children = append(children, amlDev)
	}

	var out []byte
	out, err = appendScope(nil, "\\_SB_", children...)
	if err != nil {
		return nil, err
	}
	return out, nil
}
