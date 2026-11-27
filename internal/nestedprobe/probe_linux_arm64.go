package nestedprobe

import (
	"encoding/binary"
	"fmt"
	"unsafe"
)

// at guestBase: movz x0, #probeByte; movz x1, #2, lsl #16 (probeAddr);
// str x0, [x1]; b . -- the store to unmapped memory is an mmio exit. wfi
// is not used: kvm handles it in the kernel and need not return.
var guestCode = le32(0xd2800000|probeByte<<5, 0xd2a00041, 0xf9000020, 0x14000000)

// spinCode never exits, for the timeout test.
var spinCode = le32(0x14000000) // b .

var (
	kvmArmPreferredTarget = ior(kvmio, 0xaf, 32)
	kvmArmVcpuInit        = iow(kvmio, 0xae, 32)
	kvmSetOneReg          = iow(kvmio, 0xac, 16)
)

// the core register id of pc: KVM_REG_ARM64 | KVM_REG_SIZE_U64 |
// KVM_REG_ARM_CORE | its offset in kvm_regs (after 31 regs and sp) / 4.
const regPC = 0x6030000000100000 | (32*8)/4

func le32(ws ...uint32) []byte {
	b := make([]byte, 4*len(ws))
	for i, w := range ws {
		binary.LittleEndian.PutUint32(b[4*i:], w)
	}
	return b
}

func setupVcpu(vm, cpu int) error {
	var init [32]byte
	if _, err := ioctl(vm, kvmArmPreferredTarget, uintptr(unsafe.Pointer(&init))); err != nil {
		return fmt.Errorf("KVM_ARM_PREFERRED_TARGET: %w", err)
	}
	if _, err := ioctl(cpu, kvmArmVcpuInit, uintptr(unsafe.Pointer(&init))); err != nil {
		return fmt.Errorf("KVM_ARM_VCPU_INIT: %w", err)
	}
	pc := uint64(guestBase)
	reg := struct{ id, addr uint64 }{regPC, uint64(uintptr(unsafe.Pointer(&pc)))}
	if _, err := ioctl(cpu, kvmSetOneReg, uintptr(unsafe.Pointer(&reg))); err != nil {
		return fmt.Errorf("KVM_SET_ONE_REG pc: %w", err)
	}
	return nil
}

// checkExit wants the store: an mmio write of the byte to probeAddr.
func checkExit(run []byte) error {
	reason := u32(run, kvmRunExitReason)
	if reason != kvmExitMMIO {
		return fmt.Errorf("KVM_RUN: exit reason %d, want an mmio exit (%d)", reason, kvmExitMMIO)
	}
	e := kvmRunExit
	addr, data, length, write := u64(run, e), u64(run, e+8), u32(run, e+16), run[e+20]
	if addr != probeAddr || write != 1 || length != 8 || data != probeByte {
		return fmt.Errorf("KVM_RUN: mmio exit addr %#x write %d len %d data %#x, want a write of %#x to %#x", addr, write, length, data, probeByte, probeAddr)
	}
	return nil
}
