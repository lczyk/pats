package nestedprobe

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// the linux ioctl encoding (asm-generic, which amd64 and arm64 use).
func ioc(dir, typ, nr, size uintptr) uintptr { return dir<<30 | size<<16 | typ<<8 | nr }
func io_(typ, nr uintptr) uintptr            { return ioc(0, typ, nr, 0) }
func iow(typ, nr, size uintptr) uintptr      { return ioc(1, typ, nr, size) }
func ior(typ, nr, size uintptr) uintptr      { return ioc(2, typ, nr, size) }

const (
	kvmio   = 0xAE
	vhostio = 0xAF
)

var (
	kvmGetAPIVersion       = io_(kvmio, 0x00)
	kvmCreateVM            = io_(kvmio, 0x01)
	kvmGetVcpuMmapSize     = io_(kvmio, 0x04)
	kvmCreateVcpu          = io_(kvmio, 0x41)
	kvmSetUserMemoryRegion = iow(kvmio, 0x46, 32)
	kvmRun                 = io_(kvmio, 0x80)

	vhostSetOwner         = io_(vhostio, 0x01)
	vhostVsockSetGuestCID = iow(vhostio, 0x60, 8)
)

// the kvm_run fields the probe reads.
const (
	kvmRunExitReason = 8
	kvmRunExit       = 32 // the exit union
	kvmExitIO        = 2
	kvmExitMMIO      = 6
)

// guest memory: one page at guestBase, the code at its start.
const guestBase = 0x1000

// the value the guest code hands back, and where (an io port on amd64, an
// unmapped address on arm64).
const (
	probeByte = 0x42
	probePort = 0x10
	probeAddr = 0x20000
)

// runTimeout bounds the vcpu run: a vcpu that never exits is a failure.
var runTimeout = 10 * time.Second

func ioctl(fd int, req, arg uintptr) (uintptr, error) {
	r, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, arg)
	if e != 0 {
		return r, e
	}
	return r, nil
}

func openDev(path string) (int, string, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err == nil {
		return fd, "", nil
	}
	if errors.Is(err, syscall.ENOENT) {
		return -1, Missing, fmt.Errorf("no %s", path)
	}
	return -1, Failed, fmt.Errorf("open %s: %w", path, err)
}

func checkKVM() (string, error) {
	kvm, v, err := openDev("/dev/kvm")
	if err != nil {
		return v, err
	}
	keep := false
	leak := func() { keep = true }
	release := func(f func()) {
		if !keep {
			f()
		}
	}
	defer release(func() { syscall.Close(kvm) })
	fail := func(what string, err error) (string, error) { return Failed, fmt.Errorf("%s: %w", what, err) }
	if api, err := ioctl(kvm, kvmGetAPIVersion, 0); err != nil || api != 12 {
		return fail("KVM_GET_API_VERSION", fmt.Errorf("%d, %v", api, err))
	}
	vmfd, err := ioctl(kvm, kvmCreateVM, 0)
	if err != nil {
		return fail("KVM_CREATE_VM", err)
	}
	vm := int(vmfd)
	defer release(func() { syscall.Close(vm) })
	mem, err := syscall.Mmap(-1, 0, os.Getpagesize(), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_ANONYMOUS)
	if err != nil {
		return fail("mmap guest memory", err)
	}
	defer release(func() { syscall.Munmap(mem) })
	copy(mem, guestCode)
	region := struct {
		slot, flags                       uint32
		guestPhys, size, userspaceAddress uint64
	}{0, 0, guestBase, uint64(len(mem)), uint64(uintptr(unsafe.Pointer(&mem[0])))}
	if _, err := ioctl(vm, kvmSetUserMemoryRegion, uintptr(unsafe.Pointer(&region))); err != nil {
		return fail("KVM_SET_USER_MEMORY_REGION", err)
	}
	cpufd, err := ioctl(vm, kvmCreateVcpu, 0)
	if err != nil {
		return fail("KVM_CREATE_VCPU", err)
	}
	cpu := int(cpufd)
	defer release(func() { syscall.Close(cpu) })
	size, err := ioctl(kvm, kvmGetVcpuMmapSize, 0)
	if err != nil {
		return fail("KVM_GET_VCPU_MMAP_SIZE", err)
	}
	run, err := syscall.Mmap(cpu, 0, int(size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return fail("mmap kvm_run", err)
	}
	defer release(func() { syscall.Munmap(run) })
	if err := setupVcpu(vm, cpu); err != nil {
		return Failed, err
	}
	err = runVcpu(cpu)
	if errors.Is(err, errRunTimeout) {
		// a vcpu that would not stop is still in the kernel: leave its fds
		// and mappings to the process's end rather than pull them from
		// under it.
		leak()
		return Failed, err
	}
	if err != nil {
		return fail("KVM_RUN", err)
	}
	if err := checkExit(run); err != nil {
		return Failed, err
	}
	return OK, nil
}

var errRunTimeout = errors.New("KVM_RUN: the vcpu did not exit in time")

// runVcpu runs KVM_RUN on a thread of its own, under runTimeout: past it, a
// signal to that thread makes KVM_RUN return EINTR, and the run stops. a
// vcpu the signals cannot stop either is left running: the probe is a
// process of its own, which prints its line and exits, and that ends it.
func runVcpu(cpu int) error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGUSR1)
	defer signal.Stop(sig)
	var stop atomic.Bool
	tid := make(chan int, 1)
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		tid <- syscall.Gettid()
		for {
			_, err := ioctl(cpu, kvmRun, 0)
			if errors.Is(err, syscall.EINTR) {
				if stop.Load() {
					done <- errRunTimeout
					return
				}
				continue
			}
			done <- err
			return
		}
	}()
	t := <-tid
	select {
	case err := <-done:
		return err
	case <-time.After(runTimeout):
	}
	stop.Store(true)
	for range 50 {
		_ = syscall.Tgkill(os.Getpid(), t, syscall.SIGUSR1)
		select {
		case err := <-done:
			return err
		case <-time.After(20 * time.Millisecond):
		}
	}
	return errRunTimeout
}

func checkVhostVsock() (string, error) {
	if _, err := os.Stat("/dev/vhost-vsock"); errors.Is(err, os.ErrNotExist) {
		// lxd loads it itself when it starts a vm.
		if out, err := exec.Command("modprobe", "vhost_vsock").CombinedOutput(); err != nil {
			return Missing, fmt.Errorf("no /dev/vhost-vsock, and modprobe vhost_vsock: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	var last error
	for range 5 {
		fd, v, err := openDev("/dev/vhost-vsock")
		if err != nil {
			return v, err
		}
		if _, err := ioctl(fd, vhostSetOwner, 0); err != nil {
			syscall.Close(fd)
			return Failed, fmt.Errorf("VHOST_SET_OWNER: %w", err)
		}
		cid := randCID()
		_, err = ioctl(fd, vhostVsockSetGuestCID, uintptr(unsafe.Pointer(&cid)))
		syscall.Close(fd)
		if err == nil {
			return OK, nil
		}
		last = fmt.Errorf("VHOST_VSOCK_SET_GUEST_CID %d: %w", cid, err)
		if !errors.Is(err, syscall.EADDRINUSE) {
			return Failed, last
		}
	}
	return Failed, last
}

// randCID is a guest cid as lxd picks one: past the reserved 0-2, under
// the u32 max (VMADDR_CID_ANY).
func randCID() uint64 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return 3 + uint64(binary.LittleEndian.Uint32(b[:])%(1<<31))
}

func checkVsock() (string, error)    { return openOnly("/dev/vsock") }
func checkVhostNet() (string, error) { return openOnly("/dev/vhost-net") }

func openOnly(path string) (string, error) {
	fd, v, err := openDev(path)
	if err != nil {
		return v, err
	}
	syscall.Close(fd)
	return OK, nil
}

func u32(b []byte, off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }
func u64(b []byte, off int) uint64 { return binary.LittleEndian.Uint64(b[off:]) }
