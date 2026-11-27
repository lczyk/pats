package nestedprobe

import (
	"encoding/binary"
	"fmt"
	"unsafe"
)

// real mode at guestBase: mov al, probeByte; out probePort, al; hlt.
var guestCode = []byte{0xB0, probeByte, 0xE6, probePort, 0xF4}

// spinCode never exits, for the timeout test.
var spinCode = []byte{0xEB, 0xFE} // jmp .

var (
	kvmGetRegs  = ior(kvmio, 0x81, 144)
	kvmSetRegs  = iow(kvmio, 0x82, 144)
	kvmGetSregs = ior(kvmio, 0x83, 312)
	kvmSetSregs = iow(kvmio, 0x84, 312)
)

// setupVcpu puts the vcpu in real mode with cs at 0 and rip at the code.
func setupVcpu(_, cpu int) error {
	var sregs [312]byte
	if _, err := ioctl(cpu, kvmGetSregs, uintptr(unsafe.Pointer(&sregs))); err != nil {
		return fmt.Errorf("KVM_GET_SREGS: %w", err)
	}
	// cs is the first kvm_segment: base u64 at 0, selector u16 at 12.
	binary.LittleEndian.PutUint64(sregs[0:], 0)
	binary.LittleEndian.PutUint16(sregs[12:], 0)
	if _, err := ioctl(cpu, kvmSetSregs, uintptr(unsafe.Pointer(&sregs))); err != nil {
		return fmt.Errorf("KVM_SET_SREGS: %w", err)
	}
	var regs [144]byte
	if _, err := ioctl(cpu, kvmGetRegs, uintptr(unsafe.Pointer(&regs))); err != nil {
		return fmt.Errorf("KVM_GET_REGS: %w", err)
	}
	binary.LittleEndian.PutUint64(regs[128:], guestBase) // rip
	binary.LittleEndian.PutUint64(regs[136:], 2)         // rflags, its fixed bit
	if _, err := ioctl(cpu, kvmSetRegs, uintptr(unsafe.Pointer(&regs))); err != nil {
		return fmt.Errorf("KVM_SET_REGS: %w", err)
	}
	return nil
}

// checkExit wants the out: an io exit, out, one byte, the port and the byte.
func checkExit(run []byte) error {
	reason := u32(run, kvmRunExitReason)
	if reason != kvmExitIO {
		return fmt.Errorf("KVM_RUN: exit reason %d, want an io exit (%d)", reason, kvmExitIO)
	}
	e := kvmRunExit
	dir, size, port := run[e], run[e+1], binary.LittleEndian.Uint16(run[e+2:])
	data := run[u64(run, e+8)]
	if dir != 1 || size != 1 || port != probePort || data != probeByte {
		return fmt.Errorf("KVM_RUN: io exit dir %d size %d port %#x data %#x, want out of %#x to %#x", dir, size, port, data, probeByte, probePort)
	}
	return nil
}
